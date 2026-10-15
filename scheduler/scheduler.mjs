#!/usr/bin/env node
import http from 'node:http';
import { readFileSync, openSync, closeSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import { spawn, execFileSync } from 'node:child_process';
import { setTimeout as delay } from 'node:timers/promises';
import { terminalText, continuation } from './continuation.mjs';

const socketPath = process.env.SCHEDULER_SOCKET || `${process.env.XDG_RUNTIME_DIR || `/run/user/${process.getuid()}`}/agent-scheduler.sock`;
function request(method, path, data) {
  return new Promise((resolve, reject) => {
    const deadline = setTimeout(() => req.destroy(Error('scheduler request timed out')), 15_000);
    const req = http.request({ socketPath, method, path, headers: data ? { 'Content-Type': 'application/json' } : {} }, res => {
      let result = '';
      res.on('error', reject);
      res.on('aborted', () => reject(Error('scheduler response interrupted')));
      res.setEncoding('utf8');
      res.on('data', chunk => { result += chunk; });
      res.on('end', () => {
        let payload;
        try { payload = JSON.parse(result); } catch { return reject(Error('invalid response from scheduler')); }
        if (res.statusCode >= 400) reject(Error(payload.error || `HTTP ${res.statusCode}`)); else resolve(payload);
      });
    });
    req.on('error', error => reject(Error(`${error.message}; is agent-scheduler.service running?`)));
    req.on('close', () => clearTimeout(deadline));
    req.end(data ? JSON.stringify(data) : undefined);
  });
}
const [command, action, arg, file] = process.argv.slice(2);
const usage = `Usage:
  scheduler job apply <file.json>  Create or update a job by id
  scheduler job patch <id> <file.json>  Patch only specified configuration fields
  scheduler job list               List job status and next occurrence
  scheduler job show <id>          Show full configuration
  scheduler job pause <id>          Disable future scheduled runs
  scheduler job resume <id>         Enable future scheduled runs
  scheduler job delete <id>         Delete job and its run history (irreversible)
  scheduler job run <id>           Start immediately (regardless of enabled)
  scheduler runs [id]              Show recent runs (optionally filter by job)
  scheduler snapshot              Show consistent jobs and retained run metadata JSON
  scheduler logs <run-id>          Print a capped, terminal-safe log tail
  scheduler follow <run-id>        Observe output until completion (never opens an agent)
  scheduler join <run-id>          Monitor command/live run, or resume completed agent
  scheduler stop <run-id>          Cancel run and its owned control group
  scheduler password generate      Set a random password; print it once
  scheduler password set           Read password from stdin (no echo if typed in a terminal)

Five-field cron expression, IANA timezone. Job JSON: id, name, schedule, timezone,
prompt (agent runners) or argv (command string array), cwd, runner (omp|claude-print|claude-sdk|command); optional model,
timeoutMinutes (default 30), enabled (default true), permissionMode
(dontAsk default|acceptEdits|bypassPermissions), allowedTools (Claude rule array),
additionalDirectories (Claude absolute directory array). Password reset logs out all browsers.`;
async function detail(id, offset) {
  return request('GET', `/runs/${encodeURIComponent(id)}${offset === undefined ? '' : `?offset=${offset}`}`);
}
async function follow(id, initial) {
  let response = initial || await detail(id);
  for (;;) {
    process.stdout.write(terminalText(response.output));
    if (response.run.status !== 'running' && response.offset >= (response.size || 0)) {
      process.stderr.write(`\nRun ${id}: ${response.run.status}${response.run.outcome ? ` / ${response.run.outcome}` : ''}${response.run.reason ? ` (${terminalText(response.run.reason)})` : ''}\n`);
      if (response.run.status !== 'success') process.exitCode = 1;
      return;
    }
    if (!response.output || response.offset >= (response.size || 0)) await delay(750);
    response = await detail(id, response.offset);
  }
}
async function joinRun(id) {
  const first = await detail(id);
  if (first.run.status === 'running') return follow(id, first);
  if (first.run.continuationBlocked) throw Error(first.run.continuationBlocked);
  if (first.run.runner === 'command' && !first.run.continuation) return follow(id, first);
  // Lock the retained log inode. Multiple scheduler joins cannot write one session.
  const fd = openSync(first.run.log, 'r');
  try {
    try { execFileSync('flock', ['--nonblock', '3'], { stdio: ['ignore', 'pipe', 'pipe', fd] }); }
    catch { throw Error('This run is already open in another scheduler join, or flock is unavailable'); }
    const target = await continuation(first.run);
    const latest = await detail(id);
    if (latest.run.status === 'running') return follow(id, latest);
    if (latest.run.continuationBlocked) throw Error(latest.run.continuationBlocked);
    if (latest.run.id !== first.run.id || latest.run.log !== first.run.log || latest.run.runner !== first.run.runner || latest.run.cwd !== first.run.cwd || latest.run.sessionId !== first.run.sessionId || latest.run.sessionDir !== first.run.sessionDir || JSON.stringify(latest.run.continuation) !== JSON.stringify(first.run.continuation)) throw Error('Run continuation metadata changed; try again');
    const code = await new Promise((resolve, reject) => {
      const child = spawn(target.command, target.args, { cwd: latest.run.cwd, stdio: ['inherit', 'inherit', 'inherit', fd] });
      child.once('error', reject);
      child.once('exit', (code, signal) => resolve(code ?? (signal === 'SIGINT' ? 130 : 1)));
    });
    process.exitCode = code;
  } finally { closeSync(fd); }
}
async function main() {
  let response;
  if (command === 'job' && action === 'apply' && arg) response = await request('POST', '/jobs', JSON.parse(readFileSync(arg, 'utf8')));
  else if (command === 'job' && action === 'patch' && arg && file) response = await request('POST', '/jobs/patch', { id: arg, changes: JSON.parse(readFileSync(file, 'utf8')) });
  else if (command === 'snapshot') response = await request('GET', '/snapshot');
  else if (command === 'logs' && action) { process.stdout.write(terminalText((await detail(action)).output)); return; }
  else if (command === 'follow' && action) return follow(action);
  else if (command === 'join' && action) return joinRun(action);
  else if (command === 'stop' && action) response = await request('POST', '/runs/stop', { id: action });
  else if (command === 'job' && action === 'list') {
    response = await request('GET', '/jobs');
    for (const job of response.jobs) console.log(terminalText(`${job.id}\t${job.status === 'running' ? 'running' : job.enabled ? job.status : 'paused'}\t${job.nextRun || '-'}\t${job.name}`));
    return;
  } else if (command === 'job' && action === 'show' && arg) {
    response = (await request('GET', '/jobs')).jobs.find(job => job.id === arg);
    if (!response) throw Error('job not found');
  } else if (command === 'job' && action === 'run' && arg) response = await request('POST', '/run', { id: arg });
  else if (command === 'job' && ['pause', 'resume'].includes(action) && arg) response = await request('POST', '/jobs/state', { id: arg, enabled: action === 'resume' });
  else if (command === 'job' && action === 'delete' && arg) response = await request('POST', '/jobs/delete', { id: arg });
  else if (command === 'runs') {
    response = await request('GET', '/runs');
    for (const run of response.runs.filter(run => !action || run.jobId === action).slice(0, 30)) console.log(terminalText(`${run.id}\t${run.startedAt}\t${run.jobId}\t${run.status}\t${run.outcome || '-'}\t${run.exitCode ?? '-'}\t${run.log}`));
    return;
  } else if (command === 'password' && action === 'generate') {
    const password = randomBytes(24).toString('base64url');
    await request('POST', '/password', { password });
    console.log(`Dashboard password (shown once): ${password}`);
    return;
  } else if (command === 'password' && action === 'set') {
    if (process.stdin.isTTY) {
      const stty = args => execFileSync('stty', args, { stdio: ['inherit', 'pipe', 'pipe'] });
      const restore = () => { stty(['echo']); process.stderr.write('\n'); };
      const interrupted = () => { restore(); process.exit(130); };
      process.once('SIGINT', interrupted);
      process.once('SIGTERM', interrupted);
      stty(['-echo']);
      process.stderr.write('New dashboard password: ');
      try { response = await new Promise((resolve, reject) => {
        process.stdin.once('data', resolve);
        process.stdin.once('end', () => reject(Error('No password received')));
        process.stdin.once('error', reject);
      }); }
      finally {
        restore();
        process.removeListener('SIGINT', interrupted);
        process.removeListener('SIGTERM', interrupted);
        process.stdin.pause();
      }
    } else {
      response = '';
      for await (const chunk of process.stdin) {
        response += chunk;
        if (response.length > 1026) throw Error('password must be 16-1024 characters');
      }
    }
    await request('POST', '/password', { password: String(response).trimEnd() });
    console.log('Dashboard password updated; prior sessions invalidated');
    return;
  } else { console.log(usage); if (command && command !== 'help') process.exitCode = 1; return; }
  console.log(JSON.stringify(response, null, 2));
}
main().catch(error => { console.error(`scheduler: ${terminalText(error.message)}`); process.exitCode = 1; });
