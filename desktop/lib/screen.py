"""Screen reading for agent desktops: AT-SPI accessibility tree and tesseract OCR.

Every coordinate printed is a screen pixel on the slot's display, ready for xdotool.
Called by `agent-desktop` (tree, locate, click, ocr); not meant to be run directly.
"""

import argparse
import os
import re
import subprocess
import sys
import tempfile
from dataclasses import dataclass

import gi

gi.require_version("Atspi", "2.0")
from gi.repository import Atspi  # noqa: E402

S = Atspi.StateType
MAX_NODES_PER_APP = 8000
ACTIONABLE_ROLES = {
    "push button", "toggle button", "link", "entry", "password text", "check box",
    "radio button", "combo box", "menu item", "check menu item", "radio menu item",
    "page tab", "spin button", "slider", "list item", "tree item", "heading", "menu",
}


@dataclass
class Element:
    source: str
    role: str
    name: str
    x: int
    y: int
    w: int
    h: int
    actionable: bool = False
    flags: str = ""
    window: str = ""

    @property
    def center(self):
        return self.x + self.w // 2, self.y + self.h // 2

    def line(self):
        cx, cy = self.center
        name = self.name if len(self.name) <= 80 else self.name[:77] + "..."
        flags = f"  [{self.flags}]" if self.flags else ""
        return f'{self.role} "{name}" @{cx},{cy}  {self.w}x{self.h}{flags}'


def attempt(fn, default=None):
    try:
        return fn()
    except Exception:
        return default


def read_proc(pid, name):
    try:
        with open(f"/proc/{pid}/{name}", "rb") as f:
            return f.read()
    except OSError:
        return b""


def environ_display(pid):
    for kv in read_proc(pid, "environ").split(b"\0"):
        if kv.startswith(b"DISPLAY="):
            return kv[8:].decode(errors="replace").split(".")[0]
    return None


def process_display(pid):
    """DISPLAY of a process. Chromium overwrites its own /proc environ, so fall back to
    the per-slot profile dir or unit of `agent-desktop browser`, then to parent processes."""
    display = environ_display(pid)
    if display:
        return display
    match = (
        re.search(rb"--user-data-dir=\S*/agent-desktop/chromium-(\d+)(?!\d)", read_proc(pid, "cmdline"))
        or re.search(rb"agent-desktop-(\d+)-chromium\.service", read_proc(pid, "cgroup"))
    )
    if match:
        slot = match.group(1).decode()
        return ":99" if slot == "0" else f":10{slot}"
    for _ in range(5):
        stat = read_proc(pid, "stat").decode(errors="replace")
        fields = stat.rsplit(")", 1)[-1].split()
        if len(fields) < 2 or fields[1] in ("0", "1"):
            return None
        pid = int(fields[1])
        display = environ_display(pid)
        if display:
            return display
    return None


def apps_on(display):
    desktop = Atspi.get_desktop(0)
    for i in range(attempt(desktop.get_child_count, 0)):
        app = attempt(lambda: desktop.get_child_at_index(i))
        pid = attempt(app.get_process_id) if app else None
        if pid and process_display(pid) == display:
            yield app


def state_flags(states, role):
    flags = []
    if states.contains(S.FOCUSED):
        flags.append("focused")
    if states.contains(S.CHECKED):
        flags.append("checked")
    if states.contains(S.SELECTED) and role in ("page tab", "list item", "tree item", "menu item"):
        flags.append("selected")
    if states.contains(S.EXPANDABLE):
        flags.append("expanded" if states.contains(S.EXPANDED) else "collapsed")
    if not states.contains(S.SENSITIVE):
        flags.append("disabled")
    return ",".join(flags)


