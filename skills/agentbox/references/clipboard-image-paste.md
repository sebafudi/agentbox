# Clipboard image paste over SSH (research, not built)

Goal: paste a screenshot from the Mac (Ghostty -> SSH -> zellij) into claude, codex or omp on agentbox and have it attach as a real image ("[Image #1]"), not text.

Status: researched 2026-10, parked. Nothing installed. Versions below are the ones checked then.

## Why it does not work today

Ghostty Cmd+V sends text only (bracketed paste). An image never crosses the SSH connection. Each agent reads the clipboard of the machine it runs on, and agentbox has no clipboard.

| Agent (version checked) | How it reads an image on Linux | Over SSH now |
|---|---|---|
| claude 2.1.296 | Ctrl+V runs `xclip -selection clipboard -t TARGETS -o`, then `-t image/png -o` (or `wl-paste`) | Fails: no `DISPLAY` |
| codex 0.160.1 (musl) | Ctrl+V uses the `arboard` crate, which talks X11 directly (x11rb) | Fails: needs a real X server. A fake `xclip` script does not help |
| omp 18.7.0 | Native `readImageFromClipboard`, but the paste handler returns early if `SSH_CONNECTION`, `SSH_TTY` or `SSH_CLIENT` is set | Skipped by design. omp expects OSC 5522 instead |

omp also accepts a pasted image path and attaches it. Over SSH it says: "this path is local to your terminal - paste the image directly (clipboard image-paste shortcut) to send its bytes".

## Terminal-native route (OSC 5522)

OSC 5522 is kitty's extended clipboard protocol (MIME types, images). With private mode `?5522h` on, the terminal sends paste events that the app can ask for as `image/png`. omp already supports it.

Blockers when checked:
- Ghostty 1.3.1 (the version on the Mac) does not have it. Support is only in Ghostty dev builds.
- zellij 0.45.1 sits between Ghostty and the app and would also need to pass the sequence through.
- claude does not support it yet (anthropics/claude-code#42712).

Check this first if we come back: a stable Ghostty with OSC 5522 plus zellij passthrough would make omp work with no bridge.

## Preferred design if built: on-paste X clipboard bridge

1. Box: a small dedicated Xvfb, e.g. `Xvfb :50 -screen 0 8x8x24 -nolisten tcp`, as a user unit. It only holds the clipboard and is kept separate from `:99` (agent desktop) and `:10N` (per-agent desktops). `xclip` is already installed.
2. Box: in `~/.bashrc`, `export DISPLAY=${DISPLAY:-:50}` for SSH and zellij shells. zellij panes run bash, so they pick it up.
3. Mac: Hammerspoon. Bind Ctrl+V and Cmd+V only while Ghostty is the frontmost app:
   - Clipboard has an image: write it as PNG, run `ssh agentbox 'DISPLAY=:50 xclip -selection clipboard -t image/png -i'`, then send Ctrl+V to Ghostty. claude and codex read it natively.
   - Clipboard has text: pass the key through unchanged.
   - Use SSH `ControlMaster`/`ControlPersist` so the upload is fast. Fall back from the LAN host alias to the tunnel alias off the LAN.
4. omp: a wrapper that runs omp with `SSH_CONNECTION`, `SSH_TTY` and `SSH_CLIENT` unset. Risk: omp uses the same check for its notification style and probably for copying text via OSC 52. Unsetting them could send omp's copies to the box X clipboard instead of the Mac. Test copying out of omp before keeping this.
5. `xclip -i` stays running in the background as the selection owner until the next copy. No clipboard manager is needed.

An omp launched for computer use runs with `DISPLAY=:99`, so it would not see images pushed to `:50`.

## Alternatives considered

- Auto-push: a Mac watcher pushes every copied image to the box. Simpler, no key interception, but every screenshot lands on the box.
- Path paste: upload the PNG to the box (e.g. `~/.cache/clip/<ts>.png`) and paste its remote path. All three agents turn a pasted image path into an attachment. Simplest and needs no X server, but it needs a Mac hotkey to type the path.
- `xclip` shim on PATH that fetches the image from the Mac through an `ssh -R` socket: covers claude only (codex and omp do not call the `xclip` binary).
