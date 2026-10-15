# agentbox

An agentbox is a dedicated Linux VM that hosts coding agents (Claude Code, Codex, omp) for one person who only ever reaches it remotely, from a laptop or a phone, through SSH and Zellij. It runs on a small home hypervisor, keeps agents busy while you are away, and is disposable: everything that matters lives in git.

This repository describes what an agentbox consists of and holds the tooling for working with one.

> **This is my setup, not a product.** Every choice here (Arch, Zellij, omp next to Claude Code and Codex, the keybindings, the tunnel, the limits) reflects my own needs, hardware and habits. It is not meant to be installed as-is. Read it as a worked example, keep what fits you, and have your agent tune the rest to your own preferences before building anything.

The intended use: give an agent this repository, a hypervisor and your own requirements, and let it build *your* box, using mine as the reference. There is deliberately no step-by-step installer; the skill describes the finished box, and the agent works out how to get there on your hardware.

![agentbox-login home screen](login/screenshots/home.png)

## What is here

| Path | What it is |
|---|---|
| [`skills/agentbox/`](skills/agentbox/SKILL.md) | The agentbox skill (Agent Skills format): layout, SSH workspace, packages, runtimes, Docker, browsers and virtual desktops, background and scheduled agents, networking, telemetry, resource limits, recovery. Edit the "This box" table and the topology template for your own machine. |
| [`login/`](login/README.md) | `agentbox-login`, the Go/Bubble Tea SSH home screen: launch or resume agents and shells in Zellij, search sessions, watch per-session CPU/RSS, manage scheduled jobs. Also `agentbox-facts`, the live machine snapshot that agents read. |

## What a box looks like

- **VM**: Arch Linux with `linux-lts`, 4 vCPU, ~20 GiB fixed RAM plus zram, 128 GB disk, nightly hypervisor backups.
- **Access**: SSH keys only, over the LAN (mDNS), Tailscale, and optionally a Cloudflare Tunnel behind Cloudflare Access for networks where Tailscale is blocked. No inbound ports needed, so it works behind CGNAT.
- **Workspace**: every interactive SSH login lands in `agentbox-login`, which starts or rejoins Zellij sessions running agents or shells.
- **Agents**: Claude Code, Codex and omp from their native installers, plus an `agent` script for headless runs as systemd units and a persistent cron scheduler for recurring agent jobs.
- **Runtimes**: none system-wide; each project pins its own with mise.
- **Browsers**: headless Chromium for tests, and per-agent X11 virtual desktops (Xvfb, openbox, x11vnc, noVNC) you can watch and take over from a browser.
- **Self-knowledge**: machine notes, hard rules, the skill, and a live facts snapshot imported into every agent session, kept current by the agents themselves.

## How mine was built

The box was not set up by hand. From a different computer (my Mac), I gave a coding agent root access to the Proxmox host and described the box I wanted. The agent did the rest by itself:

1. Created the VM on Proxmox (CPU, RAM, disk, UEFI) and booted it from the Arch ISO it downloaded and verified.
2. Installed Arch over SSH from the live environment (`pacstrap`, systemd-boot, `linux-lts`, networking, sshd with keys only), then rebooted into the installed system.
3. Provisioned the user, packages, shell, Zellij, Docker, Tailscale, the Cloudflare Tunnel and the coding agents.
4. Wrote the machine notes, the hard rules and this skill, so later agents running *on* the box know where they are.

After that, root access to the hypervisor was no longer needed: everything since has been built by agents running on the box itself, which keep the skill current as they go. If you do the same, take the hypervisor credentials back once the VM exists.

## Using it

1. Have your agent read the skill and the topology template, then adapt them to your own hardware, network, tools and preferences. Change anything you would do differently; it is a starting point, not a spec.
2. Let it build the box (as above), or rebuild parts of an existing one.
3. On the box, clone into `~/code/agentbox` and link your adapted skill where your agents find it, for example `~/.agents/skills/agentbox` (omp) and `~/.claude/skills/agentbox` (Claude Code).
4. Build the workspace: see [`login/README.md`](login/README.md).

Keep your own copy of the skill on the box as the source of truth and let agents update it as the box changes. Do not publish generated state (`machine-state.md`, `~/.local/state/`): it contains session names, paths, commands and network details.

## Not here (yet)

Components that exist on the reference box but are not published in this repository: the `agent-desktop` scripts and skill, the agent scheduler service, the services hub and the telemetry collector. The skill describes what they do.
