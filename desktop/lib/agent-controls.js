// Owner switches for noVNC's vnc.html: "Your control" and "Pause agents", the agents holding
// the slot, and their share requests. Talks to agent-desktop-control.service (:6081).
//
// Three modes: a floating panel (default) that can be dragged anywhere and snaps to an edge
// when dropped near one, the same panel collapsed into a small indicator, or a bar docked
// above the remote screen (the screen shrinks to fit below it). Snapped to the left or right
// edge, the panel becomes a narrow icon rail and the indicator a slim vertical tab; holders,
// messages and share requests open in a flyout beside the rail (the agents badge toggles it,
// and a new request opens it).
//
// "Stop" shuts the whole desktop down (browser included) after a confirmation dialog that names
// the agents still holding it.
//
// The VNC password is remembered in this browser's localStorage and also answers noVNC's own
// password prompt, so it is typed once per browser.
// noVNC's UI object, loaded in start(). WebKit (iOS Safari) can run this module while vnc.html's
// own module (which has a top-level await) is still initialising app/ui.js, so a static import
// throws "Cannot access 'UI' before initialization" (noVNC shows "Script error."). Wait for it.
let UI;

async function loadUI() {
  const ui = await import('./app/ui.js');
  for (let i = 0; i < 200; i++) {
    try { return ui.default; } catch { await new Promise(resolve => setTimeout(resolve, 50)); }
  }
  return ui.default;
}

const PASSWORD_KEY = 'agentDesktopPassword';
const LAYOUT_KEY = 'agentSwitchesLayout';
const SNAP_DISTANCE = 24;
const DRAG_THRESHOLD = 4;
const params = new URLSearchParams(location.search);
const slot = (params.get('path') ?? '').match(/token=d(\d+)\b/)?.[1] ?? params.get('slot');
// Direct LAN/Tailscale visits serve noVNC on :6080 and the API on :6081. Behind a reverse proxy
// or tunnel (AGENT_DESKTOP_PUBLIC_HOST) both share one hostname and the proxy routes /api/ to :6081.
const apiOrigin = location.port === '6080' ? `${location.protocol}//${location.hostname}:6081` : location.origin;
const api = `${apiOrigin}/api/slots/${slot}`;
// Named slots (~/.config/agent-desktop/slots) show their name; the status reports it as "label".
let slotLabel = slot;