def accessibility_elements(display, width, height):
    elements, seen_boxes = [], set()
    for app in apps_on(display):
        stack = [(app, 0, attempt(app.get_name, "") or "")]
        visited = 0
        while stack and visited < MAX_NODES_PER_APP:
            obj, depth, window = stack.pop()
            visited += 1
            role = attempt(obj.get_role_name, "") or ""
            states = attempt(obj.get_state_set)
            if states is None or (depth > 1 and not states.contains(S.VISIBLE)):
                continue
            name = " ".join((attempt(obj.get_name, "") or "").split())
            if role == "frame" and name:
                window = name
            if name and depth > 0 and states.contains(S.SHOWING):
                ext = attempt(lambda: obj.get_extents(Atspi.CoordType.SCREEN))
                if ext and ext.width >= 2 and ext.height >= 2:
                    cx, cy = ext.x + ext.width // 2, ext.y + ext.height // 2
                    box = (name, ext.x, ext.y, ext.width, ext.height)
                    if 0 <= cx < width and 0 <= cy < height and box not in seen_boxes:
                        seen_boxes.add(box)
                        actionable = (
                            role in ACTIONABLE_ROLES
                            or states.contains(S.FOCUSABLE)
                            or states.contains(S.EDITABLE)
                        )
                        elements.append(Element(
                            "a11y", role, name, ext.x, ext.y, ext.width, ext.height,
                            actionable, state_flags(states, role), window,
                        ))
            for i in reversed(range(attempt(obj.get_child_count, 0))):
                child = attempt(lambda: obj.get_child_at_index(i))
                if child is not None:
                    stack.append((child, depth + 1, window))
    return elements


def screenshot(display, path):
    subprocess.run(
        ["magick", "import", "-silent", "-window", "root", path],
        check=True, env={**os.environ, "DISPLAY": display},
    )


def ocr_lines(display, region=None, scale=2):
    """Returns OCR text lines, each a list of word Elements in screen coordinates."""
    ox, oy, crop = 0, 0, []
    if region:
        x0, y0, x1, y1 = region
        ox, oy, crop = x0, y0, ["-crop", f"{x1 - x0}x{y1 - y0}+{x0}+{y0}", "+repage"]
    with tempfile.TemporaryDirectory() as tmp:
        raw, prepared = f"{tmp}/raw.png", f"{tmp}/prepared.png"
        screenshot(display, raw)
        subprocess.run(
            ["magick", raw, *crop, "-colorspace", "Gray", "-resize", f"{scale * 100}%", prepared],
            check=True,
        )
        tsv = subprocess.run(
            ["tesseract", prepared, "-", "--psm", "11", "tsv"],
            check=True, capture_output=True, text=True,
        ).stdout
    lines = {}
    for row in tsv.splitlines()[1:]:
        cols = row.split("\t")
        if len(cols) < 12 or cols[0] != "5" or not cols[11].strip():
            continue
        if float(cols[10]) < 30:
            continue
        left, top, w, h = (int(v) for v in cols[6:10])
        word = Element(
            "ocr", "text", cols[11].strip(),
            ox + left // scale, oy + top // scale, max(1, w // scale), max(1, h // scale),
        )
        lines.setdefault(tuple(cols[2:5]), []).append(word)
    return list(lines.values())


def union(words):
    x0 = min(w.x for w in words)
    y0 = min(w.y for w in words)
    x1 = max(w.x + w.w for w in words)
    y1 = max(w.y + w.h for w in words)
    return Element("ocr", "text", " ".join(w.name for w in words), x0, y0, x1 - x0, y1 - y0)


def ocr_matches(lines, query, exact):
    hits = []
    for words in lines:
        for i in range(len(words)):
            joined = ""
            for j in range(i, len(words)):
                joined = f"{joined} {words[j].name}".strip().casefold()
                matched = joined == query if exact else query in joined
                if matched:
                    rest = " ".join(w.name for w in words[i + 1:j + 1]).casefold()
                    if exact or query not in rest:
                        hits.append(union(words[i:j + 1]))
                    break
                if len(joined) > len(query) + 40:
                    break
    best = {}
    for hit in hits:
        key = hit.center
        if key not in best or len(hit.name) < len(best[key].name):
            best[key] = hit
    return list(best.values())


def a11y_matches(elements, query, role, exact):
    hits = []
    for el in elements:
        name = el.name.casefold()
        if role and role.casefold() not in el.role:
            continue
        if (exact and name != query) or (not exact and query not in name):
            continue
        hits.append(el)
    return sorted(hits, key=lambda el: (el.name.casefold() != query, not el.actionable, len(el.name)))


