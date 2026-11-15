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
import ssl
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

root = Path(sys.argv[1])
vhost = (root / "infrastructure/deploy/nginx/concordvoice.conf").read_text()
aop = (root / "infrastructure/deploy/nginx/cloudflare-aop-client-ip.conf").read_text()
admin = (root / "infrastructure/deploy/nginx/admin-console.conf.template").read_text()
ctl = (root / "infrastructure/deploy/concord-ctl.sh").read_text()


def check_headers(source, name):
    proxy_count = len(re.findall(r"(?m)^\s*proxy_pass\s+[^;]+;", source))
    assert proxy_count, f"{name}: no proxy locations found"
    for header in ("X-Real-IP", "X-Forwarded-For"):
        values = re.findall(rf"(?m)^\s*proxy_set_header\s+{header}\s+([^;]+);", source)
        assert len(values) == proxy_count, f"{name}: {header} missing or duplicated"
        assert all(value.strip() == "$remote_addr" for value in values), f"{name}: {header} trusts caller input"


check_headers(vhost, "public vhost")
check_headers(admin, "admin vhost")
assert vhost.count("    # CONCORD_CLIENT_IP_PROVENANCE") == 1, "client-IP provenance marker must be API-only and unique"
assert "# HTTPS — Media Plane" in vhost and vhost.index("CONCORD_CLIENT_IP_PROVENANCE") < vhost.index("# HTTPS — Media Plane"), "media must not receive AOP"
assert admin.count("    # CONCORD_ADMIN_CLIENT_IP_PROVENANCE") == 1, "admin client-IP provenance marker must be unique"
assert "if ($ssl_server_name != $server_name) { return 444; }" in admin, "admin must bind TLS SNI to its configured server name before AOP real-IP restoration"
admin_renderer = re.search(r"(?ms)^    render_admin_config\(\) \{(.*?)^    \}", ctl)
assert admin_renderer, "managed admin renderer missing"
assert 'validate_aop_fragment && aop_ca_safe || return 1' in admin_renderer.group(1), "admin renderer must fail closed unless the complete AOP fragment and fixed CA validate"
assert "CONCORD_ADMIN_CLIENT_IP_PROVENANCE" in admin_renderer.group(1), "admin renderer does not consume the provenance marker"
assert "provenance_markers != 1" in admin_renderer.group(1), "admin renderer must reject missing or duplicate provenance markers"
assert re.search(r"(?m)^ssl_client_certificate /etc/concord/nginx/cloudflare-aop-ca\.pem;$", aop), "AOP CA path changed"
assert [line for line in aop.splitlines() if line and not line.startswith("#")] == [
    "ssl_client_certificate /etc/concord/nginx/cloudflare-aop-ca.pem;",
    "ssl_verify_client on;",
    "ssl_verify_depth 2;",
    "set_real_ip_from 0.0.0.0/0;",
    "set_real_ip_from ::/0;",
    "real_ip_header CF-Connecting-IP;",
    "real_ip_recursive off;",
], "AOP real-IP restoration must remain inseparable from required mTLS"
api_start = vhost.index("# HTTPS — API + WebSocket Hub")
media_start = vhost.index("# HTTPS — Media Plane")
api_block = vhost[api_start:media_start]
assert 'map "$ssl_server_name:$validated_host" $api_tls_name_match {' in vhost, "API SNI/Host guard map missing"
tls_map = re.search(r'map "\$ssl_server_name:\$validated_host" \$api_tls_name_match \{(?P<body>.*?)^\}', vhost, re.MULTILINE | re.DOTALL)
assert tls_map, "API SNI/Host guard map is unterminated"
tls_pairs = set(re.findall(r'^\s*"([^"]+)"\s+1;$', tls_map.group("body"), re.MULTILINE))
assert tls_pairs == {
    "api.concordvoice.chat:api.concordvoice.chat",
    "concordvoice.chat:concordvoice.chat",
    "api.concordvoice.chat:concordvoice.chat",
    "concordvoice.chat:api.concordvoice.chat",
}, f"API SNI/Host guard allow-list drifted: {tls_pairs}"
assert "if ($api_tls_name_match = 0) { return 444; }" in api_block, "API must reject Host routing after a different TLS SNI handshake"
assert "if ($api_tls_name_match = 0) { return 444; }" not in vhost[media_start:], "media must not receive the API SNI/Host guard"
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

