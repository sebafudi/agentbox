"""HTTP control API for the owner's switches on agent desktops (used by agent-controls.js in vnc.html).

GET  /api/slots/N                  status (holders, requests, pause and input state)
POST /api/slots/N/input  {"on": b} let the owner's mouse and keyboard through
POST /api/slots/N/block  {"on": b} pause or resume agents
POST /api/slots/N/grant  {"pid": n} let a requesting agent share the slot
POST /api/slots/N/deny   {"pid": n}
POST /api/slots/N/release          clear all holders
POST /api/slots/N/stop             stop the desktop (and its browser), even while agents hold it

Every request needs "Authorization: Bearer <VNC password>". All state changes go through the
agent-desktop CLI, so the CLI stays the only place that knows the rules.
"""

import hmac
import json
import os
import re
import subprocess
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CLI = os.path.expanduser("~/.local/bin/agent-desktop")
PASSWORD_FILE = os.path.expanduser("~/.vnc/passwd.txt")
PORT = 6081
# Slots 0-9 and named slots (20-99); the CLI rejects numbers that are not configured.
ROUTE = re.compile(r"^/api/slots/(\d{1,2})(?:/(input|block|grant|deny|release|stop))?$")
ALLOWED_ORIGIN = re.compile(r"^https?://[^/]+:6080$")


def vnc_password():
    with open(PASSWORD_FILE) as f:
        return f.readline().strip()


def cli(*args, log=True, timeout=20):
    env = None if log else {**os.environ, "AGENT_DESKTOP_LOGGED": "1"}
    result = subprocess.run([CLI, *args], capture_output=True, text=True, timeout=timeout, env=env)
    return result.returncode, (result.stdout + result.stderr).strip()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send_cors(self):
        origin = self.headers.get("Origin", "")
        if ALLOWED_ORIGIN.match(origin):
            self.send_header("Access-Control-Allow-Origin", origin)
            self.send_header("Vary", "Origin")
            self.send_header("Access-Control-Allow-Headers", "Authorization, Content-Type")
            self.send_header("Access-Control-Allow-Methods", "GET, POST")

    def reply(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_cors()
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def authorized(self):
        supplied = self.headers.get("Authorization", "").removeprefix("Bearer ").encode()
        if supplied and hmac.compare_digest(supplied, vnc_password().encode()):
            return True
        time.sleep(1)
        self.reply(401, {"error": "wrong password"})
        return False

    def route(self):
        match = ROUTE.match(self.path.split("?")[0])
        if not match:
            self.reply(404, {"error": "not found"})
        return match

    def status(self, slot):
        code, out = cli("status", slot, "--json", log=False)
        return json.loads(out) if code == 0 else {"error": out}

    def do_OPTIONS(self):
        self.send_response(204)
        self.send_cors()
        self.end_headers()

    def do_GET(self):
        match = self.route()
        if match and not match.group(2) and self.authorized():
            self.reply(200, self.status(match.group(1)))

    def do_POST(self):
        match = self.route()
        if not match or not match.group(2) or not self.authorized():
            return
        slot, action = match.groups()
        length = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            body = {}
        if action in ("input", "block"):
            args = [action, slot, "on" if body.get("on") else "off"]
        elif action in ("grant", "deny"):
            args = [action, slot, str(int(body.get("pid", 0))), "--force"]
        elif action == "stop":
            args = ["stop", slot, "--force"]
        else:
            args = ["release", slot, "--force"]
        code, out = cli(*args, timeout=60 if action == "stop" else 20)
        self.reply(200 if code == 0 else 409, {"ok": code == 0, "message": out, "state": self.status(slot)})


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
