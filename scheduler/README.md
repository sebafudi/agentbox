# agent-scheduler

A small persistent cron service for coding agents on an agentbox. It runs recurring jobs as literal commands, omp, Claude Code print mode or the Claude Agent SDK, keeps their history and logs, lets you follow or resume a run from the terminal, and serves a private read-only HTTPS dashboard.

> **This is my personal tool, not a product.** It fits my box: one owner, a systemd user manager with lingering, Node pinned with mise, agents installed in the home directory, a dashboard reachable only on my LAN and tailnet. Read it as a worked example and adapt paths, limits, runners and the security model to your own setup.

The `agentbox-login` home screen in [`../login/`](../login/README.md) has a full job editor on the same API, and the agent-facing guide lives in [`../skills/agent-scheduler/SKILL.md`](../skills/agent-scheduler/SKILL.md).

## Layout

| Path | What it is |
|---|---|
| `server.mjs` | The daemon: scheduler loop, owner-only Unix socket API, HTTPS dashboard. |
| `scheduler.mjs` | The `scheduler` CLI, a client of the Unix socket. |
| `jobs.mjs` | Job validation, cron/timezone handling, partial patches, per-job stats. |
| `run-process.mjs` | Starts one run in its own systemd unit, captures and caps output, parses final reports. |
| `run-unit.mjs` | Transient unit naming, `systemd-run` arguments, stop and cgroup cleanup. |
| `run-exec.mjs` | Launch gate that holds the workload until the daemon has seen its unit. |
| `run-files.mjs` | Per-run log locks used for retention, deletion and interactive joins. |
| `run-view.mjs` | Private run view and the hook for resuming wrapper-launched Claude sessions. |
| `continuation.mjs` | Works out the exact omp/Claude session to resume for `scheduler join`. |
| `sdk-runner.mjs` | Runner for the `claude-sdk` job type. |
| `public/` | Dashboard page, script and styles (no external assets). |
| `bin/scheduler` | CLI wrapper, installed as `~/.local/bin/scheduler`. |
| `systemd/agent-scheduler.service` | User unit, installed in `~/.config/systemd/user/`. |
| `*.test.mjs` | `node --test` suites. |

## Requirements

- Linux with a systemd user manager, unified cgroups (v2), `systemd-run`, and `flock` (util-linux).
- Node pinned in `mise.toml` (currently `24.21.0`; `package.json` requires `>=24`).
- `openssl` to create the dashboard certificate (also used by the tests).
- For agent runners: `omp` and/or `claude` on the service's `PATH`, already authenticated. The SDK runner uses `@anthropic-ai/claude-agent-sdk` and the same Claude login.

## Install

```bash
cd ~/code/agentbox/scheduler
mise trust && mise install
mise exec -- npm ci

# Private state directory and a self-signed dashboard certificate.
install -d -m 700 ~/.local/state/agent-scheduler
openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
  -keyout ~/.local/state/agent-scheduler/tls.key \
  -out ~/.local/state/agent-scheduler/tls.crt \
  -subj "/CN=$(hostname).local" \
  -addext "subjectAltName=DNS:$(hostname).local,IP:<your LAN or tailnet IP>"
chmod 600 ~/.local/state/agent-scheduler/tls.*

install -m 755 bin/scheduler ~/.local/bin/scheduler
install -m 644 systemd/agent-scheduler.service ~/.config/systemd/user/
loginctl enable-linger "$USER"
systemctl --user daemon-reload
systemctl --user enable --now agent-scheduler.service
scheduler password generate   # dashboard password, printed once
```

