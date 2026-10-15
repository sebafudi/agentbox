---
name: agentbox
description: Operating an agentbox, a dedicated Linux VM that hosts coding agents (Claude Code, Codex, omp) for one owner who only reaches it remotely - its layout, SSH/Zellij workspace, packages and per-project language runtimes, Docker, headless and virtual-desktop browsers, background and scheduled agents, networking (LAN, Tailscale, Cloudflare Tunnel), exposing dev servers, resource pressure, services and troubleshooting.
---

# agentbox

An agentbox is a disposable, single-purpose VM whose only job is to run coding agents for its owner. The owner never sits at it: they connect over SSH from a laptop or phone, usually into Zellij, and read the agents' output from there. Nothing on it is production; the box can be rebuilt, but the projects in `~/code` are real work and live in git.

This skill describes one specific agentbox: its owner's personal setup, shaped by their hardware, network, tools and habits. It is not meant to be used as-is. Treat it as a worked example: before building or operating a box from it, adapt it to the new owner's needs and preferences (distro, multiplexer, agents, access paths, limits, keybindings) and rewrite the parts that do not fit. It describes the finished box, not install steps; the agent works out how to get there on the hardware at hand. [references/history.md](references/history.md) explains how the original was built.

## Recreating this box: interview the owner first

If you are an agent asked to build a box from this skill, do not copy it. Before touching any hardware, ask the owner the questions below (a few at a time, offering this box's choice as the default) and get answers to them. Then rewrite this skill and [references/topology.md](references/topology.md) with their answers, removing everything that does not apply, so the new box gets a skill that describes *it*. Once the VM is up, copy the rewritten skill, machine notes and rules onto the box itself; from that moment the box's copy is the source of truth and the one you worked on is only a draft (see "Keeping this skill current"). If you are operating the original box, skip this section.

1. **Hypervisor**: Which host and hypervisor (Proxmox or something else)? How will you reach it, and when should that access be handed back? What else runs on the host that must never be disturbed, and how much CPU, RAM and disk can this VM take?
2. **Base system**: Which distro and kernel (this box: Arch + `linux-lts`)? Which username and hostname? Passwordless sudo or not? How is the VM backed up?
3. **Access**: Which devices will you connect from (laptop, phone), and on which networks? Is any of them managed or filtered? LAN only, Tailscale, a Cloudflare Tunnel with your own domain, or something else? Is the home connection behind CGNAT?
4. **Workspace**: Zellij or tmux, and which keybindings? Do you want the `agentbox-login` home screen on every SSH login? Which shell and prompt tools?
5. **Agents**: Which coding agents (Claude Code, Codex, omp, others), and which accounts or subscriptions? What should they be allowed to do without asking (push, open PRs, touch shared repos, install packages)? Which models should they use by default, and how frugal should they be with tokens?
6. **Git**: Which identity and signing setup? Which hosts and organisations do you work with, and what is off limits?
7. **Runtimes and services**: Per-project runtimes via mise, or something else? Docker for databases?
8. **Optional parts**: Which of these do you want: virtual desktops you can watch from a browser, headless background agents, a cron scheduler for agents, a hub of exposed services, token telemetry, a live facts snapshot?
9. **Hard rules**: Which actions could lock you out or cost you money, and must never happen without asking?

The values below are the original box's, with private details replaced by examples. They show the shape of an answer, not a recommendation.

| Value | Original box | Notes |
|---|---|---|
| Hostname | `agentbox` | mDNS name `agentbox.local` on the LAN |
| User | `owner` (uid 1000) | single user, passwordless sudo, member of `docker` |
| Hypervisor | Proxmox VE node `pve`, VM id `102` | any KVM hypervisor works; see [references/topology.md](references/topology.md) |
| VM size | 4 vCPU, 20 GiB RAM (no ballooning), 128 GB disk | plus zram swap |
| LAN address | `192.168.1.50` (DHCP) | |
| Tailscale address | `100.x.y.z` | MagicDNS may be off; then give the 100.x address, not `*.ts.net` |
| Public domain | `example.com` | only if you use a Cloudflare Tunnel |

## References

- [references/topology.md](references/topology.md): the hypervisor host, neighbouring VMs, the home network, every way in, and the owner's devices.
- [references/history.md](references/history.md): design decisions and why, plus known quirks.
- [references/automations.md](references/automations.md): timers and services that run without anyone logged in, and lessons for building new automations.
- [references/clipboard-image-paste.md](references/clipboard-image-paste.md): parked research on pasting clipboard images from a Mac into agents running over SSH.
- The SSH workspace (`agentbox-login`) and the facts collector are in this repository under `login/`; their README is the source of truth for keys and behavior.

## Keeping this skill current

- Treat this file as maintained operating instructions, not a generated inventory. Whenever an agent changes the box's tooling, SSH or Zellij behavior, services, automations or operating rules, it updates the matching section here **in the same task**, then refreshes `agentbox-facts`.
- The copy on the box is the source of truth. Agents on the box edit skills, `AGENTS.md` and docs in place, so any copy elsewhere is likely stale. Before writing a file onto the box from outside (scp, rsync, ssh heredoc), read the live version and patch it rather than overwrite it.
- For behavior that changes independently of this prose, read the canonical source: `~/.bashrc` and `login/` for SSH, `~/.config/zellij/` for Zellij, `~/.config/systemd/user/` for units. On the next agentbox task, reconcile any drift between those files and this skill.

## Agent instructions and live state

Agents learn about the box from three layers:

1. **Machine notes** (`AGENTS.md` for omp/Codex, `~/.claude/CLAUDE.md` for Claude Code, which can import the same file): what the box is, where it runs, how to reach it, the owner's standing preferences, what runs in the background. Short; links here for details.
2. **Hard rules** (`RULES.md` or a section of the same file): things that lock the owner out if done wrong. Never change sshd, networking, the firewall, Tailscale, the tunnel, the bootloader or the owner's account; never reboot or power off unless the owner asks in this conversation; never partial-upgrade.
3. **Live snapshot**: `agentbox-facts` (Go, `login/cmd/agentbox-facts/`) writes `machine-state.md` atomically: uptime and load, memory and swap, disk, failed units, LAN and Tailscale addresses, tunnel hostnames, non-loopback listeners, running user units and timers, containers, Zellij sessions, git status of each project under `~/code`, tool versions. A 15-minute user timer plus a path unit on watched config files refresh it, and the machine notes import it. Run `agentbox-facts` for a fresh view before relying on anything time-sensitive.

## Layout

| What | Where |
|---|---|
| Projects | `~/code/<name>`; groups of related checkouts in `~/code/<group>/<repo>` |
| Agentbox tooling | `~/code/agentbox/` (this repository) |
| Shared skills | `~/.agents/skills/<name>/SKILL.md` (Agent Skills format). omp reads this directory directly; Claude Code gets one symlink per skill in `~/.claude/skills/`. |
| omp config | `~/.omp/agent/` (`AGENTS.md`, `RULES.md`, `config.yml`) |
| Agent binaries | `~/.local/bin/` (`omp`, `claude`, `codex`, `agentbox-login`, `agentbox-facts`, helper scripts) |
| User units | `~/.config/systemd/user/` (linger enabled, so they run without a login) |
| State | `~/.local/state/<tool>/` (caches, run history, snapshots; never published) |
| System env | `/etc/environment` (`PUPPETEER_EXECUTABLE_PATH`, `CHROME_PATH`, `EDITOR`); PATH (`~/.local/bin`, mise shims) for every session, including non-interactive `ssh host cmd`, via `/etc/security/pam_env.conf` |
| Docker config | `/etc/docker/daemon.json` (json-file logs, 10 MB x 3) |
| sshd drop-in | `/etc/ssh/sshd_config.d/10-agentbox.conf` (keys only, no root login); agents never edit it |
| Network | `/etc/systemd/network/20-wired.network` (DHCP + mDNS); agents never edit it |

## Base system

- Arch Linux (rolling), kernel `linux-lts`, UEFI + systemd-boot, ext4, zram swap instead of a swap partition.
- Bash with starship, zoxide, fzf, direnv and mise activated.
- Tooling from the official repos: git, git-lfs, gh, lazygit, delta, difftastic, neovim, ripgrep, fd, fzf, bat, eza, jq, yq (go-yq), xh, curl, ast-grep, shfmt, just, hyperfine, tokei, tldr, watchexec, entr, sqlite, glow, docker + compose + buildx, lazydocker, zellij, tmux, mosh, btop, dust, duf, ncdu, strace, lsof, dig, socat, netcat, iperf3, cloudflared, tailscale, chromium, xorg-server-xvfb, openbox, tint2, x11vnc, at-spi2-core, python-atspi, tesseract.
- **No language runtimes are installed system-wide on purpose.** Each project pins its own with mise.
- Git identity is the owner's (a GitHub noreply address works well), with an SSH key generated on the box and every commit GPG-signed automatically. Agents never change the identity or disable signing.
- The hypervisor backs up the whole VM nightly. That is not a substitute for pushing to git.

Interactive aliases worth keeping in `~/.bashrc`:

```bash
alias l='eza --long --icons=always --group-directories-first'
alias la='eza --long --icons=always --all --group-directories-first'
```

## SSH workspace and coding agents

- Interactive SSH logins run `agentbox-login` from `~/.bashrc`: a Go Bubble Tea home screen with quick launch (`o/r` new/resume omp, `c/l` new/resume Claude, `s` Zellij shell, `h` plain shell), recent sessions (`1`-`9`/`0`, arrows, Enter, mouse), search (`/`), preferences (`p`) and a scheduled-jobs view (`a`, Ctrl-J). It is not a second SSH server: OpenSSH still authenticates, a crashed binary falls through to Bash, and `NO_ZELLIJ=1` bypasses it. Esc logs out; detaching from Zellij returns to it.
- Search matches session name, topic, pane cwd, foreground or last shell command and tab. Filters (Ctrl-G agents, Ctrl-B shells, Ctrl-V saved/resurrectable, Ctrl-Y T3 chats) and the history span (Ctrl-T) persist under `~/.local/state/agentbox-login/`. Rows show per-session CPU and RSS sampled from `/proc`, with history graphs in search.
- `agentbox-login-refresh.timer` runs `agentbox-login --refresh` about every 15 seconds so the session cache is always warm; the UI never waits for collection.
- Inside Zellij, `Ctrl b`, `Ctrl f` opens `agentbox-login --find-agent` as a floating finder; Enter switches session, Esc closes. The binding lives in `~/.config/zellij/config.kdl`, the key list in `~/.config/zellij/cheatsheet.txt`.
- Zellij is locked by default with `Ctrl b` to unlock, session resurrection on, `zjstatus` as the top tab bar and the native status bar at the bottom. `Ctrl b`, `Ctrl d` detaches quickly.
- Claude Code uses Anthropic's native per-user installer at `~/.local/bin/claude` (no Node needed). The launcher starts it with `--allow-dangerously-skip-permissions`, so bypass mode is reachable through Shift+Tab but never the default.
- Codex CLI uses OpenAI's standalone installer at `~/.local/bin/codex` (no Node needed); `codex login --device-auth` signs in remotely.
- omp lives at `~/.local/bin/omp` and self-updates with `omp update`.
- Keys and full behavior: `login/README.md` in this repository.

## Packages

- Install from the official repos: `sudo pacman -S --needed <pkg>`. Search: `pacman -Ss <term>`; which package owns a file: `pacman -F <file>`.
- Full upgrade: `sudo pacman -Syu` (plus `yay -Sua` for AUR). A kernel upgrade needs a reboot; tell the owner instead of rebooting.
- **Never partial upgrades** (`pacman -Sy <pkg>`). If a package download 404s, the fix is `sudo pacman -Syu`.
- AUR helper: `yay`, only for things not in the official repos; review the PKGBUILD of unfamiliar packages.
- Cache cleanup runs weekly via `paccache.timer`.

## Language runtimes (mise)

```bash
cd ~/code/app
mise use node@lts          # writes mise.toml; installs into ~/.local/share/mise
mise use python@3.13 uv@latest
mise install               # everything a checked-out repo's mise.toml / .tool-versions asks for
mise ls
```

Shims are on PATH in every shell, including non-interactive ones. Respect a repo's `.nvmrc`, `.python-version`, `.tool-versions` or `rust-toolchain.toml`; mise reads most of them. Global runtimes (`mise use -g`) only when the owner asks. Never install runtimes with pacman, and never `pip install` or `npm -g` into system paths.

A systemd unit with a fixed PATH that omits mise shims must name the pinned runtime's install directory (`~/.local/share/mise/installs/<tool>/<version>/bin`) explicitly.

## Docker

- `docker compose up -d` for databases and queues; prefer it over host installs.
- Clean up after yourself: `docker compose down`; check `docker system df`; prune only what you created.
- `lazydocker` for an overview.

## Browser (headless)

- Chromium at `/usr/bin/chromium`; `PUPPETEER_EXECUTABLE_PATH` and `CHROME_PATH` point at it. Agent browser tools run it headless with no display.
- In projects, point Playwright or Puppeteer at the system binary (`executablePath: process.env.CHROME_PATH`) instead of downloading browsers.
- Safari-only bugs: with `webkitgtk-6.0` installed, a small `webkit-console URL` script under `xvfb-run -a` loads a page in WebKit and prints JS errors with stacks, which iOS Safari hides as "Script error.".
- Headed-only cases: `xvfb-run -a chromium ...` or the virtual desktops below.

## Virtual desktops and computer use

Not running by default, to save RAM.

- **Slot 0**: `xdesktop.target` = Xvfb `:99` (1920x1080) + openbox + x11vnc on `127.0.0.1:5900`. Start it, export `DISPLAY=:99`, stop it when done. An agent doing computer use needs `DISPLAY=:99` in its own process environment.
- **Per-agent desktops**: an `agent-desktop` command manages slots 1-9, each an `agent-desktop@N.target` = Xvfb `:10N` (1280x800) + openbox + tint2 taskbar + x11vnc on `127.0.0.1:590N`. Commands: `list`, `start [N]`, `stop N`, `run N -- CMD`, `shot N`, `url N`, plus screen reading and acting (`tree`, `locate`, `click`, `ocr`, `zoom`, `clickxy`, `type`, `key`, `scroll`) backed by the AT-SPI accessibility tree with tesseract OCR as fallback. The a11y bus must be enabled (`org.a11y.Status IsEnabled=true`) or AT-SPI sees no apps.
- `agent-desktop browser N [URL]` starts Chromium in the slot as a transient unit with its own profile, `--force-renderer-accessibility` and a DevTools port; `agent-desktop agent N claude|codex|omp` runs an agent on the slot with chrome-devtools-mcp attached. Reading order that works best: MCP, then accessibility tree, then OCR, then screenshots.
- **Watching**: websockify + noVNC serve every slot in the browser on port 6080, with an index of running desktops. x11vnc on slots 1-9 starts view-only; a small control API (port 6081, VNC password auth) backs a switch bar injected into noVNC: take control, pause agents (freezes their scopes with `systemctl --user freeze`), grant or deny requests, free or stop a slot.
- **Idle reaper**: a 5-minute timer stops slots that no live agent holds and nobody watches after 60 idle minutes (10 when available RAM is low). Slot 0 is never reaped.
- **Named slots**: slots 1-9 are throwaway (profiles may be wiped or reused). Anything that needs a persistent login gets its own named, high-numbered slot, one per purpose.

## Networking and remote access

- **LAN**: DHCP on the hypervisor's bridge, mDNS name `<hostname>.local`. Current address: `ip -4 -br addr show`.
- **Tailscale**: the box joins the owner's tailnet. If logged out, the owner runs `sudo tailscale up`; agents never invent auth keys. If the tailnet's DNS servers are unreachable from the box, keep `--accept-dns=false`. Leave `--accept-routes` off unless the owner needs a subnet route.
- **Cloudflare Tunnel** (optional, for networks where Tailscale is blocked, or behind CGNAT with no port forwarding): `cloudflared` as a system service maps `agentbox.<domain>` to `ssh://localhost:22`, with clients using `ProxyCommand cloudflared access ssh --hostname %h`. HTTP services can be added as single-level hostnames (`agentbox-x.<domain>`; Universal SSL covers one level), each behind a Cloudflare Access application, with `originRequest.access` on every ingress rule so cloudflared itself rejects requests without a valid Access token. Validate with `cloudflared tunnel ingress validate` and `ingress rule URL`, then add the DNS route. Anything on the tunnel is reachable from the internet: agents add hostnames only when the owner asks, and never use quick tunnels (`trycloudflare`).
- **Hub**: a small user service (port 6070) lists every exposed service with LAN, Tailscale and public links, unit state, a local probe and an anonymous probe of each public hostname (`Access login` is fine, `OPEN without login` is a red flag). It reads public hostnames from the tunnel config and flags any other non-loopback listener.
- **Dev servers** bind `0.0.0.0` so the owner can open them; report `http://<hostname>.local:<port>` and `http://<tailscale-ip>:<port>`. Vite `--host 0.0.0.0`, Next `-H 0.0.0.0`, Rails `-b 0.0.0.0`, uvicorn `--host 0.0.0.0`.
- Long-running processes are systemd user units (or the agent's own named services), never detached `&` processes.
- Listening ports: `ss -ltnp`.

## Telemetry

A one-minute user timer reads Claude, Codex and omp transcripts plus the box's own action logs into a SQLite database: token usage per model, tools, skills and context sources, subagents, durations and outcomes. It stores sizes, counts and timings, never prompt, tool or output content; session titles come from the tools' own generated titles. A read-only dashboard (port 6090) shows totals, per-session timelines and what filled each context, reading content from the transcripts on demand. Launchers export a run id so sessions and their actions join into runs.

## Resource pressure

- Agents share a few vCPUs and a fixed amount of RAM, plus zram. Check `free -h` and `btop`; OOM kills show in `journalctl -k | grep -i oom`.
- Disk: `duf`, `dust -d1 ~`, `docker system df`, `du -sh ~/.local/share/mise ~/.cache`.
- Keep build parallelism modest (`-j2`/`-j4`, `CARGO_BUILD_JOBS=2`), stop dev servers and containers you started, and never let two agents edit the same checkout (use git worktrees).
- If the hypervisor hosts anything important (home automation, other VMs), RAM, disk I/O and network floods on the agentbox hurt it too.

## Background agents (`agent`)

A small `agent` script runs headless agents as transient systemd user units (`agent-<name>`), so they survive SSH disconnects and can be driven from another machine (`ssh agentbox agent ...`) as remote subagents.

```bash
agent spawn <name> [-C dir] [-m model] [-t thinking] [-T max-time] "<prompt>"
agent status [name]        # running | done | failed
agent wait <name>          # block, print the final answer (exit 1 on failure)
agent logs <name> [-f]
agent stop <name>
agent take <name>          # reopen the finished run's session interactively
agent rm <name>            # re-spawning a name archives the old run
```

State per run: `~/.omp/runs/<name>/` (`prompt`, `cwd`, `output.log`, `stderr.log`, `exit`, `session/`).

## Scheduled agents (`agent-scheduler`)

A persistent Node service (`agent-scheduler.service`, started at boot through linger) runs five-field cron jobs with an IANA timezone: literal-argv commands, omp, Claude print or the Claude Agent SDK, as the owner. Each run gets its own systemd user-service cgroup, at most two run in parallel, overlaps of the same job are skipped, one missed occurrence is caught up after downtime, logs are capped per run and history is retained for the last few hundred runs. A `scheduler` CLI talks to it over a user-only Unix socket, and `agentbox-login` has a full job editor on the same API. A private HTTPS dashboard (self-signed, password login) is reachable on LAN and Tailscale only, never through the tunnel.

## Services and logs

```bash
systemctl --failed; systemctl --user --failed
journalctl -b -p warning
journalctl --user -u <unit> -f
systemctl --user list-timers
```

Enabled system services: systemd-networkd, systemd-resolved, systemd-timesyncd, sshd, qemu-guest-agent, docker, tailscaled, cloudflared (if used), fstrim.timer, paccache.timer, systemd-boot-update.

## Recovery (owner-side)

If SSH or networking breaks, the owner reaches a root console from the hypervisor (serial console or web console, both with root autologin). Snapshots and rollbacks happen on the hypervisor, never from inside the VM. Agents on the box have no credentials for the hypervisor, the router or neighbouring machines, and never look for any.
