"""Run a command and append one action record to ~/.local/state/agentbox-telemetry/actions/.

Usage: actlog.py TOOL -- COMMAND [ARGS...]

The command's stdout and stderr pass through unchanged; the record keeps only what kind of
action it was (subcommand, slot, flag names), how long it took, its exit code, output sizes
and a coarse outcome. Positional text (click targets, typed text, URLs) is kept as a length.
Logging never changes the command's exit code or output; if the record cannot be written,
the command still runs normally.
"""

import json
import os
import re
import socket
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone

ACTIONS_DIR = os.path.expanduser("~/.local/state/agentbox-telemetry/actions")
KEPT_FLAG_VALUES = {"--nth", "--kind", "--role", "--max"}
SESSION_ENV = ("AGENTBOX_RUN_ID", "SCHEDULER_RUN_ID", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "OMP_SESSION_ID")
SESSION_FILE_ROOTS = tuple(os.path.expanduser(p) for p in ("~/.omp/", "~/.codex/", "~/.local/state/agent-scheduler/"))
TRAILING_ID = re.compile(r"([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$")
EXIT_OUTCOMES = {3: "ambiguous", 4: "paused", 5: "held"}
DETAILS = (
    ("ambiguous", re.compile(r"ambiguous: .*?\((\d+) different targets")),
    ("not_found", re.compile(r"not found|is not on this page|no match", re.I)),
    ("clicked", re.compile(r"^clicked \d+,\d+ via ([a-z]+(?: [a-z]+)?)", re.M)),
)
SAMPLE_BYTES = 4096


def pump(source, target, counter, sample):
    while chunk := os.read(source.fileno(), 65536):
        counter[0] += len(chunk)
        if len(sample[0]) < SAMPLE_BYTES:
            sample[0] += chunk[:SAMPLE_BYTES - len(sample[0])]
        try:
            target.buffer.write(chunk)
            target.buffer.flush()
        except (BrokenPipeError, ValueError):
            pass


def describe_args(argv):
    """Subcommand, slot, flags and the total length of free text, without the text itself."""
    command, rest = (argv[0] if argv else ""), argv[1:]
    slot = rest[0] if rest and rest[0].isdigit() else None
    if slot is not None:
        rest = rest[1:]
    flags, numbers, text_chars, i = {}, 0, 0, 0
    while i < len(rest):
        arg = rest[i]
        if arg.startswith("--"):
            name, _, value = arg.partition("=")
            if name in KEPT_FLAG_VALUES and not value and i + 1 < len(rest):
                value = rest[i + 1]
                i += 1
            flags[name] = value if name in KEPT_FLAG_VALUES else True
        elif re.fullmatch(r"-?\d+(\.\d+)?", arg):
            numbers += 1
        else:
            text_chars += len(arg)
        i += 1
    return command, slot, flags, numbers, text_chars


def outcome(returncode, sample):
    """A coarse result: exit codes 3/4/5 have fixed meanings in agent-desktop, the rest comes from the output."""
    text = sample.decode(errors="replace")
    for name, pattern in DETAILS:
        match = pattern.search(text)
        if match:
            return EXIT_OUTCOMES.get(returncode, name), (match.group(1) if match.groups() else None)
    return EXIT_OUTCOMES.get(returncode, "error" if returncode else None), None


def caller():
    return {
        "pid": os.environ.get("CALLER_PID"),
        "name": os.environ.get("CALLER_NAME"),
        "agent": os.environ.get("CALLER_IS_AGENT") == "1",
        "ssh": bool(os.environ.get("SSH_CONNECTION")),
    }


def parent_pid(pid):
    with open(f"/proc/{pid}/stat") as f:
        return int(f.read().rsplit(")", 1)[1].split()[1])


def open_session_file(max_depth=12):
    """The agent transcript an ancestor process is writing (omp keeps its session file open)."""
    pid = os.getppid()
    for _ in range(max_depth):
        if pid <= 1:
            return None
        try:
            for fd in os.listdir(f"/proc/{pid}/fd"):
                target = os.readlink(f"/proc/{pid}/fd/{fd}")
                if target.endswith(".jsonl") and target.startswith(SESSION_FILE_ROOTS):
                    return target
            pid = parent_pid(pid)
        except OSError:
            return None
    return None


def write_record(tool, argv, started_wall, started, returncode, out, err, sample):
    command, slot, flags, numbers, text_chars = describe_args(argv)
    result, detail = outcome(returncode, sample)
    sessions = {k.lower(): os.environ[k] for k in SESSION_ENV if os.environ.get(k)}
    session_id = sessions.get("claude_code_session_id") or sessions.get("codex_thread_id") or sessions.get("omp_session_id")
    session_file = None if session_id else open_session_file()
    if session_file:
        match = TRAILING_ID.search(session_file)
        session_id = match.group(1) if match else None
    scheduler_run = sessions.get("scheduler_run_id")
    record = {
        "ts": datetime.fromtimestamp(started_wall, timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"),
        "key": f"{socket.gethostname()}:{os.getpid()}:{int(started_wall * 1e6)}",
        "kind": "action",
        "name": f"{tool} {command}".strip(),
        "run_id": sessions.get("agentbox_run_id") or (f"scheduler:{scheduler_run}" if scheduler_run else None),
        "session_id": session_id,
        "agent": os.environ.get("CALLER_NAME"),
        "project": os.getcwd(),
        "duration_ms": round((time.monotonic() - started) * 1000),
        "ok": 1 if returncode == 0 else 0,
        "args_chars": text_chars,
        "result_chars": out + err,
        "attrs": {
            "tool": tool, "command": command, "slot": slot, "flags": flags or None, "numbers": numbers or None,
            "exit": returncode, "stdout_bytes": out, "stderr_bytes": err,
            "outcome": result, "outcome_detail": detail, "caller": caller(), "sessions": sessions or None,
            "session_file": session_file,
        },
    }
    os.makedirs(ACTIONS_DIR, exist_ok=True)
    path = os.path.join(ACTIONS_DIR, f"{tool}-{datetime.now().strftime('%Y-%m-%d')}.jsonl")
    line = (json.dumps(record, separators=(",", ":")) + "\n").encode()
    fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
    try:
        os.write(fd, line)
    finally:
        os.close(fd)


def main():
    if len(sys.argv) < 4 or sys.argv[2] != "--":
        print(__doc__, file=sys.stderr)
        return 2
    tool, command = sys.argv[1], sys.argv[3:]
    started_wall, started = time.time(), time.monotonic()
    child = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    out, err, sample_out, sample_err = [0], [0], [b""], [b""]
    threads = [threading.Thread(target=pump, args=(child.stdout, sys.stdout, out, sample_out)),
               threading.Thread(target=pump, args=(child.stderr, sys.stderr, err, sample_err))]
    for thread in threads:
        thread.start()
    while True:
        try:
            returncode = child.wait()
            break
        except KeyboardInterrupt:
            continue
    for thread in threads:
        thread.join()
    try:
        write_record(tool, command[1:], started_wall, started, returncode, out[0], err[0], sample_out[0] + b"\n" + sample_err[0])
    except Exception as error:
        print(f"actlog: could not write the action record: {error}", file=sys.stderr)
    return returncode if returncode >= 0 else 128 - returncode


if __name__ == "__main__":
    sys.exit(main())
