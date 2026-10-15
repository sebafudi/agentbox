"""Whole-page access to a slot's Chromium over the DevTools protocol (127.0.0.1:922N).

The desktop accessibility tree only shows what is on screen; this reads the entire page of the
active tab. `find` matches in tiers: links, buttons and form fields (by label or placeholder),
then other elements that look clickable (pointer cursor, tabindex), then plain text; same-origin
iframes included. `--ax` searches the browser's own accessibility tree instead (slower, sees
shadow DOM). Matches can be scrolled into view (`show`) or clicked with a real X click.
Called by `agent-desktop` (find, show, page, ready, click --page/--wait, browser); not meant to be run directly.
"""

import argparse
import base64
import json
import os
import socket
import struct
import subprocess
import sys
import time
import urllib.parse
import urllib.request

NAVIGATION_GRACE = 0.6
FIND_LIMIT = 15


class CDP:
    """Minimal DevTools websocket client (stdlib only)."""

    def __init__(self, ws_url, timeout=10):
        url = urllib.parse.urlparse(ws_url)
        self.sock = socket.create_connection((url.hostname, url.port), timeout=timeout)
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall((
            f"GET {url.path} HTTP/1.1\r\nHost: {url.hostname}:{url.port}\r\nUpgrade: websocket\r\n"
            f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n"
        ).encode())
        self.buf = b""
        while b"\r\n\r\n" not in self.buf:
            self.buf += self._recv()
        head, self.buf = self.buf.split(b"\r\n\r\n", 1)
        if b" 101 " not in head.split(b"\r\n", 1)[0]:
            raise ConnectionError("DevTools websocket handshake failed")
        self.next_id = 0
        self.events = []

    def _recv(self):
        chunk = self.sock.recv(65536)
        if not chunk:
            raise ConnectionError("DevTools connection closed")
        return chunk

    def _take(self, n):
        if len(self.buf) < n:
            chunks, size = [self.buf], len(self.buf)
            while size < n:
                chunks.append(self._recv())
                size += len(chunks[-1])
            self.buf = b"".join(chunks)
        data, self.buf = self.buf[:n], self.buf[n:]
        return data

    def _send_frame(self, opcode, payload):
        header = bytearray([0x80 | opcode])
        n = len(payload)
        if n < 126:
            header.append(0x80 | n)
        elif n < 65536:
            header.append(0x80 | 126)
            header += struct.pack(">H", n)
        else:
            header.append(0x80 | 127)
            header += struct.pack(">Q", n)
        mask = os.urandom(4)
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        self.sock.sendall(bytes(header) + mask + masked)

    def _message(self):
        parts = []
        while True:
            b0, b1 = self._take(2)
            opcode, n = b0 & 0x0F, b1 & 0x7F
            if n == 126:
                n = struct.unpack(">H", self._take(2))[0]
            elif n == 127:
                n = struct.unpack(">Q", self._take(8))[0]
            mask = self._take(4) if b1 & 0x80 else None
            data = self._take(n)
            if mask:
                data = bytes(b ^ mask[i % 4] for i, b in enumerate(data))
            if opcode == 8:
                raise ConnectionError("DevTools connection closed")
            if opcode == 9:
                self._send_frame(10, data)
                continue
            if opcode == 10:
                continue
            parts.append(data)
            if b0 & 0x80:
                return json.loads(b"".join(parts))

    def call(self, method, **params):
        self.next_id += 1
        self._send_frame(1, json.dumps({"id": self.next_id, "method": method, "params": params}).encode())
        while True:
            message = self._message()
            if message.get("id") == self.next_id:
                if "error" in message:
                    raise RuntimeError(f"{method}: {message['error'].get('message')}")
                return message.get("result", {})
            self.events.append(message)

    def wait_event(self, names, timeout):
        deadline = time.monotonic() + timeout
        for i, event in enumerate(self.events):
            if event.get("method") in names:
                del self.events[: i + 1]
                return event
        self.events.clear()
        while (left := deadline - time.monotonic()) > 0:
            self.sock.settimeout(left)
            try:
                message = self._message()
            except (socket.timeout, TimeoutError):
                break
            if message.get("method") in names:
                self.sock.settimeout(10)
                return message
        self.sock.settimeout(10)
        return None

    def js(self, expression):
        result = self.call("Runtime.evaluate", expression=expression, returnByValue=True, awaitPromise=True)
        if "exceptionDetails" in result:
            raise RuntimeError(result["exceptionDetails"].get("exception", {}).get("description", "script error"))
        return result["result"].get("value")