The unit and wrapper assume the checkout lives at `~/code/agentbox/scheduler`; change `WorkingDirectory=` in the unit, and set `SCHEDULER_HOME` for the wrapper, if yours is elsewhere. Both start Node through `mise exec`, which replaces itself with `node`, so systemd's signals and stop timeout reach the server directly. The server refuses to start without `tls.key` and `tls.crt`.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `SCHEDULER_DATA_DIR` | `~/.local/state/agent-scheduler` | State file, logs, TLS material, lock file. Created with mode 0700. |
| `SCHEDULER_SOCKET` | `$XDG_RUNTIME_DIR/agent-scheduler.sock` | Owner-only (0600) Unix socket for the CLI and `agentbox-login`. |
| `SCHEDULER_PORT` | `8457` | HTTPS dashboard port, bound on `0.0.0.0`. `0` picks a free port. |

Fixed limits in the source: at most two runs in parallel, 2 MiB of captured output per run, 200 retained run records, seven-day sliding dashboard sessions, five wrong passwords per address per 15 minutes.

Only one daemon can own a data directory (a `flock` on `server.lock`), and every run unit name contains a UUID, so independent instances with their own data directory, socket and port can share one user manager. That is how the tests run.

## Jobs

```json
{
  "id": "weekday-report",
  "name": "Weekday report",
  "schedule": "0 9 * * 1-5",
  "timezone": "Europe/Warsaw",
  "cwd": "/home/you/code/my-project",
  "prompt": "Review yesterday's changes and write a local report. Do not publish anything.",
  "runner": "claude-print",
  "timeoutMinutes": 30,
  "enabled": true,
  "permissionMode": "dontAsk"
}
```

| Field | Notes |
|---|---|
| `id` | Lowercase letters, digits and hyphens, 1-40 characters. Applying the same `id` updates the job and keeps its history. |
| `name` | 1-100 characters. The only job field the logged-out dashboard shows. |
| `schedule` | Five-field cron (`minute hour day-of-month month day-of-week`). No seconds; randomized `H` fields are rejected. |
| `timezone` | IANA zone. Daylight-saving gaps and repeats resolve to real, strictly increasing instants. |
| `cwd` | Existing absolute directory. |
| `runner` | `command`, `omp`, `claude-print` or `claude-sdk`. |
| `argv` | `command` only: nonempty string array including the executable, passed literally (no shell, no splitting, no interpolation). Empty strings after the executable are kept. |
| `prompt` | Required for agent runners, up to 200 000 characters. |
| `model` | Optional for agent runners; empty means the runner's default. |
| `timeoutMinutes` | 1-1440, default 30. |
| `enabled` | Default `true`. Paused jobs can still be run by hand. |
| `permissionMode` | Claude runners: `dontAsk` (default), `acceptEdits` or `bypassPermissions`. |
| `allowedTools` | Claude runners: explicit tool rules, for example `["Read", "Bash(git status:*)"]`. |
| `additionalDirectories` | Claude runners: extra existing absolute directories. |

Settings that the active runner does not use (a saved `argv` on an agent job, a prompt or Claude permissions on a command job) are kept across applies, patches and runner switches, so switching back restores them. Invalid values are rejected atomically even when inactive.

How each runner starts:

- `command`: the `argv` as given.
- `omp`: `omp -p --allow-home --session-dir <run>.session [--model M] @<prompt file>`. The session directory is kept so the run can be resumed.
- `claude-print`: `claude -p --session-id <uuid> --permission-mode <mode> --permission-prompts none [--model M] [--allowedTools ...] [--add-dir ...]`, with the prompt on stdin. `bypassPermissions` also passes `--allow-dangerously-skip-permissions`.
- `claude-sdk`: `sdk-runner.mjs` calls `query()` from the Agent SDK with the same options and a fixed session ID.

`allowedTools` and `permissionMode` are Claude's approval policy, not an OS sandbox. Prefer `dontAsk` with a narrow allowlist; use `bypassPermissions` only for jobs you fully trust.

Every run gets `SCHEDULER_RUN_ID` and `SCHEDULER_JOB_ID` in its environment.

### Scheduling rules

