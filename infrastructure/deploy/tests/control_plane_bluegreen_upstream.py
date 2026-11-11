#!/usr/bin/env python3
"""Stub control-plane upstream and test clients for the nginx blue/green proof.

Standard library only. Driven by test-control-plane-bluegreen-nginx.sh.

serve --port N --name blue|green
    GET /health answers 200 with the body <name>. GET /api/v1/ws with a
    WebSocket upgrade does the RFC 6455 handshake, sends one text frame
    "<name>", then echoes every text frame back unmasked. SIGTERM follows the
    shape of Go's http.Server.Shutdown: close the listener, hang up idle
    keep-alive connections, let in-flight requests finish, keep open
    WebSockets for up to 5 s, then exit 0.
ws-client --port N [--path P]
    Opens a WebSocket and prints "hello <first frame>". Each stdin line is sent
    as a masked text frame and the reply is printed as "echo <text>". On EOF it
    sends a close frame and exits 0. Any failure prints "ERR <reason>", exit 1.
get --port N
    One GET /health; prints "<status> <body>" or "ERR <reason>".
probe --port N --mode fresh|keepalive --out FILE --stop FILE
    GET /health in a loop until the stop file exists, one line per request as
    for get. fresh opens a new connection per request; keepalive reuses one.
    A reused connection the server closed while idle is retried once on a new
    connection, as HTTP clients do for idempotent requests; the number of such
    retries is the last line, prefixed with "#".
ports-free PORT...
    Exits 1 and names each loopback port that cannot be bound.
"""

import argparse
import base64
import hashlib
import http.client
import os
import selectors
import signal
import socket
import struct
import sys
import threading
import time

WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
WS_PATH = "/api/v1/ws"
DRAIN_SECONDS = 5.0
OP_TEXT, OP_CLOSE, OP_PING, OP_PONG = 0x1, 0x8, 0x9, 0xA


def ws_accept(key):
    return base64.b64encode(hashlib.sha1((key + WS_GUID).encode()).digest()).decode()


def ws_send(sock, opcode, payload, mask):
    head = bytearray([0x80 | opcode])
    bit = 0x80 if mask else 0
    n = len(payload)
    if n < 126:
        head.append(bit | n)
    elif n < 1 << 16:
        head.append(bit | 126)
        head += struct.pack("!H", n)
    else:
        head.append(bit | 127)
        head += struct.pack("!Q", n)
    if mask:
        key = os.urandom(4)
        head += key
        payload = bytes(b ^ key[i % 4] for i, b in enumerate(payload))
    sock.sendall(bytes(head) + payload)


def read_exact(rfile, n):
    data = rfile.read(n)
    if data is None or len(data) != n:
        raise ConnectionError("connection closed mid-frame")
    return data


def ws_recv(rfile):
    """Read one frame; return (opcode, unmasked payload). Our peers never fragment."""
    b0, b1 = read_exact(rfile, 2)
    n = b1 & 0x7F
    if n == 126:
        (n,) = struct.unpack("!H", read_exact(rfile, 2))
    elif n == 127:
        (n,) = struct.unpack("!Q", read_exact(rfile, 8))
    key = read_exact(rfile, 4) if b1 & 0x80 else None
    payload = read_exact(rfile, n)
    if key:
        payload = bytes(b ^ key[i % 4] for i, b in enumerate(payload))
    return b0 & 0x0F, payload


def read_headers(rfile):
    headers = {}
    while True:
        line = rfile.readline(65537)
        if not line:
            raise ConnectionError("connection closed in headers")
        if line in (b"\r\n", b"\n"):
            return headers
        name, _, value = line.decode("latin-1").partition(":")
        headers[name.strip().lower()] = value.strip()


def hangup(conn):
    try:
        conn.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass


