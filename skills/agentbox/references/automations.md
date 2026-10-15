# Automations on an agentbox

Background jobs that run without anyone logged in. Check live status with `systemctl --user list-timers`, `systemctl --user status agent-scheduler.service`, `scheduler job list` and `agent status`.

| Automation | Trigger | What it does |
|---|---|---|
| `agentbox-facts.timer` (+ path unit) | boot, every 15 min, and on watched config changes | Regenerates the live `machine-state.md` snapshot imported into agent sessions. |
| `agentbox-login-refresh.timer` | boot + every 15 s | `agentbox-login --refresh`: keeps the SSH/Zellij session picker cache warm; bounded concurrent reads, no model calls. |
| `agent-desktop-reap.timer` | boot + every 5 min | Stops idle per-agent desktops that no live agent holds and nobody watches; no model calls. |
| telemetry timer | every minute | Reads agent transcripts into the telemetry database (sizes and counts only). |
| `agent-scheduler.service` | boot + continuous | Runs the owner's cron jobs (commands, omp, Claude print, Claude Agent SDK), stores history, serves a private dashboard. |

## Agent scheduler

The scheduler checks due jobs every few seconds, launches at most two in parallel (agents share a few vCPUs and fixed RAM), skips overlaps of the same job and records runs interrupted by a restart as failed. After downtime it catches up one missed occurrence rather than replaying a backlog. Jobs, TLS material and logs live under `~/.local/state/agent-scheduler/` with private permissions.

Commands run as literal argv (no implicit shell). Every run lives in its own systemd user-service cgroup, including `setsid` descendants; ownership is persisted before a gated launch, and cleanup after a restart happens before any new request or catch-up, failing closed. Completed agent runs can be resumed in their exact saved session; active ones can only be monitored read-only.

The dashboard is reachable on the LAN and Tailscale only, with a self-signed certificate (renew it manually before it expires) and a password login with sliding sessions. It is never published through the tunnel, and passwords never go into prompts.

## Building new automations (lessons learned)

- **Spend tokens only when there is real work.** Detect changes with plain bash and git (diff remote refs, filter by changed paths) and start a model only after a cheap check says it is needed. Never launch a model on every tick.
- **Never background work from a systemd service** (`&`, `nohup`): the unit's cgroup is killed when its main process exits. Start long work as its own unit (`agent spawn ...` or `systemd-run --user`).
- **Never resume the owner's interactive session from automation.** Two processes writing one session corrupts it. Use a fresh session with a self-contained prompt or runbook file.
- **Watch remote refs, not local branches** your own agent commits to, or the automation triggers itself.
- **Give every watcher a dry-run mode** and test it by editing its saved snapshot, not by pushing to real remotes.
- **Put a hard time cap on agent runs** (the agent's own limit plus a systemd `RuntimeMaxSec` slightly above it).
- **Never touch shared or organisation remotes** from automation unless the owner approved that exact action; keep work on local branches.
- **Register new scheduled jobs paused**, verify them with fakes, then enable them for the next tick.
