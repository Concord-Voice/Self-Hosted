#!/usr/bin/env bash
# Verify every shipped proxy overwrites client address headers, then probe
# representative HTTP, WebSocket, and admin locations through real nginx.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
python3 - "$root" <<'PY'
import http.client
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

root = Path(sys.argv[1])
vhost = (root / "infrastructure/deploy/nginx/concordvoice.conf").read_text()
admin = (root / "infrastructure/deploy/nginx/admin-console.conf.template").read_text()


def check_headers(source, name):
    proxy_count = len(re.findall(r"(?m)^\s*proxy_pass\s+[^;]+;", source))
    assert proxy_count, f"{name}: no proxy locations found"
    for header in ("X-Real-IP", "X-Forwarded-For"):
        values = re.findall(rf"(?m)^\s*proxy_set_header\s+{header}\s+([^;]+);", source)
        assert len(values) == proxy_count, f"{name}: {header} missing or duplicated"
        assert all(value.strip() == "$remote_addr" for value in values), f"{name}: {header} trusts caller input"


check_headers(vhost, "public vhost")
check_headers(admin, "admin vhost")
assert not re.search(r"(?m)^\s*(?:set_real_ip_from|real_ip_header|real_ip_recursive)\b", vhost), "generic real-IP restoration remains"
try:
    check_headers(vhost.replace("$remote_addr;", "$proxy_add_x_forwarded_for;", 1), "mutated vhost")
except AssertionError:
    pass
else:
    raise AssertionError("address-header check is vacuous")
print("PASS: all proxy locations overwrite address headers; mutation rejected")

nginx = shutil.which("nginx")
if not nginx:
    if os.environ.get("CI"):
        raise SystemExit("FAIL: nginx required in CI for live proxy proof")
    print("SKIP: live nginx probe (nginx unavailable locally)")
    raise SystemExit(0)


def location_headers(source, marker):
    start = source.find(marker)
    assert start >= 0, f"missing {marker}"
    end = source.find("\n    }", start)
    assert end > start, f"unclosed {marker}"
    block = source[start:end]
    lines = re.findall(r"(?m)^\s*proxy_set_header\s+[^;]+;", block)
    assert len(lines) >= 4, f"incomplete {marker} proxy headers"
    return "\n".join(line.replace("${ADMIN_DOMAIN}", "admin.example.test") for line in lines)


class Echo(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({key.lower(): self.headers.get_all(key) for key in ("X-Real-IP", "X-Forwarded-For")}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass


upstream = ThreadingHTTPServer(("127.0.0.1", 0), Echo)
threading.Thread(target=upstream.serve_forever, daemon=True).start()
with socket.socket() as reserve:
    reserve.bind(("127.0.0.1", 0))
    listen_port = reserve.getsockname()[1]

with tempfile.TemporaryDirectory(prefix="cv-trusted-proxy-") as temporary:
    temp = Path(temporary)
    conf = temp / "nginx.conf"
    conf.write_text(f"""worker_processes 1;
pid {temp}/nginx.pid;
error_log {temp}/error.log;
events {{ worker_connections 128; }}
http {{
    map $host $validated_host {{ default $host; }}
    map $http_upgrade $validated_upgrade {{ default $http_upgrade; }}
    map $validated_upgrade $connection_upgrade {{ default upgrade; }}
    upstream control_plane {{ server 127.0.0.1:{upstream.server_port}; }}
    upstream media_plane {{ server 127.0.0.1:{upstream.server_port}; }}
    server {{
        listen 127.0.0.1:{listen_port};
        location ^~ /api/v1/updates/ {{
            proxy_pass http://control_plane;
            {location_headers(vhost, '    location ^~ /api/v1/updates/ {')}
        }}
        location /socket.io/ {{
            proxy_pass http://media_plane;
            {location_headers(vhost, '    location /socket.io/ {')}
        }}
        location /admin/ {{
            proxy_pass http://control_plane;
            {location_headers(admin, '    location /admin/ {')}
        }}
    }}
}}
""")
    subprocess.run([nginx, "-p", f"{temp}/", "-c", str(conf), "-t"], check=True, capture_output=True)
    process = subprocess.Popen([nginx, "-p", f"{temp}/", "-c", str(conf), "-g", "daemon off;"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        for _ in range(50):
            try:
                connection = http.client.HTTPConnection("127.0.0.1", listen_port, timeout=1)
                connection.request("GET", "/api/v1/updates/probe", headers={"Host": "example.test"})
                connection.getresponse().read()
                connection.close()
                break
            except OSError:
                time.sleep(0.1)
        else:
            raise AssertionError("nginx did not start")

        for path in ("/api/v1/updates/probe", "/socket.io/?EIO=4&transport=websocket", "/admin/probe"):
            connection = http.client.HTTPConnection("127.0.0.1", listen_port, timeout=3)
            headers = {"Host": "example.test", "X-Forwarded-For": "198.51.100.12", "X-Real-IP": "198.51.100.13", "CF-Connecting-IP": "198.51.100.14"}
            if path.startswith("/socket.io/"):
                headers.update({"Upgrade": "websocket", "Connection": "Upgrade"})
            connection.request("GET", path, headers=headers)
            response = connection.getresponse()
            body = json.loads(response.read())
            connection.close()
            assert response.status == 200, f"{path}: HTTP {response.status}"
            assert body == {"x-real-ip": ["127.0.0.1"], "x-forwarded-for": ["127.0.0.1"]}, f"{path}: {body}"
        print("PASS: forged address headers discarded through nginx HTTP, WebSocket, and admin locations")
    finally:
        process.terminate()
        process.wait(timeout=5)
        upstream.shutdown()
PY