class Upstream:
    def __init__(self, port, name):
        self.name = name
        self.listener = socket.create_server(("127.0.0.1", port))
        self.cond = threading.Condition()
        self.conns = {}  # socket -> "idle" | "active" | "ws"
        self.draining = False

    def set_state(self, conn, state):
        """Record the connection state; refuse to go idle once draining."""
        with self.cond:
            if state == "idle" and self.draining:
                return False
            self.conns[conn] = state
            return True

    def handle(self, conn):
        rfile = conn.makefile("rb")
        try:
            while self.set_state(conn, "idle"):
                line = rfile.readline(65537)
                if not line:
                    break
                self.set_state(conn, "active")
                parts = line.decode("latin-1").split()
                headers = read_headers(rfile)
                length = int(headers.get("content-length", "0") or 0)
                if length:
                    read_exact(rfile, length)
                if len(parts) != 3:
                    break
                method, path, version = parts
                path = path.split("?", 1)[0]
                upgrade = headers.get("upgrade", "").lower() == "websocket"
                if method == "GET" and path == WS_PATH and upgrade and "sec-websocket-key" in headers:
                    self.set_state(conn, "ws")
                    self.websocket(conn, rfile, headers["sec-websocket-key"])
                    break
                if method == "GET" and path == "/health":
                    status, body = "200 OK", self.name.encode()
                else:
                    status, body = "404 Not Found", b"not found"
                close = (
                    version != "HTTP/1.1"
                    or headers.get("connection", "").lower() == "close"
                    or self.draining
                )
                head = (
                    f"HTTP/1.1 {status}\r\nContent-Type: text/plain\r\n"
                    f"Content-Length: {len(body)}\r\n"
                    + ("Connection: close\r\n" if close else "")
                    + "\r\n"
                )
                conn.sendall(head.encode() + body)
                if close:
                    break
        except (OSError, ValueError):
            pass
        finally:
            with self.cond:
                self.conns.pop(conn, None)
                self.cond.notify_all()
            rfile.close()
            conn.close()

    def websocket(self, conn, rfile, key):
        conn.sendall(
            (
                "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"
                f"Connection: Upgrade\r\nSec-WebSocket-Accept: {ws_accept(key)}\r\n\r\n"
            ).encode()
        )
        ws_send(conn, OP_TEXT, self.name.encode(), mask=False)
        while True:
            opcode, payload = ws_recv(rfile)
            if opcode == OP_TEXT:
                ws_send(conn, OP_TEXT, payload, mask=False)
            elif opcode == OP_PING:
                ws_send(conn, OP_PONG, payload, mask=False)
            elif opcode == OP_CLOSE:
                ws_send(conn, OP_CLOSE, payload[:2], mask=False)
                return

    def serve(self):
        wake_r, wake_w = socket.socketpair()
        wake_r.setblocking(False)
        wake_w.setblocking(False)
        signal.set_wakeup_fd(wake_w.fileno(), warn_on_full_buffer=False)
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, lambda *_: None)
        sel = selectors.DefaultSelector()
        sel.register(self.listener, selectors.EVENT_READ)
        sel.register(wake_r, selectors.EVENT_READ)
        stopping = False
        while not stopping:
            for key, _ in sel.select():
                if key.fileobj is wake_r:
                    stopping = True
                    break
                try:
                    conn, _ = self.listener.accept()
                except OSError:
                    continue
                threading.Thread(target=self.handle, args=(conn,), daemon=True).start()
        self.shutdown()

    def shutdown(self):
        self.listener.close()
        deadline = time.monotonic() + DRAIN_SECONDS
        with self.cond:
            self.draining = True
            for conn, state in self.conns.items():
                if state == "idle":
                    hangup(conn)
            while self.conns:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    break
                self.cond.wait(remaining)
            for conn in list(self.conns):
                hangup(conn)
        print(f"{self.name}: stopped", file=sys.stderr, flush=True)


def one_line(status, body):
    text = " ".join(body.decode("utf-8", "replace").split())
    return f"{status} {text[:60]}"


def cmd_serve(args):
    Upstream(args.port, args.name).serve()
    return 0


def cmd_get(args):
    conn = http.client.HTTPConnection("127.0.0.1", args.port, timeout=2)
    try:
        conn.request("GET", "/health", headers={"Connection": "close"})
        resp = conn.getresponse()
        print(one_line(resp.status, resp.read()))
    except (OSError, http.client.HTTPException) as exc:
        print(f"ERR {type(exc).__name__}")
    finally:
        conn.close()
    return 0


