const statusList = document.querySelector('#status-list');
const statusError = document.querySelector('#status-error');
const updatedAt = document.querySelector('#updated-at');
const loginPanel = document.querySelector('#login-panel');
const loginForm = document.querySelector('#login-form');
const loginButton = document.querySelector('#login-button');
const loginError = document.querySelector('#login-error');
const privateContent = document.querySelector('#private-content');
const detailsError = document.querySelector('#details-error');
const jobsList = document.querySelector('#jobs-list');
const runsList = document.querySelector('#runs-list');
const logoutButton = document.querySelector('#logout-button');
const refreshButton = document.querySelector('#refresh-button');
const historySearch = document.querySelector('#history-search');
const historyJob = document.querySelector('#history-job');
const historyResult = document.querySelector('#history-result');
const historyPeriod = document.querySelector('#history-period');
const historyClear = document.querySelector('#history-clear');
const historyCount = document.querySelector('#history-count');

let authenticated = false;
let requestVersion = 0;
let authPending = false;
let activeRefresh = 0;
let statusSignature = null;
let cachedJobs = [];
let cachedRuns = [];
const historyGroups = new Map();
const historyPageSize = 10;
let historyJobSignature = null;

function text(value, fallback = 'Not specified') {
  return value === null || value === undefined || value === '' ? fallback : String(value);
}

function node(tag, className, content) {
  const item = document.createElement(tag);
  if (className) item.className = className;
  if (content !== undefined) item.textContent = content;
  return item;
}

function notice(target, message) {
  target.textContent = message;
  target.hidden = !message;
}

function displayTime(value, fallback = 'Not yet') {
  if (!value) return node('span', '', fallback);
  const raw = String(value);
  const timestamp = Date.parse(raw);
  if (!Number.isFinite(timestamp)) return node('span', '', raw);
  const time = node('time', '', new Intl.DateTimeFormat(undefined, {
    year: 'numeric', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit',
    timeZoneName: 'short',
  }).format(timestamp));
  time.dateTime = new Date(timestamp).toISOString();
  time.title = raw;
  return time;
}

function displayDuration(value) {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) return 'Not available';
  if (value < 60_000) return `${(value / 1000).toFixed(value < 10_000 ? 1 : 0)}s`;
  const seconds = Math.floor(value / 1000);
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
}

function replaceCards(target, fragment) {
  const expanded = new Map([...target.querySelectorAll('details[data-key]')].map(detail => [
    detail.dataset.key, {
      open: detail.open,
      scroll: detail.querySelector('pre')?.scrollTop || 0,
      focused: detail.querySelector('summary') === document.activeElement,
    },
  ]));
  target.replaceChildren(fragment);
  for (const detail of target.querySelectorAll('details[data-key]')) {
    const previous = expanded.get(detail.dataset.key);
    if (!previous) continue;
    detail.open = previous.open;
    const pre = detail.querySelector('pre');
    if (pre) pre.scrollTop = previous.scroll;
    if (previous.focused) detail.querySelector('summary').focus({ preventScroll: true });
  }
}

function addField(list, label, value) {
  const wrapper = node('div', 'field');
  wrapper.append(node('dt', '', label));
  const description = node('dd');
  description.append(value instanceof Node ? value : document.createTextNode(String(value)));
  wrapper.append(description);
  list.append(wrapper);
}

function badge(status) {
  const allowed = ['idle', 'running', 'success', 'failed'];
  const safeStatus = allowed.includes(status) ? status : 'unknown';
  return node('span', `badge badge-${safeStatus}`, safeStatus[0].toUpperCase() + safeStatus.slice(1));
}

function addWorkflow(list, run) {
  const workflow = run?.workflow;
  if (!workflow) return;
  const failed = run.status === 'failed' || ['incomplete', 'blocked', 'interrupted', 'failure'].includes(workflow.outcome);
  const result = node('span', `workflow-outcome badge badge-${failed ? 'failed' : 'success'}`,
    `${text(workflow.outcome)}${failed ? ': attention required' : ''}`);
  result.dataset.outcome = workflow.outcome;
  addField(list, 'Workflow result', result);
  addField(list, 'Workflow phase', node('span', 'workflow-phase', text(workflow.phase)));
  addField(list, 'Workflow scope', node('span', 'workflow-scope', text(workflow.scope)));
  const links = node('span', 'workflow-links');
  for (const url of Array.isArray(workflow.githubUrls) ? workflow.githubUrls : []) {
    if (typeof url !== 'string' || url.length > 2048 ||
        !/^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/(?:pull|issues)\/[1-9]\d*$/.test(url)) continue;
    if (links.childNodes.length) links.append(document.createTextNode(' · '));
    const link = node('a', '', url);
    link.href = url;
    link.target = '_blank';
    link.rel = 'noopener noreferrer';
    links.append(link);
  }
  if (!links.childNodes.length) links.textContent = 'None';
  addField(list, 'Workflow publications', links);
  addField(list, 'Workflow blocker', node('span', 'workflow-blocker', text(workflow.blocker, 'None')));
}

