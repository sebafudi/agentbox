# agentbox-telemetry

Token and tool telemetry for every coding agent on the box. A one-minute timer reads Claude Code, Codex and omp transcripts, plus the box's own action and run logs, into one SQLite database. A read-only dashboard shows totals, models, tools, runs and sessions, down to what filled each session's context.

> This is my personal setup, published as a worked example. The transcript parsers follow the formats of the agent versions I run; expect to adjust them when a client changes its format, and drop the parts (desktop actions, scheduler, `agent` runs) you do not have.

## The rule: sizes, never content

The database stores sizes, counts, durations, token numbers and outcomes. It never stores prompt, tool or output content. The only text kept per session is the short title the agent itself generated or the owner set (Claude `ai-title` / `agent-name`, omp `title`, Codex `thread_name` from `~/.codex/session_index.jsonl`) and the git branch. Skill names and the paths of instruction files are kept as identifiers. Every event keeps the transcript file and line it came from, so the dashboard can read the content from the transcript on demand, without copying it. Keep it that way when extending it.

## Collector (`collect.py`)

Sources, all read-only:

| Source | Files |
|---|---|
| `claude` | `~/.claude/projects/**/*.jsonl` (sessions and subagents) |
| `codex` | `~/.codex/sessions/**`, `~/.codex/archived_sessions/**` |
| `omp` | `~/.omp/agent/sessions/**`, plus the session dirs of `agent spawn` runs (`~/.omp/runs/`) and scheduler omp runs |
| `actions` | `~/.local/state/agentbox-telemetry/actions/*.jsonl` (written by `actlog.py`) |
| `runs` | `~/.local/state/agentbox-telemetry/runs/*.jsonl` (written by `runlog.py`) |

- Each file is read from a saved byte offset up to its last complete line; a file whose inode changed or that shrank is read again. Every event has a unique `(source, key)`, so re-reading is harmless and the database can be deleted and rebuilt from the transcripts at any time (`collect.py collect --rebuild`).
- Event kinds: `llm_call` (input, cache read, cache write, output and reasoning tokens, cost where the client reports it), `tool_call` / `tool_exec` (argument and result sizes, duration, ok, truncation), `user_prompt` (size), `context` (system prompt, memory files, skill and tool listings, attachments, reminders, injected messages, compaction summaries, with sizes), `skill` (each skill load), `compaction`, `turn`, `error`, `action`, `pr`.
- **Runs** join sessions and actions to one launch of an agent: `agent spawn` runs (from `~/.omp/runs`), scheduler runs (from the scheduler's `state.json`), running omp processes whose environment carries `AGENTBOX_RUN_ID` or `SCHEDULER_RUN_ID`, action records with a run id, and `runlog.py` records. Subagent sessions inherit their parent's run.
- Tables: `events`, `files` (offsets), `session_meta`, `rate_limits` (Codex), `runs`, `session_runs`; views `sessions`, `run_events`, `run_totals`.
- A lock file prevents overlapping runs. With `--quiet` (as the timer runs it) it prints only on errors.

```bash
python3 ~/.local/lib/agentbox-telemetry/collect.py collect [--rebuild] [--quiet]
python3 ~/.local/lib/agentbox-telemetry/collect.py summary [--days N]   # tokens per model, heaviest tools, actions, runs
sqlite3 ~/.local/state/agentbox-telemetry/events.db                     # ad hoc queries
```

Known gaps come from the transcripts themselves: Claude subagent messages carry usage from the start of the stream, so their output tokens are too low (flagged `partial_output`); per-session Claude totals (`reported_cost_usd`, `model_usage`) are complete.

## Run and action logs

- **`runlog.py`**: `runlog.py start LAUNCHER NAME [KEY=VALUE...]` records a run starting under `$AGENTBOX_RUN_ID`; `runlog.py claude-hook` is a Claude Code `SessionStart` hook that links the new Claude session to the current run. Both write nothing outside a run and never fail the caller. A new launcher should export `AGENTBOX_RUN_ID` and call `runlog.py start`.
- **`actlog.py`**: `actlog.py TOOL -- COMMAND [ARGS...]` runs a command with output passed through unchanged and appends one action record: subcommand, slot, flag names, the length (not the text) of free arguments, duration, exit code, output sizes, a coarse outcome, and the calling session or run if it can tell. On my box it wraps every short `agent-desktop` command.

## Dashboard (`dashboard.py`, `dashboard.html`)

Serves the page and a JSON API on port 6090. The database is opened with `query_only`, so the collector stays the only writer.

| Path | Returns |
|---|---|
| `/` | the page: overview (`#/`), session list (`#/sessions`), one session (`#/session/<source>/<id>`) |
| `/api/stats?days=N[&source=&model=&name=]` | tokens, cost, tools, actions, runs, heaviest sessions, Codex rate limits |
| `/api/sessions?days=N&q=&source=[&project=&branch=&model=&run=]` | sessions with titles, searchable |
| `/api/session?source=&id=[&agent=&model=&kind=&name=]` | one session: totals, per-call context, tools, skills, what filled the context, subagents, events |
| `/api/event?id=N` | one event's input and result, read from its transcript line (images included) |
| `/api/peek?ids=N,M` | one-line previews of event inputs |

Clicking a row, chart bar or underlined value sets a filter; filters live in the URL hash, so Back undoes them. The page loads Chart.js 4.4.1 from cdnjs; everything else is inline.

## Files

| In this repo | Installed at |
|---|---|
| `collect.py`, `dashboard.py`, `dashboard.html`, `actlog.py`, `runlog.py` | `~/.local/lib/agentbox-telemetry/` |
| `systemd/agentbox-telemetry.service`, `.timer` | `~/.config/systemd/user/` (timer enabled: 2 minutes after boot, then every minute, `Nice=15`, idle I/O) |
| `systemd/agentbox-telemetry-dashboard.service` | `~/.config/systemd/user/` (enabled, `--host 0.0.0.0 --port 6090`) |
| database and logs | `~/.local/state/agentbox-telemetry/` (`events.db`, `actions/`, `runs/`) |

## Dependencies

System `python3` only (standard library, including `sqlite3`; SQLite JSON functions are used in queries). Services run it with `-I`. Optionally the `sqlite3` CLI for ad hoc queries.

## Security notes

- The database holds no content, but the dashboard can show any transcript line on request through `/api/event` and `/api/peek`, which means prompts, tool output, file contents and screenshots. It has no login of its own and binds `0.0.0.0`: anyone who can reach port 6090 on the LAN or the tailnet can read your agent transcripts. Bind it to `127.0.0.1` or a Tailscale address if that matters, and only publish it through a tunnel behind an access policy.
- `/api/event` only opens files that resolve inside the home directory, and only the file and line recorded for an event.
- Action and run logs are written with mode `0600`. Never publish `~/.local/state/agentbox-telemetry/`: it contains session titles, project paths, branches and run names.
