#!/usr/bin/env python3
"""Minimal webhook receiver for end-to-end alert notification tests.
Logs every POST body (appended JSON line) to a file and returns 200."""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

LOG_PATH = "/tmp/webhook_hits.log"


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length).decode("utf-8", "replace")
        with open(LOG_PATH, "a", encoding="utf-8") as f:
            f.write(body + "\n")
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 19090
    open(LOG_PATH, "w").close()
    HTTPServer(("0.0.0.0", port), Handler).serve_forever()