async function getJson(path) {
  const response = await fetch(path, {
    credentials: 'same-origin', cache: 'no-store', signal: AbortSignal.timeout(10_000),
  });
  if (!response.ok) {
    const error = new Error(`Request failed (${response.status})`);
    error.status = response.status;
    throw error;
  }
  return response.json();
}

function requireArray(payload, property) {
  if (!payload || !Array.isArray(payload[property])) throw new Error('Invalid server response');
  return payload[property];
}

function renderStatus(jobs) {
  const entries = jobs.map((job) => ({
    name: text(job?.name, 'Unnamed job'),
    status: job?.status,
  }));
  const signature = JSON.stringify(entries);
  if (signature === statusSignature) return;
  statusSignature = signature;
  const fragment = document.createDocumentFragment();
  if (!entries.length) fragment.append(node('li', 'empty-state', 'No jobs are configured.'));
  for (const job of entries) {
    const item = node('li', 'status-item');
    item.append(node('span', 'status-name', job.name), badge(job.status));
    fragment.append(item);
  }
  statusList.replaceChildren(fragment);
}

function sortedRuns(runs) {
  return [...runs].sort((left, right) => {
    const leftTime = Date.parse(left?.startedAt) || 0;
    const rightTime = Date.parse(right?.startedAt) || 0;
    return rightTime - leftTime;
  });
}

function renderJobs(jobs, runs) {
  const fragment = document.createDocumentFragment();
  if (!jobs.length) fragment.append(node('p', 'empty-state', 'No jobs are configured.'));
  const recentRuns = sortedRuns(runs);
  for (const job of jobs) {
    const jobRuns = recentRuns.filter((run) => String(run?.jobId) === String(job?.id));
    const latest = job?.lastRun ?? jobRuns[0];
    const latestCompleted = ['success', 'failed'].includes(latest?.status) ? latest
      : jobRuns.find((run) => run?.status === 'success' || run?.status === 'failed');
    const card = node('article', 'job-card');
    const heading = node('div', 'card-heading');
    heading.append(node('h3', '', text(job?.name, 'Unnamed job')), badge(job?.status));
    card.append(heading);

    const summary = node('dl', 'fields');
    addField(summary, 'Next run', displayTime(job?.enabled ? job.nextRun : null,
      job?.enabled ? 'Not scheduled' : 'Disabled'));
    addField(summary, 'Last run', displayTime(latest?.startedAt));
    addField(summary, 'Last outcome', text(latestCompleted?.outcome ?? latestCompleted?.status ?? latest?.status, 'No runs yet'));
    addField(summary, 'Last completed duration', displayDuration(job?.stats?.lastDurationMs ??
      latestCompleted?.durationMs ?? latest?.durationMs));
    if (latest?.reason) addField(summary, 'Last run diagnostic', latest.reason);
    addWorkflow(summary, latest);
    if (job?.stats) {
      addField(summary, 'Retained runs', String(job.stats.total));
      addField(summary, 'Retained outcomes',
        `${job.stats.success} succeeded · ${job.stats.failed} failed · ${job.stats.running} running`);
    }
    addField(summary, 'Schedule', text(job?.schedule));
    addField(summary, 'Timezone', text(job?.timezone));
    addField(summary, 'Enabled', job?.enabled ? 'Yes' : 'No');
    addField(summary, 'Runner', text(job?.runner));
    if (['claude-print', 'claude-sdk'].includes(job?.runner)) addField(summary, 'Claude permissions', text(job?.permissionMode));
    if (['claude-print', 'claude-sdk'].includes(job?.runner) && job.allowedTools) addField(summary, 'Allowed tools', JSON.stringify(job.allowedTools));
    if (['claude-print', 'claude-sdk'].includes(job?.runner) && job.additionalDirectories) addField(summary, 'Additional directories', JSON.stringify(job.additionalDirectories));
    if (job?.runner !== 'command') addField(summary, 'Model', text(job?.model, 'Default'));
    addField(summary, 'Timeout', job?.timeoutMinutes == null
      ? 'Not specified' : `${job.timeoutMinutes} minutes`);
    addField(summary, 'Working directory', text(job?.cwd));
    addField(summary, 'Job ID', text(job?.id));
    card.append(summary);

    const prompt = node('details', 'detail-block');
    prompt.dataset.key = job.id;
    prompt.append(node('summary', '', job.runner === 'command' ? 'View command argv' : 'View prompt'),
      node('pre', 'detail-text', job.runner === 'command' ? JSON.stringify(job.argv, null, 2) : text(job?.prompt, 'No prompt')));
    card.append(prompt);
    fragment.append(card);
  }
  replaceCards(jobsList, fragment);
}

