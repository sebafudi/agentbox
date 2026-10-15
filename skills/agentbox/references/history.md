# agentbox decisions and quirks

## How the box came to be

1. Built by an agent, not by hand. From another computer, the owner gave a coding agent root access to the Proxmox host and described the box they wanted. The agent created the VM, booted it from the Arch ISO (checksum verified), installed Arch with `pacstrap` over SSH from the live environment (cloud-init injected the SSH key) and provisioned it. Once the VM existed, hypervisor access was no longer needed; everything after that was built by agents running on the box.
2. Remote access first through Tailscale, then a Cloudflare Tunnel because the owner's work laptop cannot run Tailscale.
3. Zellij chosen over tmux, configured locked by default with `Ctrl b` to unlock, session resurrection, and an agent-aware resume hook.
4. An SSH key and a GPG signing key generated on the box for GitHub.
5. Self-knowledge for agents: machine notes, hard rules, this skill and the `agentbox-facts` live snapshot imported into every session.
6. Remote subagents: the `agent` script runs headless agents as systemd user units so an agent on the laptop can delegate work to the box.
7. Later additions: the `agentbox-login` SSH workspace, per-agent virtual desktops watchable from a browser, a persistent cron scheduler for agents, a hub of exposed services, and token telemetry.

## Decisions and why

| Decision | Reason |
|---|---|
| VM, not a container | Real isolation, Docker without nesting hacks, hypervisor snapshots. |
| Arch + `linux-lts` | Current packages; the LTS kernel reduces breakage on a rolling distro. |
| No system language runtimes | Runtimes per project via mise, pinned in each repo, so projects never fight over versions. |
| Passwordless sudo, no passwords at all | Disposable single-purpose box; SSH keys are the only way in; the hypervisor console is the recovery path. |
| Chromium + Xvfb, no desktop environment | Headless Chromium covers web testing; X11 virtual displays (not Wayland) exist for computer use, which needs X11 capture and XTEST. |
| zram instead of a swap partition | Compressed swap in RAM, no disk partitioning. |
| Fixed RAM, no ballooning | Agents size their work from `free`; ballooning made that number move. |
| Tailscale DNS off (when the tailnet's DNS is unreachable) | Accepting a dead nameserver made every lookup time out, which hung agent startup on model discovery. |
| PATH via `/etc/security/pam_env.conf` | Arch's bash does not read `.bashrc` for non-interactive SSH, so `ssh agentbox omp` could not find the binary. |
| GitHub noreply commit email | Keeps a work email off personal commits; commits are still verified on GitHub. |
| Long work as its own systemd unit | A unit's cgroup is killed when its main process exits, so work backgrounded with `&` from a service dies with it. |

## Known quirks

- Behind CGNAT, Tailscale sometimes relays through DERP. A stuck path was once fixed by restarting `tailscaled` (owner-approved only, since it can cut the session).
- `cloudflared` from the Arch repos may lag upstream by a release; it updates with `pacman -Syu`.
- Zellij only saves sessions that differ from the default single tab; `exit` in the last pane ends a session for good.