const style = document.createElement('style');
style.textContent = `
  body.agent_docked #noVNC_container { position: fixed; inset: var(--agent-bar-height, 0px) 0 0 0;
    width: auto; height: auto; }
  #agent_switches { position: fixed; z-index: 200; display: flex; align-items: center; gap: 6px;
    background: #1e1e2ef2; color: #cdd6f4; font: 13px/1.3 system-ui, sans-serif;
    border: 1px solid #45475a; border-radius: 10px; padding: 5px; box-shadow: 0 3px 12px #000a; }
  #agent_switches.docked { inset: 0 0 auto 0; border-radius: 0; border-width: 0 0 1px; box-shadow: none; padding: 4px 8px; }
  #agent_switches.floating { max-width: min(560px, calc(100vw - 8px)); }
  #agent_switches.collapsed { padding: 4px 10px; border-radius: 999px; cursor: grab; touch-action: none; }
  #agent_switches .content { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; flex: 1; min-width: 0; }
  #agent_switches.collapsed .content, #agent_switches.collapsed .tools, #agent_switches.collapsed .grip { display: none; }
  #agent_switches .indicator { display: none; align-items: center; gap: 6px; white-space: nowrap; user-select: none; }
  #agent_switches.collapsed .indicator { display: flex; }
  #agent_switches .grip { cursor: grab; color: #7f849c; padding: 0 4px; user-select: none; touch-action: none; font-size: 16px; }
  #agent_switches.docked .grip { display: none; }
  #agent_switches.dragging, #agent_switches.dragging .grip { cursor: grabbing; }
  #agent_switches .tools { display: flex; gap: 2px; }
  #agent_switches .tools button { background: transparent; border-color: transparent; color: #a6adc8; padding: 3px 7px; }
  #agent_switches .tools button:hover { border-color: #45475a; }
  #agent_switches button, #agent_switches input { font: inherit; font-weight: 600; border-radius: 6px;
    border: 1px solid #45475a; padding: 5px 10px; background: #313244; color: #cdd6f4; cursor: pointer;
    min-width: 0; line-height: 1.3; background-image: none; }
  #agent_switches input { font-weight: 400; cursor: text; width: 140px; }
  #agent_switches form { display: flex; flex-wrap: wrap; gap: 6px; }
  #agent_switches button:disabled { opacity: .5; cursor: default; }
  #agent_switches .on.control { background: #a6e3a1; border-color: #a6e3a1; color: #11111b; }
  #agent_switches .pause { border-color: #f38ba8; color: #f38ba8; background: transparent; }
  #agent_switches .on.pause { background: #f38ba8; color: #11111b; }
  #agent_switches .info { color: #a6adc8; }
  #agent_switches .message { color: #f9e2af; }
  #agent_switches .message:empty { display: none; }
  #agent_switches .requests { display: flex; flex-direction: column; gap: 4px; flex-basis: 100%; }
  #agent_switches .requests:empty { display: none; }
  #agent_switches .request { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; padding: 3px 6px;
    border: 1px solid #f9e2af; border-radius: 6px; }
  #agent_switches .request div { display: flex; gap: 6px; }
  #agent_switches .request button { padding: 2px 8px; }
  #agent_switches button { display: inline-flex; align-items: center; justify-content: center; gap: 5px; }
  #agent_switches svg { width: 16px; height: 16px; flex: none; fill: none; stroke: currentColor; stroke-width: 2;
    stroke-linecap: round; stroke-linejoin: round; }
  #agent_switches .badge, #agent_switches .lock { display: none; }
  #agent_switches.vertical { flex-direction: column; align-items: stretch; gap: 6px; width: 56px; padding: 6px 5px; }
  #agent_switches.vertical .grip { text-align: center; padding: 0; line-height: 1; }
  #agent_switches.vertical .content, #agent_switches.vertical .tools { flex-direction: column; align-items: stretch;
    flex-wrap: nowrap; gap: 6px; }
  #agent_switches.vertical .tools { border-top: 1px solid #45475a; padding-top: 6px; gap: 2px; }
  #agent_switches.vertical .tools button { padding: 5px 0; }
  #agent_switches.vertical .content > button { flex-direction: column; gap: 2px; padding: 6px 0 4px;
    font-size: 10px; font-weight: 600; letter-spacing: .02em; }
  #agent_switches.vertical .content > button svg { width: 20px; height: 20px; }
  #agent_switches.vertical .badge { display: inline-flex; }
  #agent_switches .badge b { font-size: 17px; line-height: 20px; }
  #agent_switches .badge.asking { background: #f9e2af; border-color: #f9e2af; color: #11111b; }
  #agent_switches .badge.open { box-shadow: inset 0 0 0 2px #89b4fa; }
  #agent_switches.vertical .lock { display: flex; justify-content: center; padding: 6px 0; color: #f9e2af; }
  #agent_switches .armed { background: #f38ba8; border-color: #f38ba8; color: #11111b; }
  #agent_switches .flyout { position: absolute; top: 0; left: calc(100% + 6px); width: 260px;
    display: flex; flex-direction: column; gap: 6px; padding: 8px; background: #1e1e2ef2;
    border: 1px solid #45475a; border-radius: 10px; box-shadow: 0 3px 12px #000a; }
  #agent_switches .flyout.to-left { left: auto; right: calc(100% + 6px); }
  #agent_switches .flyout.from-bottom { top: auto; bottom: 0; }
  #agent_switches .flyout[hidden] { display: none; }
  #agent_switches .flyout .info { white-space: pre-line; }
  #agent_switches .flyout input { width: auto; flex: 1; }
  #agent_switches .flyout .request { flex-direction: column; align-items: stretch; }
  #agent_switches .flyout .request button { flex: 1; }
  #agent_switches .dot { width: 9px; height: 9px; border-radius: 50%; background: #7f849c; flex: none; }
  #agent_switches .dot.control { background: #a6e3a1; }
  #agent_switches .dot.paused { background: #f38ba8; }
  #agent_switches .indicator b { display: none; font-size: 15px; }
  #agent_switches.collapsed.side { padding: 8px 4px; width: 30px; }
  #agent_switches.side .indicator { flex-direction: column; gap: 6px; flex: 1; }
  #agent_switches.side .indicator b { display: block; }
  #agent_switches.side .indicator span:last-child { writing-mode: vertical-rl; }
  #agent_switches .stop { border-color: #f38ba8; color: #f38ba8; background: transparent; }
  #agent_stop_dialog { position: fixed; inset: 0; z-index: 300; display: flex; align-items: center;
    justify-content: center; background: #11111bb3; font: 14px/1.4 system-ui, sans-serif; }
  #agent_stop_dialog[hidden] { display: none; }
  #agent_stop_dialog .box { width: min(420px, calc(100vw - 32px)); background: #1e1e2e; color: #cdd6f4;
    border: 1px solid #f38ba8; border-radius: 10px; padding: 16px; box-shadow: 0 6px 24px #000c; }
  #agent_stop_dialog h2 { margin: 0 0 8px; font-size: 16px; }
  #agent_stop_dialog p { margin: 0 0 8px; color: #a6adc8; white-space: pre-line; }
  #agent_stop_dialog p.warn { color: #f9e2af; }
  #agent_stop_dialog .buttons { display: flex; justify-content: flex-end; gap: 8px; margin-top: 14px; }
  #agent_stop_dialog button { font: inherit; font-weight: 600; border-radius: 6px; padding: 6px 14px;
    border: 1px solid #45475a; background: #313244; color: #cdd6f4; cursor: pointer; background-image: none; }
  #agent_stop_dialog button.danger { background: #f38ba8; border-color: #f38ba8; color: #11111b; }
  #agent_switches.asking.collapsed { background: #f9e2af; color: #11111b; border-color: #f9e2af; }
`;
document.head.append(style);