- The loop ticks every five seconds. Due jobs start oldest-due-first, up to two at a time. A job that is already running is not started again; its due time just advances.
- A job blocked by the global cap keeps its due time and starts when a slot frees up.
- The next occurrence is persisted. After downtime, an overdue job runs once, not once per missed occurrence.
- A failed run is not retried; the next attempt is the next scheduled occurrence.
- Changing the schedule, timezone or enabled state recomputes the next run; other edits keep it. Pausing does not stop a run in progress.

### Outcomes and reports

Each run has a coarse `status` (`running`, `success`, `failed`) and, once finished, an `outcome`: `idle`, `skipped`, `processed`, `success`, `failure`, `timeout`, `cancelled` or `interrupted`.

`command` and `claude-sdk` runs may end their stdout with a JSON object reporting the outcome; it can be pretty-printed and may follow arbitrary log text. Only the last 64 KiB of stdout is inspected, and stderr never counts:

```json
{ "outcome": "idle", "reason": "Nothing new since the last check" }
```

`outcome` (or `status`) must be one of `idle`, `skipped`, `processed`, `success`, `failure`. Without a report, exit 0 means `success`. A nonzero exit, an explicit `failure`, a timeout, a cancellation or a runtime error always wins over a positive report.

A report may also carry a `workflow` object describing a multi-step job:

```json
{
  "outcome": "failure",
  "reason": "Required verification did not finish",
  "workflow": {
    "runId": "<SCHEDULER_RUN_ID>",
    "outcome": "incomplete",
    "phase": "verification",
    "scope": "selected module",
    "githubUrls": ["https://github.com/owner/repo/pull/123"],
    "blocker": "Required check still running"
  }
}
```

The scheduler keeps it only if `runId`, `outcome` and `phase` are nonempty strings, truncates every field (128, 64, 64, 512 and 4096 characters), strips control and bidirectional-override characters, and keeps at most ten distinct `https://github.com/<owner>/<repo>/pull|issues/<n>` links with no credentials, query or fragment. The metadata is descriptive: the job itself must decide whether its work is complete, and report `failure` with a nonzero exit if it is not. A foreground wrapper should wait for everything it starts; exiting zero while background work continues is not completion.

## CLI

```text
scheduler job apply <file.json>        Create or update a job by id
scheduler job patch <id> <file.json>   Change only the given fields
scheduler job list                     Status and next occurrence per job
scheduler job show <id>                Full configuration
scheduler job pause|resume <id>        Disable or enable scheduled runs
scheduler job delete <id>              Delete the job, its history and logs
scheduler job run <id>                 Start now, even when paused
scheduler runs [id]                    Recent runs, optionally for one job
scheduler snapshot                     Jobs and retained runs as one JSON document
scheduler logs <run-id>                Capped, terminal-safe log tail
scheduler follow <run-id>              Stream output until the run ends
scheduler join <run-id>                Follow a live run, or resume a finished agent run
scheduler stop <run-id>                Cancel the run and everything in its cgroup
scheduler password generate|set        Set the dashboard password
```

Output is stripped of terminal escape sequences and control characters. `follow` exits nonzero if the run failed.

`join` never guesses. A running job is followed read-only. A finished `omp` run resumes the single transcript in its saved session directory; a finished Claude run resumes its saved session ID in the original `cwd`, after checking that the session still exists. Missing or ambiguous sessions are errors, never a fallback to `--continue`. Plain command runs are log-only. A command job that launches Claude itself can be made resumable by extending `wrapperContinuation` in `run-view.mjs` to map the run to the exact session it started, validated from evidence the wrapper wrote for that run. While a run is joined, its log file is locked, so a second `join` is refused and retention or deletion will not remove its transcript.

## Dashboard

`https://<host>:8457` serves a single page with a strict Content Security Policy and no external assets.

