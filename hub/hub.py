"""Agentbox hub: one page listing everything this VM exposes, with links, ports and status.

Usage: hub.py [--host 0.0.0.0] [--port 6070] [--lan-host NAME.local]

Sources, read on every refresh (cached for REFRESH seconds):
  ~/.config/agentbox-hub/services.json   known services: port, name, unit, description
  /etc/cloudflared/config.yml            tunnel ingress, i.e. which ports have a public hostname
  ss -ltnp                               what actually listens on a non-loopback address
  systemctl (--user) is-active           unit state
Probes: a local request to each listening port, and an unauthenticated request to each public
hostname to check that Cloudflare Access (or the origin JWT check) stops anonymous visitors.

  /             the page (hub.html)
  /api/status   the cached snapshot as JSON; ?refresh=1 rebuilds it first
"""

import argparse
import ipaddress
import json
import os
import re
import socket
import ssl
import subprocess
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

HERE = os.path.dirname(os.path.abspath(__file__))
PAGE = os.path.join(HERE, "hub.html")
REGISTRY = os.path.expanduser("~/.config/agentbox-hub/services.json")
TUNNEL_CONFIG = "/etc/cloudflared/config.yml"
LAN_HOST = f"{socket.gethostname().split('.')[0]}.local"  # mDNS name; --lan-host overrides it
REFRESH = 30
TIMEOUT = 4
LOOPBACK = re.compile(r"^(127\.|\[::1\]|::1|\[::ffff:127\.)|%lo\b")
INSECURE = ssl.create_default_context()
INSECURE.check_hostname = False
INSECURE.verify_mode = ssl.CERT_NONE


def run(*cmd):
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=TIMEOUT).stdout
    except (OSError, subprocess.TimeoutExpired):
        return ""


def is_lan(address):
    """A private (RFC 1918 / ULA) address other than loopback, as ss prints it ("[fd00::1]", "10.0.0.5%eth0")."""
    try:
        ip = ipaddress.ip_address(address.strip("[]").split("%")[0])
    except ValueError:
        return False
    return ip.is_private and not ip.is_loopback and not ip.is_link_local and ip not in ipaddress.ip_network("100.64.0.0/10")


def tailscale_ip():
    return run("tailscale", "ip", "-4").strip().split("\n")[0] or None


def listeners():
    """Ports listening on a non-loopback address -> {addresses, process}."""
    found = {}
    for line in run("ss", "-ltnpH").splitlines():
        parts = line.split()
        if len(parts) < 4:
            continue
        addr, _, port = parts[3].rpartition(":")
        if LOOPBACK.search(addr) or not port.isdigit():
            continue
        proc = re.search(r'users:\(\("([^"]+)"', line)
        entry = found.setdefault(int(port), {"addresses": [], "process": None})
        entry["addresses"].append(addr)
        entry["process"] = entry["process"] or (proc.group(1) if proc else None)
    return found


def ingress():
    """Tunnel ingress rules that point at a local port."""
    try:
        config = json.loads(run("yq", "-o=json", TUNNEL_CONFIG) or "{}")
    except ValueError:
        return []
    rules = []
    for rule in config.get("ingress") or []:
        if not rule.get("hostname"):
            continue
        url = urlparse(rule.get("service", ""))
        default = {"ssh": 22, "http": 80, "https": 443}.get(url.scheme)
        port = url.port or default
        if not port:
            continue
        access = (rule.get("originRequest") or {}).get("access") or {}
        rules.append({"hostname": rule["hostname"], "path": rule.get("path"), "port": port,
                      "scheme": url.scheme, "origin_jwt": bool(access.get("required"))})
    return rules


def unit_states(user_units, system_units):
    states = {}
    for scope, units in (("user", user_units), ("system", system_units)):
        if not units:
            continue
        args = ["systemctl", "--user"] if scope == "user" else ["systemctl"]
        out = run(*args, "is-active", *units).splitlines()
        states.update(zip(units, out + ["unknown"] * (len(units) - len(out))))
    return states


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


OPENER = urllib.request.build_opener(NoRedirect, urllib.request.HTTPSHandler(context=INSECURE))


def fetch(url):
    start = time.monotonic()
    request = urllib.request.Request(url, method="GET", headers={"User-Agent": "agentbox-hub"})
    try:
        with OPENER.open(request, timeout=TIMEOUT) as response:
            code, headers = response.status, response.headers
    except urllib.error.HTTPError as error:
        code, headers = error.code, error.headers
    except Exception as error:  # connection refused, timeout, TLS
        return {"code": None, "error": type(error).__name__, "ms": None}
    return {"code": code, "location": headers.get("Location"), "ms": round((time.monotonic() - start) * 1000)}


def probe_local(port, kind):
    if kind in ("http", "https"):
        result = fetch(f"{kind}://127.0.0.1:{port}/")
        result["ok"] = result["code"] is not None and result["code"] < 500
        return result
    start = time.monotonic()
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=TIMEOUT):
            return {"ok": True, "ms": round((time.monotonic() - start) * 1000)}
    except OSError as error:
        return {"ok": False, "error": type(error).__name__}