function needsAttention(run) {
  return run.status === 'failed' ||
    ['failure', 'timeout', 'cancelled', 'interrupted'].includes(run.outcome) ||
    ['incomplete', 'blocked', 'interrupted', 'failure'].includes(run.workflow?.outcome);
}

function quietRun(run) {
  return ['idle', 'skipped'].includes(run.outcome);
}

function runResult(run) {
  return text(run.workflow?.outcome ?? run.outcome ?? run.status, 'Unknown result');
}

function matchesHistory(run, jobName, words, cutoff) {
  if (cutoff && !(Date.parse(run.startedAt) >= cutoff)) return false;
  const result = historyResult.value;
  if (result === 'attention' && !needsAttention(run)) return false;
  if (result === 'running' && run.status !== 'running') return false;
  if (result === 'processed' && !(run.outcome === 'processed' ||
      ['pr_created', 'issue_created'].includes(run.workflow?.outcome))) return false;
  if (result === 'idle' && !quietRun(run)) return false;
  if (result === 'success' && run.status !== 'success') return false;
  if (!words.length) return true;
  const searchable = [jobName, run.jobName, run.jobId, run.id, run.runner, run.status,
    run.outcome, run.startedAt, run.finishedAt, run.reason, run.workflow?.outcome,
    run.workflow?.phase, run.workflow?.scope, run.workflow?.blocker,
    ...(run.workflow?.githubUrls ?? []).flatMap(url => [url, `#${String(url).split('/').pop()}`])].filter(value => value != null).join(' ').toLowerCase();
  return words.every(word => searchable.includes(word));
}

function runCard(run) {
  const card = node('details', 'run-entry');
  card.dataset.key = `run:${run.id}`;
  const heading = node('summary');
  const headingText = node('span', 'run-entry-summary');
  headingText.append(displayTime(run.startedAt), badge(run.status),
    node('span', '', runResult(run)), node('span', 'muted', displayDuration(run.durationMs)));
  heading.append(headingText);
  card.append(heading);
  const content = node('div', 'run-entry-content');
  const metadata = node('dl', 'fields');
  addField(metadata, 'Started', displayTime(run.startedAt));
  addField(metadata, 'Finished', displayTime(run.finishedAt, 'In progress'));
  addField(metadata, 'Runner', text(run.runner));
  addField(metadata, 'Outcome', text(run.outcome, run.status));
  addField(metadata, 'Exit code', text(run.exitCode, '-'));
  addField(metadata, 'Duration', displayDuration(run.durationMs));
  if (run.reason) addField(metadata, 'Diagnostic', run.reason);
  addWorkflow(metadata, run);
  addField(metadata, 'Run ID', text(run.id));
  content.append(metadata);
  const log = node('details', 'detail-block');
  log.dataset.key = `log:${run.id}`;
  log.append(node('summary', '', 'View log'), node('pre', 'detail-text', text(run.log, 'No log output')));
  content.append(log);
  card.append(content);
  return card;
}

function renderGroupRuns(body, runs, state, summary) {
  const fragment = document.createDocumentFragment();
  for (const run of runs.slice(0, state.limit)) fragment.append(runCard(run));
  const shown = Math.min(state.limit, runs.length);
  const page = node('div', 'history-page');
  page.append(node('span', 'muted', `Showing ${shown} of ${runs.length} matching runs · newest first`));
  if (shown < runs.length) {
    const more = node('button', 'button button-secondary history-more', `Show ${Math.min(historyPageSize, runs.length - shown)} more`);
    more.type = 'button';
    more.addEventListener('click', () => {
      state.limit += historyPageSize;
      renderGroupRuns(body, runs, state, summary);
      (body.querySelector('.history-more') ?? summary).focus({ preventScroll: true });
    });
    page.append(more);
  }
  fragment.append(page);
  replaceCards(body, fragment);
}