const ICONS = {
  pointer: '<path d="M5 3l14 7-6 2-2 7z"/>',
  eye: '<path d="M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>',
  pause: '<path d="M8 5v14M16 5v14"/>',
  play: '<path d="M7 5l12 7-12 7z"/>',
  free: '<path d="M18 6L6 18M6 6l12 12"/>',
  collapse: '<path d="M5 12h14"/>',
  dock: '<path d="M4 4h16M12 20V9M7 14l5-5 5 5"/>',
  float: '<rect x="4" y="8" width="12" height="12" rx="2"/><path d="M8 4h10a2 2 0 0 1 2 2v10"/>',
  power: '<path d="M12 3v9"/><path d="M6.3 6.3a8 8 0 1 0 11.4 0"/>',
  lock: '<rect x="5" y="11" width="14" height="10" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/>',
};

const element = (tag, props = {}) => Object.assign(document.createElement(tag), props);
const icon = (name) => {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.innerHTML = ICONS[name];
  return svg;
};
const setButton = (button, iconName, label = '') =>
  button.replaceChildren(icon(iconName), ...(label ? [element('span', { textContent: label })] : []));
const panel = element('div', { id: 'agent_switches' });
const grip = element('span', { className: 'grip', textContent: '⠿', title: 'Drag to move; drop near an edge to snap to it' });
const content = element('div', { className: 'content' });
const tools = element('div', { className: 'tools' });
const collapseButton = element('button', { title: 'Collapse to a small indicator (Alt+H)' });
const dockButton = element('button');
const indicator = element('div', { className: 'indicator', title: 'Click to show the switches (Alt+H); drag to move' });
const indicatorDot = element('span', { className: 'dot' });
const indicatorSlot = element('b', { textContent: slotLabel });
const indicatorText = element('span');
indicator.append(indicatorDot, indicatorSlot, indicatorText);
tools.append(collapseButton, dockButton);
const flyout = element('div', { className: 'flyout', hidden: true });
panel.append(grip, indicator, content, tools, flyout);

