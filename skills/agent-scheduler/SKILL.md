---
name: agent-scheduler
description: Create, inspect, pause, and run scheduled commands, omp, or Claude Code jobs on an agentbox; operate the private job dashboard and inspect run failures.
---

# Agent scheduler on an agentbox

Use this skill when the owner asks for a recurring automated run. The service is `agent-scheduler.service` (a systemd user unit); jobs live in `~/.local/state/agent-scheduler/state.json` and are managed only through the `scheduler` CLI or the `agentbox-login` job editor, never by editing the state file. Source, install steps and the full reference: [`scheduler/README.md`](../../scheduler/README.md) in this repository, usually checked out at `~/code/agentbox/scheduler/`.

The dashboard is `https://<host>.local:8457` on the LAN and `https://<tailnet IP>:8457` on Tailscale, with a self-signed certificate; check its fingerprint before accepting a browser warning. It stays on private networks and is never published through a tunnel. Logged out it shows job names and coarse status only; schedules, prompts, directories and logs need the password.

## Create or update a job

1. Confirm with the owner the cadence, working directory and expected effects (tokens spent, files edited, anything published). Use a five-field cron expression (`minute hour day-of-month month day-of-week`, no seconds, no `H`) and an IANA timezone such as `Europe/Warsaw`. Check that the directory exists. Do not let two agents edit the same checkout at once; give conflicting work its own worktree.
2. Write the job as JSON in a temporary file. **Never put credentials in a prompt or argv**: both are stored and shown in the signed-in dashboard.

```json
{
  "id": "daily-review",
  "name": "Daily review",
  "schedule": "0 9 * * 1-5",
  "timezone": "Europe/Warsaw",
  "prompt": "Review yesterday's changes in this checkout and write a brief report locally. Do not push or publish without explicit permission.",
  "cwd": "/home/you/code/my-project",
  "runner": "omp",
  "model": "",
  "timeoutMinutes": 30,
  "enabled": true
}
```

3. `scheduler job apply job.json`, then `scheduler job show daily-review` to check the stored schedule, `nextRun` and runner. Applying the same `id` keeps run history and any inactive runner settings. For partial edits use `scheduler job patch <id> changes.json`, which changes only the given fields; changing schedule, timezone or enabled state recomputes the next run.
4. Smoke it with `scheduler job run daily-review` (works while paused) and `scheduler runs daily-review`; read the output with `scheduler logs <run-id>`. Runs count against the job timeout and the global limit of two parallel runs.
5. Delete temporary JSON files that contain prompts.

Runners:

- `command`: runs a literal `argv` string array (executable first) with no shell, splitting or interpolation, and no model call. Create new command jobs paused (`"enabled": false`) until a manual run looks right, e.g. `"runner": "command", "argv": ["/usr/bin/printf", "%s\\n", "two words"]`.
- `omp`: `omp -p` with the prompt.
- `claude-print`: `claude -p` with the prompt on stdin.
- `claude-sdk`: the Claude Agent SDK `query()`.

All runners run as the owner in `cwd`. Agent runners need a prompt; `model` is optional (empty means the runner's default). Claude runners take `permissionMode`: `dontAsk` (default), `acceptEdits`, or `bypassPermissions` only when the owner explicitly approves unrestricted behaviour. Prefer `dontAsk` with a narrow `allowedTools` list and, if needed, `additionalDirectories`; this is Claude's approval policy, not an OS sandbox, and anything not allowed should fail visibly rather than be skipped silently.

## Reporting results from a job

Each run gets `SCHEDULER_RUN_ID` and `SCHEDULER_JOB_ID`. A `command` (or `claude-sdk`) run may end stdout with a JSON report such as `{"outcome":"idle","reason":"nothing new"}`; valid outcomes are `idle`, `skipped`, `processed`, `success`, `failure`. Without one, exit 0 is `success`. A nonzero exit, timeout, cancellation or runtime error always overrides a positive report. An optional `workflow` object (`runId`, `outcome`, `phase`, and optional `scope`, `blocker`, GitHub issue/PR `githubUrls`) is shown on the signed-in dashboard and flagged when it is incomplete, blocked, interrupted or failed. It is descriptive only: a job must itself report `failure` and exit nonzero when its work did not finish, and must not exit 0 while background work it started is still running.

## Operate

- `scheduler job list`, `scheduler job show <id>`, `scheduler runs [id]`, `scheduler snapshot` (jobs and retained runs as JSON).
- `scheduler follow <run-id>` streams output to completion and exits nonzero on failure. `scheduler stop <run-id>` cancels the run and its whole cgroup (including `setsid` descendants) and records `outcome: cancelled`.
- `scheduler join <run-id>` follows a running job read-only, or resumes a finished `omp`/Claude run in its exact saved session and original directory. Missing sessions are errors, never a fallback to `--continue`. Plain command runs are log-only. Only one join per run at a time.
- `scheduler job pause|resume <id>` stops or restarts future scheduled runs; pausing does not stop a run in progress.
- `scheduler job delete <id>` irreversibly removes the job, its history and logs. Ask the owner before deleting their jobs. Running or joined jobs cannot be deleted.
- `agentbox-login`: **a** or **Ctrl-J** opens scheduled jobs. **n** creates a paused job, **e** edits (runner, schedule, timezone, cwd, timeout, enabled, prompt/model or argv JSON), **Ctrl-S** saves, **Space** pauses or enables, **r** runs now, **m** monitors or resumes the selected run, **x** stops it, **d** deletes after confirmation, **Esc** goes back. The editor never changes Claude permission settings.
- Signed-in dashboard history is grouped by job, ten runs per page, searchable by job, run ID, diagnostic, workflow scope or publication link (`#123`), with job, result and period filters. Dashboard configuration is read-only.
- `scheduler password set` (no echo) or `scheduler password generate` (printed once) sets the dashboard password and logs out every browser. Never paste the password into jobs or documentation.
- Diagnose the service itself with `systemctl --user status agent-scheduler.service` and `journalctl --user -u agent-scheduler.service`. It is enabled with linger and restarts on failure.

## Rules worth knowing

- Overlapping runs of the same job are skipped; at most two runs execute at once, oldest-due first. After downtime each overdue job runs once, not once per missed occurrence. Failed runs are not retried before the next scheduled tick.
- Restarting the service kills any runs still active and records them as `interrupted`.
- Output is capped at 2 MiB per run and history at the newest 200 runs.
- Never start detached work from a job (`nohup`, `&`, a separate `systemd-run` or background agent unit): it dies with the run's cgroup or escapes it.
- Jobs can spend tokens, edit files and publish. Scope every one of those effects explicitly in the prompt or argv, and let a cheap check decide whether a model is needed at all.