function renderRuns(runs) {
  const groups = new Map();
  const jobs = new Map(cachedJobs.map(job => [String(job.id), job]));
  for (const run of sortedRuns(runs)) {
    const id = String(run.jobId ?? '');
    if (!groups.has(id)) groups.set(id, {
      id, name: text(jobs.get(id)?.name ?? run.jobName, 'Unnamed job'), runs: [],
    });
    groups.get(id).runs.push(run);
  }
  const ordered = [...groups.values()].sort((left, right) =>
    left.name.localeCompare(right.name) || left.id.localeCompare(right.id));
  const jobSignature = JSON.stringify(ordered.map(group => [group.id, group.name]));
  if (jobSignature !== historyJobSignature) {
    const selected = historyJob.value;
    const options = document.createDocumentFragment();
    const all = node('option', '', 'All jobs');
    all.value = '';
    options.append(all);
    for (const group of ordered) {
      const option = node('option', '', group.name);
      option.value = group.id;
      options.append(option);
    }
    historyJob.replaceChildren(options);
    historyJob.value = groups.has(selected) ? selected : '';
    historyJobSignature = jobSignature;
  }
  const words = historySearch.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const cutoff = historyPeriod.value ? Date.now() - Number(historyPeriod.value) * 86_400_000 : 0;
  const fragment = document.createDocumentFragment();
  let matching = 0;
  let visibleJobs = 0;
  for (const group of ordered) {
    if (historyJob.value && historyJob.value !== group.id) continue;
    const filtered = group.runs.filter(run => matchesHistory(run, group.name, words, cutoff));
    if (!filtered.length) continue;
    matching += filtered.length;
    visibleJobs++;
    if (!historyGroups.has(group.id)) historyGroups.set(group.id, { open: false, limit: historyPageSize });
    const state = historyGroups.get(group.id);
    const detail = node('details', 'history-group');
    detail.dataset.key = `job:${group.id}`;
    detail.dataset.jobId = group.id;
    detail.open = state.open;
    const summary = node('summary');
    summary.append(node('span', 'history-group-title', group.name));
    const attention = group.runs.filter(needsAttention).length;
    const quiet = group.runs.filter(quietRun).length;
    summary.append(node('span', 'history-group-info',
      `${filtered.length} of ${group.runs.length} retained runs · ${attention} need attention · ${quiet} idle/skipped`));
    const latest = node('span', 'history-group-latest');
    latest.append(document.createTextNode('Latest match: '), displayTime(filtered[0].startedAt),
      document.createTextNode(` · ${runResult(filtered[0])} `), badge(filtered[0].status));
    summary.append(latest);
    const body = node('div', 'history-group-body');
    detail.append(summary, body);
    if (state.open) renderGroupRuns(body, filtered, state, summary);
    detail.addEventListener('toggle', () => {
      if (!detail.isConnected) return;
      state.open = detail.open;
      if (detail.open && !body.childNodes.length) renderGroupRuns(body, filtered, state, summary);
    });
    fragment.append(detail);
  }
  if (!visibleJobs) fragment.append(node('p', 'empty-state history-empty', runs.length
    ? 'No runs match these filters. Clear filters to see retained history.' : 'No runs yet.'));
  historyCount.textContent = `${matching} of ${runs.length} retained runs · ${visibleJobs} ${visibleJobs === 1 ? 'job' : 'jobs'}`;
  replaceCards(runsList, fragment);
}

function filterHistory() {
  for (const state of historyGroups.values()) state.limit = historyPageSize;
  renderRuns(cachedRuns);
}

historySearch.addEventListener('input', filterHistory);
for (const control of [historyJob, historyResult, historyPeriod]) control.addEventListener('change', filterHistory);
historyClear.addEventListener('click', () => {
  historySearch.value = historyJob.value = historyResult.value = historyPeriod.value = '';
  filterHistory();
  historySearch.focus();
});

function setAuthenticated(value) {
  if (authenticated === value) return;
  authenticated = value;
  loginPanel.hidden = value;
  privateContent.hidden = !value;
  logoutButton.hidden = !value;
  if (!value) {
    cachedJobs = [];
    cachedRuns = [];
    jobsList.replaceChildren();
    runsList.replaceChildren();
    historyGroups.clear();
    historySearch.value = historyJob.value = historyResult.value = historyPeriod.value = '';
    historyCount.textContent = '';
    historyJob.replaceChildren(node('option', '', 'All jobs'));
    historyJob.firstChild.value = '';
    historyJobSignature = null;
    notice(detailsError, '');
  } else {
    jobsList.textContent = 'Loading configuration…';
    runsList.textContent = 'Loading run history…';
    notice(loginError, '');
  }
}

