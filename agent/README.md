# agent

A small Bash script that runs headless coding agents (omp in print mode) in the background as transient systemd user units, so they survive SSH disconnects and can be started, waited on and read from another machine (`ssh agentbox agent ...`). That makes the box usable as a pool of remote subagents.

> This is my personal setup, published as a worked example. It drives omp; adapt the command line to the agent you use (Claude Code `-p`, `codex exec`, ...) and to your own paths.

## Usage

```bash
agent spawn <name> [-C dir] [-m model] [-t thinking] [-T max-time] [-R hard-cap] <prompt...>
agent status [name]        # table of all runs (running | done | failed), or one run in detail
agent wait <name>          # block until done, print the final answer (exit 1 on failure)
agent logs <name> [-f]     # output and stderr so far, or follow the output
agent stop <name>          # stop the unit, record exit 143
agent take <name>          # reopen the finished run's session interactively in omp
agent rm <name>            # forget a finished run
agent help
```

- `-C` sets the working directory (default `~/code`), `-m`, `-t` and `-T` pass `--model`, `--thinking` and `--max-time` to omp, and `-R` sets systemd's `RuntimeMaxSec` as a hard cap that kills the run no matter what the agent does.
- Names are 1-40 characters of `[A-Za-z0-9._-]`; a name that is still running cannot be spawned again.

## How it works

`spawn` writes the run's state, then starts `systemd-run --user --unit agent-<name> --collect` with the chosen working directory, a few environment variables passed through (`PATH`, browser paths, editor, omp stream timeouts) and stdout/stderr redirected to files. The unit runs `omp -p --session-dir <run>/session ... "<prompt>"`, then records the exit code and finish time. State is the unit's activity plus the `exit` file.

Each run also exports `AGENTBOX_RUN_ID=agent:<name>:<started>`, which the [telemetry collector](../telemetry/README.md) uses to group the run's sessions and actions; the collector also reads the run directories directly.

State per run, in `~/.omp/runs/<name>/`:

| File | Content |
|---|---|
| `prompt` | the prompt as given |
| `cwd` | working directory |
| `started`, `finished` | Unix timestamps |
| `exit` | exit code |
| `output.log`, `stderr.log` | the agent's stdout (the final answer) and stderr |
| `session/` | the omp session, so `agent take` can continue it interactively |

Re-spawning a name moves the previous run to `~/.omp/runs/.archive/<name>-<YYYYmmdd-HHMMSS>/` instead of overwriting its report and session.

## Files

| In this repo | Installed at |
|---|---|
| `agent` | `~/.local/bin/agent` (executable) |

No unit files: every run is its own transient unit. Linger must be enabled for the user so runs keep going without a login session.

## Dependencies

Bash, systemd (user instance), coreutils (`date -d`), and the agent CLI itself (`omp` on PATH).

## Security notes

- Runs execute as the user with whatever permissions the agent CLI grants in print mode; anything a prompt asks for, the agent may do. Give background agents the same rules as interactive ones.
- Prompts and outputs are stored in plain files under `~/.omp/runs/`; keep that directory out of anything you publish.
- Anyone who can SSH in as the user can spawn agents, which spends that user's subscription or API tokens.