def targets(port):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/json/list", timeout=5) as response:
        return [t for t in json.load(response) if t.get("type") == "page" and "webSocketDebuggerUrl" in t]


def active_tab(port):
    """The tab on screen: the first page target whose document is visible."""
    pages = targets(port)
    if not pages:
        sys.exit("agent-desktop: no browser tab open (agent-desktop browser N URL)")
    for target in pages:
        cdp = CDP(target["webSocketDebuggerUrl"])
        if cdp.js("document.visibilityState") == "visible":
            return cdp
        cdp.sock.close()
    return CDP(pages[0]["webSocketDebuggerUrl"])


KINDS = ("auto", "link", "button", "field", "clickable", "text", "any")
TIER = {"link": 0, "button": 0, "field": 0, "clickable": 1, "other": 1, "text": 2}

SEARCH_JS = r"""
(() => {
  const query = %(query)s.toLowerCase(), exact = %(exact)s, kind = %(kind)s;
  const ACTION = 'a[href], button, summary, [role=button], [role=link], [role=tab], [role=menuitem], ' +
    '[role=menuitemcheckbox], [role=menuitemradio], [role=option], [role=checkbox], [role=radio], [role=switch], ' +
    '[role=treeitem], [onclick], input[type=button], input[type=submit], input[type=reset], input[type=image], ' +
    'input[type=checkbox], input[type=radio]';
  const FIELD = 'input:not([type=hidden]), textarea, select, [contenteditable=""], [contenteditable=true], ' +
    '[role=textbox], [role=combobox], [role=searchbox], [role=spinbutton], [role=slider]';
  const norm = (s) => (s || '').replace(/\s+/g, ' ').trim();
  const hit = (s) => { const h = norm(s).toLowerCase(); return !!h && (exact ? h === query : h.includes(query)); };
  const visible = (el) => !!el && (el.checkVisibility ? el.checkVisibility({ visibilityProperty: true }) : el.getClientRects().length > 0);
  const role = (el) => el.getAttribute('role') || ({ A: 'link', BUTTON: 'button', SUMMARY: 'button', SELECT: 'combo box',
    TEXTAREA: 'entry' }[el.tagName] || (el.tagName === 'INPUT' ? 'input:' + el.type : el.tagName.toLowerCase()));
  const kindOf = (el) => el.matches('a[href], [role=link]') ? 'link' : el.matches(ACTION) ? 'button' : 'field';
  const byIds = (doc, ids) => (ids || '').split(/\s+/).map((id) => doc.getElementById(id)?.innerText).join(' ');
  const names = (el) => el.matches(ACTION) && !el.matches('input')
    ? [el.innerText, el.getAttribute('aria-label'), el.title, el.querySelector('img[alt]')?.alt]
    : [...(el.labels || [])].map((l) => l.innerText).concat([el.getAttribute('aria-label'),
        byIds(el.ownerDocument, el.getAttribute('aria-labelledby')), el.placeholder, el.title, el.value, el.name]);
  const context = (el, text) => {
    const block = el.closest('p, li, td, dd, dt, figcaption, blockquote, h1, h2, h3, h4');
    const all = norm(block?.innerText);
    const at = all.indexOf(text);
    if (at < 0 || all.length <= text.length) return '';
    const start = Math.max(0, at - 60), end = Math.min(all.length, at + text.length + 60);
    return (start ? '…' : '') + all.slice(start, end) + (end < all.length ? '…' : '');
  };
  const describe = (el) => `${role(el)} "${norm(el.innerText || el.getAttribute('aria-label') || el.title).slice(0, 60)}"`;

  const docs = [];
  const collect = (doc, ox, oy) => {
    docs.push({ doc, ox, oy });
    for (const frame of doc.querySelectorAll('iframe, frame')) {
      let inner = null;
      try { inner = frame.contentDocument; } catch {}
      if (!inner?.body || !visible(frame)) continue;
      const r = frame.getBoundingClientRect();
      collect(inner, ox + r.left + frame.clientLeft, oy + r.top + frame.clientTop);
    }
  };
  collect(document, 0, 0);

  const entries = [], seen = new Set();
  const add = (target, el, k, text, extra, frame) => {
    const rects = target.getClientRects ? [...target.getClientRects()] : [];
    const r = rects[0] || el.getBoundingClientRect();
    const top = r.top + frame.oy, left = r.left + frame.ox;
    entries.push({ target, item: { kind: k, role: role(el), text: norm(text).slice(0, 120),
      href: el.closest('a[href]')?.getAttribute('href') || '', exact: norm(text).toLowerCase() === query,
      onScreen: r.bottom + frame.oy > 0 && top < innerHeight && r.right + frame.ox > 0 && left < innerWidth,
      y: Math.round(top + scrollY), context: '', inside: '', frame: frame.doc !== document, ...extra } });
  };

  for (const frame of docs) {
    for (const el of frame.doc.querySelectorAll(ACTION + ', ' + FIELD)) {
      if (seen.has(el) || !visible(el)) continue;
      const name = names(el).find(hit);
      if (name === undefined) continue;
      seen.add(el);
      const k = kindOf(el), text = k === 'field' ? name : norm(el.innerText) || name;
      const value = k === 'field' && el.value && el.value !== name ? { context: `value: "${norm(el.value).slice(0, 80)}"` } : {};
      add(el, el, k, text, { context: context(el, norm(text)), ...value }, frame);
    }
  }

  const pointerRoot = (el) => {
    let found = null;
    for (let a = el, depth = 0; a && a.tagName !== 'BODY' && depth < 12; a = a.parentElement, depth++) {
      if (a.closest(ACTION + ', ' + FIELD) === a) return null;
      const pointer = getComputedStyle(a).cursor === 'pointer' || (a.hasAttribute('tabindex') && a.tabIndex >= 0);
      if (pointer) found = a; else if (found) break;
    }
    return found;
  };
  let textTotal = 0;
  for (const frame of docs) {
    const labelled = frame.doc.querySelectorAll('[aria-label], [title]');
    for (const el of labelled) {
      const name = [el.getAttribute('aria-label'), el.title].find(hit);
      if (name === undefined || seen.has(el) || el.closest(ACTION + ', ' + FIELD) || !visible(el) || !pointerRoot(el)) continue;
      seen.add(el);
      add(el, el, 'clickable', name, {}, frame);
    }
    const walker = frame.doc.createTreeWalker(frame.doc.body, NodeFilter.SHOW_TEXT);
    while (walker.nextNode()) {
      const node = walker.currentNode, value = node.nodeValue, parent = node.parentElement;
      const at = value.toLowerCase().indexOf(query);
      if (at < 0 || (exact && norm(value).toLowerCase() !== query)) continue;
      if (!parent || ['SCRIPT', 'STYLE', 'NOSCRIPT', 'TEMPLATE'].includes(parent.tagName) || !visible(parent)) continue;
      const action = parent.closest(ACTION + ', ' + FIELD);
      if (action && seen.has(action)) continue;
      const root = action ? null : pointerRoot(parent);
      if (root) {
        if (seen.has(root)) continue;
        seen.add(root);
        add(root, root, 'clickable', norm(root.innerText) || norm(value), { context: context(root, norm(value)) }, frame);
        continue;
      }
      textTotal++;
      if (textTotal > 40) continue;
      const range = frame.doc.createRange();
      range.setStart(node, at);
      range.setEnd(node, Math.min(value.length, at + query.length));
      const snippet = norm(value.slice(Math.max(0, at - 60), at + query.length + 60));
      add(range, parent, 'text', snippet, { exact: norm(value).toLowerCase() === query,
        inside: action ? describe(action) : '' }, frame);
    }
  }

  const counts = {};
  for (const e of entries) counts[e.item.kind] = (counts[e.item.kind] || 0) + 1;
  if (textTotal > 40) counts.text = textTotal;
  let chosen;
  if (kind === 'any') chosen = entries;
  else if (kind === 'auto') {
    const primary = entries.filter((e) => e.item.kind !== 'text');
    chosen = primary.length ? primary : entries;
  } else chosen = entries.filter((e) => e.item.kind === kind);
  chosen.sort((a, b) => a.item.y - b.item.y);
  window.__agentDesktopMatches = chosen.map((e) => e.target);
  return { found: chosen.map((e) => e.item), counts, scrollY: Math.round(scrollY),
    pageHeight: document.documentElement.scrollHeight, viewport: innerHeight };
})()
"""

