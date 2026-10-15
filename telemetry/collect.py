"""Collect agent telemetry from local transcripts into one SQLite database.

Sources (read only, never modified):
  claude  ~/.claude/projects/**/*.jsonl         Claude Code sessions and subagents
  codex   ~/.codex/{sessions,archived_sessions}  Codex rollouts
  omp     ~/.omp/agent/sessions/**/*.jsonl      omp sessions and subagents, plus the session
          dirs of `agent spawn` runs and scheduler omp runs
  actions ~/.local/state/agentbox-telemetry/actions/*.jsonl  our own tools' action logs
  runs    ~/.local/state/agentbox-telemetry/runs/*.jsonl     run starts and session links (runlog.py)

Runs: a run is one launch of an agent (agent spawn, agent-desktop agent, a scheduler run).
Every collect also reads the scheduler's state.json and ~/.omp/runs, links omp processes that
are running inside a run, and links sessions seen in actions that carry a run id. session_runs
maps sessions to runs; the run_events and run_totals views put events and tokens per run.

Every file is read from a saved byte offset, only up to its last complete line. A file whose
inode changed or that shrank is read again from the start. Every event has a unique
(source, key), so reading a line twice changes nothing, and the database can be deleted and
rebuilt from the transcripts at any time.

Events store sizes and counts, never prompt, tool or output content. The one piece of text kept
per session is the short title the agent itself generated or the owner set (Claude ai-title or
agent-name, omp title, Codex thread_name), plus the git branch; prompts are never copied.
Skill names and the paths of instruction files (CLAUDE.md, AGENTS.md, attached files) are kept
as identifiers. Every event keeps its transcript file and line, so a viewer can look the content
up in the transcript itself (the dashboard does this on demand).

What fills the context is recorded as `context` events (name = category, args_chars = size):
system prompts, memory files, skill and tool listings, attached files, reminders, injected
messages and compaction summaries. `skill` events record each skill load (name = skill).

Known gaps in the transcripts themselves:
  - Claude subagent messages carry usage from the start of the stream, so their output tokens
    are too low (attrs.partial_output). Input and cache tokens are right. Per-session totals
    in session_meta.reported_cost_usd / attrs.model_usage come from Claude and are complete.
  - Claude compaction calls and small side calls (titles) have no message, only those totals.

Usage: collect.py [collect] [--rebuild] | summary [--days N]
"""

import argparse
import fcntl
import glob
import json
import os
import re
import sqlite3
import sys
import time
from datetime import datetime, timezone

HOME = os.path.expanduser("~")
STATE_DIR = os.path.join(HOME, ".local/state/agentbox-telemetry")
DB_PATH = os.path.join(STATE_DIR, "events.db")
ACTIONS_DIR = os.path.join(STATE_DIR, "actions")
SOURCES = {
    "claude": [".claude/projects/**/*.jsonl"],
    "codex": [".codex/sessions/**/*.jsonl", ".codex/archived_sessions/**/*.jsonl"],
    "omp": [".omp/agent/sessions/**/*.jsonl", ".omp/runs/*/session/**/*.jsonl", ".omp/runs/.archive/*/session/**/*.jsonl",
            ".local/state/agent-scheduler/logs/*.session/**/*.jsonl"],
    "actions": [".local/state/agentbox-telemetry/actions/*.jsonl"],
    "runs": [".local/state/agentbox-telemetry/runs/*.jsonl"],
}
SCHEDULER_STATE = os.path.join(HOME, ".local/state/agent-scheduler/state.json")
CODEX_SESSION_INDEX = os.path.join(HOME, ".codex/session_index.jsonl")
TITLE_CHARS = 200
OMP_RUNS = os.path.join(HOME, ".omp/runs")
TRAILING_ID = re.compile(r"([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$")
TOKEN_COLUMNS = ("input_tokens", "cached_tokens", "cache_write_tokens", "output_tokens", "reasoning_tokens")
EVENT_COLUMNS = (
    "source", "key", "kind", "ts", "session_id", "agent", "run_id", "project", "model", "name",
    *TOKEN_COLUMNS, "cost_usd", "duration_ms", "args_chars", "result_chars", "ok", "attrs", "file", "line",
)
TRUNCATED_TOKENS = re.compile(r"original token count: (\d+)")
SKILL_BASE = re.compile(r"^Base directory for this skill: (\S+)")
SKILL_REF = re.compile(r"(?:skill://|/skills/(?:\.system/)?)([A-Za-z0-9_-][A-Za-z0-9_.-]*)(?:/([^\s\"':]*))?")
READ_TOOLS = {"read", "view"}
CLAUDE_ATTACHMENTS = {
    "nested_memory": ("memory", "content"), "file": ("file", "content"), "skill_listing": ("skill_listing", "content"),
    "deferred_tools_delta": ("tool_listing", "addedLines"), "deferred_tools_record": ("tool_listing", "entries"),
    "mcp_instructions_delta": ("tool_listing", "addedBlocks"), "agent_listing_delta": ("tool_listing", "addedLines"),
    "queued_command": ("queued_message", "prompt"),
}
CLAUDE_ATTACHMENTS_SKIPPED = {"prompt_snapshot", "thinking_drop", "credential_org", "remote_session_change",
                              "compact_file_reference", "command_permissions", "output_style"}
COMMIT_EVERY = 5000

