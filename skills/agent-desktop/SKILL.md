---
name: agent-desktop
description: Use a virtual Linux desktop on the agentbox for computer use - driving GUI apps or a visible (non-headless) browser by screenshots, clicks and typing - while the owner watches live in a web browser. Use when a task needs a real screen, the owner asks to "watch", "see what you're doing", "use the desktop" or "computer use", or several agents need separate screens.
---

# Agent desktops

Each agent gets its own X display, so several agents can work side by side without
fighting over one screen. The owner watches any of them in a browser over Tailscale or the LAN.
The scripts, units and setup notes live in `desktop/` of the agentbox repository
(installed to `~/.local/bin`, `~/.local/lib/agent-desktop`, `~/.config/systemd/user`).

| Slot | Display | Size | Notes |
|---|---|---|---|
| 0 | `:99` | 1920x1080 | the original `xdesktop.target` |
| 1-9 | `:101`-`:109` | 1280x800 | agent desktops, one per agent, tint2 taskbar at the bottom |
| named (20-99) | `:10NN` | 1280x800 | persistent, purpose-bound desktops from `~/.config/agent-desktop/slots`, never given out by `start` |

## Throwaway slots vs named slots (owner's policy)

- **Slots 1-9 are throwaway.** Their Chromium profiles (`~/.local/state/agent-desktop/chromium-N`)
  survive a stop, but nothing there is worth keeping: any agent may get any slot, and logins,
  cookies or state left there can be overwritten or wiped at any time. Never build on a login in
  1-9 and never ask the owner to log in there for something meant to last.
- **Anything that must stay logged in gets its own named slot**, outside 1-9: one per
  purpose, e.g. a specialised agent that needs its own accounts. A named slot is never handed out by
  `start`, its profile is only used for that purpose, and it has a skill saying who uses it and how.
- **Keep named slots stopped when idle.** The profile on disk is what persists, not the running
  desktop, so stopping one costs nothing but the restart.

Adding a named slot: add a line `name=NN` to `~/.config/agent-desktop/slots` (next free
internal number from 20 up to 99; `#` starts a comment). Slot NN gets display `:10NN`, VNC
`590NN`, DevTools port `9210+NN` (slot 20: 9230) and the profile `chromium-NN`. Every command
then takes the name or the number (`agent-desktop start work`), the reaper covers it and the
browser view shows its name. Write a skill for its purpose saying who uses it and how. If the
owner needs to log in, ask them to do it through the desktop view; to reuse an existing login,
copy the profile while both browsers are stopped.

## Get a desktop

```bash
agent-desktop list                 # running slots, who holds them, browser URLs
agent-desktop start                # claims the first free slot 1-9, prints slot, DISPLAY, URL
agent-desktop start 3              # a specific slot
agent-desktop release 3            # done with it but leave it running for the owner
agent-desktop stop 3               # done and nobody needs it: frees RAM
```

Tell the owner the URL `start` printed. The owner watches at `http://<box>.local:6080/`
(LAN) or `http://<tailscale-ip>:6080/` (plus `https://$AGENT_DESKTOP_PUBLIC_HOST/` when one is
set); each desktop opens in noVNC (`vnc.html`) with the
owner's switches in a floating panel. The VNC password is in `~/.vnc/passwd.txt`; never print it.

## Sharing a desktop with other agents

A slot belongs to the agent that first used it (`start`, or any action on it). The lease is tied
to your agent process and ends when it exits, so a crashed agent never blocks a slot.

- Someone else's slot refuses your actions with **exit 5**. Do not work around it (no raw
  xdotool on their display). Use a free slot, or ask to share:
  `agent-desktop request 3 "need to check the checkout page they have open"`. That waits up to
  90s (`--wait SECS`); it is never granted automatically. Exit 0 = you share it now, 6 = denied
  (use another slot), 7 = no answer yet (rerun to keep waiting, or move on).
- When you hold a slot and someone asks, every desktop command prints the request on stderr.
  Answer at a safe point, not mid-step: `agent-desktop grant 3 PID` (they join you) or
  `agent-desktop deny 3 PID`. Grant when your work there is done or theirs will not collide.
- Sharing: input actions run one at a time per slot, and you get a warning when the other holder
  acted since your last step. Take a fresh `shot`/`tree` after such a warning. Keep the shared
  part short and `release` as soon as you are done.
- `agent-desktop status 3` shows holders, idle times, pending requests and the owner's switches.

## The owner's switches (never touch them)

- **Your control** (off by default): the owner's mouse and keyboard only reach the desktop when
  they switch it on. Do not ask them to, and do not change it (`input` refuses agents).
- **Pause agents**: every action command fails with **exit 4** and the desktop-browser MCP
  freezes. Do not work around it. Run `agent-desktop wait 3` (blocks until resumed), then take
  a fresh screenshot before continuing, since the owner may have changed things.
- **Stop** (after a confirmation): the owner shut the desktop down from the browser, even while
  you held it. Your next command fails with "slot N is not running". Don't restart it on your
  own unless the task still needs a screen; then `agent-desktop start` gives you a fresh one.

## Run apps on it

```bash
agent-desktop browser 3 https://example.com      # the slot's Chromium (new tab if already open)
agent-desktop browser 3 https://example.com --reuse  # navigate the current tab instead
agent-desktop run 3 -- some-gui-app &            # anything else, with DISPLAY=:103
agent-desktop agent 3 claude                     # an agent on the slot (claude, codex, omp)
```

