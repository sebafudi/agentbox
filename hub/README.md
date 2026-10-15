# agentbox-hub

One page that lists everything the box exposes: each known service with its LAN, Tailscale and public links, the state of its systemd unit, a local probe, and an anonymous probe of every public hostname. It also flags any other port that listens on a non-loopback address, and tunnel rules that point at a port where nothing listens.

> This is my personal setup, published as a worked example. Adapt the service list, ports and policy to your own box rather than installing it as-is.

## How it works

`hub.py` is a small `http.server` app using only the Python standard library. On each refresh (cached for 30 seconds, or on demand with the Recheck button) it reads:

| Source | What it gives |
|---|---|
| `~/.config/agentbox-hub/services.json` | the known services: port, name, kind (`http`, `https`, `api`, `ssh`, `tcp`), user or system unit, description |
| `/etc/cloudflared/config.yml` (via `yq`) | tunnel ingress rules, so which ports have a public hostname, and whether `originRequest.access.required` is set |
| `ss -ltnpH` | what actually listens on a non-loopback address, and which process |
| `systemctl [--user] is-active` | unit state, plus `cloudflared.service` and `tailscaled.service` |
| `tailscale ip -4` | the Tailscale address used for the Tailscale links |

Then it probes, in parallel with short timeouts:

- **Locally**: an HTTP(S) request to `127.0.0.1:<port>` for web services (TLS not verified, redirects not followed), or a TCP connect for the rest.
- **Publicly**: an unauthenticated `GET https://<hostname>/` for every tunnel hostname, to check what an anonymous visitor gets. A redirect to `cloudflareaccess.com` shows as `Access login` (good), a 403 as `blocked: not in Access app` (cloudflared's own Access check refused it), and a 200 as `OPEN without login` (a red flag).

A port counts as reachable over the LAN when it binds a wildcard or a private address (RFC 1918 or IPv6 ULA), and over Tailscale when it binds a wildcard or the Tailscale address. LAN links use `<hostname>.local` (mDNS); `--lan-host` overrides the name.

The page (`hub.html`, no external assets) polls `/api/status` every 15 seconds, works in light and dark mode, and highlights the links of the way you opened it: a tunnel hostname (taken from the same tunnel config), the Tailscale address, or anything else as LAN.

| Path | Returns |
|---|---|
| `/` | the page |
| `/api/status` | the cached snapshot as JSON; `?refresh=1` rebuilds it first |

## Files

| In this repo | Installed at |
|---|---|
| `hub.py`, `hub.html` | `~/.local/lib/agentbox-hub/` |
| `services.example.json` | `~/.config/agentbox-hub/services.json` (edit it for your services) |
| `systemd/agentbox-hub.service` | `~/.config/systemd/user/` (enabled; linger keeps it running without a login) |

The unit runs `/usr/bin/python3 -I hub.py --host 0.0.0.0 --port 6070` and restarts on failure.

## Dependencies

- System `python3` (standard library only; no mise runtime, no packages).
- `ss` (iproute2), `systemctl`, and optionally `tailscale`, `yq` (go-yq) and `cloudflared`'s config file. Missing tools just leave their part of the page empty.

## Security notes

- The hub has no login of its own. It binds `0.0.0.0` so it is reachable on the LAN and the tailnet; publishing it through the tunnel only makes sense behind a Cloudflare Access application (with `originRequest.access` on the ingress rule).
- It only reads: it never changes units, the tunnel or the firewall. It needs no root; `ss -p` shows process names only for the user's own sockets.
- The public probes send one anonymous request to each of your own tunnel hostnames per refresh.
- The snapshot shows ports, process names, addresses and hostnames. Treat it as private, like any other live state of the box.