def probe_public(hostname):
    """What an anonymous visitor gets. 'access' and 'origin-jwt' are the safe outcomes."""
    result = fetch(f"https://{hostname}/")
    code, location = result["code"], result.get("location") or ""
    if code in (301, 302, 303, 307) and "cloudflareaccess.com" in location:
        verdict = "access"        # Cloudflare Access login page
    elif code == 403:
        verdict = "origin-jwt"    # cloudflared refused a request without an Access token
    elif code is None or code in (502, 503, 520, 521, 522, 523, 524, 530):
        verdict = "down"
    elif code == 404:
        verdict = "no-route"
    else:
        verdict = "open"          # reachable without login
    result["verdict"] = verdict
    return result


def snapshot():
    registry = json.load(open(REGISTRY))
    listening = listeners()
    rules = ingress()
    ts_ip = tailscale_ip()
    known = {s["port"] for s in registry}
    user_units = [s["unit"] for s in registry if s.get("unit")]
    system_units = [s["system_unit"] for s in registry if s.get("system_unit")] + ["cloudflared.service", "tailscaled.service"]
    states = unit_states(user_units, system_units)

    hostnames = sorted({r["hostname"] for r in rules})
    with ThreadPoolExecutor(max_workers=8) as pool:
        public = dict(zip(hostnames, pool.map(probe_public, hostnames)))
        local = dict(zip([s["port"] for s in registry],
                         pool.map(lambda s: probe_local(s["port"], s.get("kind", "tcp")) if s["port"] in listening
                                  else {"ok": False, "error": "not listening"}, registry)))

    def addresses(port, kind):
        entry = listening.get(port)
        if not entry:
            return []
        scheme = kind if kind in ("http", "https") else None
        wildcard = any(a in ("0.0.0.0", "*", "[::]") for a in entry["addresses"])
        out = []
        if wildcard or any(is_lan(a) for a in entry["addresses"]):
            out.append({"via": "LAN", "host": LAN_HOST, "url": f"{scheme}://{LAN_HOST}:{port}/" if scheme else None})
        if ts_ip and (wildcard or ts_ip in entry["addresses"]):
            out.append({"via": "Tailscale", "host": ts_ip, "url": f"{scheme}://{ts_ip}:{port}/" if scheme else None})
        return out

    services = []
    for svc in registry:
        port, kind = svc["port"], svc.get("kind", "tcp")
        unit = svc.get("unit") or svc.get("system_unit")
        tunnel = [dict(r, probe=public[r["hostname"]]) for r in rules if r["port"] == port]
        services.append({
            "name": svc["name"], "port": port, "kind": kind, "about": svc.get("about"),
            "unit": unit, "unit_state": states.get(unit), "listening": port in listening,
            "process": (listening.get(port) or {}).get("process"),
            "local": local[port], "private": addresses(port, kind), "tunnel": tunnel,
        })
    unknown = [port for port in sorted(listening) if port not in known]
    with ThreadPoolExecutor(max_workers=8) as pool:
        speaks_http = dict(zip(unknown, pool.map(lambda p: probe_local(p, "http")["code"] is not None, unknown)))
    others = [{"port": port, "process": entry["process"], "addresses": entry["addresses"], "http": speaks_http[port],
               "private": addresses(port, "http" if speaks_http[port] else "tcp"),
               "tunnel": [dict(r, probe=public[r["hostname"]]) for r in rules if r["port"] == port]}
              for port, entry in sorted(listening.items()) if port not in known]
    return {
        "generated": int(time.time()),
        "host": {"lan": LAN_HOST, "tailscale": ts_ip, "public": hostnames,
                 "cloudflared": states.get("cloudflared.service"), "tailscaled": states.get("tailscaled.service")},
        "services": services, "others": others,
        "unmatched_rules": [dict(r, probe=public[r["hostname"]]) for r in rules
                            if r["port"] not in known and r["port"] not in listening],
    }


class Cache:
    def __init__(self):
        self.lock = threading.Lock()
        self.data = None
        self.at = 0

    def get(self, force=False):
        with self.lock:
            if force or self.data is None or time.time() - self.at > REFRESH:
                try:
                    self.data = snapshot()
                except Exception as error:  # keep serving the last good snapshot
                    if self.data is None:
                        return {"error": f"{type(error).__name__}: {error}"}
                    self.data["error"] = f"{type(error).__name__}: {error}"
                self.at = time.time()
            return self.data


CACHE = Cache()


class Handler(BaseHTTPRequestHandler):
    def send(self, code, body, kind):
        data = body.encode() if isinstance(body, str) else body
        self.send_response(code)
        self.send_header("Content-Type", kind)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        url = urlparse(self.path)
        if url.path == "/":
            with open(PAGE, "rb") as page:
                return self.send(200, page.read(), "text/html; charset=utf-8")
        if url.path == "/api/status":
            data = CACHE.get(force="refresh=1" in url.query)
            return self.send(200, json.dumps(data), "application/json")
        self.send(404, "not found\n", "text/plain")

    def log_message(self, *args):
        pass


def main():
    global LAN_HOST
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=6070)
    parser.add_argument("--lan-host", help=f"name shown for LAN links (default {LAN_HOST})")
    args = parser.parse_args()
    if args.lan_host:
        LAN_HOST = args.lan_host
    threading.Thread(target=CACHE.get, daemon=True).start()
    ThreadingHTTPServer((args.host, args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