`browser` runs Chromium as the transient unit `agent-desktop-3-chromium` with its own profile
(`~/.local/state/agent-desktop/chromium-3`), maximized, with accessibility on and the DevTools
protocol on `127.0.0.1:9223` (port `922N`, localhost only). Use it instead of launching Chromium
yourself; that is what makes the browser readable by `tree` and drivable by the MCP below.
Slots 1-9 have a 30px taskbar at the bottom (open windows, `desktop :10N` label, clock), so the
usable area is 1280x770.

## Pick the right way to see and act (best first, fall back down)

1. **Browser task, desktop-browser MCP.** `agent-desktop agent N claude|codex` attaches a
   `desktop-browser` MCP (chrome-devtools-mcp) to the slot's visible Chromium. Use
   `take_snapshot` and click/fill by uid: exact, no pixels, and the owner still watches it
   happen. Without that launcher, any MCP client can run `agent-desktop mcp-browser N` (stdio).
2. **Any app with accessibility, `tree` / `locate` / `click TEXT`.** Reads the AT-SPI tree
   (Chromium from `browser`, GTK, Qt) and prints on-screen elements with click points:
   ```bash
   agent-desktop tree 3                     # actionable elements: role "name" @x,y  WxH [flags]
   agent-desktop tree 3 --all --grep price  # also static text, filtered
   agent-desktop click 3 "Sign in" --role button
   agent-desktop click 3 "Standard" --nth 2 # exit 3 + candidate list when ambiguous
   ```
   `click`/`locate` try the tree first and fall back to OCR automatically. When an exact name
   is shared by a link or button and plain text (a heading, a label), the actionable one wins.
   The tree only covers what is on screen; for web pages use the whole-page commands below
   instead of scrolling and searching.
3. **No accessibility (Xlib apps, canvas, games, remote screens), `ocr`.**
   `agent-desktop ocr 3 [X0 Y0 X1 Y1]` prints text lines with click points (tesseract);
   `click 3 TEXT --ocr` skips the tree. OCR misreads small or stylised text, so check it.
4. **Icons, images, layout, anything unlabelled: vision.** `agent-desktop shot 3` then read the
   PNG (slots 1-9: screenshot pixel = screen pixel), `agent-desktop zoom 3 X0 Y0 X1 Y1` for small
   text (it prints how to map zoomed pixels back), then `agent-desktop clickxy 3 X Y`.

### Web pages: read the whole page, not just the screen

These talk to the slot's Chromium over the DevTools port and see the active tab's entire page,
including everything scrolled off screen. Clicks are still real mouse clicks the owner sees.

```bash
agent-desktop page 3                      # title, URL, page height, heading outline
agent-desktop page 3 --text --max 4000    # the page's text
agent-desktop find 3 "Universal"          # matches with kind, target URL, where they are
                                          # (on screen / N screens below) and the sentence around them
agent-desktop find 3 "Email" --kind field # only one kind: link, button, field, clickable, text, any
agent-desktop show 3 "Total due"          # scroll a match (plain text too) into view, then shot/zoom it
agent-desktop click 3 "Universal Pictures" --page --wait
                                          # scroll it into view, click, wait for the next page
agent-desktop click 3 Shrek --page --exact --nth 2 --wait
agent-desktop ready 3                     # wait until the current page has loaded
```

`find` matches in tiers and shows the best ones: links, buttons and form fields (fields by label,
placeholder or aria-label), plus other elements that look clickable (pointer cursor or tabindex,
e.g. cards and icon buttons in app UIs). Plain text is listed only when nothing clickable
matches; otherwise a line like `also: 81 text (--kind text)` says it exists. Same-origin iframes
are searched too. Content inside web components (shadow DOM) is invisible to it: when something
you can see is "not on this page", add `--ax` to search the browser's accessibility tree instead
(slower on big pages; works with `find`, `show` and `click`). Output is capped at 15 targets,
best first; links to the same URL fold into one line (`same target: 12, 15`), and a last line
says how many more exist. Narrow the text or `--kind` before reaching for `--all`.

`click --page` refuses with exit 3 when matches lead to different targets. It lists one line per
target (exact matches only, when there are any) with its number; pass that as `--nth K`.
Matches that all lead to the same URL are not ambiguous. It also refuses (exit 3)
when only plain text matches, since clicking text rarely does anything; use `show`, or
`--kind text` when you really mean to click it. `--kind` and `--ax` imply `--page`. `--wait` also works on
plain `click` and `clickxy`: it returns once a click-triggered navigation has loaded (or right
away when the click did not navigate).

Typing and keys go to the focused element: `agent-desktop type 3 "hello"`,
`agent-desktop key 3 ctrl+l Return`, `agent-desktop scroll 3 down 5 [X Y]`.

After every consequential step (submit, navigate, open dialog) verify with `shot` (or a fresh
`tree`/snapshot). Never chain many blind actions. Exit codes: 0 ok, 1 not found, 3 ambiguous,
4 paused by the owner, 5 held by another agent.

Raw xdotool bypasses sharing and pausing, so only use it on a slot you hold and when no command
covers what you need (`DISPLAY=:103 xdotool ...`); find Chromium windows with
`xdotool search --name " - Chromium$"` (`--onlyvisible` misses windows on Xvfb).
omp has built-in computer use: start it with `agent-desktop agent N omp`, then `/computer on`.

## Rules

- Prefer headless tools (chrome-devtools MCP, Playwright) when nobody needs to watch; desktops cost RAM.
- `stop N` also stops the slot's browser unit.
- A slot nobody holds or watches is stopped automatically after 60 idle minutes (10 when RAM is low) by `agent-desktop-reap.timer`; slots you hold are never reaped, so `release` only when you are really done. Its Chromium profile (logins, cookies) survives; open tabs do not.
- Never enter credentials the owner has not given you for this task. If a login is needed, stop
  and ask the owner to type it through the browser view.
- Stop (or release) desktops you started when the task ends.
