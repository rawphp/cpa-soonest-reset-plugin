#!/usr/bin/env python3
"""A fake Anthropic Messages upstream for scripts/smoke.sh.

Each API key answers with its own Anthropic-Ratelimit-Unified-7d-Reset header.
POST /control {"key": ..., "status": 429|200} switches a key to rate-limited
or back. GET /served lists the key of every /v1/messages request in order.

Usage: fake_upstream.py PORT_FILE KEY=EPOCH [KEY=EPOCH ...]
"""

import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

resets = {}
status = {}
served = []
lock = threading.Lock()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def send(self, code, body, headers=None):
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        for name, value in (headers or {}).items():
            self.send_header(name, value)
        self.end_headers()
        self.wfile.write(raw)

    def body(self):
        length = int(self.headers.get("Content-Length") or 0)
        return json.loads(self.rfile.read(length) or b"{}")

    def do_GET(self):
        if self.path == "/served":
            with lock:
                self.send(200, list(served))
            return
        self.send(404, {"error": "not found"})

    def do_POST(self):
        if self.path == "/control":
            req = self.body()
            with lock:
                status[req["key"]] = int(req["status"])
            self.send(200, {"ok": True})
            return
        if not self.path.startswith("/v1/messages"):
            self.send(404, {"error": "not found"})
            return
        self.body()
        key = self.headers.get("x-api-key") or ""
        if not key:
            key = (self.headers.get("Authorization") or "").removeprefix("Bearer ").strip()
        with lock:
            served.append(key)
            code = status.get(key, 200)
        reset = resets.get(key)
        headers = {}
        if reset is not None:
            headers["anthropic-ratelimit-unified-7d-reset"] = str(reset)
            headers["anthropic-ratelimit-unified-7d-status"] = "rejected" if code == 429 else "allowed"
        if code == 429:
            headers["retry-after"] = "3600"
            self.send(429, {"type": "error", "error": {"type": "rate_limit_error", "message": "weekly limit reached"}}, headers)
            return
        self.send(200, {
            "id": "msg_smoke",
            "type": "message",
            "role": "assistant",
            "model": "claude-smoke",
            "content": [{"type": "text", "text": "ok"}],
            "stop_reason": "end_turn",
            "stop_sequence": None,
            "usage": {"input_tokens": 1, "output_tokens": 1},
        }, headers)


def main():
    port_file = sys.argv[1]
    for arg in sys.argv[2:]:
        key, epoch = arg.split("=", 1)
        resets[key] = int(epoch)
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    with open(port_file, "w") as f:
        f.write(str(server.server_address[1]))
    server.serve_forever()


if __name__ == "__main__":
    main()