SCROLL_TO_JS = r"""
(() => {
  const target = window.__agentDesktopMatches[%(index)d];
  const el = target.startContainer ? target.startContainer.parentElement : target;
  el.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
  return new Promise((done) => requestAnimationFrame(() => requestAnimationFrame(() => {
    let ox = 0, oy = 0;
    for (let win = el.ownerDocument.defaultView; win && win !== window && win.frameElement; win = win.parent) {
      const f = win.frameElement, fr = f.getBoundingClientRect();
      ox += fr.left + f.clientLeft;
      oy += fr.top + f.clientTop;
    }
    const height = el.ownerDocument.defaultView.innerHeight;
    const rects = [...target.getClientRects()].filter((r) => r.width > 1 && r.height > 1);
    const r = rects.find((r) => r.top >= 0 && r.bottom <= height) || rects[0] || el.getBoundingClientRect();
    done({ x: ox + r.left + Math.min(r.width / 2, 40), y: oy + r.top + r.height / 2,
      originX: screenX + (outerWidth - innerWidth) / 2, originY: screenY + outerHeight - innerHeight,
      zoom: devicePixelRatio });
  })));
})()
"""

AX_REGISTER_FN = r"""
function () {
  const el = this.nodeType === 1 ? this : this.parentElement;
  if (!el) return null;
  window.__agentDesktopMatches.push(this.nodeType === 1 ? el : (() => {
    const range = document.createRange();
    range.selectNodeContents(this);
    return range;
  })());
  const r = el.getBoundingClientRect();
  return { href: el.closest('a[href]')?.getAttribute('href') || '',
    onScreen: r.bottom > 0 && r.top < innerHeight && r.right > 0 && r.left < innerWidth,
    y: Math.round(r.top + scrollY) };
}
"""