SCHEMA = """
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY,
  source TEXT NOT NULL,
  key TEXT NOT NULL,
  kind TEXT NOT NULL,
  ts TEXT NOT NULL,
  session_id TEXT,
  agent TEXT,
  run_id TEXT,
  project TEXT,
  model TEXT,
  name TEXT,
  input_tokens INTEGER,
  cached_tokens INTEGER,
  cache_write_tokens INTEGER,
  output_tokens INTEGER,
  reasoning_tokens INTEGER,
  cost_usd REAL,
  duration_ms INTEGER,
  args_chars INTEGER,
  result_chars INTEGER,
  ok INTEGER,
  attrs TEXT,
  file TEXT,
  line INTEGER,
  UNIQUE (source, key)
);
CREATE INDEX IF NOT EXISTS events_ts ON events (ts);
CREATE INDEX IF NOT EXISTS events_session ON events (source, session_id);
CREATE INDEX IF NOT EXISTS events_kind_name ON events (kind, name);
CREATE INDEX IF NOT EXISTS events_run ON events (run_id) WHERE run_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS files (
  path TEXT PRIMARY KEY,
  source TEXT NOT NULL,
  inode INTEGER,
  offset INTEGER NOT NULL DEFAULT 0,
  line INTEGER NOT NULL DEFAULT 0,
  bad_lines INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT '{}',
  updated TEXT
);

CREATE TABLE IF NOT EXISTS session_meta (
  source TEXT NOT NULL,
  session_id TEXT NOT NULL,
  parent_session_id TEXT,
  client_version TEXT,
  reported_cost_usd REAL,
  attrs TEXT,
  title TEXT,
  branch TEXT,
  PRIMARY KEY (source, session_id)
);

CREATE TABLE IF NOT EXISTS rate_limits (
  source TEXT NOT NULL,
  ts TEXT NOT NULL,
  window TEXT NOT NULL,
  used_percent REAL,
  window_minutes INTEGER,
  resets_at TEXT,
  PRIMARY KEY (source, ts, window)
);

CREATE TABLE IF NOT EXISTS runs (
  run_id TEXT PRIMARY KEY,
  launcher TEXT,
  name TEXT,
  started TEXT,
  finished TEXT,
  status TEXT,
  cwd TEXT,
  attrs TEXT
);

CREATE TABLE IF NOT EXISTS session_runs (
  source TEXT NOT NULL,
  session_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  via TEXT,
  PRIMARY KEY (source, session_id)
);

DROP VIEW IF EXISTS sessions;
CREATE VIEW sessions AS
SELECT e.source, e.session_id, r.run_id,
       min(e.ts) AS started, max(e.ts) AS ended,
       max(e.project) AS project,
       group_concat(DISTINCT e.model) AS models,
       sum(e.kind = 'llm_call') AS llm_calls,
       sum(e.kind = 'tool_call') AS tool_calls,
       sum(e.kind = 'user_prompt') AS prompts,
       sum(coalesce(e.input_tokens, 0)) AS input_tokens,
       sum(coalesce(e.cached_tokens, 0)) AS cached_tokens,
       sum(coalesce(e.cache_write_tokens, 0)) AS cache_write_tokens,
       sum(coalesce(e.output_tokens, 0)) AS output_tokens,
       sum(e.cost_usd) AS cost_usd,
       m.reported_cost_usd, m.parent_session_id, m.client_version, m.title, m.branch
FROM events e
LEFT JOIN session_meta m ON m.source = e.source AND m.session_id = e.session_id
LEFT JOIN session_runs r ON r.source = e.source AND r.session_id = e.session_id
WHERE e.session_id IS NOT NULL
GROUP BY e.source, e.session_id;

DROP VIEW IF EXISTS run_events;
CREATE VIEW run_events AS
SELECT coalesce(e.run_id, l.run_id) AS run, e.*
FROM events e
LEFT JOIN session_runs l ON l.source = e.source AND l.session_id = e.session_id
WHERE e.run_id IS NOT NULL OR l.run_id IS NOT NULL;

DROP VIEW IF EXISTS run_totals;
CREATE VIEW run_totals AS
SELECT r.run_id, r.launcher, r.name, r.started, r.finished, r.status, max(x.ts) AS last_event,
       count(DISTINCT CASE WHEN x.kind = 'llm_call' THEN x.source || ':' || x.session_id END) AS sessions,
       group_concat(DISTINCT x.model) AS models,
       sum(x.kind = 'llm_call') AS llm_calls,
       sum(x.kind = 'tool_call') AS tool_calls,
       sum(CASE WHEN x.kind = 'llm_call' THEN x.input_tokens END) AS input_tokens,
       sum(CASE WHEN x.kind = 'llm_call' THEN x.cached_tokens END) AS cached_tokens,
       sum(CASE WHEN x.kind = 'llm_call' THEN x.cache_write_tokens END) AS cache_write_tokens,
       sum(CASE WHEN x.kind = 'llm_call' THEN x.output_tokens END) AS output_tokens,
       sum(x.cost_usd) AS cost_usd,
       (SELECT sum(m.reported_cost_usd) FROM session_runs l JOIN session_meta m USING (source, session_id)
        WHERE l.run_id = r.run_id) AS reported_cost_usd,
       sum(x.kind = 'action') AS actions,
       sum(x.kind = 'action' AND x.ok = 0) AS failed_actions,
       sum(CASE WHEN x.kind = 'action' THEN x.result_chars END) AS action_output_chars
FROM runs r
LEFT JOIN run_events x ON x.run = r.run_id
GROUP BY r.run_id;
"""


def size(value):
    if value is None:
        return None
    if isinstance(value, str):
        return len(value)
    return len(json.dumps(value, ensure_ascii=False, separators=(",", ":")))


def text_of(value):
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "".join(text_of(item) for item in value)
    if isinstance(value, dict):
        return text_of(value.get("text") or value.get("content") or "")
    return ""


