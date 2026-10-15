import https from 'node:https';
import http from 'node:http';
import { readFileSync, writeFileSync, renameSync, mkdirSync, chmodSync, existsSync, unlinkSync, lstatSync, openSync } from 'node:fs';
import { join, dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import net from 'node:net';
import { execFileSync } from 'node:child_process';
import { randomBytes, randomUUID, createHash, scryptSync, timingSafeEqual } from 'node:crypto';
import { nextOccurrence, configuredJob, patchJob, jobStats } from './jobs.mjs';
import { readLog, startProcess } from './run-process.mjs';
import { lockRunFiles } from './run-files.mjs';
import { cleanupRunUnit, newRunUnit } from './run-unit.mjs';
import { privateRun } from './run-view.mjs';

const root = dirname(fileURLToPath(import.meta.url));
const dataDir = resolve(process.env.SCHEDULER_DATA_DIR || join(process.env.HOME, '.local/state/agent-scheduler'));
const socket = process.env.SCHEDULER_SOCKET || join(process.env.XDG_RUNTIME_DIR || `/run/user/${process.getuid()}`, 'agent-scheduler.sock');
const port = Number(process.env.SCHEDULER_PORT || 8457);
const tlsKey = join(dataDir, 'tls.key');
const tlsCert = join(dataDir, 'tls.crt');
const statePath = join(dataDir, 'state.json');
const expiryMs = 7 * 24 * 60 * 60 * 1000;
const maxConcurrent = 2;
process.umask(0o077);
mkdirSync(join(dataDir, 'logs'), { recursive: true, mode: 0o700 });
chmodSync(dataDir, 0o700);
// flock locks the inherited open file description; this fd keeps it until exit.
const stateLock = openSync(join(dataDir, 'server.lock'), 'a', 0o600);
try { execFileSync('flock', ['--nonblock', '3'], { stdio: ['ignore', 'pipe', 'pipe', stateLock] }); }
catch { throw Error(`Another scheduler owns ${dataDir}, or flock is unavailable`); }
if (!existsSync(tlsKey) || !existsSync(tlsCert)) throw Error(`TLS key/certificate missing in ${dataDir}; provision before starting`);
let state = existsSync(statePath) ? JSON.parse(readFileSync(statePath, 'utf8')) : { jobs: [], runs: [], sessions: [], password: null };
state.pendingCleanup ||= [];
state.ownedUnits ||= [];
function save(next = state) {
  const temp = `${statePath}.${process.pid}.tmp`;
  writeFileSync(temp, JSON.stringify(next, null, 2), { mode: 0o600 });
  renameSync(temp, statePath);
  state = next;
}
if (!existsSync(statePath)) save();
const active = new Map();
// Reconcile only persisted, globally unique owned units, before accepting any
// requests or starting catch-up. Failure leaves ownership intact and fails closed.
for (const owned of state.ownedUnits) await cleanupRunUnit(owned.unit);
state.ownedUnits = [];
for (const run of state.runs) {
  if (run.status === 'running') {
    run.status = 'failed';
    run.finishedAt = new Date().toISOString();
    run.durationMs = Math.max(0, Date.parse(run.finishedAt) - Date.parse(run.startedAt));
    run.exitCode = null;
    run.reason = 'Service restarted while run was active';
    run.outcome = 'interrupted';
  }
}
persistRun();

function status(job) {
  if (active.has(job.id)) return 'running';
  const last = state.runs.find(run => run.jobId === job.id);
  return last ? (last.status === 'success' ? 'success' : 'failed') : 'idle';
}
function publicJobs() { return state.jobs.map(job => ({ name: job.name, status: status(job) })); }
function privateJobs() {
  return state.jobs.map(job => {
    const runs = state.runs.filter(run => run.jobId === job.id);
    return { ...job, status: status(job), lastRun: runs[0] ? privateRun(runs[0]) : null, stats: jobStats(runs) };
  });
}
function cleanupRuns() {
  const pendingCleanup = [];
  for (const run of state.pendingCleanup) {
    let files;
    try {
      files = lockRunFiles(dataDir, [run]);
      files.remove();
    } catch (error) {
      pendingCleanup.push(run);
      if (error.code !== 'RUN_JOINED') console.error(`Run cleanup failed: ${error.message}`);
    } finally { files?.release(); }
  }
  if (pendingCleanup.length !== state.pendingCleanup.length) save({ ...state, pendingCleanup });
}
function persistRun(next = state) {
  const runs = next.runs.slice();
  const removed = [];
  while (runs.length > 200) {
    const index = runs.findLastIndex(run => run.status !== 'running');
    if (index < 0) break;
    removed.push(...runs.splice(index, 1));
  }
  const pendingCleanup = [...next.pendingCleanup, ...removed.map(({ id, jobId }) => ({ id, jobId }))];
  save({ ...next, runs, pendingCleanup });
  cleanupRuns();
}
function finish(run, code, reason, result) {
  if (run.status !== 'running') return;
  run.finishedAt = new Date().toISOString();
  run.exitCode = code;
  run.durationMs = Math.max(0, Date.parse(run.finishedAt) - Date.parse(run.startedAt));
  run.status = code === 0 && !reason ? 'success' : 'failed';
  run.outcome = result?.outcome || (run.status === 'success' ? 'success' : 'failure');
  if (result?.clean) state.ownedUnits = state.ownedUnits.filter(owned => owned.unit !== run.unit);
  if (result?.workflow) run.workflow = result.workflow;
  if (reason || result?.reason) run.reason = reason || result.reason;
  active.delete(run.jobId);
  try { persistRun(); }
  catch (error) { console.error(`Run persistence failed: ${error.message}`); process.exitCode = 1; shutdown(); }
}
let shuttingDown = false;
function launch(job, nextRun) {
  if (shuttingDown || active.has(job.id) || active.size >= maxConcurrent) return false;
  const id = `${Date.now()}-${randomBytes(4).toString('hex')}`;
  const log = join(dataDir, 'logs', `${id}.log`);
  const run = { id, jobId: job.id, jobName: job.name, runner: job.runner, cwd: job.cwd, unit: newRunUnit(), status: 'running', startedAt: new Date().toISOString(), finishedAt: null, exitCode: null, log };
  const args = [];
  let command;
  let promptFile;
  const env = { ...process.env, SCHEDULER_RUN_ID: id, SCHEDULER_JOB_ID: job.id };
  if (job.runner === 'omp') {
    command = 'omp';
    run.sessionDir = join(dataDir, 'logs', `${id}.session`);
    args.push('-p', '--allow-home', '--session-dir', run.sessionDir);
    if (job.model) args.push('--model', job.model);
    promptFile = join(run.sessionDir, 'prompt.txt');
    args.push(`@${promptFile}`);
  } else if (job.runner === 'claude-print') {
    command = 'claude';
    run.sessionId = randomUUID();
    args.push('-p', '--session-id', run.sessionId, '--permission-mode', job.permissionMode, '--permission-prompts', 'none');
    if (job.permissionMode === 'bypassPermissions') args.push('--allow-dangerously-skip-permissions');
    if (job.model) args.push('--model', job.model);
    if (job.allowedTools?.length) args.push('--allowedTools', ...job.allowedTools);
    if (job.additionalDirectories?.length) args.push('--add-dir', ...job.additionalDirectories);
  } else if (job.runner === 'command') {
    [command] = job.argv;
    args.push(...job.argv.slice(1));
  } else {
    command = process.execPath;
    run.sessionId = randomUUID();
    args.push(join(root, 'sdk-runner.mjs'));
    env.SCHEDULER_MODEL = job.model;
    env.SCHEDULER_PERMISSION_MODE = job.permissionMode;
    env.SCHEDULER_SESSION_ID = run.sessionId;
    env.SCHEDULER_ALLOWED_TOOLS = JSON.stringify(job.allowedTools || []);
    env.SCHEDULER_ADDITIONAL_DIRECTORIES = JSON.stringify(job.additionalDirectories || []);
  }
  const jobs = nextRun === undefined ? state.jobs : state.jobs.map(item => item.id === job.id ? { ...item, nextRun } : item);
  persistRun({ ...state, jobs, runs: [run, ...state.runs], ownedUnits: [...state.ownedUnits, { unit: run.unit, runId: run.id }] });
  const processRun = startProcess({
    command, args, cwd: job.cwd, env, prompt: job.runner === 'command' ? undefined : job.prompt, promptFile, log, unit: run.unit, report: job.runner === 'command' || job.runner === 'claude-sdk',
    timeoutMs: job.timeoutMinutes * 60_000,
    onContainmentFailure: reason => { console.error(reason); process.exitCode = 1; shutdown(); },
    onFinish: (code, reason, result) => finish(run, code, reason, result),
  });
  active.set(job.id, processRun);
  return true;
}
function tick() {
  if (shuttingDown) return;
  cleanupRuns();
  const now = new Date();
  const due = state.jobs.filter(job => job.enabled && job.nextRun && new Date(job.nextRun) <= now)
    .sort((a, b) => Date.parse(a.nextRun) - Date.parse(b.nextRun) || a.id.localeCompare(b.id));
  for (const job of due) {
    const nextRun = nextOccurrence(job.schedule, job.timezone, now);
    if (active.has(job.id)) {
      save({ ...state, jobs: state.jobs.map(item => item.id === job.id ? { ...item, nextRun } : item) });
    } else launch(job, nextRun);
  }
}
let ticker;
function safeTick() {
  try { tick(); } catch (error) { console.error(`Scheduler tick failed: ${error.message}`); process.exitCode = 1; shutdown(); }
}

const hash = token => createHash('sha256').update(token).digest('hex');
function setPassword(password) {
  if (typeof password !== 'string' || password.length < 16 || password.length > 1024) throw Error('password must be 16-1024 characters');
  const salt = randomBytes(16).toString('hex');
  save({ ...state, password: { salt, hash: scryptSync(password, salt, 64).toString('hex') }, sessions: [] });
}
function correctPassword(password) {
  if (!state.password || typeof password !== 'string' || password.length < 16 || password.length > 1024) return false;
  const actual = scryptSync(password, state.password.salt, 64);
  return timingSafeEqual(actual, Buffer.from(state.password.hash, 'hex'));
}
function cookieToken(req) { return /(?:^|;\s*)scheduler_session=([a-f0-9]{64})(?:;|$)/.exec(req.headers.cookie || '')?.[1]; }
function session(req, res) {
  const token = cookieToken(req);
  const entry = token && state.sessions.find(s => s.hash === hash(token) && s.expires > Date.now());
  if (!entry) return null;
  entry.expires = Date.now() + expiryMs;
  res.setHeader('Set-Cookie', `scheduler_session=${token}; HttpOnly; Secure; SameSite=Strict; Path=/; Max-Age=604800`);
  save();
  return entry;
}
function json(res, code, data) {
  res.writeHead(code, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff', 'Referrer-Policy': 'no-referrer' });
  res.end(JSON.stringify(data));
}
async function body(req) {
  let text = '';
  req.setEncoding('utf8');
  for await (const chunk of req) { text += chunk; if (Buffer.byteLength(text) > 2 * 1024 * 1024) throw Error('request too large'); }
  return JSON.parse(text || '{}');
}
const attempts = new Map();
const assets = { '/': ['index.html', 'text/html'], '/app.js': ['app.js', 'text/javascript'], '/style.css': ['style.css', 'text/css'] };
function logTail(file) { return readLog(file).output; }

async function handle(req, res, internal = false) {
  try {
    const url = new URL(req.url, 'https://localhost');
    const path = url.pathname;
    if (internal) {
      if (req.headers.origin || req.headers['sec-fetch-site']) return json(res, 403, { error: 'browser requests are not allowed on the local API' });
      if (req.method === 'GET' && path === '/snapshot') return json(res, 200, { jobs: privateJobs(), runs: state.runs.map(privateRun) });
      if (req.method === 'GET' && path.startsWith('/runs/')) {
        const run = state.runs.find(run => run.id === decodeURIComponent(path.slice(6)));
        if (!run) return json(res, 404, { error: 'run not found' });
        const cursor = url.searchParams.get('offset');
        if (cursor !== null && !/^(0|[1-9][0-9]*)$/.test(cursor)) return json(res, 400, { error: 'invalid log offset' });
        const offset = cursor === null ? undefined : Number(cursor);
        if (offset !== undefined && !Number.isSafeInteger(offset)) return json(res, 400, { error: 'invalid log offset' });
        return json(res, 200, { run: privateRun(run), ...readLog(run.log, offset, run.status !== 'running') });
      }
      if (req.method === 'POST' && path === '/runs/stop') {
        const { id } = await body(req);
        const run = state.runs.find(run => run.id === id);
        if (!run) return json(res, 404, { error: 'run not found' });
        if (run.status !== 'running') return json(res, 409, { error: 'run has already finished' });
        active.get(run.jobId)?.stop('Cancelled by owner');
        return json(res, 200, { ok: true, run });
      }
      if (req.method === 'POST' && path === '/jobs/patch') {
        const { id, changes } = await body(req);
        const index = state.jobs.findIndex(job => job.id === id);
        if (index < 0) return json(res, 404, { error: 'job not found' });
        const job = patchJob(state.jobs[index], changes);
        const jobs = state.jobs.slice();
        jobs[index] = job;
        save({ ...state, jobs });
        return json(res, 200, { job });
      }
      if (req.method === 'GET' && path === '/jobs') return json(res, 200, { jobs: privateJobs() });
      if (req.method === 'GET' && path === '/runs') return json(res, 200, { runs: state.runs.map(privateRun) });
      if (req.method === 'POST' && path === '/jobs') {
        const input = await body(req);
        const old = state.jobs.findIndex(j => j.id === input?.id);
        const job = configuredJob(input, state.jobs[old]);
        const jobs = state.jobs.slice();
        if (old === -1) jobs.push(job); else jobs[old] = job;
        save({ ...state, jobs }); return json(res, 200, { job });
      }
      if (req.method === 'POST' && path === '/jobs/state') {
        const { id, enabled } = await body(req);
        const job = state.jobs.find(j => j.id === id);
        if (!job) return json(res, 404, { error: 'job not found' });
        if (typeof enabled !== 'boolean') return json(res, 400, { error: 'enabled must be boolean' });
        const updated = patchJob(job, { enabled });
        save({ ...state, jobs: state.jobs.map(item => item === job ? updated : item) });
        return json(res, 200, { job: updated });
      }
      if (req.method === 'POST' && path === '/jobs/delete') {
        const { id } = await body(req);
        const index = state.jobs.findIndex(j => j.id === id);
        if (index < 0) return json(res, 404, { error: 'job not found' });
        if (active.has(id)) return json(res, 409, { error: 'job is running' });
        const removed = [...state.runs.filter(r => r.jobId === id), ...state.pendingCleanup.filter(r => r.jobId === id)];
        let files;
        try { files = lockRunFiles(dataDir, removed); }
        catch (error) {
          if (error.code === 'RUN_JOINED') return json(res, 409, { error: error.message });
          throw error;
        }
        try {
          const pendingCleanup = [...state.pendingCleanup, ...state.runs.filter(r => r.jobId === id).map(({ id, jobId }) => ({ id, jobId }))];
          save({ ...state, jobs: state.jobs.filter(j => j.id !== id), runs: state.runs.filter(r => r.jobId !== id), pendingCleanup });
          files.remove();
          save({ ...state, pendingCleanup: state.pendingCleanup.filter(run => run.jobId !== id) });
        } finally { files.release(); }
        return json(res, 200, { ok: true });
      }
      if (req.method === 'POST' && path === '/run') {
        const { id } = await body(req);
        const job = state.jobs.find(j => j.id === id);
        if (!job) return json(res, 404, { error: 'job not found' });
        if (!launch(job)) return json(res, 409, { error: 'job already running or at concurrency limit' });
        return json(res, 200, { run: state.runs[0] });
      }
      if (req.method === 'POST' && path === '/password') { setPassword((await body(req)).password); return json(res, 200, { ok: true }); }
      return json(res, 404, { error: 'not found' });
    }
    if (req.method === 'GET' && assets[path]) {
      const [file, type] = assets[path];
      res.writeHead(200, { 'Content-Type': `${type}; charset=utf-8`, 'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff', 'Content-Security-Policy': "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'" });
      return res.end(readFileSync(join(root, 'public', file)));
    }
    if (req.method === 'GET' && path === '/api/status') return json(res, 200, { jobs: publicJobs() });
    if (req.method === 'GET' && path === '/api/me') return json(res, 200, { authenticated: Boolean(session(req, res)) });
    if (req.method === 'POST') {
      const origin = req.headers.origin;
      if (!origin || origin !== `https://${req.headers.host}` || !req.headers['content-type']?.startsWith('application/json')) return json(res, 403, { error: 'same-origin JSON request required' });
    }
    if (req.method === 'POST' && path === '/api/login') {
      const credentials = await body(req);
      const addr = req.socket.remoteAddress;
      for (const [key, value] of attempts) if (value.until < Date.now()) attempts.delete(key);
      const a = attempts.get(addr) || { count: 0, until: Date.now() + 15 * 60_000 };
      if (a.until < Date.now()) { a.count = 0; a.until = Date.now() + 15 * 60_000; }
      if (a.count >= 5) return json(res, 429, { error: 'Too many attempts; try again later' });
      if (!correctPassword(credentials.password)) { a.count++; attempts.set(addr, a); return json(res, 401, { error: 'Incorrect password' }); }
      attempts.delete(addr);
      const token = randomBytes(32).toString('hex');
      state.sessions.push({ hash: hash(token), expires: Date.now() + expiryMs });
      state.sessions = state.sessions.filter(s => s.expires > Date.now());
      save();
      res.setHeader('Set-Cookie', `scheduler_session=${token}; HttpOnly; Secure; SameSite=Strict; Path=/; Max-Age=604800`);
      return json(res, 200, { ok: true });
    }
    const auth = session(req, res);
    if (!auth) return json(res, 401, { error: 'Login required' });
    if (req.method === 'POST' && path === '/api/logout') {
      state.sessions = state.sessions.filter(s => s !== auth); save();
      res.setHeader('Set-Cookie', 'scheduler_session=; HttpOnly; Secure; SameSite=Strict; Path=/; Max-Age=0');
      return json(res, 200, { ok: true });
    }
    if (req.method === 'GET' && path === '/api/jobs') return json(res, 200, { jobs: privateJobs() });
    if (req.method === 'GET' && path === '/api/runs') {
      const runs = state.runs.map(run => ({ ...privateRun(run), log: logTail(run.log) }));
      return json(res, 200, { runs });
    }
    return json(res, 404, { error: 'not found' });
  } catch (error) { return json(res, 400, { error: error.message }); }
}
const local = http.createServer((req, res) => void handle(req, res, true));
const web = https.createServer({ key: readFileSync(tlsKey), cert: readFileSync(tlsCert) }, (req, res) => void handle(req, res));
for (const server of [local, web]) {
  server.requestTimeout = 15_000;
  server.headersTimeout = 10_000;
  server.on('error', error => { console.error(error); process.exitCode = 1; shutdown(); });
}
// Only remove a stale socket owned by this uid, never steal a live daemon's socket.
if (existsSync(socket)) {
  const info = lstatSync(socket);
  if (!info.isSocket() || info.uid !== process.getuid()) throw Error(`Refusing to replace ${socket}`);
  const live = await new Promise((resolve, reject) => {
    const probe = net.createConnection(socket);
    probe.setTimeout(1000, () => { probe.destroy(); reject(Error('Socket probe timed out')); });
    probe.once('connect', () => { probe.destroy(); resolve(true); });
    probe.once('error', error => error.code === 'ECONNREFUSED' || error.code === 'ENOENT' ? resolve(false) : reject(error));
  });
  if (live) throw Error(`Scheduler already listening on ${socket}`);
  unlinkSync(socket);
}
await Promise.all([
  new Promise((resolve, reject) => { local.once('error', reject); local.listen(socket, () => { chmodSync(socket, 0o600); resolve(); }); }),
  new Promise((resolve, reject) => { web.once('error', reject); web.listen(port, '0.0.0.0', resolve); }),
]);
console.log(`agent-scheduler listening on https://0.0.0.0:${port}`);
ticker = setInterval(safeTick, 5000);
safeTick();
function shutdown() {
  if (shuttingDown) return;
  shuttingDown = true;
  clearInterval(ticker);
  for (const run of active.values()) run.stop('Service shutting down');
  local.close();
  web.close();
  local.closeAllConnections();
  web.closeAllConnections();
}
process.on('SIGTERM', shutdown);
process.on('SIGINT', shutdown);
