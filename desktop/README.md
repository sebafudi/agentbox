# agent-desktop

Virtual X11 desktops for coding agents. Each agent gets its own screen, with a visible Chromium and any other GUI app, and you watch it (or take over) live from a web browser on your laptop or phone.

> **This is my personal setup, not a product.** It is copied from my own box with private details removed. Paths, ports, sizes and limits reflect my hardware and habits; adapt them to yours rather than installing it as-is. There is no installer: the pieces below are what an agent needs to rebuild it on your box.

The agent-facing usage guide is the skill in [`../skills/agent-desktop/SKILL.md`](../skills/agent-desktop/SKILL.md). This page describes the parts and how they fit.

## What is here

| Path | Installs to | What it is |
|---|---|---|
| `bin/agent-desktop` | `~/.local/bin/` | The whole CLI (bash): slots, sharing between agents, owner switches, screen reading and input. `agent-desktop --help` prints the command reference. |
| `bin/webkit-console` | `~/.local/bin/` | Debug helper: loads a URL in WebKitGTK (Safari's engine) under `xvfb-run` and prints JS errors and console output. Used to chase iOS Safari-only failures in the browser view. |
| `lib/screen.py` | `~/.local/lib/agent-desktop/` | Screen reading: AT-SPI accessibility tree of apps on a display, plus tesseract OCR fallback. Prints click points in screen pixels. |
| `lib/page.py` | `~/.local/lib/agent-desktop/` | Whole-page access to a slot's Chromium over the DevTools protocol (stdlib-only websocket client): page outline, text, find, scroll into view, click with a real X click, wait for navigation. |
| `lib/control.py` | `~/.local/lib/agent-desktop/` | HTTP control API on port 6081 behind the owner's switch bar. Every state change goes through the CLI. |
| `lib/agent-controls.js` | `~/.local/lib/agent-desktop/` | The switch bar injected into noVNC's `vnc.html`: floating, collapsible or docked panel. |
| `lib/desk.html` | `~/.local/lib/agent-desktop/` | Tiny redirect, `desk.html?slot=N` opens slot N in noVNC. |
| `config/tint2rc` | `~/.config/agent-desktop/` | Taskbar for slots 1-9 (open windows, display label, clock). |
| `systemd/*` | `~/.config/systemd/user/` | User units for slot 0, the per-slot templates, the web gateway, the control API and the idle reaper. |

## The pieces

**Slot 0** is `xdesktop.target`: Xvfb `:99` at 1920x1080, openbox, and x11vnc on `127.0.0.1:5900` (`xvfb.service`, `openbox.service`, `x11vnc.service`). It predates the rest, is started by hand when needed, and is never reaped.

**Slots 1-9** are throwaway per-agent desktops. `agent-desktop@N.target` pulls in `agent-xvfb@N` (Xvfb `:10N`, 1280x800), `agent-openbox@N`, `agent-tint2@N` (30px taskbar at the bottom) and `agent-x11vnc@N` (view-only VNC on `127.0.0.1:590N`). `agent-desktop start` claims the first free one.

**Named slots** are persistent, purpose-bound desktops outside 1-9, for anything that has to stay logged in somewhere. They are listed in `~/.config/agent-desktop/slots`, one `name=NN` line each with NN from 20 to 99 (`#` starts a comment), for example:

```
# name=number
work=20
```

Slot NN uses display `:10NN`, VNC `590NN`, DevTools port `9210+NN` (slot 20: 9230) and the Chromium profile `chromium-NN`. Commands take the name or the number, `start` never hands one out, the reaper covers them, and the browser view shows the name.

**Sharing.** A slot belongs to the agent process that first used it; the lease ends when that process exits. Other agents get exit 5 and can `request` to share; the holder grants or denies. Input actions are serialised per slot, and co-holders are warned when someone else acted. State lives in `$XDG_RUNTIME_DIR/agent-desktop/<slot>/`.

**Browser.** `agent-desktop browser N [URL]` runs Chromium as the transient unit `agent-desktop-N-chromium.service` (part of the slot's target) with its own profile in `~/.local/state/agent-desktop/chromium-N`, `--force-renderer-accessibility`, and DevTools on `127.0.0.1:922N`. `agent-desktop agent N claude|codex|omp` starts an agent with `DISPLAY` set and, for Claude Code and Codex, a `desktop-browser` MCP server: `agent-desktop mcp-browser N`, which runs `chrome-devtools-mcp` (via `npx`) against that DevTools port inside a systemd scope so the owner's pause can freeze it.

**Screen reading.** `tree`, `locate`, `click TEXT` read the AT-SPI accessibility tree (Chromium, GTK, Qt); the script enables the a11y bus (`org.a11y.Status IsEnabled=true`) on start, without which AT-SPI sees no apps. `ocr` and the OCR fallback use ImageMagick plus tesseract. `page`, `find`, `show`, `ready` and `click --page` use the DevTools protocol and see the whole page, not just the screen. `shot` and `zoom` take screenshots for vision. `clickxy`, `type`, `key`, `scroll` drive xdotool.

**Web view.** `agent-desktop-web.service` runs websockify with noVNC on port 6080. The CLI builds `~/.local/share/agent-desktop/web/` on demand: symlinks to the noVNC install, a copy of `vnc.html` with `agent-controls.js` injected, an `index.html` listing running slots, and a websockify token file mapping `dN` to each slot's VNC port. Desktop URLs look like `http://<host>:6080/vnc.html?autoconnect=1&resize=scale&path=websockify%3Ftoken%3DdN`.

**Owner switches.** `agent-desktop-control.service` serves the API on port 6081; the switch bar in noVNC calls it. Switches: *Your control* (x11vnc view-only off/on, off by default), *Pause agents* (agent actions exit 4, MCP scopes are frozen with `systemctl --user freeze`), grant/deny share requests, *Free slot* (clear holders), *Stop* (with a confirmation naming the holders). Agents cannot flip the owner's switches from the CLI.

**Idle reaper.** `agent-desktop-reap.timer` runs `agent-desktop reap` every 5 minutes. It stops slots 1-9 and named slots that no live agent holds and nobody watches (no VNC connection) once idle for 60 minutes, or 10 minutes when available RAM drops below 3 GiB. `--dry-run` shows what it would do.

## Dependencies

Arch Linux package names; adjust for your distribution.

- Display and input: `xorg-server-xvfb`, `openbox`, `tint2`, `x11vnc`, `xdotool`
- Screen reading: `at-spi2-core`, `python-gobject`, `imagemagick` (the `magick` command), `tesseract`, `tesseract-data-eng`
- Browser: `chromium`
- Web view: `novnc` and `websockify` from the AUR (the script expects noVNC under `/usr/share/webapps/novnc`)
- Shell tools used by the script: `iproute2` (`ss`), `util-linux` (`flock`), `curl`, `systemd`
- `chrome-devtools-mcp`: Node from mise, run as `mise exec node@24 -- npx -y chrome-devtools-mcp@latest`; no global Node install
- `webkit-console` only: `webkitgtk-6.0` (with `xvfb-run` from `xorg-server-xvfb`)
- Optional: `tailscale` (the CLI prints Tailscale URLs from `tailscale ip -4`)

The Python parts use the system `/usr/bin/python3` with only the standard library plus PyGObject.

The CLI also calls two optional helpers from the agentbox telemetry component (`~/.local/lib/agentbox-telemetry/actlog.py` and `runlog.py`, not published here). When they are missing it skips them.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `AGENT_DESKTOP_PUBLIC_HOST` | empty | Public HTTPS hostname that serves the web view (for example a tunnel behind an auth proxy, which must also route `/api/` to port 6081). Empty means no public URL is printed. |
| `AGENT_DESKTOP_LAN_HOST` | `$(uname -n).local` | Host name used in LAN URLs (mDNS). |
| `AGENT_DESKTOP_SLOTS` | `~/.config/agent-desktop/slots` | Named slots file. |

Set them where agents run (shell profile or the environment of the units that start agents). Ports (6080 web, 6081 API, `590N` VNC, `922N` DevTools), display sizes and the reaper limits are constants in the script and units.

## Security notes

- x11vnc reads its password from `~/.vnc/passwd` (create with `x11vnc -storepasswd`). The control API compares the bearer token with the first line of `~/.vnc/passwd.txt`, a plain-text copy of the same password. Keep both files mode 600 and never print them.
- VNC servers listen on localhost only; websockify (6080) and the control API (6081) listen on all interfaces so the LAN and Tailscale can reach them. Restrict that with a firewall if your LAN is not trusted.
- Slots 1-9 and named slots start view-only. The owner's mouse and keyboard only reach the desktop after switching *Your control* on.
- VNC password auth is weak (8 characters, no transport encryption over plain HTTP). Do not expose ports 6080 or 6081 to the internet directly; put them behind an authenticating proxy or tunnel, or use a VPN.
- Agents act with your user's privileges and see whatever the slot's browser profile is logged in to. Keep logins in named slots and treat slots 1-9 as disposable.