def iso_from_ms(ms):
    return datetime.fromtimestamp(ms / 1000, timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def iso_from_s(seconds):
    return iso_from_ms(seconds * 1000) if seconds else None


def compact(attrs):
    attrs = {k: v for k, v in attrs.items() if v not in (None, "", {}, [])}
    return json.dumps(attrs, separators=(",", ":")) if attrs else None


class Sink:
    """Writes normalised events for one file, filling in file context and line numbers."""

    def __init__(self, db, source, path):
        self.db = db
        self.source = source
        self.path = path
        self.line = 0
        self.count = 0

    def event(self, kind, key, ts, **fields):
        if not key or not ts:
            return
        fields["attrs"] = compact(fields.get("attrs") or {})
        row = {"source": self.source, "key": key, "kind": kind, "ts": ts, "file": self.path, "line": self.line, **fields}
        columns = [c for c in EVENT_COLUMNS if c in row]
        keep = []
        for c in columns:
            if c in ("source", "key", "kind", "ts", "file", "line"):
                continue
            if c in TOKEN_COLUMNS:
                keep.append(f"{c} = max(coalesce({c}, 0), coalesce(excluded.{c}, 0))")
            else:
                keep.append(f"{c} = coalesce(excluded.{c}, {c})")
        self.db.execute(
            f"INSERT INTO events ({', '.join(columns)}) VALUES ({', '.join('?' * len(columns))}) "
            f"ON CONFLICT (source, key) DO UPDATE SET {', '.join(keep)}",
            [row[c] for c in columns],
        )
        self.count += 1

    def finish_tool(self, key, ts, result_chars, ok, attrs=None):
        """Completes a tool_call event with its result size, outcome and duration."""
        self.db.execute(
            "UPDATE events SET result_chars = ?, ok = ?, "
            "duration_ms = max(0, CAST(round((julianday(?) - julianday(ts)) * 86400000) AS INTEGER)), "
            "attrs = CASE WHEN ? IS NULL THEN attrs ELSE json_patch(coalesce(attrs, '{}'), ?) END "
            "WHERE source = ? AND key = ?",
            (result_chars, ok, ts, attrs, attrs, self.source, key),
        )

    def session(self, session_id, **fields):
        if not session_id:
            return
        columns = ["source", "session_id", *fields]
        self.db.execute(
            f"INSERT INTO session_meta ({', '.join(columns)}) VALUES ({', '.join('?' * len(columns))}) "
            f"ON CONFLICT (source, session_id) DO UPDATE SET "
            + ", ".join(f"{c} = coalesce(excluded.{c}, {c})" for c in fields),
            [self.source, session_id, *fields.values()],
        )

    def rate_limit(self, ts, window, info):
        self.db.execute(
            "INSERT OR IGNORE INTO rate_limits VALUES (?, ?, ?, ?, ?, ?)",
            (self.source, ts, window, info.get("used_percent"), info.get("window_minutes"), iso_from_s(info.get("resets_at"))),
        )


def skill_ref(value):
    """The skill a read targets: skill://NAME[/part] or .../skills/NAME/SKILL.md (and its reference files)."""
    match = SKILL_REF.search(value if isinstance(value, str) else json.dumps(value or ""))
    return (match.group(1), match.group(2) if match.group(2) not in (None, "", "SKILL.md") else None) if match else (None, None)


def truncation_attrs(text):
    match = TRUNCATED_TOKENS.search(text or "")
    return json.dumps({"original_tokens": int(match.group(1))}) if match else None


def claude_line(sink, state, d):
    kind = d.get("type")
    ts = d.get("timestamp")
    session_id = d.get("sessionId") or state.get("session_id")
    state["session_id"] = session_id
    if d.get("cwd"):
        state["cwd"] = d["cwd"]
    common = {"session_id": session_id, "agent": state.get("agent"), "project": state.get("cwd")}
    if d.get("version"):
        sink.session(session_id, client_version=d["version"])
    branch = d.get("gitBranch")
    if branch and branch != "HEAD" and branch != state.get("branch"):
        state["branch"] = branch
        sink.session(session_id, branch=branch)

    if kind == "assistant":
        message = d.get("message") or {}
        model = message.get("model")
        if d.get("isApiErrorMessage"):
            sink.event("error", d.get("uuid"), ts, **common, model=model, name=d.get("error") or d.get("apiError"), ok=0,
                       attrs={"status": d.get("apiErrorStatus"), "api_error": d.get("apiError"),
                              "message": text_of(message.get("content"))[:200]})
            return
        usage = message.get("usage") or {}
        if usage and model != "<synthetic>":
            cache = usage.get("cache_creation") or {}
            sink.event(
                "llm_call", message.get("id") or d.get("requestId") or d.get("uuid"), ts, **common, model=model,
                name="sidechain" if d.get("isSidechain") else "main",
                input_tokens=usage.get("input_tokens"),
                cached_tokens=usage.get("cache_read_input_tokens"),
                cache_write_tokens=usage.get("cache_creation_input_tokens"),
                output_tokens=usage.get("output_tokens"),
                reasoning_tokens=(usage.get("output_tokens_details") or {}).get("thinking_tokens"),
                attrs={"request_id": d.get("requestId"), "stop_reason": message.get("stop_reason"),
                       "partial_output": message.get("stop_reason") is None or None,
                       "cache_write_1h": cache.get("ephemeral_1h_input_tokens"),
                       "cache_write_5m": cache.get("ephemeral_5m_input_tokens"),
                       "service_tier": usage.get("service_tier"), "effort": d.get("perTurnEffort")},
            )
        for block in message.get("content") or []:
            if isinstance(block, dict) and block.get("type") in ("tool_use", "server_tool_use"):
                sink.event("tool_call", block.get("id"), ts, **common, model=model, name=block.get("name"),
                           args_chars=size(block.get("input")), attrs={"message_id": message.get("id")})
                if str(block.get("name")).lower() in READ_TOOLS:
                    skill, part = skill_ref(block.get("input"))
                    if skill:
                        sink.event("skill", f"{block.get('id')}:skill", ts, **common, name=skill, attrs={"via": "read", "part": part})

    elif kind == "user":
        message = d.get("message") or {}
        content = message.get("content")
        results = [b for b in content if isinstance(b, dict) and b.get("type") == "tool_result"] if isinstance(content, list) else []
        for block in results:
            sink.finish_tool(block.get("tool_use_id"), ts, size(block.get("content")), 0 if block.get("is_error") else 1,
                             truncation_attrs(text_of(block.get("content"))))
        text = text_of(content) if not results else ""
        if d.get("isCompactSummary"):
            sink.event("context", d.get("uuid"), ts, **common, name="compaction_summary", args_chars=len(text))
        elif d.get("isMeta") and text:
            skill = SKILL_BASE.match(text)
            if skill:
                sink.event("skill", d.get("uuid"), ts, **common, name=os.path.basename(skill.group(1).rstrip("/")),
                           args_chars=len(text), attrs={"via": "inject"})
            else:
                sink.event("context", d.get("uuid"), ts, **common, name="injected", args_chars=len(text), attrs={"item": "meta"})
        elif not results and content:
            sink.event("user_prompt", d.get("uuid"), ts, **common, args_chars=len(text),
                       name="sidechain" if d.get("isSidechain") else "main")

    elif kind == "attachment":
        claude_attachment(sink, state, d, ts, common)

    elif kind == "system":
        subtype = d.get("subtype")
        if subtype == "compact_boundary":
            meta = d.get("compactMetadata") or {}
            sink.event("compaction", d.get("uuid"), ts, **common, name=meta.get("trigger"), duration_ms=meta.get("durationMs"),
                       attrs={"pre_tokens": meta.get("preTokens"), "post_tokens": meta.get("postTokens")})
        elif subtype == "turn_duration":
            sink.event("turn", d.get("uuid"), ts, **common, duration_ms=d.get("durationMs"),
                       attrs={"messages": d.get("messageCount")})

    elif kind == "cost-state":
        sink.session(d.get("sessionId"), reported_cost_usd=d.get("totalCostUSD"),
                     attrs=compact({"model_usage": d.get("modelUsage"), "api_ms": d.get("totalAPIDuration"),
                                    "tool_ms": d.get("totalToolDuration"), "lines_added": d.get("totalLinesAdded"),
                                    "lines_removed": d.get("totalLinesRemoved")}))

    elif kind == "agent-name" and d.get("agentName"):
        state["named"] = True
        sink.session(session_id, title=d["agentName"][:TITLE_CHARS])

    elif kind == "ai-title" and d.get("aiTitle") and not state.get("named"):
        sink.session(session_id, title=d["aiTitle"][:TITLE_CHARS])

    elif kind == "pr-link":
        sink.event("pr", d.get("prUrl"), ts or state.get("last_ts"), **common, name=d.get("prUrl"))

    if ts:
        state["last_ts"] = ts


def claude_attachment(sink, state, d, ts, common):
    """Context the client adds around prompts: memory files, listings, attached files, reminders."""
    attachment = d.get("attachment") or {}
    kind = attachment.get("type")
    if kind == "prompt_snapshot":
        chars = size(attachment.get("systemPrompt"))
        if chars and chars != state.get("system_chars"):
            state["system_chars"] = chars
            sink.event("context", d.get("uuid"), ts, **common, name="system_prompt", args_chars=chars)
        return
    if not kind or kind in CLAUDE_ATTACHMENTS_SKIPPED:
        return
    category, field = CLAUDE_ATTACHMENTS.get(kind, ("reminder", None))
    value = attachment.get(field) if field else {k: v for k, v in attachment.items() if k != "type"}
    chars = len(text_of(value)) if isinstance(value, str) else size(value)
    if chars:
        sink.event("context", d.get("uuid"), ts, **common, name=category, args_chars=chars,
                   attrs={"item": attachment.get("displayPath") or attachment.get("filename") or kind})


def codex_line(sink, state, d):
    kind = d.get("type")
    ts = d.get("timestamp")
    payload = d.get("payload") or {}
    common = lambda: {"session_id": state.get("session_id"), "project": state.get("cwd"), "model": state.get("model")}

    if kind == "session_meta":
        state.update(session_id=payload.get("id"), cwd=payload.get("cwd"))
        sink.session(payload.get("id"), client_version=payload.get("cli_version"),
                     branch=(payload.get("git") or {}).get("branch") or None,
                     attrs=compact({"originator": payload.get("originator"), "source": payload.get("source")}))
    elif kind == "turn_context":
        settings = (payload.get("collaboration_mode") or {}).get("settings") or {}
        state.update(model=payload.get("model"), cwd=payload.get("cwd") or state.get("cwd"),
                     effort=settings.get("reasoning_effort") or payload.get("effort"))
    elif kind == "token_usage_record":
        usage = payload.get("usage") or {}
        cached = usage.get("cached_input_tokens") or 0
        sink.event(
            "llm_call", payload.get("response_id"), ts, **common(),
            input_tokens=max(0, (usage.get("input_tokens") or 0) - cached),
            cached_tokens=cached,
            cache_write_tokens=usage.get("cache_write_input_tokens"),
            output_tokens=usage.get("output_tokens"),
            reasoning_tokens=usage.get("reasoning_output_tokens"),
            attrs={"turn_id": payload.get("turn_id"), "effort": state.get("effort")},
        )
    elif kind == "response_item":
        item_type = payload.get("type")
        if item_type == "message" and payload.get("role") in ("developer", "system", "user"):
            codex_context(sink, state, d, payload, ts, common())
        elif item_type in ("custom_tool_call", "function_call", "local_shell_call", "tool_search_call", "web_search_call"):
            sink.event("tool_call", payload.get("call_id") or payload.get("id"), ts, **common(),
                       name=payload.get("name") or item_type,
                       args_chars=size(payload.get("input") or payload.get("arguments") or payload.get("action")))
        elif item_type in ("custom_tool_call_output", "function_call_output"):
            output = payload.get("output")
            text = text_of(output)
            ok = 1 if text.startswith("Script completed") else 0 if text.startswith("Script ") else None
            sink.finish_tool(payload.get("call_id"), ts, size(output), ok, truncation_attrs(text))
    elif kind == "event_msg":
        event_type = payload.get("type")
        if event_type == "token_count":
            limits = payload.get("rate_limits") or {}
            for window in ("primary", "secondary"):
                if limits.get(window):
                    sink.rate_limit(ts, window, limits[window])
        elif event_type == "item_completed":
            codex_item(sink, payload, ts, common())
        elif event_type == "task_complete":
            sink.event("turn", payload.get("turn_id"), ts, **common(), duration_ms=payload.get("duration_ms"),
                       attrs={"ttft_ms": payload.get("time_to_first_token_ms")})
        elif event_type in ("error", "stream_error", "turn_aborted"):
            sink.event("error", f"{state.get('session_id')}:{d.get('ordinal') or ts}", ts, **common(), name=event_type, ok=0,
                       attrs={"message": str(payload.get("message") or payload.get("reason") or "")[:200]})


def codex_context(sink, state, d, payload, ts, common):
    """Instructions Codex sends as messages; real prompts are recorded from UserMessage items instead."""
    for index, part in enumerate(payload.get("content") or []):
        text = text_of(part)
        if payload.get("role") != "user":
            category = "skill_listing" if text.lstrip().startswith("<skills_instructions>") else "system_prompt"
        elif text.lstrip().startswith("<environment_context>"):
            category = "reminder"
        elif "AGENTS.md instructions" in text[:200]:
            category = "memory"
        else:
            continue
        sink.event("context", f"{state.get('session_id')}:ctx:{d.get('ordinal') or ts}:{index}", ts, **common,
                   name=category, args_chars=len(text))


def codex_item(sink, payload, ts, common):
    """Records what ran inside a model tool call (commands, MCP tools, web search) as tool_exec."""
    item = payload.get("item") or {}
    item_type = item.get("type")
    if item_type == "UserMessage":
        sink.event("user_prompt", item.get("id"), ts, **common, args_chars=size(text_of(item.get("content"))))
        return
    if item_type == "CommandExecution":
        name = (item.get("parsed_cmd") or [{}])[0].get("type") or "command"
        ok = 1 if item.get("exit_code") == 0 else 0
        result = item.get("aggregated_output")
        skill, part = skill_ref(item.get("command")) if "SKILL.md" in str(item.get("command")) or name == "read" else (None, None)
        if skill:
            sink.event("skill", f"{item.get('id')}:skill", ts, **common, name=skill, attrs={"via": "read", "part": part})
    elif item_type == "McpToolCall":
        name = f"mcp__{item.get('server')}__{item.get('tool')}"
        result = item.get("result")
        ok = 0 if (result or {}).get("isError") or item.get("status") == "failed" else 1
    elif item_type == "Extension":
        name = item.get("kind") or "extension"
        result = item.get("results")
        ok = 0 if item.get("status") == "failed" else 1
    else:
        return
    started, completed = payload.get("started_at_ms"), payload.get("completed_at_ms")
    sink.event("tool_exec", item.get("id"), ts, **common, name=name, ok=ok, result_chars=size(result),
               duration_ms=completed - started if started and completed else None,
               attrs={"turn_id": payload.get("turn_id")})


def omp_line(sink, state, d):
    kind = d.get("type")
    ts = d.get("timestamp")
    entry_key = lambda: f"{state.get('session_id')}:{d.get('id')}"
    common = lambda: {"session_id": state.get("session_id"), "agent": state.get("agent"),
                      "project": state.get("cwd"), "model": state.get("model")}

    if kind == "session_init":
        sink.event("context", entry_key(), ts, **common(), name="system_prompt", args_chars=size(d.get("systemPrompt")),
                   attrs={"item": d.get("agent")})
    elif kind == "custom_message":
        details = d.get("details") if isinstance(d.get("details"), dict) else {}
        if d.get("customType") == "skill-prompt":
            sink.event("skill", entry_key(), ts, **common(), name=details.get("name"), args_chars=size(text_of(d.get("content"))),
                       attrs={"via": "inject"})
        else:
            sink.event("context", entry_key(), ts, **common(), name="injected", args_chars=size(text_of(d.get("content"))),
                       attrs={"item": d.get("customType")})
    elif kind == "session":
        state.update(session_id=d.get("id"), cwd=d.get("cwd"))
        parent = d.get("parentSession")
        parent_id = os.path.basename(parent).rsplit("_", 1)[-1].removesuffix(".jsonl") if parent else None
        sink.session(d.get("id"), parent_session_id=parent_id)
    elif kind == "model_change":
        state["model"] = d.get("model")
    elif kind == "model_usage":
        usage = d.get("usage") or {}
        sink.event("llm_call", entry_key(), ts, **{**common(), "model": d.get("model")}, name=d.get("purpose"),
                   input_tokens=usage.get("input"), cached_tokens=usage.get("cacheRead"),
                   cache_write_tokens=usage.get("cacheWrite"), output_tokens=usage.get("output"),
                   cost_usd=(usage.get("cost") or {}).get("total"), ok=0 if d.get("errorMessage") else 1,
                   attrs={"role": d.get("role"), "provider": d.get("provider"), "stop_reason": d.get("stopReason"),
                          "error": (d.get("errorMessage") or "")[:200]})
    elif kind == "compaction":
        sink.event("compaction", entry_key(), ts, **common(), name="compaction",
                   attrs={k: v for k, v in d.items() if isinstance(v, (int, float)) and not isinstance(v, bool)} |
                         {"summary_chars": size(d.get("summary"))})
        sink.event("context", entry_key() + ":summary", ts, **common(), name="compaction_summary", args_chars=size(d.get("summary")))
    elif kind == "message":
        message = d.get("message") or {}
        role = message.get("role")
        if role == "assistant":
            omp_assistant(sink, state, d, message, ts, entry_key(), common())
        elif role == "toolResult":
            content = message.get("content")
            sink.finish_tool(message.get("toolCallId"), ts, size(content), 0 if message.get("isError") else 1,
                             truncation_attrs(text_of(content)))
        elif role == "user":
            sink.event("user_prompt", entry_key(), ts, **common(), args_chars=size(text_of(message.get("content"))))


def omp_assistant(sink, state, d, message, ts, key, common):
    model = message.get("model") or common["model"]
    state["model"] = model
    common = {**common, "model": model}
    usage = message.get("usage") or {}
    error = message.get("errorMessage")
    sink.event("llm_call", key, ts, **common, name="main",
               input_tokens=usage.get("input"), cached_tokens=usage.get("cacheRead"),
               cache_write_tokens=usage.get("cacheWrite"), output_tokens=usage.get("output"),
               cost_usd=(usage.get("cost") or {}).get("total"), ok=0 if error or message.get("stopReason") == "error" else 1,
               attrs={"provider": message.get("provider"), "response_id": message.get("responseId"),
                      "stop_reason": message.get("stopReason"), "error": (error or "")[:200]})
    for block in message.get("content") or []:
        if isinstance(block, dict) and block.get("type") == "toolCall":
            sink.event("tool_call", block.get("id"), ts, **common, name=block.get("name"),
                       args_chars=size(block.get("arguments")), attrs={"llm_call": key})
            if str(block.get("name")).lower() in READ_TOOLS:
                skill, part = skill_ref(block.get("arguments"))
                if skill:
                    sink.event("skill", f"{block.get('id')}:skill", ts, **common, name=skill, attrs={"via": "read", "part": part})


def actions_line(sink, state, d):
    """Our own tools' action logs: one JSON object per action, already in event form."""
    fields = {c: d.get(c) for c in EVENT_COLUMNS if c not in ("source", "key", "kind", "ts", "file", "line", "attrs")}
    sink.event(d.get("kind") or "action", d.get("key"), d.get("ts"),
               **{k: v for k, v in fields.items() if v is not None}, attrs=d.get("attrs") or {})


def upsert_run(db, run_id, **fields):
    if not run_id:
        return
    fields = {k: v for k, v in fields.items() if v is not None}
    columns = ["run_id", *fields]
    db.execute(
        f"INSERT INTO runs ({', '.join(columns)}) VALUES ({', '.join('?' * len(columns))}) ON CONFLICT (run_id) DO "
        + (f"UPDATE SET {', '.join(f'{c} = coalesce(excluded.{c}, {c})' for c in fields)}" if fields else "NOTHING"),
        [run_id, *fields.values()],
    )


def link_session(db, source, session_id, run_id, via):
    if source and session_id and run_id:
        db.execute("INSERT OR IGNORE INTO session_runs VALUES (?, ?, ?, ?)", (source, session_id, run_id, via))


def runs_line(sink, state, d):
    """runlog.py records: a run starting, or a session that belongs to a run."""
    if d.get("type") == "run":
        upsert_run(sink.db, d.get("run_id"), launcher=d.get("launcher"), name=d.get("name"), started=d.get("ts"),
                   cwd=d.get("cwd"), attrs=compact(d.get("attrs") or {}))
    elif d.get("type") == "link":
        upsert_run(sink.db, d.get("run_id"))
        link_session(sink.db, d.get("source"), d.get("session_id"), d.get("run_id"), d.get("via"))


PARSERS = {"claude": claude_line, "codex": codex_line, "omp": omp_line, "actions": actions_line, "runs": runs_line}


def initial_state(source, path):
    state = {}
    if source == "claude" and "/subagents/" in path:
        state["agent"] = os.path.basename(path).removesuffix(".jsonl")
    if source == "omp" and os.path.isfile(os.path.dirname(path) + ".jsonl"):
        state["agent"] = os.path.basename(path).removesuffix(".jsonl")
    return state


def ingest_file(db, source, path, stats):
    try:
        st = os.stat(path)
    except FileNotFoundError:
        return
    row = db.execute("SELECT inode, offset, line, bad_lines, state FROM files WHERE path = ?", (path,)).fetchone()
    if row and row[0] == st.st_ino and row[1] <= st.st_size:
        inode, offset, line, bad, state = row[0], row[1], row[2], row[3], json.loads(row[4])
    else:
        inode, offset, line, bad, state = st.st_ino, 0, 0, 0, initial_state(source, path)
    if offset == st.st_size:
        return

    sink = Sink(db, source, path)
    parse = PARSERS[source]
    with open(path, "rb") as f:
        f.seek(offset)
        while True:
            raw = f.readline()
            if not raw.endswith(b"\n"):
                break
            offset += len(raw)
            line += 1
            sink.line = line
            try:
                record = json.loads(raw)
            except (json.JSONDecodeError, UnicodeDecodeError):
                bad += 1
                continue
            if not isinstance(record, dict):
                continue
            try:
                parse(sink, state, record)
            except Exception as error:
                bad += 1
                stats["errors"].append(f"{path}:{line}: {type(error).__name__}: {error}")
            if sink.count >= COMMIT_EVERY:
                save_file(db, path, source, inode, offset, line, bad, state)
                db.commit()
                stats["events"] += sink.count
                sink.count = 0
    if source == "omp":
        omp_title(sink, state, path)
    save_file(db, path, source, inode, offset, line, bad, state)
    db.commit()
    stats["events"] += sink.count
    stats["files"] += 1


def omp_title(sink, state, path):
    """omp rewrites its padded first line in place when the title changes, so it is read again on every change."""
    try:
        with open(path, "rb") as f:
            first = json.loads(f.readline())
    except (OSError, ValueError):
        return
    if isinstance(first, dict) and first.get("type") == "title" and first.get("title"):
        sink.session(state.get("session_id"), title=str(first["title"])[:TITLE_CHARS])


def codex_titles(db):
    """Codex keeps thread names in a separate index; the last line for a thread wins."""
    try:
        with open(CODEX_SESSION_INDEX) as f:
            lines = f.readlines()
    except OSError:
        return
    for raw in lines:
        try:
            d = json.loads(raw)
        except ValueError:
            continue
        if isinstance(d, dict) and d.get("id") and d.get("thread_name"):
            db.execute("UPDATE session_meta SET title = ? WHERE source = 'codex' AND session_id = ?",
                       (d["thread_name"][:TITLE_CHARS], d["id"]))
    db.commit()


def save_file(db, path, source, inode, offset, line, bad, state):
    db.execute(
        "INSERT INTO files (path, source, inode, offset, line, bad_lines, state, updated) VALUES (?, ?, ?, ?, ?, ?, ?, ?) "
        "ON CONFLICT (path) DO UPDATE SET inode = excluded.inode, offset = excluded.offset, line = excluded.line, "
        "bad_lines = excluded.bad_lines, state = excluded.state, updated = excluded.updated",
        (path, source, inode, offset, line, bad, json.dumps(state), datetime.now(timezone.utc).isoformat(timespec="seconds")),
    )


def connect():
    os.makedirs(ACTIONS_DIR, exist_ok=True)
    db = sqlite3.connect(DB_PATH, timeout=30)
    db.execute("PRAGMA journal_mode = WAL")
    db.execute("PRAGMA synchronous = NORMAL")
    existing = {row[1] for row in db.execute("PRAGMA table_info(session_meta)")}
    for column in ("title", "branch"):
        if existing and column not in existing:
            db.execute(f"ALTER TABLE session_meta ADD COLUMN {column} TEXT")
    db.executescript(SCHEMA)
    return db


def session_files(directory):
    return glob.glob(os.path.join(glob.escape(directory), "**/*.jsonl"), recursive=True)


def link_files(db, source, paths, run_id, via):
    for path in paths:
        match = TRAILING_ID.search(path)
        if match:
            link_session(db, source, match.group(1), run_id, via)


def scheduler_runs(db):
    """Runs of the agent scheduler; claude runners have a session id, omp runners a session dir."""
    try:
        runs = json.load(open(SCHEDULER_STATE)).get("runs") or []
    except (OSError, ValueError):
        return
    for run in runs:
        run_id = f"scheduler:{run.get('id')}"
        upsert_run(db, run_id, launcher="scheduler", name=run.get("jobId"), started=run.get("startedAt"),
                   finished=run.get("finishedAt"), status=run.get("outcome") or run.get("status"), cwd=run.get("cwd"),
                   attrs=compact({"runner": run.get("runner"), "exit": run.get("exitCode"), "unit": run.get("unit")}))
        if run.get("sessionId") and str(run.get("runner", "")).startswith("claude"):
            link_session(db, "claude", run["sessionId"], run_id, "scheduler")
        if run.get("sessionDir"):
            link_files(db, "omp", session_files(run["sessionDir"]), run_id, "scheduler")


def read_small(path):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return None


def omp_agent_runs(db):
    """`agent spawn` runs: ~/.omp/runs/<name>/ and archived ~/.omp/runs/.archive/<name>-<stamp>/."""
    for directory in glob.glob(os.path.join(OMP_RUNS, "*/")) + glob.glob(os.path.join(OMP_RUNS, ".archive/*/")):
        directory = directory.rstrip("/")
        started = read_small(os.path.join(directory, "started"))
        if not started or not started.isdigit():
            continue
        base = os.path.basename(directory)
        name = re.sub(r"-\d{8}-\d{6}$", "", base) if "/.archive/" in directory else base
        run_id = f"agent:{name}:{started}"
        exit_code = read_small(os.path.join(directory, "exit"))
        upsert_run(db, run_id, launcher="agent", name=name, started=iso_from_s(int(started)),
                   finished=iso_from_s(int(f)) if (f := read_small(os.path.join(directory, "finished"))) and f.isdigit() else None,
                   status=None if exit_code is None else "success" if exit_code == "0" else f"exit {exit_code}",
                   cwd=read_small(os.path.join(directory, "cwd")))
        link_files(db, "omp", session_files(os.path.join(directory, "session")), run_id, "agent")


def running_omp_sessions(db):
    """omp keeps its session file open; link it to the run in the process environment."""
    for pid in filter(str.isdigit, os.listdir("/proc")):
        try:
            if read_small(f"/proc/{pid}/comm") != "omp":
                continue
            with open(f"/proc/{pid}/environ", "rb") as f:
                env = dict(item.split(b"=", 1) for item in f.read().split(b"\0") if b"=" in item)
            run_id = (env.get(b"AGENTBOX_RUN_ID") or b"").decode() or (
                f"scheduler:{env[b'SCHEDULER_RUN_ID'].decode()}" if env.get(b"SCHEDULER_RUN_ID") else None)
            if not run_id:
                continue
            upsert_run(db, run_id)
            files = [os.readlink(f"/proc/{pid}/fd/{fd}") for fd in os.listdir(f"/proc/{pid}/fd")]
            link_files(db, "omp", [p for p in files if p.endswith(".jsonl")], run_id, "process")
        except (OSError, ValueError):
            continue


def link_runs(db):
    scheduler_runs(db)
    omp_agent_runs(db)
    running_omp_sessions(db)
    db.execute(
        "INSERT OR IGNORE INTO runs (run_id) SELECT DISTINCT run_id FROM events WHERE run_id IS NOT NULL")
    db.execute(
        "INSERT OR IGNORE INTO session_runs "
        "SELECT DISTINCT CASE WHEN json_extract(attrs, '$.sessions.claude_code_session_id') IS NOT NULL THEN 'claude' "
        "WHEN json_extract(attrs, '$.sessions.codex_thread_id') IS NOT NULL THEN 'codex' "
        "WHEN json_extract(attrs, '$.session_file') IS NOT NULL THEN 'omp' END, session_id, run_id, 'action' "
        "FROM events WHERE kind = 'action' AND run_id IS NOT NULL AND session_id IS NOT NULL")
    for _ in range(3):
        db.execute(
            "INSERT OR IGNORE INTO session_runs SELECT m.source, m.session_id, l.run_id, 'parent' FROM session_meta m "
            "JOIN session_runs l ON l.source = m.source AND l.session_id = m.parent_session_id")
    db.commit()


def source_files(source):
    paths = set()
    for pattern in SOURCES[source]:
        paths.update(glob.glob(os.path.join(HOME, pattern), recursive=True))
    return sorted(paths, key=lambda p: os.path.getmtime(p) if os.path.exists(p) else 0)


def collect(rebuild=False, quiet=False):
    os.makedirs(STATE_DIR, exist_ok=True)
    with open(os.path.join(STATE_DIR, "collect.lock"), "w") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            if not quiet:
                print("another collect run is in progress")
            return 0
        if rebuild:
            for suffix in ("", "-wal", "-shm"):
                if os.path.exists(DB_PATH + suffix):
                    os.remove(DB_PATH + suffix)
        started = time.monotonic()
        db = connect()
        stats = {"files": 0, "events": 0, "errors": []}
        for source in SOURCES:
            for path in source_files(source):
                ingest_file(db, source, path, stats)
        for name, step in (("linking runs", link_runs), ("codex titles", codex_titles)):
            try:
                step(db)
            except Exception as error:
                stats["errors"].append(f"{name}: {type(error).__name__}: {error}")
        db.close()
        for error in stats["errors"][:20]:
            print(error, file=sys.stderr)
        if not quiet or stats["errors"]:
            print(f"read {stats['files']} changed files, wrote {stats['events']} events, "
                  f"{len(stats['errors'])} errors, {time.monotonic() - started:.1f}s")
        return 1 if stats["errors"] else 0


def summary(days):
    db = connect()
    since = datetime.fromtimestamp(time.time() - days * 86400, timezone.utc).isoformat()
    print(f"Last {days} days (since {since[:16]}Z)\n")
    print(f"{'source':8} {'model':28} {'calls':>7} {'input':>12} {'cached':>13} {'cache_wr':>11} {'output':>10} {'cost$':>9}")
    for row in db.execute(
        "SELECT source, coalesce(model, '?'), count(*), sum(input_tokens), sum(cached_tokens), sum(cache_write_tokens), "
        "sum(output_tokens), sum(cost_usd) FROM events WHERE kind = 'llm_call' AND ts >= ? "
        "GROUP BY 1, 2 ORDER BY 1, 3 DESC", (since,)):
        print(f"{row[0]:8} {row[1][:28]:28} {row[2]:>7} {row[3] or 0:>12,} {row[4] or 0:>13,} {row[5] or 0:>11,} "
              f"{row[6] or 0:>10,} {row[7] or 0:>9.2f}")
    print(f"\n{'top tools by result size':40} {'calls':>7} {'result chars':>14} {'avg ms':>9} {'fail':>5}")
    for row in db.execute(
        "SELECT source || ' ' || name, count(*), sum(result_chars), avg(duration_ms), sum(ok = 0) FROM events "
        "WHERE kind = 'tool_call' AND ts >= ? GROUP BY source, name ORDER BY 3 DESC LIMIT 15", (since,)):
        print(f"{row[0][:40]:40} {row[1]:>7} {row[2] or 0:>14,} {row[3] or 0:>9.0f} {row[4] or 0:>5}")
    print(f"\n{'actions':40} {'calls':>7} {'output chars':>14} {'avg ms':>9} {'fail':>5}  outcomes")
    for row in db.execute(
        "SELECT name, count(*), sum(result_chars), avg(duration_ms), sum(ok = 0), "
        "group_concat(DISTINCT json_extract(attrs, '$.outcome')) FROM events "
        "WHERE kind = 'action' AND ts >= ? GROUP BY name ORDER BY 3 DESC LIMIT 15", (since,)):
        print(f"{row[0][:40]:40} {row[1]:>7} {row[2] or 0:>14,} {row[3] or 0:>9.0f} {row[4] or 0:>5}  {row[5] or ''}")
    print(f"\n{'runs with tokens or actions':44} {'sess':>4} {'calls':>6} {'input+cache':>12} {'output':>9} "
          f"{'cost$':>7} {'acts':>5} {'fail':>4}")
    for row in db.execute(
        "SELECT run_id, coalesce(launcher, '?') || ' ' || coalesce(name, ''), sessions, llm_calls, "
        "coalesce(input_tokens, 0) + coalesce(cached_tokens, 0) + coalesce(cache_write_tokens, 0), output_tokens, "
        "coalesce(cost_usd, reported_cost_usd), actions, failed_actions FROM run_totals "
        "WHERE coalesce(started, '9') >= ? AND (llm_calls > 0 OR actions > 0) ORDER BY started DESC LIMIT 15", (since,)):
        print(f"{row[1][:44]:44} {row[2] or 0:>4} {row[3] or 0:>6} {row[4] or 0:>12,} {row[5] or 0:>9,} "
              f"{row[6] or 0:>7.2f} {row[7] or 0:>5} {row[8] or 0:>4}")
    print()
    for row in db.execute("SELECT kind, count(*) FROM events WHERE ts >= ? GROUP BY kind ORDER BY 2 DESC", (since,)):
        print(f"{row[0]:12} {row[1]:>8}")


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = parser.add_subparsers(dest="command")
    run = sub.add_parser("collect", help="ingest new transcript lines (default)")
    run.add_argument("--rebuild", action="store_true", help="delete the database and read everything again")
    run.add_argument("--quiet", action="store_true")
    report = sub.add_parser("summary", help="token and tool totals")
    report.add_argument("--days", type=float, default=7)
    args = parser.parse_args()
    if args.command == "summary":
        summary(args.days)
        return 0
    return collect(getattr(args, "rebuild", False), getattr(args, "quiet", False))


if __name__ == "__main__":
    sys.exit(main())