AX_KINDS = {"link": "link", "button": "button", "menuitem": "button", "menuitemcheckbox": "button",
            "menuitemradio": "button", "tab": "button", "checkbox": "button", "radio": "button",
            "switch": "button", "option": "button", "treeitem": "button", "PopUpButton": "button",
            "textbox": "field", "searchbox": "field", "combobox": "field", "spinbutton": "field",
            "slider": "field", "StaticText": "text"}
AX_LIMIT = 60

OUTLINE_JS = r"""
(() => {
  const heads = [...document.querySelectorAll('h1, h2, h3, h4')].filter((h) => h.checkVisibility?.() ?? true);
  return { title: document.title, url: location.href, scrollY: Math.round(scrollY), viewport: innerHeight,
    pageHeight: document.documentElement.scrollHeight, links: document.links.length,
    headings: heads.map((h) => ({ level: +h.tagName[1], text: h.innerText.replace(/\s+/g, ' ').trim().slice(0, 100),
      y: Math.round(h.getBoundingClientRect().top + scrollY) })) };
})()
"""


def search(cdp, args):
    if args.ax:
        return ax_search(cdp, args.text, args.exact, args.kind)
    return cdp.js(SEARCH_JS % {"query": json.dumps(" ".join(args.text.split())), "exact": json.dumps(args.exact),
                               "kind": json.dumps(args.kind)})