def cmd_probe(args):
    fresh = args.mode == "fresh"
    headers = {"Connection": "close"} if fresh else {}
    conn, reused, retries = None, False, 0
    with open(args.out, "w", buffering=1) as out:
        while not os.path.exists(args.stop):
            if conn is None:
                conn = http.client.HTTPConnection("127.0.0.1", args.port, timeout=5)
                reused = False
            try:
                conn.request("GET", "/health", headers=headers)
                resp = conn.getresponse()
                line = one_line(resp.status, resp.read())
            except (ConnectionResetError, BrokenPipeError) as exc:
                conn.close()
                conn = None
                if reused:
                    retries += 1
                else:
                    out.write(f"ERR {type(exc).__name__}\n")
                continue
            except (OSError, http.client.HTTPException) as exc:
                conn.close()
                conn = None
                out.write(f"ERR {type(exc).__name__}\n")
                continue
            out.write(line + "\n")
            if fresh or resp.will_close:
                conn.close()
                conn = None
            else:
                reused = True
        out.write(f"# reused-connection retries: {retries}\n")
    return 0


def cmd_ws_client(args):
    try:
        sock = socket.create_connection(("127.0.0.1", args.port), timeout=10)
        key = base64.b64encode(os.urandom(16)).decode()
        sock.sendall(
            (
                f"GET {args.path} HTTP/1.1\r\nHost: 127.0.0.1\r\nUpgrade: websocket\r\n"
                f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\n"
                "Sec-WebSocket-Version: 13\r\n\r\n"
            ).encode()
        )
        rfile = sock.makefile("rb")
        status = rfile.readline(65537).decode("latin-1").split()
        headers = read_headers(rfile)
        if len(status) < 2 or status[1] != "101":
            raise ConnectionError(f"handshake status {' '.join(status[1:2]) or 'none'}")
        if headers.get("sec-websocket-accept") != ws_accept(key):
            raise ConnectionError("bad Sec-WebSocket-Accept")
        opcode, payload = ws_recv(rfile)
        if opcode != OP_TEXT:
            raise ConnectionError(f"first frame opcode {opcode}")
        print(f"hello {payload.decode()}", flush=True)
        while True:
            line = sys.stdin.readline()
            if not line:
                break
            ws_send(sock, OP_TEXT, line.rstrip("\n").encode(), mask=True)
            opcode, payload = ws_recv(rfile)
            if opcode != OP_TEXT:
                raise ConnectionError(f"reply opcode {opcode}")
            print(f"echo {payload.decode()}", flush=True)
        ws_send(sock, OP_CLOSE, struct.pack("!H", 1000), mask=True)
        ws_recv(rfile)
        sock.close()
    except (OSError, ValueError) as exc:
        print(f"ERR {type(exc).__name__}: {exc}", flush=True)
        return 1
    return 0


def cmd_ports_free(args):
    busy = []
    for port in args.ports:
        try:
            socket.create_server(("127.0.0.1", port)).close()
        except OSError:
            busy.append(str(port))
    if busy:
        print(" ".join(busy))
        return 1
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("serve")
    p.add_argument("--port", type=int, required=True)
    p.add_argument("--name", choices=("blue", "green"), required=True)
    p.set_defaults(fn=cmd_serve)
    p = sub.add_parser("ws-client")
    p.add_argument("--port", type=int, required=True)
    p.add_argument("--path", default=WS_PATH)
    p.set_defaults(fn=cmd_ws_client)
    p = sub.add_parser("get")
    p.add_argument("--port", type=int, required=True)
    p.set_defaults(fn=cmd_get)
    p = sub.add_parser("probe")
    p.add_argument("--port", type=int, required=True)
    p.add_argument("--mode", choices=("fresh", "keepalive"), required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--stop", required=True)
    p.set_defaults(fn=cmd_probe)
    p = sub.add_parser("ports-free")
    p.add_argument("ports", type=int, nargs="+")
    p.set_defaults(fn=cmd_ports_free)
    args = parser.parse_args()
    return args.fn(args)


if __name__ == "__main__":
    sys.exit(main())