- **Logged out**: job names and coarse status (`idle`, `running`, `success`, `failed`) only. Never schedules, prompts, argv, directories, workflow metadata or logs, even for a signed-in caller of that endpoint.
- **Signed in**: read-only job configuration (argv and prompts behind an explicit expand), workflow details, and run history grouped by job: ten runs per page, search across job names, run IDs, diagnostics, workflow scope and publication links (including `#123`), filters for job, result and period, and per-run log tails. Filters and expanded panels survive the 30-second refresh; logout clears them.
- Configuration cannot be changed from the browser; use the CLI or `agentbox-login`.

The password is stored as a salted scrypt hash. `scheduler password set` reads it without echo (16-1024 characters); `scheduler password generate` prints a random one once. Changing it logs out every browser. Sessions are random 256-bit tokens stored only as SHA-256 hashes, sent in a `Secure`, `HttpOnly`, `SameSite=Strict` cookie that slides for seven days. POST requests must be same-origin JSON.

The certificate is self-signed: check its fingerprint (`openssl x509 -in ~/.local/state/agent-scheduler/tls.crt -noout -fingerprint -sha256`) before accepting the browser warning, and renew it before it expires. Keep the dashboard on trusted networks (LAN, tailnet); do not publish it through a public tunnel.

## Security model

- Everything runs as the owner. There is no privilege separation between the scheduler and its jobs, so a job can do anything the owner can.
- The control API is a Unix socket with mode 0600 in the owner's runtime directory. It rejects requests carrying browser headers (`Origin`, `Sec-Fetch-Site`).
- State, logs and keys live in a 0700 directory and are written with umask 077. `state.json` is replaced atomically; do not edit it while the service runs.
- Keep secrets out of prompts and argv: both are stored in `state.json` and shown in the signed-in dashboard.

## Run containment

Every run executes in its own transient user service, `agent-scheduler-run-<UUID>.service`, started with `systemd-run --user --wait --pipe` and `KillMode=control-group`, `TimeoutStopSec=5s`, `SendSIGKILL=yes` and a `RuntimeMaxSec` matching the job timeout.

- The unit name is persisted in `state.json` before `systemd-run` is called. `run-exec.mjs` then holds the workload until the daemon has seen the unit running; if its stdin closes first (for example because the daemon crashed while the start was queued) it exits without running anything.
- Normal exit, timeout, cancellation and shutdown all stop the whole cgroup, including `setsid` and double-forked descendants, then check that it is empty.
- On startup the daemon stops every persisted unit and checks that their cgroups are empty before it opens the socket or catches up overdue jobs. Runs that were active are recorded as `interrupted`. If cleanup fails, ownership is kept and the daemon refuses to start.
- A timeout or cancellation stays a failure even when the workload handles SIGTERM and exits 0.
- Do not start detached work from a job (`nohup`, `&`, a separate `systemd-run` unit): it either dies with the run's cgroup or escapes the scheduler's accounting.

## Retention

- Captured stdout and stderr are capped at 2 MiB per run; the transcript an agent writes to its own session storage is not covered by that cap.
- History keeps the 200 newest run records, never pruning a running one. Logs and omp session directories of pruned runs are deleted; a run that is open in `scheduler join` is skipped and retried on later ticks, also across restarts.
- Deleting a job removes its history and logs, and is refused while the job is running or one of its runs is joined.
- Nothing calls a model when no job is due.

## Tests

```bash
mise exec -- npm test
```

`npm test` runs `node --test *.test.mjs`: job validation and cron/DST handling, literal argv, report parsing and outcome precedence, log capping and UTF-8-safe cursors, the launch gate, cgroup cleanup of detached descendants, crash and restart reconciliation, retention with locked transcripts, `join` safety, and the public/private API split. The tests start real `systemd-run --user` units and throwaway daemons on temporary data directories, sockets and ports, with fake commands only; they never call a model or touch the installed service.

For a manual smoke test, start `server.mjs` with a temporary `SCHEDULER_DATA_DIR` holding a throwaway certificate, `SCHEDULER_SOCKET` pointing into it and `SCHEDULER_PORT=0`, point `scheduler.mjs` at the same socket, and apply disabled jobs that run harmless local commands.