def ax_search(cdp, text, exact, kind):
    """Whole-page search through the browser's accessibility tree: slower, but sees shadow DOM."""
    query = " ".join(text.split()).lower()
    cdp.call("DOM.enable")
    cdp.call("Accessibility.enable")
    hits = []
    for node in cdp.call("Accessibility.getFullAXTree")["nodes"]:
        if node.get("ignored") or "backendDOMNodeId" not in node:
            continue
        role = str(node.get("role", {}).get("value", ""))
        name = " ".join(str(node.get("name", {}).get("value", "")).split())
        if role == "InlineTextBox" or not name:
            continue
        if not (name.lower() == query if exact else query in name.lower()):
            continue
        hits.append({"kind": AX_KINDS.get(role, "other"), "role": role, "text": name[:120],
                     "exact": name.lower() == query, "node": node["backendDOMNodeId"]})
    counts = {}
    for h in hits:
        counts[h["kind"]] = counts.get(h["kind"], 0) + 1
    if kind == "auto":
        hits = [h for h in hits if TIER[h["kind"]] == 0] or hits
    elif kind != "any":
        hits = [h for h in hits if h["kind"] == kind]
    cdp.js("window.__agentDesktopMatches = []")
    found = []
    for h in hits[:AX_LIMIT]:
        try:
            obj = cdp.call("DOM.resolveNode", backendNodeId=h.pop("node"))["object"]["objectId"]
            where_ = cdp.call("Runtime.callFunctionOn", functionDeclaration=AX_REGISTER_FN, objectId=obj,
                              returnByValue=True)["result"].get("value")
        except RuntimeError:
            continue
        if where_:
            found.append({**h, **where_, "context": "", "inside": "", "frame": False})
    page = cdp.js("({ scrollY: Math.round(scrollY), pageHeight: document.documentElement.scrollHeight, "
                  "viewport: innerHeight })")
    return {"found": found, "counts": counts, **page}


def where(match, page):
    if match["onScreen"]:
        return "on screen"
    delta = match["y"] - page["scrollY"]
    direction = "below" if delta > 0 else "above"
    screens = abs(delta) // max(1, page["viewport"])
    if not screens:
        return f"just {direction}"
    return f"{screens} screen{'s' if screens > 1 else ''} {direction}"


def describe(i, match, page):
    kind, role = match["kind"], match["role"]
    label = kind if role in (kind, "") or kind == "text" else f"{kind} {role}"
    text = f"…{match['text']}…" if kind == "text" else f"\"{match['text']}\""
    href = f" -> {match['href']}" if match["href"] and kind != "text" else ""
    notes = [where(match, page)] + (["in a frame"] if match.get("frame") else [])
    line = f"{i:>2} {label} {text}{href}  ({', '.join(notes)})"
    if match.get("inside"):
        line += f"\n     inside {match['inside']}"
    if match["context"]:
        line += f"\n     {match['context']}"
    return line


def print_matches(page, stream, only=None, limit=None):
    """Prints matches with their --nth numbers and returns how many were left out.

    With a limit, links to a target already listed fold into that line, and the best kinds
    (exact, clickable) come first.
    """
    found = page["found"]
    leaders, repeats, first_by_target = [], {}, {}
    for i, match in enumerate(found):
        if only is not None and i not in only:
            continue
        target = match["href"] if limit and match["kind"] != "text" else None
        if target in first_by_target:
            repeats[first_by_target[target]].append(i + 1)
            continue
        if target:
            first_by_target[target] = i
        repeats[i] = []
        leaders.append(i)
    if limit:
        leaders.sort(key=lambda i: (TIER[found[i]["kind"]], not found[i]["exact"]))
    for i in leaders[:limit]:
        line = describe(i + 1, found[i], page)
        if repeats[i]:
            numbers = ", ".join(map(str, repeats[i][:6])) + (", ..." if len(repeats[i]) > 6 else "")
            line += f"\n     same target: {numbers}"
        print(line, file=stream)
    return len(leaders) - len(leaders[:limit])