async function refresh() {
  if (authPending || (activeRefresh && activeRefresh === requestVersion)) return;
  const version = ++requestVersion;
  activeRefresh = version;
  refreshButton.disabled = true;
  try {
    const [statusResult, sessionResult] = await Promise.allSettled([
      getJson('/api/status'), getJson('/api/me'),
    ]);
    if (version !== requestVersion) return;
    if (statusResult.status === 'fulfilled') {
      try {
        renderStatus(requireArray(statusResult.value, 'jobs'));
        updatedAt.textContent = `Updated ${new Intl.DateTimeFormat(undefined, {
          hour: 'numeric', minute: '2-digit', second: '2-digit',
        }).format(new Date())}`;
        notice(statusError, '');
      } catch {
        notice(statusError, 'Job status is unavailable. Try refreshing.');
      }
    } else {
      notice(statusError, 'Job status is unavailable. Try refreshing.');
    }

    if (sessionResult.status === 'rejected') {
      if (sessionResult.reason.status === 401 || sessionResult.reason.status === 403) {
        setAuthenticated(false);
      } else {
        notice(authenticated ? detailsError : loginError,
          'Could not check your session. Try refreshing.');
      }
      return;
    }
    const signedIn = sessionResult.value?.authenticated === true;
    const wasAuthenticated = authenticated;
    setAuthenticated(signedIn);
    if (!signedIn) {
      if (wasAuthenticated) notice(loginError, 'Your session has ended. Sign in again.');
      return;
    }

    const [jobsResult, runsResult] = await Promise.allSettled([
      getJson('/api/jobs'), getJson('/api/runs'),
    ]);
    if (version !== requestVersion) return;
    if ([jobsResult, runsResult].some((result) => result.status === 'rejected' &&
      (result.reason.status === 401 || result.reason.status === 403))) {
      setAuthenticated(false);
      notice(loginError, 'Your session has ended. Sign in again.');
      return;
    }
    const problems = [];
    if (jobsResult.status === 'fulfilled') {
      try { cachedJobs = requireArray(jobsResult.value, 'jobs'); }
      catch { problems.push('configuration'); }
    } else problems.push('configuration');
    if (runsResult.status === 'fulfilled') {
      try { cachedRuns = requireArray(runsResult.value, 'runs'); }
      catch { problems.push('run history'); }
    } else problems.push('run history');
    renderJobs(cachedJobs, cachedRuns);
    renderRuns(cachedRuns);
    notice(detailsError, problems.length ? `Could not update ${problems.join(' and ')}. Showing the last available data.` : '');
  } finally {
    if (activeRefresh === version) activeRefresh = 0;
    if (version === requestVersion) refreshButton.disabled = false;
  }
}

loginForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  if (authPending) return;
  authPending = true;
  const passwordInput = loginForm.elements.password;
  const password = passwordInput.value;
  passwordInput.value = '';
  notice(loginError, '');
  loginButton.disabled = true;
  ++requestVersion;
  refreshButton.disabled = true;
  try {
    const response = await fetch('/api/login', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password }),
      signal: AbortSignal.timeout(10_000),
    });
    if (!response.ok) {
      notice(loginError, response.status === 429
        ? 'Too many sign-in attempts. Wait 15 minutes before trying again.'
        : response.status === 401 || response.status === 403
          ? 'Incorrect password.' : 'Could not sign in. Try again.');
      passwordInput.focus();
      return;
    }
  } catch {
    notice(loginError, 'Could not sign in. Try again.');
  } finally {
    ++requestVersion;
    authPending = false;
    loginButton.disabled = false;
    await refresh();
  }
});

logoutButton.addEventListener('click', async () => {
  if (authPending) return;
  authPending = true;
  let failed = false;
  ++requestVersion;
  refreshButton.disabled = true;
  setAuthenticated(false);
  notice(loginError, '');
  logoutButton.disabled = true;
  try {
    const response = await fetch('/api/logout', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
      signal: AbortSignal.timeout(10_000),
    });
    if (!response.ok) throw new Error('Logout failed');
    ++requestVersion;
    setAuthenticated(false);
    loginForm.elements.password.focus();
  } catch {
    failed = true;
  } finally {
    ++requestVersion;
    authPending = false;
    refreshButton.disabled = false;
    logoutButton.disabled = false;
    if (failed) {
      await refresh();
      notice(authenticated ? detailsError : loginError, 'Could not log out. Try again.');
    }
  }
});

refreshButton.addEventListener('click', () => { refresh(); });
document.addEventListener('visibilitychange', () => {
  if (!document.hidden) refresh();
});
setInterval(() => {
  if (!document.hidden) refresh();
}, 30_000);
refresh();