const control = element('button', { className: 'control', title: 'Let your mouse and keyboard through (Alt+C)', disabled: true });
const pause = element('button', { className: 'pause', title: 'Stop all agent actions on this desktop (Alt+P)', disabled: true });
const holders = element('span', { className: 'info' });
const message = element('span', { className: 'message' });
const release = element('button', { title: 'Clear all agent holders of this slot', disabled: true });
const stop = element('button', { className: 'stop', title: 'Shut this desktop down (asks first)', disabled: true });
const stopDialog = element('div', { id: 'agent_stop_dialog', hidden: true });
const stopBox = element('div', { className: 'box', role: 'alertdialog' });
const stopTitle = element('h2');
const stopDetails = element('p');
const stopHolders = element('p', { className: 'warn' });
const stopCancel = element('button', { textContent: 'Cancel' });
const stopConfirm = element('button', { className: 'danger', textContent: 'Stop desktop' });
const stopButtons = element('div', { className: 'buttons' });
stopButtons.append(stopCancel, stopConfirm);
stopBox.append(stopTitle, stopDetails, stopHolders, stopButtons);
stopDialog.append(stopBox);
const badge = element('button', { className: 'badge', title: 'Agents and share requests' });
const lockMark = element('span', { className: 'lock', title: 'Locked: enter the VNC password' });
lockMark.append(icon('lock'));
const requests = element('div', { className: 'requests' });
const unlock = element('form');
const passwordInput = element('input', { type: 'password', placeholder: 'VNC password', autocomplete: 'current-password' });
unlock.append(
  element('input', { type: 'text', autocomplete: 'username', value: 'vnc', hidden: true }),
  passwordInput,
  element('button', { type: 'submit', textContent: 'Unlock switches' }),
);

let password = '';
let state = null;
let flashTimer;
let autoFilledPrompt = false;
let flyoutOpen = false;
let seenRequests = new Set();
let arrangement = '';
const layout = { mode: 'floating', collapsed: false, x: null, y: 0, edges: ['top'] };

function readStorage(key) {
  try { return localStorage.getItem(key); } catch { return null; }
}

function writeStorage(key, value) {
  try { value === null ? localStorage.removeItem(key) : localStorage.setItem(key, value); } catch {}
}

function rememberPassword(value) {
  password = value;
  writeStorage(PASSWORD_KEY, value || null);
}

function flash(text) {
  message.textContent = text;
  clearTimeout(flashTimer);
  if (text) flashTimer = setTimeout(() => { message.textContent = ''; updateFlyout(); }, 8000);
  updateFlyout();
}

function arrange() {
  const vertical = panel.classList.contains('vertical');
  const locked = !password;
  const key = `${vertical}:${locked}`;
  if (key === arrangement) return;
  arrangement = key;
  if (locked) {
    content.replaceChildren(...(vertical ? [lockMark] : [unlock, message]));
    flyout.replaceChildren(...(vertical ? [unlock, message] : []));
  } else {
    content.replaceChildren(control, pause, ...(vertical ? [badge] : [holders]), release, stop,
      ...(vertical ? [] : [message, requests]));
    flyout.replaceChildren(...(vertical ? [holders, message, requests] : []));
  }
}

function updateFlyout() {
  const vertical = panel.classList.contains('vertical');
  const show = vertical && (!password || flyoutOpen || Boolean(message.textContent));
  flyout.hidden = !show;
  flyout.classList.toggle('to-left', layout.edges.includes('right'));
  flyout.classList.toggle('from-bottom', layout.edges.includes('bottom'));
  badge.classList.toggle('open', show);
}

function showUnlock(error = '') {
  rememberPassword('');
  arrange();
  flash(error);
  renderIndicator();
}

function showSwitches() {
  arrange();
}