def hidden_note(page, kind):
    shown = {m["kind"] for m in page["found"]}
    rest = {k: n for k, n in page["counts"].items() if k not in shown}
    if kind not in ("auto", "any") or not rest:
        return ""
    return "also: " + ", ".join(f"{n} {k} (--kind {k})" for k, n in sorted(rest.items()))


def cmd_find(cdp, args):
    page = search(cdp, args)
    if not page["found"]:
        print(f'"{args.text}" is not on this page' + ("" if args.ax else " (shadow DOM? try --ax)"), file=sys.stderr)
        return 1
    left_out = print_matches(page, sys.stdout, limit=None if args.all else FIND_LIMIT)
    if left_out:
        print(f"({left_out} more targets not shown; narrow the text, use --exact or --kind, or pass --all)")
    if note := hidden_note(page, args.kind):
        print(note)
    return 0


def pick(page, query, nth, allow_text):
    found = page["found"]
    if nth:
        if nth > len(found):
            return None, f"--nth {nth} but only {len(found)} match(es)"
        return nth - 1, None
    best = min(TIER[m["kind"]] for m in found)
    if best == 2 and not allow_text:
        return None, (f'only plain text matches "{query}", nothing clickable; look at it with show, '
                      "or pass --kind text to click the text itself")
    pool = [i for i, m in enumerate(found) if TIER[m["kind"]] == best]
    pools = [pool]
    exact = [i for i in pool if found[i]["exact"]]
    if exact:
        pools.append(exact)
    for candidates in reversed(pools):
        destinations = {found[i]["href"] or f"#{i}" for i in candidates}
        if len(destinations) == 1:
            on_screen = [i for i in candidates if found[i]["onScreen"]]
            return (on_screen or candidates)[0], None
    shortlist = {}
    for i in pools[-1]:
        shortlist.setdefault(found[i]["href"] or f"#{i}", i)
    return None, (f'ambiguous: "{query}" matches {len(found)} elements ({len(shortlist)} different targets); '
                  "pass --nth K", list(shortlist.values()))


def scroll_to(cdp, args, allow_text):
    page = search(cdp, args)
    if not page["found"]:
        print(f'not found on the page: "{args.text}"' + ("" if args.ax else " (shadow DOM? try --ax)"), file=sys.stderr)
        return None, None, 1
    index, error = pick(page, args.text, args.nth, allow_text)
    if error:
        message, shortlist = error if isinstance(error, tuple) else (error, None)
        print(message + ":", file=sys.stderr)
        print_matches(page, sys.stderr, shortlist)
        if shortlist and len(shortlist) < len(page["found"]):
            print(f'({len(page["found"]) - len(shortlist)} more partial or duplicate matches; '
                  f'`find` lists them all)', file=sys.stderr)
        return None, None, 3
    point = cdp.js(SCROLL_TO_JS % {"index": index})
    zoom = point["zoom"] or 1
    xy = (round(point["originX"] + point["x"] * zoom), round(point["originY"] + point["y"] * zoom))
    return page["found"][index], xy, 0


def xclick(display, x, y, mode):
    button, repeat = ("3", "1") if mode == "--right" else ("1", "2" if mode == "--double" else "1")
    subprocess.run(["xdotool", "mousemove", str(x), str(y), "click", "--repeat", repeat, button],
                   check=True, env={**os.environ, "DISPLAY": display})


def click_and_wait(cdp, display, x, y, mode, wait, timeout):
    if wait:
        cdp.call("Page.enable")
        cdp.events.clear()
    xclick(display, x, y, mode)
    if not wait:
        return ""
    started = cdp.wait_event({"Page.frameStartedLoading", "Page.frameRequestedNavigation"}, NAVIGATION_GRACE)
    if not started:
        return "  (no page load)"
    loaded = cdp.wait_event({"Page.loadEventFired", "Page.frameStoppedLoading"}, timeout)
    try:
        title = cdp.js("document.title")
    except (RuntimeError, ConnectionError):
        title = ""
    return f"  loaded: {title}" if loaded else f"  (still loading after {timeout:g}s)"


