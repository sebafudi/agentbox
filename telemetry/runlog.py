"""Record agent runs and which sessions belong to them, for collect.py.

Usage:
  runlog.py start LAUNCHER NAME [KEY=VALUE...]   record a run started under $AGENTBOX_RUN_ID
  runlog.py claude-hook                          Claude Code SessionStart hook (JSON on stdin)

A run is one launch of an agent by a launcher (agent spawn, agent-desktop agent, the scheduler).
Launchers export AGENTBOX_RUN_ID; scheduler runs already carry SCHEDULER_RUN_ID. Records go to
~/.local/state/agentbox-telemetry/runs/runs-YYYY-MM.jsonl and hold names and ids, never prompts.
Nothing is written when the process is not part of a run, and errors never fail the caller.
"""

import json
import os
import sys
from datetime import datetime, timezone

RUNS_DIR = os.path.expanduser("~/.local/state/agentbox-telemetry/runs")


def current_run_id():
    if os.environ.get("AGENTBOX_RUN_ID"):
        return os.environ["AGENTBOX_RUN_ID"]
    if os.environ.get("SCHEDULER_RUN_ID"):
        return f"scheduler:{os.environ['SCHEDULER_RUN_ID']}"
    return None


def append(record):
    record = {"ts": datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"), **record}
    os.makedirs(RUNS_DIR, exist_ok=True)
    path = os.path.join(RUNS_DIR, f"runs-{datetime.now().strftime('%Y-%m')}.jsonl")
    fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
    try:
        os.write(fd, (json.dumps(record, separators=(",", ":")) + "\n").encode())
    finally:
        os.close(fd)


def start(launcher, name, pairs):
    run_id = current_run_id()
    if not run_id:
        return
    attrs = dict(pair.split("=", 1) for pair in pairs if "=" in pair)
    append({"type": "run", "run_id": run_id, "launcher": launcher, "name": name, "cwd": os.getcwd(), "attrs": attrs or None})


def claude_hook():
    run_id = current_run_id()
    if not run_id:
        return
    event = json.load(sys.stdin)
    if event.get("session_id"):
        append({"type": "link", "run_id": run_id, "source": "claude", "session_id": event["session_id"],
                "via": f"claude-hook:{event.get('source') or 'start'}"})


def main():
    try:
        if sys.argv[1:2] == ["start"] and len(sys.argv) >= 4:
            start(sys.argv[2], sys.argv[3], sys.argv[4:])
        elif sys.argv[1:2] == ["claude-hook"]:
            claude_hook()
        else:
            print(__doc__, file=sys.stderr)
            return 2
    except Exception as error:
        print(f"runlog: {error}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