async function call(path = '', body) {
  const response = await fetch(api + path, {
    method: body ? 'POST' : 'GET',
    headers: { Authorization: `Bearer ${password}`, 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (response.status === 401) {
    showUnlock('wrong password');
    throw new Error('unauthorized');
  }
  const data = await response.json();
  if (data.message && !data.ok) flash(data.message);
  return data.state ?? data;
}

function ago(seconds) {
  const s = Math.max(0, Math.round(Date.now() / 1000 - seconds));
  return s < 120 ? `${s}s` : `${Math.round(s / 60)}m`;
}

function syncViewOnly(viewOnly) {
  const checkbox = document.getElementById('noVNC_setting_view_only');
  if (checkbox && checkbox.checked !== viewOnly) {
    checkbox.checked = viewOnly;
    UI.saveSetting('view_only');
  }
  if (UI.rfb && UI.rfb.viewOnly !== viewOnly) UI.updateViewOnly();
}

function requestRow(request) {
  const row = element('div', { className: 'request' });
  const answers = element('div');
  answers.append(
    element('button', { textContent: 'Let it share', onclick: () => act('/grant', { pid: request.pid }) }),
    element('button', { textContent: 'Deny', onclick: () => act('/deny', { pid: request.pid }) }),
  );
  row.append(
    element('span', { textContent: `${request.name} ${request.pid} wants to share (${request.age} ago): ${request.reason}` }),
    answers,
  );
  return row;
}

function renderSwitches() {
  const known = Boolean(state && !state.error);
  control.disabled = pause.disabled = !known;
  const compact = panel.classList.contains('vertical');
  const controlOn = known && state.user_input;
  const paused = known && state.agents_blocked;
  control.classList.toggle('on', controlOn);
  setButton(control, controlOn ? 'pointer' : 'eye', compact ? (controlOn ? 'yours' : 'view')
    : controlOn ? 'You have control' : 'Your control: off');
  pause.classList.toggle('on', paused);
  setButton(pause, paused ? 'play' : 'pause', compact ? (paused ? 'resume' : 'pause')
    : paused ? 'Agents paused: resume' : 'Pause agents');
  const armed = Boolean(release.dataset.armed);
  release.classList.toggle('armed', armed);
  setButton(release, 'free', compact ? (armed ? 'sure?' : 'free') : armed ? 'Click again to free' : 'Free slot');
  release.disabled = !known || !state.holders.length;
  setButton(stop, 'power', compact ? 'stop' : 'Stop');
  stop.disabled = !known || !state.running;
  renderBadge(known);
  if (!state) holders.textContent = 'connecting…';
  else if (state.error) holders.textContent = state.error;
  else if (!state.running) holders.textContent = 'desktop not running';
  else holders.textContent = state.holders.length
    ? state.holders.map((h) => `${h.name} ${h.pid} · ${ago(h.active)} ago`).join(compact ? '\n' : ' | ')
    : 'no agent';
  requests.replaceChildren(...(known ? state.requests.map(requestRow) : []));
  renderIndicator();
  updateFlyout();
}

function renderBadge(known) {
  const asking = known ? state.requests.length : 0;
  const fresh = known && state.requests.some((r) => !seenRequests.has(r.pid));
  if (fresh) flyoutOpen = true;
  seenRequests = new Set(known ? state.requests.map((r) => r.pid) : []);
  badge.classList.toggle('asking', asking > 0);
  badge.replaceChildren(
    element('b', { textContent: !known ? '–' : asking || state.holders.length }),
    element('span', { textContent: asking ? 'asks' : 'agents' }),
  );
  badge.title = !known ? 'No status yet'
    : `${state.holders.length} agent(s) holding this slot${asking ? `, ${asking} asking to share` : ''}. Click for details.`;
}

function renderIndicator() {
  indicatorSlot.textContent = slotLabel;
  const asking = state?.requests?.length ?? 0;
  indicatorDot.classList.toggle('control', Boolean(state?.user_input));
  indicatorDot.classList.toggle('paused', Boolean(state?.agents_blocked));
  panel.classList.toggle('asking', asking > 0);
  const mode = !password ? 'locked'
    : !state || state.error ? 'no status'
    : state.agents_blocked ? 'agents paused'
    : state.user_input ? 'you have control' : 'view only';
  const requestsText = asking ? ` · ${asking} request${asking > 1 ? 's' : ''}` : '';
  const side = panel.classList.contains('side');
  indicatorText.textContent = side ? `${mode}${requestsText}` : `Desktop ${slotLabel}: ${mode}${requestsText}`;
}

function render(next) {
  state = next;
  if (state?.label) slotLabel = state.label;
  showSwitches();
  renderSwitches();
  if (state && !state.error) syncViewOnly(!state.user_input);
}

async function act(path, body) {
  try { render(await call(path, body)); } catch (error) { if (error.message !== 'unauthorized') flash(String(error)); }
}

async function refresh() {
  if (!password || document.hidden) return;
  try {
    render(await call());
  } catch (error) {
    if (error.message === 'unauthorized') return;
    state = null;
    renderSwitches();
    holders.textContent = `switch API unreachable (${apiOrigin})`;
  }
}

function toggleControl() { if (password && state && !state.error) act('/input', { on: !state.user_input }); }
function togglePause() { if (password && state && !state.error) act('/block', { on: !state.agents_blocked }); }

function confirmRelease() {
  if (release.dataset.armed) {
    delete release.dataset.armed;
    renderSwitches();
    act('/release', {});
    return;
  }
  release.dataset.armed = '1';
  renderSwitches();
  setTimeout(() => { delete release.dataset.armed; renderSwitches(); }, 3000);
}

function openStopDialog() {
  if (!password || !state || state.error || !state.running) return;
  stopTitle.textContent = `Stop desktop ${slotLabel}?`;
  stopDetails.textContent = 'This closes its browser and every window on it. '
    + 'Agents can start it again later, but tabs and unsaved work are lost.';
  stopHolders.textContent = state.holders.length
    ? `Still in use by:\n${state.holders.map((h) => `${h.name} ${h.pid} · active ${ago(h.active)} ago`).join('\n')}\n`
      + 'Its next desktop command will fail until it starts a desktop again.'
    : '';
  stopDialog.hidden = false;
  stopCancel.focus();
}

function closeStopDialog() {
  stopDialog.hidden = true;
}

async function confirmStop() {
  closeStopDialog();
  stop.disabled = true;
  flash('stopping desktop…');
  try {
    const next = await call('/stop', {});
    render(next);
    if (!next.running) flash(`desktop ${slotLabel} stopped`);
  } catch (error) {
    if (error.message !== 'unauthorized') flash(String(error));
  }
}

function answerVncPrompt() {
  const dialog = document.getElementById('noVNC_credentials_dlg');
  if (!dialog) return;
  const answer = () => {
    if (!dialog.classList.contains('noVNC_open')) return;
    if (autoFilledPrompt || !password) return;
    autoFilledPrompt = true;
    document.getElementById('noVNC_password_input').value = password;
    document.getElementById('noVNC_credentials_button').click();
  };
  new MutationObserver(answer).observe(dialog, { attributes: true, attributeFilter: ['class'] });
  answer();
  new MutationObserver(() => {
    if (document.documentElement.classList.contains('noVNC_connected')) autoFilledPrompt = false;
  }).observe(document.documentElement, { attributes: true, attributeFilter: ['class'] });
  document.getElementById('noVNC_credentials_button')?.addEventListener('click', () => {
    const typed = document.getElementById('noVNC_password_input')?.value;
    if (typed && typed !== password) {
      rememberPassword(typed);
      setTimeout(refresh, 500);
    }
  }, true);
}

function saveLayout() {
  writeStorage(LAYOUT_KEY, JSON.stringify(layout));
}

function loadLayout() {
  try { Object.assign(layout, JSON.parse(readStorage(LAYOUT_KEY)) ?? {}); } catch {}
}

function clamp(value, min, max) {
  return Math.max(min, Math.min(max, value));
}

function onSideEdge() {
  return layout.mode === 'floating' && layout.edges.some((edge) => edge === 'left' || edge === 'right');
}

function isVertical() {
  return onSideEdge() && !layout.collapsed;
}

function updateOrientation() {
  const changed = panel.classList.contains('vertical') !== isVertical()
    || panel.classList.contains('side') !== onSideEdge();
  panel.classList.toggle('vertical', isVertical());
  panel.classList.toggle('side', onSideEdge());
  return changed;
}

function place() {
  if (layout.mode === 'docked') {
    panel.style.left = panel.style.top = '';
    return;
  }
  const maxX = Math.max(0, window.innerWidth - panel.offsetWidth);
  const maxY = Math.max(0, window.innerHeight - panel.offsetHeight);
  let x = layout.x ?? maxX / 2;
  let y = layout.y ?? 0;
  if (layout.edges.includes('left')) x = 0;
  if (layout.edges.includes('right')) x = maxX;
  if (layout.edges.includes('top')) y = 0;
  if (layout.edges.includes('bottom')) y = maxY;
  panel.style.left = `${clamp(x, 0, maxX)}px`;
  panel.style.top = `${clamp(y, 0, maxY)}px`;
}

function applyLayout() {
  const docked = layout.mode === 'docked';
  panel.classList.toggle('docked', docked);
  panel.classList.toggle('floating', !docked);
  panel.classList.toggle('collapsed', !docked && layout.collapsed);
  updateOrientation();
  arrange();
  renderSwitches();
  document.body.classList.toggle('agent_docked', docked);
  setButton(dockButton, docked ? 'float' : 'dock', docked ? 'Float' : '');
  dockButton.title = docked ? 'Detach into a floating panel' : 'Move to a bar above the screen';
  collapseButton.hidden = docked;
  place();
  saveLayout();
}

function snappedEdges(x, y) {
  const maxX = window.innerWidth - panel.offsetWidth;
  const maxY = window.innerHeight - panel.offsetHeight;
  const edges = [];
  if (x <= SNAP_DISTANCE) edges.push('left');
  else if (x >= maxX - SNAP_DISTANCE) edges.push('right');
  if (y <= SNAP_DISTANCE) edges.push('top');
  else if (y >= maxY - SNAP_DISTANCE) edges.push('bottom');
  return edges;
}

function makeDraggable(handle, isActive, onClick) {
  let drag = null;
  handle.addEventListener('pointerdown', (event) => {
    if (event.button !== 0 || !isActive()) return;
    drag = { x: event.clientX, y: event.clientY, left: panel.offsetLeft, top: panel.offsetTop, moved: false };
    try { handle.setPointerCapture(event.pointerId); } catch {}
    event.preventDefault();
  });
  handle.addEventListener('pointermove', (event) => {
    if (!drag) return;
    const dx = event.clientX - drag.x;
    const dy = event.clientY - drag.y;
    if (!drag.moved && Math.hypot(dx, dy) < DRAG_THRESHOLD) return;
    drag.moved = true;
    panel.classList.add('dragging');
    layout.x = clamp(drag.left + dx, 0, window.innerWidth - panel.offsetWidth);
    layout.y = clamp(drag.top + dy, 0, window.innerHeight - panel.offsetHeight);
    layout.edges = snappedEdges(layout.x, layout.y);
    if (updateOrientation()) {
      arrange();
      renderSwitches();
    }
    place();
  });
  const finish = () => {
    if (!drag) return;
    const { moved } = drag;
    drag = null;
    panel.classList.remove('dragging');
    if (moved) applyLayout(); else onClick?.();
  };
  handle.addEventListener('pointerup', finish);
  handle.addEventListener('pointercancel', finish);
}

async function start() {
  UI = await loadUI();
  if (!slot) return;
  control.onclick = toggleControl;
  pause.onclick = togglePause;
  release.onclick = confirmRelease;
  stop.onclick = openStopDialog;
  stopCancel.onclick = closeStopDialog;
  stopConfirm.onclick = confirmStop;
  stopDialog.addEventListener('click', (event) => { if (event.target === stopDialog) closeStopDialog(); });
  stopDialog.addEventListener('keydown', (event) => {
    event.stopPropagation();
    if (event.key === 'Escape') closeStopDialog();
  });
  badge.onclick = () => { flyoutOpen = !flyoutOpen; updateFlyout(); };
  setButton(collapseButton, 'collapse');
  collapseButton.onclick = () => { layout.collapsed = true; applyLayout(); };
  dockButton.onclick = () => {
    layout.mode = layout.mode === 'docked' ? 'floating' : 'docked';
    layout.collapsed = false;
    applyLayout();
  };
  unlock.addEventListener('submit', (event) => {
    event.preventDefault();
    rememberPassword(passwordInput.value);
    passwordInput.value = '';
    state = null;
    flash('');
    showSwitches();
    renderSwitches();
    refresh();
  });
  window.addEventListener('keydown', (event) => {
    if (!event.altKey || event.ctrlKey || event.metaKey) return;
    const toggleCollapsed = () => {
      layout.collapsed = !(layout.mode === 'floating' && layout.collapsed);
      layout.mode = 'floating';
      applyLayout();
    };
    const handler = { KeyC: toggleControl, KeyP: togglePause, KeyH: toggleCollapsed }[event.code];
    if (!handler) return;
    event.preventDefault();
    event.stopPropagation();
    handler();
  }, true);
  window.addEventListener('resize', place);

  makeDraggable(grip, () => layout.mode === 'floating');
  makeDraggable(panel, () => layout.mode === 'floating' && layout.collapsed, () => {
    layout.collapsed = false;
    applyLayout();
  });
  document.body.append(panel, stopDialog);
  new ResizeObserver(() => {
    document.body.style.setProperty('--agent-bar-height', `${panel.offsetHeight}px`);
    if (!panel.classList.contains('dragging')) place();
  }).observe(panel);

  loadLayout();
  applyLayout();
  password = readStorage(PASSWORD_KEY) || UI.getSetting('password') || '';
  answerVncPrompt();
  if (password) { showSwitches(); renderSwitches(); refresh(); } else showUnlock();
  setInterval(refresh, 2000);
  document.addEventListener('visibilitychange', refresh);
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start); else start();
