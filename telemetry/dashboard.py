"""Read-only web dashboard for the agentbox telemetry database.

Usage: dashboard.py [--host 0.0.0.0] [--port 6090]

Serves dashboard.html and its JSON API over events.db:
  /api/stats?days=N[&source=&model=&name=]
                                        tokens, cost, tools, agent-desktop actions, runs, rate limits
  /api/sessions?days=N&q=TEXT&source=S[&project=&branch=&model=&run=]
                                        sessions with their titles, searchable
  /api/session?source=S&id=ID[&agent=&model=&kind=&name=]
                                        one session: totals, per-call tokens, tools, skills, what filled the
                                        context, subagents, events
  /api/event?id=N                       one event's input and result, read from its transcript on demand
  /api/peek?ids=N,M                     one-line previews of event inputs

Filters narrow every panel to matching events, except that a panel which lists the values of a
filter (agents, models, tools, kinds) ignores that filter, so the other values stay pickable.

The database is opened with query_only, so the dashboard never writes; collect.py stays the only
writer. The database holds no content; /api/event and /api/peek read it from the transcript file
and line each event points at, and only when asked.
"""

import argparse
import base64
import itertools
import json
import os
import re
import sqlite3
import time
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

HERE = os.path.dirname(os.path.abspath(__file__))
DB_PATH = os.path.expanduser("~/.local/state/agentbox-telemetry/events.db")
PAGE = os.path.join(HERE, "dashboard.html")
HOME = os.path.expanduser("~")
REVEAL_CHARS = 20000
OMP_BLOBS = os.path.join(HOME, ".omp/agent/blobs")
IMAGE_MARK = "\u27e6image {}\u27e7"
IMAGE_MARKS = re.compile("\u27e6image (\\d+)\u27e7")
IMAGE_TYPES = re.compile(r"^image/(png|jpeg|gif|webp|svg\+xml)$")
DATA_URL = re.compile(r"^data:(image/[a-z+.-]+);base64,([A-Za-z0-9+/=\s]+)$")
MAX_IMAGE_CHARS = 12_000_000
PEEK_CHARS = 160
RESULT_SCAN_LINES = 5000
PEEK_FIELDS = ("command", "cmd", "pattern", "query", "url", "file_path", "path", "skill", "description", "prompt", "code")
MAIN_AGENT = "(agent IS NULL OR agent IN ('', 'main'))"
AGENT = "coalesce(nullif({p}agent, ''), 'main')"
STATS_FILTERS = {"source": "{p}source = ?", "model": "{p}model = ?", "name": "{p}name = ?"}
SESSION_FILTERS = {"agent": AGENT + " = ?", "model": "{p}model = ?", "kind": "{p}kind = ?", "name": "{p}name = ?"}
TOKENS = "coalesce(input_tokens, 0) + coalesce(cached_tokens, 0) + coalesce(cache_write_tokens, 0) + coalesce(output_tokens, 0)"


def rows(db, sql, params=()):
    cursor = db.execute(sql, params)
    names = [c[0] for c in cursor.description]
    return [dict(zip(names, row)) for row in cursor.fetchall()]


def connect():
    db = sqlite3.connect(DB_PATH, timeout=10)
    db.execute("PRAGMA query_only = 1")
    return db


def where_filters(filters, specs, prefix="", skip=()):
    """(" AND ..." SQL, params) for the filters that have a spec, minus the skipped ones."""
    sql, params = "", []
    for key, value in filters.items():
        if value and key in specs and key not in skip:
            sql += " AND " + specs[key].format(p=prefix)
            params.append(value)
    return sql, params