def locate(args, width, height):
    query = " ".join(args.text.casefold().split())
    hits = []
    if not args.ocr:
        hits = a11y_matches(accessibility_elements(args.display, width, height), query, args.role, args.exact)
    if not hits and not args.no_ocr and not args.role:
        hits = sorted(
            ocr_matches(ocr_lines(args.display), query, args.exact),
            key=lambda el: (el.name.casefold() != query, len(el.name), el.y, el.x),
        )
    return query, hits


def print_candidates(hits, stream):
    for i, el in enumerate(hits, 1):
        print(f"{i:>2} [{el.source}] {el.line()}", file=stream)


def cmd_locate(args, width, height):
    query, hits = locate(args, width, height)
    if not hits:
        print(f'not found: "{args.text}" (try: agent-desktop shot / zoom and read the screen)', file=sys.stderr)
        return 1
    if not args.pick:
        print_candidates(hits, sys.stdout)
        return 0
    if args.nth:
        if args.nth > len(hits):
            print(f"--nth {args.nth} but only {len(hits)} match(es):", file=sys.stderr)
            print_candidates(hits, sys.stderr)
            return 3
        chosen = hits[args.nth - 1]
    else:
        exact = [el for el in hits if el.name.casefold() == query]
        exact_actionable = [el for el in exact if el.actionable]
        unique = next((pool for pool in (hits, exact, exact_actionable) if len(pool) == 1), [])
        if not unique:
            print(f'ambiguous: "{args.text}" matches {len(hits)} elements; pass --nth K, --role or --exact:', file=sys.stderr)
            print_candidates(hits, sys.stderr)
            return 3
        chosen = unique[0]
    cx, cy = chosen.center
    print(f"{cx} {cy} {chosen.source} {chosen.role}: {chosen.name[:60]}")
    return 0


def cmd_tree(args, width, height):
    elements = accessibility_elements(args.display, width, height)
    if not args.all:
        elements = [el for el in elements if el.actionable]
    if args.grep:
        needle = args.grep.casefold()
        elements = [el for el in elements if needle in el.name.casefold()]
    if not elements:
        print("no accessible elements on screen (app without accessibility support?); "
              "fall back to: agent-desktop ocr / shot", file=sys.stderr)
        return 1
    window = None
    for el in elements:
        if el.window != window:
            window = el.window
            print(f"# {window}")
        print(el.line())
    return 0


def cmd_ocr(args, width, height):
    region = tuple(args.region) if args.region else None
    if region and len(region) != 4:
        print("region needs X0 Y0 X1 Y1", file=sys.stderr)
        return 2
    lines = ocr_lines(args.display, region)
    if not lines:
        print("no text recognised", file=sys.stderr)
        return 1
    for words in sorted(lines, key=lambda ws: (ws[0].y, ws[0].x)):
        line = union(words)
        cx, cy = line.center
        print(f'@{cx},{cy}  {line.x},{line.y} {line.w}x{line.h}  "{line.name}"')
    return 0


def main():
    parser = argparse.ArgumentParser(prog="agent-desktop-screen")
    parser.add_argument("--display", required=True)
    parser.add_argument("--size", required=True, help="WIDTHxHEIGHT")
    sub = parser.add_subparsers(dest="cmd", required=True)

    tree = sub.add_parser("tree")
    tree.add_argument("--all", action="store_true")
    tree.add_argument("--grep")

    loc = sub.add_parser("locate")
    loc.add_argument("text")
    loc.add_argument("--role")
    loc.add_argument("--exact", action="store_true")
    loc.add_argument("--nth", type=int)
    loc.add_argument("--pick", action="store_true")
    loc.add_argument("--ocr", action="store_true", help="skip the accessibility tree")
    loc.add_argument("--no-ocr", action="store_true", help="no OCR fallback")

    ocr = sub.add_parser("ocr")
    ocr.add_argument("region", nargs="*", type=int)

    args = parser.parse_args()
    width, height = (int(v) for v in args.size.split("x"))
    Atspi.set_timeout(2000, 4000)
    handler = {"tree": cmd_tree, "locate": cmd_locate, "ocr": cmd_ocr}[args.cmd]
    sys.exit(handler(args, width, height))


if __name__ == "__main__":
    main()
