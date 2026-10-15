# agentbox

An agentbox is a dedicated Linux VM that hosts coding agents (Claude Code, Codex, omp) for one person who only ever reaches it remotely, from a laptop or a phone, through SSH and Zellij. It runs on a small home hypervisor, keeps agents busy while you are away, and is disposable: everything that matters lives in git.

This repository describes what an agentbox consists of and holds the tooling for working with one.

> **This is my setup, not a product.** Every choice here (Arch, Zellij, omp next to Claude Code and Codex, the keybindings, the tunnel, the limits) reflects my own needs, hardware and habits. It is not meant to be installed as-is. Read it as a worked example, keep what fits you, and have your agent tune the rest to your own preferences before building anything.

The intended use: give an agent this repository, a hypervisor and your own requirements, and let it build *your* box, using mine as the reference. There is deliberately no step-by-step installer; the skill describes the finished box, and the agent works out how to get there on your hardware.

![agentbox-login home screen](login/screenshots/home.png)

## What is here

| Path | What it is |
|---|---|
| [`skills/agentbox/`](skills/agentbox/SKILL.md) | The agentbox skill (Agent Skills format): layout, SSH workspace, packages, runtimes, Docker, browsers and virtual desktops, background and scheduled agents, networking, telemetry, resource limits, recovery. When an agent recreates a box from it, the skill tells it what to ask you first. |
| [`login/`](login/README.md) | `agentbox-login`, the Go/Bubble Tea SSH home screen: launch or resume agents and shells in Zellij, search sessions, watch per-session CPU/RSS, manage scheduled jobs. Also `agentbox-facts`, the live machine snapshot that agents read. |
| [`desktop/`](desktop/README.md) | `agent-desktop`: per-agent X11 virtual desktops (Xvfb, openbox, tint2, x11vnc) with a visible Chromium and chrome-devtools-mcp, watched and taken over from a browser through noVNC, plus screen reading via AT-SPI and OCR and an idle reaper. Skill: [`skills/agent-desktop/`](skills/agent-desktop/SKILL.md). |
| [`scheduler/`](scheduler/README.md) | `agent-scheduler`: a persistent cron service for commands, omp, Claude print and the Claude Agent SDK, with run history, resumable agent runs, a `scheduler` CLI and a private HTTPS dashboard. Skill: [`skills/agent-scheduler/`](skills/agent-scheduler/SKILL.md). |
| [`agent/`](agent/README.md) | `agent`: runs headless agents as transient systemd units, so another machine can use the box as a pool of remote subagents. |
| [`hub/`](hub/README.md) | A one-page hub of everything the box exposes, with LAN, Tailscale and public links, unit state and an anonymous probe of each public hostname. |
| [`telemetry/`](telemetry/README.md) | Token and tool telemetry for Claude Code, Codex and omp in SQLite, with a dashboard. Stores sizes and counts, never content. |

## What a box looks like

- **VM**: Arch Linux with `linux-lts`, 4 vCPU, ~20 GiB fixed RAM plus zram, 128 GB disk, nightly hypervisor backups.
- **Access**: SSH keys only, over the LAN (mDNS), Tailscale, and optionally a Cloudflare Tunnel behind Cloudflare Access for networks where Tailscale is blocked. No inbound ports needed, so it works behind CGNAT.
- **Workspace**: every interactive SSH login lands in `agentbox-login`, which starts or rejoins Zellij sessions running agents or shells.
- **Agents**: Claude Code, Codex and omp from their native installers, plus an `agent` script for headless runs as systemd units and a persistent cron scheduler for recurring agent jobs.
- **Runtimes**: none system-wide; each project pins its own with mise.
- **Browsers**: headless Chromium for tests, and per-agent X11 virtual desktops (Xvfb, openbox, x11vnc, noVNC) you can watch and take over from a browser.
- **Self-knowledge**: machine notes, hard rules, the skill, and a live facts snapshot imported into every agent session, kept current by the agents themselves.

## How to recreate it

This is also how mine was made: no part of it was set up by hand, and it all happens in one pass from your own computer.

1. **Adapt it.** On your own computer, point a coding agent at this repository and ask it to build you a box like this. The skill tells it to interview you first (hardware, network, devices, agents, preferences, rules) and to rewrite the skill with your answers. Change anything you would do differently; it is a starting point, not a spec.
2. **Build the box.** Give the agent root access to your Proxmox host (or another hypervisor). Mine created the VM, booted the Arch ISO (downloaded and checksum-verified), installed Arch over SSH from the live environment (`pacstrap`, systemd-boot, `linux-lts`, networking, sshd with keys only) and rebooted into the installed system. Then it set up the user, packages, shell, Zellij, Docker, Tailscale, the tunnel, the coding agents and `agentbox-login` (see [`login/README.md`](login/README.md)).
3. **Move the knowledge onto the box.** The adapted skill, machine notes and hard rules are already the box's documentation, so the agent copies them from your computer onto the box (for example `~/.agents/skills/agentbox`, linked into `~/.claude/skills/agentbox`). No second clone and no starting over.
4. **Take the hypervisor access back.** Once the VM exists, the agent no longer needs root on the hypervisor.

From then on, **the box is the source of truth**. Agents running on it change the box and update its skill in the same task, so the copy on your computer goes stale. When something on your computer does need to change a file on the box, it reads the live version first, diffs it, and patches it in place; it never blindly uploads over it or downloads over its own copy. The skill itself spells this rule out for every agent.

Do not publish generated state (`machine-state.md`, `~/.local/state/`): it contains session names, paths, commands and network details.

Each component's README lists its files, where they install and what they depend on; systemd units are in each component's `systemd/` folder. Everything here is copied from the reference box with private details removed, so expect to adapt paths, ports and limits.