openssl = shutil.which("openssl")
if not openssl:
    if os.environ.get("CI"):
        raise SystemExit("FAIL: openssl required in CI for live TLS SNI/Host proof")
    print("SKIP: live TLS SNI/Host probe (openssl unavailable locally)")
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
    access_log {temp}/access.log;
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
    syntax = subprocess.run([nginx, "-p", f"{temp}/", "-c", str(conf), "-t"], capture_output=True, text=True)
    assert syntax.returncode == 0, f"nginx fixture config refused: {syntax.stderr[-1200:]}"
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


def openssl_command(*arguments):
    result = subprocess.run([openssl, *arguments], capture_output=True, text=True)
    assert result.returncode == 0, f"openssl {' '.join(arguments[:2])} failed: {result.stderr[-1200:]}"


with tempfile.TemporaryDirectory(prefix="cv-aop-sni-host-") as temporary:
    temp = Path(temporary)
    ca_key = temp / "ca.key"
    ca_cert = temp / "ca.pem"
    server_key = temp / "server.key"
    server_csr = temp / "server.csr"
    server_cert = temp / "server.pem"
    client_key = temp / "client.key"
    client_csr = temp / "client.csr"
    client_cert = temp / "client.pem"
    openssl_command("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", str(ca_key), "-out", str(ca_cert), "-subj", "/CN=Concord AOP test CA", "-days", "1")
    openssl_command("req", "-newkey", "rsa:2048", "-nodes", "-keyout", str(server_key), "-out", str(server_csr), "-subj", "/CN=api.example.test")
    openssl_command("x509", "-req", "-in", str(server_csr), "-CA", str(ca_cert), "-CAkey", str(ca_key), "-CAcreateserial", "-out", str(server_cert), "-days", "1")
    openssl_command("req", "-newkey", "rsa:2048", "-nodes", "-keyout", str(client_key), "-out", str(client_csr), "-subj", "/CN=Cloudflare AOP test client")
    openssl_command("x509", "-req", "-in", str(client_csr), "-CA", str(ca_cert), "-CAkey", str(ca_key), "-CAcreateserial", "-out", str(client_cert), "-days", "1")

    with socket.socket() as reserve:
        reserve.bind(("127.0.0.1", 0))
        tls_port = reserve.getsockname()[1]
    conf = temp / "nginx.conf"
    conf.write_text(f"""worker_processes 1;
pid {temp}/nginx.pid;
error_log {temp}/error.log;
events {{ worker_connections 128; }}
http {{
    access_log {temp}/access.log;
    map $host $validated_host {{ default ""; api.example.test api.example.test; apex.example.test apex.example.test; }}
    map \"$ssl_server_name:$validated_host\" $api_tls_name_match {{
        default 0;
        \"api.example.test:api.example.test\" 1;
        \"apex.example.test:apex.example.test\" 1;
        \"api.example.test:apex.example.test\" 1;
        \"apex.example.test:api.example.test\" 1;
    }}
    server {{
        listen 127.0.0.1:{tls_port} ssl default_server;
        server_name api.example.test apex.example.test;
        ssl_certificate {server_cert};
        ssl_certificate_key {server_key};
        ssl_client_certificate {ca_cert};
        ssl_verify_client on;
        set_real_ip_from 0.0.0.0/0;
        set_real_ip_from ::/0;
        real_ip_header CF-Connecting-IP;
        real_ip_recursive off;
        if ($api_tls_name_match = 0) {{ return 444; }}
        location / {{ return 204; }}
    }}
    server {{
        listen 127.0.0.1:{tls_port} ssl;
        server_name media.example.test;
        ssl_certificate {server_cert};
        ssl_certificate_key {server_key};
        location / {{ return 204; }}
    }}
    server {{
        listen 127.0.0.1:{tls_port} ssl;
        server_name turn.example.test;
        ssl_certificate {server_cert};
        ssl_certificate_key {server_key};
        location / {{ return 204; }}
    }}
    server {{
        listen 127.0.0.1:{tls_port} ssl;
        server_name admin.example.test;
        ssl_certificate {server_cert};
        ssl_certificate_key {server_key};
        ssl_client_certificate {ca_cert};
        ssl_verify_client on;
        set_real_ip_from 0.0.0.0/0;
        set_real_ip_from ::/0;
        real_ip_header CF-Connecting-IP;
        real_ip_recursive off;
        if ($ssl_server_name != $server_name) {{ return 444; }}
        location / {{ return 204; }}
    }}
}}
""")
    syntax = subprocess.run([nginx, "-p", f"{temp}/", "-c", str(conf), "-t"], capture_output=True, text=True)
    assert syntax.returncode == 0, f"nginx TLS fixture config refused: {syntax.stderr[-1200:]}"
    process = subprocess.Popen([nginx, "-p", f"{temp}/", "-c", str(conf), "-g", "daemon off;"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        for _ in range(50):
            try:
                with socket.create_connection(("127.0.0.1", tls_port), timeout=0.1):
                    break
            except OSError:
                time.sleep(0.1)
        else:
            raise AssertionError("TLS nginx did not start")

        def tls_request(sni, host):
            client = ssl.create_default_context(cafile=ca_cert)
            client.check_hostname = False
            client.load_cert_chain(certfile=client_cert, keyfile=client_key)
            with socket.create_connection(("127.0.0.1", tls_port), timeout=3) as raw:
                with client.wrap_socket(raw, server_hostname=sni) as connection:
                    connection.sendall(
                        f"GET / HTTP/1.1\r\nHost: {host}\r\nCF-Connecting-IP: 198.51.100.14\r\nConnection: close\r\n\r\n".encode()
                    )
                    return connection.recv(4096)

        for sni, host in (
            ("api.example.test", "api.example.test"),
            ("api.example.test", "apex.example.test"),
            ("apex.example.test", "api.example.test"),
            ("apex.example.test", "apex.example.test"),
        ):
            response = tls_request(sni, host)
            assert response.startswith(b"HTTP/1.1 204"), \
                f"same-policy API SNI/Host pair {sni}/{host} rejected: {response[:120]!r}"

        for label, sni in (
            ("media", "media.example.test"),
            ("TURN", "turn.example.test"),
            ("unknown", "unknown.example.test"),
            ("no-SNI", None),
        ):
            response = tls_request(sni, "api.example.test")
            # nginx can issue 421 for a server-name mismatch before the API
            # block's 444 guard. Either way, the request must not reach API.
            assert not response.startswith(b"HTTP/1.1 204"), \
                f"{label}-SNI/API-Host bypass was accepted: {response[:120]!r}"

        response = tls_request("admin.example.test", "admin.example.test")
        assert response.startswith(b"HTTP/1.1 204"), f"valid admin AOP request rejected: {response[:120]!r}"
        for label, sni in (
            ("media", "media.example.test"),
            ("TURN", "turn.example.test"),
            ("unknown", "unknown.example.test"),
            ("no-SNI", None),
        ):
            response = tls_request(sni, "admin.example.test")
            assert not response.startswith(b"HTTP/1.1 204"), \
                f"{label}-SNI/admin-Host bypass was accepted: {response[:120]!r}"
        print("PASS: API and admin reject media, TURN, unknown, and no-SNI Host-routing after TLS")
    finally:
        process.terminate()
        process.wait(timeout=5)
PY