def since_iso(days):
    return datetime.fromtimestamp(time.time() - days * 86400, timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")


def stats(days, filters):
    db = connect()
    since = since_iso(days)
    bucket_format = "%Y-%m-%d %H:00" if days <= 2 else "%Y-%m-%d"
    bucket = f"strftime('{bucket_format}', ts, 'localtime')"
    f, fp = where_filters(filters, STATS_FILTERS)
    window = (since, *fp)
    facet = lambda key: (lambda sql, params: (sql, (since, *params)))(*where_filters(filters, STATS_FILTERS, skip=(key,)))
    fe, fep = where_filters(filters, STATS_FILTERS, "e.")
    # Claude reports cost per session, so it can be scoped by source but not by model or tool.
    reported = not filters.get("model") and not filters.get("name") and filters.get("source") in (None, "", "claude")
    try:
        totals = rows(db, f"""
            SELECT sum(kind = 'llm_call') AS llm_calls,
                   count(DISTINCT CASE WHEN kind = 'llm_call' THEN source || ':' || session_id END) AS sessions,
                   sum(CASE WHEN kind = 'llm_call' THEN input_tokens END) AS input,
                   sum(CASE WHEN kind = 'llm_call' THEN cached_tokens END) AS cached,
                   sum(CASE WHEN kind = 'llm_call' THEN cache_write_tokens END) AS cache_write,
                   sum(CASE WHEN kind = 'llm_call' THEN output_tokens END) AS output,
                   sum(cost_usd) AS event_cost,
                   sum(kind = 'tool_call') AS tool_calls, sum(kind = 'tool_call' AND ok = 0) AS tool_fail,
                   sum(kind = 'action') AS actions, sum(kind = 'action' AND ok = 0) AS action_fail,
                   sum(kind = 'compaction') AS compactions, sum(kind = 'error') AS errors,
                   sum(kind = 'user_prompt') AS prompts
            FROM events WHERE ts >= ?{f}""", window)[0]
        totals["reported_cost"] = db.execute(
            "SELECT sum(reported_cost_usd) FROM sessions WHERE source = 'claude' AND started >= ?", (since,)).fetchone()[0] if reported else None
        models_f, models_p = facet("model")
        tools_f, tools_p = facet("name")
        sources_f, sources_p = facet("source")
        matched = f"""(SELECT count(*) FROM events e WHERE e.source = s.source AND e.session_id = s.session_id
                       AND e.ts >= ?{fe})""" if fe else "NULL"
        return {
            "days": days,
            "bucket": "hour" if days <= 2 else "day",
            "generated": datetime.now(timezone.utc).isoformat(timespec="seconds"),
            "last_event": db.execute("SELECT max(ts) FROM events WHERE kind != 'action'").fetchone()[0],
            "totals": totals,
            "tokens_series": rows(db, f"""
                SELECT {bucket} AS bucket, source, sum(input_tokens) AS input, sum(cached_tokens) AS cached,
                       sum(cache_write_tokens) AS cache_write, sum(output_tokens) AS output, count(*) AS calls
                FROM events WHERE kind = 'llm_call' AND ts >= ?{f} GROUP BY 1, 2 ORDER BY 1""", window),
            "sources": rows(db, f"""
                SELECT source, sum({TOKENS}) AS tokens FROM events
                WHERE kind = 'llm_call' AND ts >= ?{sources_f} GROUP BY 1 ORDER BY 2 DESC""", sources_p),
            "cost_series": rows(db, f"""
                SELECT {bucket} AS bucket, source, sum(cost_usd) AS cost FROM events
                WHERE cost_usd IS NOT NULL AND ts >= ?{f} GROUP BY 1, 2
                UNION ALL
                SELECT strftime('{bucket_format}', started, 'localtime'), 'claude', sum(reported_cost_usd) FROM sessions
                WHERE source = 'claude' AND reported_cost_usd IS NOT NULL AND started >= ? AND ? GROUP BY 1
                ORDER BY 1""", (*window, since, reported)),
            "actions_series": rows(db, f"""
                SELECT {bucket} AS bucket, sum(ok = 1) AS ok, sum(ok = 0) AS failed FROM events
                WHERE kind = 'action' AND ts >= ?{f} GROUP BY 1 ORDER BY 1""", window),
            "models": rows(db, f"""
                SELECT source, coalesce(model, '?') AS model, count(*) AS calls, sum(input_tokens) AS input,
                       sum(cached_tokens) AS cached, sum(cache_write_tokens) AS cache_write,
                       sum(output_tokens) AS output, sum(cost_usd) AS cost
                FROM events WHERE kind = 'llm_call' AND ts >= ?{models_f} GROUP BY 1, 2 ORDER BY 4 + 5 + 6 DESC""", models_p),
            "tools": rows(db, f"""
                SELECT source, name, count(*) AS calls, sum(result_chars) AS result_chars, sum(args_chars) AS args_chars,
                       avg(duration_ms) AS avg_ms, sum(ok = 0) AS failed,
                       sum(json_extract(attrs, '$.original_tokens') IS NOT NULL) AS truncated
                FROM events WHERE kind IN ('tool_call', 'tool_exec') AND ts >= ?{tools_f}
                GROUP BY 1, 2 ORDER BY 4 DESC LIMIT 25""", tools_p),
            "actions": rows(db, f"""
                SELECT name, count(*) AS calls, sum(result_chars) AS output_chars, avg(duration_ms) AS avg_ms,
                       sum(ok = 0) AS failed, group_concat(DISTINCT json_extract(attrs, '$.outcome')) AS outcomes,
                       group_concat(DISTINCT agent) AS callers
                FROM events WHERE kind = 'action' AND ts >= ?{tools_f} GROUP BY 1 ORDER BY 3 DESC""", tools_p),
            "runs": rows(db, f"""
                SELECT run_id, launcher, name, started, coalesce(finished, last_event) AS ended, status, sessions, models,
                       llm_calls, tool_calls,
                       coalesce(input_tokens, 0) + coalesce(cached_tokens, 0) + coalesce(cache_write_tokens, 0) AS input_all,
                       output_tokens, coalesce(cost_usd, reported_cost_usd) AS cost, actions, failed_actions
                FROM run_totals WHERE coalesce(started, last_event) >= ? AND (llm_calls > 0 OR actions > 0)
                {f"""AND run_id IN (
                    SELECT sr.run_id FROM session_runs sr JOIN events e ON e.source = sr.source AND e.session_id = sr.session_id
                    WHERE e.ts >= ?{fe}
                    UNION SELECT e.run_id FROM events e WHERE e.run_id IS NOT NULL AND e.ts >= ?{fe})""" if fe else ""}
                ORDER BY coalesce(started, last_event) DESC LIMIT 30""", (since, *((since, *fep) * 2 if fe else ()))),
            "sessions": rows(db, f"""
                SELECT * FROM (SELECT s.source, s.session_id, s.run_id, s.title, s.project, s.models, s.started, s.ended, s.llm_calls,
                       s.tool_calls, s.prompts, s.input_tokens + s.cached_tokens + s.cache_write_tokens AS input_all,
                       s.output_tokens, coalesce(s.cost_usd, s.reported_cost_usd) AS cost,
                       (SELECT count(*) FROM events c WHERE c.source = s.source AND c.session_id = s.session_id
                        AND c.kind = 'compaction') AS compactions, {matched} AS matched
                FROM sessions s WHERE s.ended >= ? AND s.llm_calls > 0)
                WHERE matched IS NULL OR matched > 0
                ORDER BY matched DESC, input_all + output_tokens DESC LIMIT 20""", (*((since, *fep) if fe else ()), since)),
            "rate_limits": rows(db, """
                SELECT strftime('%Y-%m-%d %H:%M', ts, 'localtime') AS ts, window, used_percent FROM rate_limits
                WHERE source = 'codex' AND ts >= ? ORDER BY ts""", (since,)),
        }
    finally:
        db.close()


SESSION_COLUMNS = """
    s.source, s.session_id, s.run_id, s.title, s.branch, s.project, s.models, s.started, s.ended, s.llm_calls,
    s.tool_calls, s.prompts, s.input_tokens + s.cached_tokens + s.cache_write_tokens AS input_all, s.output_tokens,
    s.cached_tokens, coalesce(s.cost_usd, s.reported_cost_usd) AS cost, s.parent_session_id, s.client_version,
    r.launcher, r.name AS run_name, r.status AS run_status,
    (SELECT count(*) FROM events c WHERE c.source = s.source AND c.session_id = s.session_id
     AND c.kind = 'compaction') AS compactions,
    (SELECT count(*) FROM session_meta k WHERE k.source = s.source AND k.parent_session_id = s.session_id) AS children,
    (SELECT title FROM session_meta p WHERE p.source = s.source AND p.session_id = s.parent_session_id) AS parent_title
"""


LIST_FILTERS = {"source": "s.source = ?", "project": "s.project = ?", "branch": "s.branch = ?",
                "model": "instr(s.models, ?) > 0", "run": "s.run_id = ?"}


def session_list(days, query, filters, subagents):
    db = connect()
    try:
        where, params = ["s.ended >= ?"], [since_iso(days)]
        for key, value in filters.items():
            if value and key in LIST_FILTERS:
                where.append(LIST_FILTERS[key])
                params.append(value)
        if not subagents and not filters.get("run"):
            where.append("s.parent_session_id IS NULL")
        for word in query.split():
            where.append("(coalesce(s.title, '') || ' ' || coalesce(s.project, '') || ' ' || s.session_id || ' ' || coalesce(s.branch, '')"
                         " || ' ' || coalesce(r.launcher, '') || ' ' || coalesce(r.name, '') || ' ' || coalesce(s.models, '')) LIKE ?")
            params.append(f"%{word}%")
        return {"sessions": rows(db, f"""
            SELECT {SESSION_COLUMNS} FROM sessions s LEFT JOIN runs r ON r.run_id = s.run_id
            WHERE {" AND ".join(where)} ORDER BY s.ended DESC LIMIT 500""", params)}
    finally:
        db.close()


def session_detail(source, session_id, filters):
    db = connect()
    key = (source, session_id)
    scope = "source = ? AND session_id = ?"

    def scoped(skip=(), prefix=""):
        """The session plus every filter except the skipped ones, as (SQL after the scope, params)."""
        sql, params = where_filters(filters, SESSION_FILTERS, prefix, skip)
        return sql, (*key, *params)

    f, fp = scoped()
    tools = ("tool_call", "tool_exec", "action")
    try:
        found = rows(db, f"""SELECT {SESSION_COLUMNS} FROM sessions s LEFT JOIN runs r ON r.run_id = s.run_id
                             WHERE s.source = ? AND s.session_id = ?""", key)
        if not found:
            return None
        meta = db.execute("SELECT attrs FROM session_meta WHERE source = ? AND session_id = ?", key).fetchone()
        # LLM calls have no tool name, so kind and name filters narrow the event panels but not the token ones.
        llm_f, llm_p = scoped(skip=("kind", "name"))
        kinds_f, kinds_p = scoped(skip=("kind",))
        models_f, models_p = scoped(skip=("model", "kind", "name"))
        agents_f, agents_p = scoped(skip=("agent", "kind", "name"))
        tools_f, tools_p = scoped(skip=("name",))
        skills_f, skills_p = scoped(skip=("kind", "name"), prefix="s.")
        totals = None
        if f:
            totals = rows(db, f"""
                SELECT count(*) AS events, min(ts) AS started, max(ts) AS ended, sum(kind = 'user_prompt') AS prompts,
                       sum(kind = 'compaction') AS compactions, sum(kind IN {tools}) AS tool_calls
                FROM events WHERE {scope}{f}""", fp)[0]
            totals.update(rows(db, f"""
                SELECT count(*) AS llm_calls,
                       sum(coalesce(input_tokens, 0) + coalesce(cached_tokens, 0) + coalesce(cache_write_tokens, 0)) AS input_all,
                       sum(cached_tokens) AS cached_tokens, sum(output_tokens) AS output_tokens, sum(cost_usd) AS cost
                FROM events WHERE {scope} AND kind = 'llm_call'{llm_f}""", llm_p)[0])
        return {
            "filters": {k: v for k, v in filters.items() if v and k in SESSION_FILTERS},
            "scope": totals,
            # The synopsis always describes the whole session.
            "overall": {k: v for k, v in session_detail(source, session_id, {}).items()
                        if k in ("kinds", "models", "agents", "tools", "skills", "events")} if f else None,
            "session": found[0],
            "meta": json.loads(meta[0]) if meta and meta[0] else {},
            "run": (rows(db, "SELECT * FROM run_totals WHERE run_id = ?", (found[0]["run_id"],)) or [None])[0],
            "kinds": rows(db, f"SELECT kind, count(*) AS n FROM events WHERE {scope}{kinds_f} GROUP BY 1 ORDER BY 2 DESC", kinds_p),
            "models": rows(db, f"""
                SELECT coalesce(model, '?') AS model, count(*) AS calls, sum(input_tokens) AS input,
                       sum(cached_tokens) AS cached, sum(cache_write_tokens) AS cache_write,
                       sum(output_tokens) AS output, sum(cost_usd) AS cost
                FROM events WHERE {scope} AND kind = 'llm_call'{models_f} GROUP BY 1 ORDER BY 2 DESC""", models_p),
            "agents": rows(db, f"""
                SELECT {AGENT.format(p="")} AS agent, count(*) AS calls, min(ts) AS started, max(ts) AS ended,
                       sum({TOKENS}) AS tokens, sum(output_tokens) AS output, sum(cost_usd) AS cost
                FROM events WHERE {scope} AND kind = 'llm_call'{agents_f} GROUP BY 1 ORDER BY 3""", agents_p),
            "calls": rows(db, f"""
                SELECT ts, {AGENT.format(p="")} AS agent, model, name, input_tokens AS input, cached_tokens AS cached,
                       cache_write_tokens AS cache_write, output_tokens AS output, cost_usd AS cost, ok
                FROM events WHERE {scope} AND kind = 'llm_call'{llm_f} ORDER BY ts, id""", llm_p),
            "tools": rows(db, f"""
                SELECT name, count(*) AS calls, sum(result_chars) AS result_chars, sum(args_chars) AS args_chars,
                       avg(duration_ms) AS avg_ms, sum(ok = 0) AS failed,
                       sum(json_extract(attrs, '$.original_tokens') IS NOT NULL) AS truncated
                FROM events WHERE {scope} AND kind IN {tools}{tools_f} GROUP BY 1 ORDER BY 3 DESC""", tools_p),
            "children": rows(db, f"""
                SELECT {SESSION_COLUMNS} FROM sessions s LEFT JOIN runs r ON r.run_id = s.run_id
                WHERE s.source = ? AND s.parent_session_id = ? ORDER BY s.started""", key),
            "skills": rows(db, f"""
                SELECT s.name, count(*) AS loads, min(s.ts) AS first, max(s.ts) AS last,
                       sum(coalesce(s.args_chars, t.result_chars)) AS chars,
                       group_concat(DISTINCT json_extract(s.attrs, '$.via')) AS via,
                       group_concat(DISTINCT json_extract(s.attrs, '$.part')) AS parts,
                       group_concat(DISTINCT {AGENT.format(p="s.")}) AS agents
                FROM events s
                LEFT JOIN events t ON t.source = s.source AND t.key = substr(s.key, 1, length(s.key) - 6)
                WHERE s.source = ? AND s.session_id = ? AND s.kind = 'skill'{skills_f} GROUP BY 1 ORDER BY 2 DESC""", skills_p),
            "context": context_sources(db, key, filters.get("agent")),
            "largest": rows(db, f"""
                SELECT id, ts, kind, name, {AGENT.format(p="")} AS agent,
                       CASE WHEN kind IN ('tool_call', 'tool_exec') THEN result_chars ELSE args_chars END AS chars
                FROM events WHERE {scope} AND kind IN ('tool_call', 'tool_exec', 'skill', 'context', 'user_prompt')
                AND chars > 0{f} ORDER BY chars DESC LIMIT 25""", fp),
            "events": rows(db, f"""
                SELECT id, ts, kind, name, {AGENT.format(p="")} AS agent, model,
                       nullif({TOKENS}, 0) AS tokens, args_chars, result_chars, duration_ms, ok, cost_usd AS cost,
                       json_extract(attrs, '$.outcome') AS outcome, json_extract(attrs, '$.pre_tokens') AS pre_tokens,
                       json_extract(attrs, '$.original_tokens') AS original_tokens
                FROM events WHERE {scope}{f} ORDER BY ts, id LIMIT 5000""", fp),
        }
    finally:
        db.close()


def context_sources(db, key, agent=None):
    """One thread's context by source (the main thread unless an agent is given): what the first call already
    carried, and what was added after it."""
    thread, thread_params = (f"{AGENT.format(p='')} = ?", (agent,)) if agent else (MAIN_AGENT, ())
    calls = rows(db, f"""
        SELECT ts, coalesce(input_tokens, 0) + coalesce(cached_tokens, 0) + coalesce(cache_write_tokens, 0) AS tokens
        FROM events WHERE source = ? AND session_id = ? AND kind = 'llm_call' AND {thread}
        AND coalesce(input_tokens, 0) + coalesce(cached_tokens, 0) + coalesce(cache_write_tokens, 0) > 0
        ORDER BY ts, id LIMIT 1""", (*key, *thread_params))
    if not calls:
        return {"first_call": None, "sources": []}
    first, start = calls[0]["ts"], calls[0]["tokens"]
    sources = rows(db, f"""
        SELECT CASE kind WHEN 'tool_call' THEN 'tool:' || name WHEN 'tool_exec' THEN 'tool:' || name
                         WHEN 'user_prompt' THEN 'prompt' WHEN 'skill' THEN 'skill' ELSE name END AS source,
               CASE WHEN kind IN ('tool_call', 'tool_exec') THEN ts < ? ELSE ts <= ? END AS at_start, count(*) AS items,
               sum(CASE WHEN kind IN ('tool_call', 'tool_exec') THEN result_chars ELSE args_chars END) AS chars
        FROM events WHERE source = ? AND session_id = ? AND {thread}
        AND (kind IN ('user_prompt', 'context') OR (kind = 'skill' AND args_chars IS NOT NULL)
             OR (kind = 'tool_call') OR (kind = 'tool_exec' AND source != 'codex'))
        GROUP BY 1, 2 HAVING chars > 0""", (first, first, *key, *thread_params))
    output = db.execute(f"""SELECT sum(output_tokens) FROM events WHERE source = ? AND session_id = ? AND kind = 'llm_call'
                            AND {thread}""", (*key, *thread_params)).fetchone()[0]
    return {"first_call": first, "first_call_tokens": start, "output_tokens": output, "sources": sources}


def as_text(value):
    return value if isinstance(value, str) else json.dumps(value, indent=2, ensure_ascii=False)


def clip(text, limit):
    return {"text": text[:limit], "chars": len(text), "truncated": len(text) > limit}


def find(value, match):
    """The first dict, depth first, for which match() is true."""
    if isinstance(value, dict):
        if match(value):
            return value
        value = list(value.values())
    if isinstance(value, list):
        for item in value:
            found = find(item, match)
            if found is not None:
                return found
    return None


def transcript_lines(path, line):
    real = os.path.realpath(path)
    if not real.startswith(HOME + "/"):
        raise PermissionError("transcript outside the home directory")
    with open(real, "rb") as f:
        for number, raw in enumerate(itertools.islice(f, line - 1, line + RESULT_SCAN_LINES), start=line):
            try:
                yield number, json.loads(raw)
            except ValueError:
                continue


def tool_input(call):
    for field in ("input", "arguments", "action", "command", "parsed_cmd"):
        if call.get(field) not in (None, ""):
            value = call[field]
            if isinstance(value, str) and value.lstrip().startswith("{"):
                try:
                    return json.loads(value)
                except ValueError:
                    pass
            return value
    return {k: v for k, v in call.items() if k not in ("type", "id", "call_id")}


def is_result_for(key):
    return lambda d: ("content" in d or "output" in d) and (
        d.get("tool_use_id") == key or d.get("toolCallId") == key
        or (d.get("call_id") == key and str(d.get("type", "")).endswith("_output")))


def plain(value):
    """Tool results are often lists of text blocks (and image marks); joined they read like the original output."""
    is_text = lambda v: isinstance(v, str) or (isinstance(v, dict) and v.get("type") in ("text", "input_text", "output_text"))
    if isinstance(value, list) and value and all(is_text(v) for v in value):
        return "\n".join(v if isinstance(v, str) else v.get("text", "") for v in value)
    return value


def image_of(block):
    """(media type, base64 data or None, error) for an image block in any of the transcript formats, else None."""
    if not isinstance(block, dict) or block.get("type") not in ("image", "input_image", "output_image"):
        return None
    source = block.get("source") if isinstance(block.get("source"), dict) else {}
    url = block.get("image_url")
    url = url.get("url") if isinstance(url, dict) else url
    if source.get("type") == "base64":
        media, data = source.get("media_type"), source.get("data")
    elif isinstance(url, str) and DATA_URL.match(url):
        media, data = DATA_URL.match(url).groups()
    else:
        media, data = block.get("mimeType") or block.get("mime_type") or block.get("media_type"), block.get("data")
    if not isinstance(data, str) or not IMAGE_TYPES.match(str(media)):
        return None
    if data.startswith("blob:sha256:"):
        digest = data.removeprefix("blob:sha256:")
        if not re.fullmatch(r"[0-9a-f]{64}", digest):
            return media, None, "unrecognised blob reference"
        try:
            with open(os.path.join(OMP_BLOBS, digest), "rb") as f:
                data = base64.b64encode(f.read()).decode()
        except OSError:
            return media, None, "the image blob is no longer available"
    if len(data) > MAX_IMAGE_CHARS:
        return media, None, f"too large to show ({len(data) // 1_000_000} MB of base64)"
    return media, data, None


def extract_images(value, images):
    """Replaces every image with a mark that names its index in images."""
    if isinstance(value, str):
        match = DATA_URL.match(value) if value.startswith("data:image/") else None
        found = (match.group(1), match.group(2), None) if match and IMAGE_TYPES.match(match.group(1)) else None
    else:
        found = image_of(value)
    if found:
        media, data, error = found
        images.append({"media_type": media, "data": data, "error": error})
        return IMAGE_MARK.format(len(images) - 1)
    if isinstance(value, dict):
        return {k: extract_images(v, images) for k, v in value.items()}
    if isinstance(value, list):
        return [extract_images(v, images) for v in value]
    return value


def lookup(db, event_id, with_result):
    found = rows(db, "SELECT id, source, key, kind, name, ts, file, line FROM events WHERE id = ?", (event_id,))
    if not found:
        return None
    event = found[0]
    key = event["key"].removesuffix(":skill")
    try:
        lines = transcript_lines(event["file"], event["line"])
        number, record = next(lines)
    except (OSError, StopIteration):
        return {**event, "missing": "the transcript line is no longer available"}
    if number != event["line"]:
        return {**event, "missing": "the transcript is shorter than expected"}
    out = {**event}
    if event["kind"] in ("tool_call", "tool_exec") or event["key"].endswith(":skill"):
        call = find(record, lambda d: key in (d.get("id"), d.get("call_id"))) or record
        out["input"] = tool_input(call)
        if with_result:
            result = call.get("aggregated_output") or call.get("result") or call.get("results")
            if result is None:
                match = is_result_for(key)
                for _, later in lines:
                    hit = find(later, match)
                    if hit is not None:
                        result = hit.get("content") if "content" in hit else hit.get("output")
                        break
            out["result"] = plain(result)
    elif event["source"] in ("actions", "runs"):
        out["input"] = record.get("attrs") or record
    else:
        message = record.get("message") if isinstance(record.get("message"), dict) else None
        out["input"] = plain(record.get("attachment") or (message or {}).get("content") or record.get("content")
                        or record.get("payload") or record.get("summary") or record.get("systemPrompt") or record)
        nested = out["input"].get("item") if isinstance(out["input"], dict) else None
        if isinstance(nested, dict) and "content" in nested:
            out["input"] = nested["content"]
        if isinstance(out["input"], dict) and isinstance(out["input"].get("content"), list):
            out["input"] = out["input"]["content"]
        out["input"] = plain(out["input"])
    return out


def reveal(event_id):
    db = connect()
    try:
        out = lookup(db, event_id, True)
    finally:
        db.close()
    if out is None:
        return None
    images = []
    for field in ("input", "result"):
        if field in out:
            value = plain(extract_images(out[field], images))
            out[field] = clip(as_text(value), REVEAL_CHARS) if value is not None else None
    shown = {int(n) for field in ("input", "result") if out.get(field) for n in IMAGE_MARKS.findall(out[field]["text"])}
    out["images"] = {n: image for n, image in enumerate(images) if n in shown}
    return out


def peek_text(value):
    if isinstance(value, dict) and value.get("pattern") and (value.get("path") or value.get("glob")):
        value = f"{value['pattern']}  in {value.get('path') or value.get('glob')}"
    if isinstance(value, dict):
        for field in PEEK_FIELDS:
            if value.get(field) not in (None, ""):
                value = value[field]
                break
    text = " ".join(as_text(value).split()) if not isinstance(value, str) else " ".join(value.split())
    return text[:PEEK_CHARS]


def peek(ids):
    db = connect()
    try:
        out = {}
        for event_id in ids[:50]:
            found = lookup(db, event_id, False)
            if found:
                out[event_id] = "" if found.get("missing") else peek_text(found.get("input"))
        return {"peek": out}
    finally:
        db.close()


class Handler(BaseHTTPRequestHandler):
    def send(self, status, body, content_type):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        url = urlparse(self.path)
        query = {k: v[0] for k, v in parse_qs(url.query).items()}
        if url.path == "/":
            with open(PAGE, "rb") as f:
                self.send(200, f.read(), "text/html; charset=utf-8")
            return
        try:
            days = min(365.0, max(0.01, float(query.get("days", "7"))))
            if url.path == "/api/stats":
                result = stats(days, query)
            elif url.path == "/api/sessions":
                result = session_list(days, query.get("q", ""), query, query.get("subagents") == "1")
            elif url.path == "/api/event":
                result = reveal(int(query.get("id", "0")))
                if result is None:
                    return self.send_json(404, {"error": "no such event"})
            elif url.path == "/api/peek":
                result = peek([int(i) for i in query.get("ids", "").split(",") if i.strip().isdigit()])
            elif url.path == "/api/session":
                result = session_detail(query.get("source", ""), query.get("id", ""), query)
                if result is None:
                    return self.send_json(404, {"error": "no such session"})
            else:
                return self.send(404, b"not found", "text/plain")
            self.send_json(200, result)
        except (ValueError, OSError, sqlite3.Error) as error:
            self.send_json(500, {"error": str(error)})

    def send_json(self, status, value):
        self.send(status, json.dumps(value).encode(), "application/json")

    def log_message(self, *args):
        pass


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=6090)
    args = parser.parse_args()
    ThreadingHTTPServer((args.host, args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