def cmd_click(cdp, args):
    match, xy, code = scroll_to(cdp, args, args.kind in ("text", "any"))
    if code:
        return code
    note = click_and_wait(cdp, args.display, *xy, args.mode, args.wait, args.timeout)
    print(f"clicked {xy[0]},{xy[1]} via page {match['kind']}: {match['text'][:60]}{note}")
    return 0


def cmd_show(cdp, args):
    match, xy, code = scroll_to(cdp, args, True)
    if code:
        return code
    print(f"scrolled to {match['kind']} \"{match['text'][:60]}\", now at {xy[0]},{xy[1]} on screen")
    return 0


def cmd_clickxy(cdp, args):
    note = click_and_wait(cdp, args.display, args.x, args.y, args.mode, True, args.timeout)
    print(f"clicked {args.x},{args.y}{note}")
    return 0


def wait_ready(cdp, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if cdp.js("document.readyState") == "complete":
                return True
        except RuntimeError:
            pass
        time.sleep(0.1)
    return False


def cmd_ready(cdp, args):
    if not wait_ready(cdp, args.timeout):
        print(f"page still loading after {args.timeout:g}s", file=sys.stderr)
        return 1
    print(cdp.js("document.title + '  ' + location.href"))
    return 0


def cmd_open(cdp, args):
    cdp.call("Page.enable")
    cdp.events.clear()
    cdp.call("Page.navigate", url=args.url)
    cdp.call("Page.bringToFront")
    cdp.wait_event({"Page.loadEventFired"}, args.timeout)
    print(f"opened in the current tab: {cdp.js('document.title')}")
    return 0


def cmd_page(cdp, args):
    if args.text:
        text = cdp.js("document.body.innerText")
        print(text if not args.max else text[: args.max] + ("\n[… truncated]" if len(text) > args.max else ""))
        return 0
    page = cdp.js(OUTLINE_JS)
    screens = page["pageHeight"] / max(1, page["viewport"])
    print(f"{page['title']}\n{page['url']}")
    print(f"page is {screens:.1f} screens tall, {page['links']} links; showing y {page['scrollY']}-{page['scrollY'] + page['viewport']}")
    for h in page["headings"]:
        here = " <- on screen" if page["scrollY"] <= h["y"] < page["scrollY"] + page["viewport"] else ""
        print(f"{'  ' * (h['level'] - 1)}{h['text']}{here}")
    return 0


def search_options(parser, nth=True):
    parser.add_argument("text")
    parser.add_argument("--exact", action="store_true")
    parser.add_argument("--kind", choices=KINDS, default="auto")
    parser.add_argument("--ax", action="store_true")
    if nth:
        parser.add_argument("--nth", type=int)


def main():
    parser = argparse.ArgumentParser(prog="agent-desktop-page")
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--display", required=True)
    parser.add_argument("--timeout", type=float, default=15)
    sub = parser.add_subparsers(dest="cmd", required=True)

    find = sub.add_parser("find")
    search_options(find, nth=False)
    find.add_argument("--all", action="store_true")

    click = sub.add_parser("click")
    search_options(click)
    click.add_argument("--wait", action="store_true")
    click.add_argument("--mode", default="")

    search_options(sub.add_parser("show"))

    clickxy = sub.add_parser("clickxy")
    clickxy.add_argument("x", type=int)
    clickxy.add_argument("y", type=int)
    clickxy.add_argument("--mode", default="")

    sub.add_parser("ready")

    open_ = sub.add_parser("open")
    open_.add_argument("url")

    page = sub.add_parser("page")
    page.add_argument("--text", action="store_true")
    page.add_argument("--max", type=int, default=0)

    args = parser.parse_args()
    try:
        cdp = active_tab(args.port)
    except OSError:
        sys.exit(f"agent-desktop: no browser with DevTools on port {args.port} (agent-desktop browser N)")
    handler = {"find": cmd_find, "click": cmd_click, "show": cmd_show, "clickxy": cmd_clickxy,
               "ready": cmd_ready, "open": cmd_open, "page": cmd_page}[args.cmd]
    sys.exit(handler(cdp, args))


if __name__ == "__main__":
    main()
