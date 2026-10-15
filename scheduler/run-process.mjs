import { spawn } from 'node:child_process';
import { openSync, writeSync, closeSync, readSync, fstatSync, writeFileSync, mkdirSync, rmSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
import { StringDecoder } from 'node:string_decoder';
import { cleanupRunUnit, newRunUnit, runUnitArgs, stopRunUnit, readRunUnit } from './run-unit.mjs';

export const maxLogBytes = 2 * 1024 * 1024;
export const logTailBytes = 20_000;

export function readLog(file, offset, final = true) {
  let fd;
  try { fd = openSync(file, 'r'); }
  catch (error) { if (error.code === 'ENOENT') return { output: '', offset: 0 }; throw error; }
  try {
    const size = fstatSync(fd).size;
    const start = offset === undefined ? Math.max(0, size - logTailBytes) : Math.min(offset, size);
    const bytes = Buffer.allocUnsafe(Math.min(logTailBytes, size - start));
    const count = readSync(fd, bytes, 0, bytes.length, start);
    // A live EOF is temporary: retain its incomplete suffix for the next poll.
    let end = count;
    if (!final || start + count < size) {
      let lead = count - 1;
      while (lead >= 0 && (bytes[lead] & 0xc0) === 0x80) lead--;
      if (lead >= 0) {
        const value = bytes[lead];
        const width = value >= 0xf0 && value <= 0xf4 ? 4 : value >= 0xe0 && value <= 0xef ? 3 : value >= 0xc2 && value <= 0xdf ? 2 : 1;
        if (lead + width > count) end = lead;
      }
    }
    return { output: bytes.subarray(0, end).toString('utf8'), offset: start + end, size };
  } finally { closeSync(fd); }
}

const reportOutcomes = new Set(['idle', 'skipped', 'processed', 'success', 'failure']);

function workflowText(value, limit) {
  return typeof value === 'string' ? value.slice(0, limit).replace(/[\u0000-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/g, ' ').trim() : '';
}

function workflowReport(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const runId = workflowText(value.runId, 128);
  const outcome = workflowText(value.outcome, 64);
  const phase = workflowText(value.phase, 64);
  if (!runId || !outcome || !phase) return null;
  const githubUrls = [];
  if (Array.isArray(value.githubUrls)) {
    for (const link of value.githubUrls) {
      if (typeof link !== 'string' || link.length > 2048) continue;
      // Publication URLs only: no credentials, queries, fragments or lookalike hosts.
      if (!/^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/(?:pull|issues)\/[1-9]\d*$/.test(link)) continue;
      if (!githubUrls.includes(link)) githubUrls.push(link);
      if (githubUrls.length === 10) break;
    }
  }
  return { runId, outcome, phase, scope: workflowText(value.scope, 512) || null,
    githubUrls, blocker: workflowText(value.blocker, 4096) || null };
}

export function parseRunReport(stdout) {
  // Only an explicit final JSON object is a report; ordinary watcher summaries
  // and arbitrary agent text never imply that work was idle or processed.
  const text = stdout.trimEnd();
  if (!text.endsWith('}')) return null;
  let depth = 0;
  let quoted = false;
  let finalStart = -1;
  // Scan backward from the final object so preceding arbitrary log text,
  // including unmatched quotes/braces, cannot poison a valid report.
  for (let index = text.length - 1; index >= 0; index--) {
    const char = text[index];
    if (char === '"') {
      let slashes = 0;
      for (let previous = index - 1; previous >= 0 && text[previous] === '\\'; previous--) slashes++;
      if (slashes % 2 === 0) quoted = !quoted;
      continue;
    }
    if (quoted) continue;
    if (char === '}') depth++;
    else if (char === '{') {
      depth--;
      if (depth === 0) { finalStart = index; break; }
    }
  }
  if (finalStart < 0) return null;
  try {
    const report = JSON.parse(text.slice(finalStart));
    const outcome = report.outcome ?? report.status;
    if (!reportOutcomes.has(outcome)) return null;
    const workflow = workflowReport(report.workflow);
    return { outcome, ...(typeof report.reason === 'string' ? { reason: report.reason.slice(0, 4096) } : {}),
      ...(workflow ? { workflow } : {}) };
  } catch { return null; }
}

// The daemon persists ownership of unit before calling this function. Detached
// sessions stay in its cgroup; no owner-wide process matching or shell is used.
export function startProcess({ command, args, cwd, env, prompt, promptFile, log, timeoutMs, unit = newRunUnit(), report = false, onFinish, onContainmentFailure }) {
  let fd;
  let child;
  let reason;
  let timeout;
  let stopping;
  let stopDeadline;
  let written = 0;
  let settled = false;
  let stdout = '';
  const decoder = new StringDecoder('utf8');
  let childClosed = false;
  let stoppingNow = false;
  const stop = why => {
    if (settled) return;
    reason ||= why;
    if (!stopping) {
      child?.stdin.end();
      stopDeadline = setTimeout(() => {
        childClosed = true;
        child?.kill('SIGKILL');
        child?.stdin.destroy();
        child?.stdout.destroy();
        child?.stderr.destroy();
        void complete(null, null, true);
      }, 8000);
      const attempt = async () => {
        if (settled || stoppingNow) return;
        stoppingNow = true;
        try { await stopRunUnit(unit); } catch {}
        finally { stoppingNow = false; }
        if (!settled) stopping = setTimeout(attempt, 250);
      };
      stopping = setTimeout(attempt, 0);
    }
  };
  const complete = async (code, signal, cleanupUncertain = false) => {
    if (settled) return;
    settled = true;
    clearTimeout(timeout);
    clearTimeout(stopping);
    clearTimeout(stopDeadline);
    let clean = false;
    let details;
    try {
      if (cleanupUncertain) throw Error('Bounded stop deadline expired; retained unit ownership for restart reconciliation');
      details = await cleanupRunUnit(unit);
      clean = true;
    } catch (error) {
      reason = `${reason ? `${reason}; ` : ''}Containment cleanup failed: ${error.message}`;
      onContainmentFailure?.(reason);
    }
    if (details?.Result === 'timeout') reason ||= 'Runtime limit exceeded';
    if (details?.ExecMainCode === '1' && /^\d+$/.test(details.ExecMainStatus)) code = Number(details.ExecMainStatus);
    if (signal) reason ||= `Terminated by ${signal}`;
    const result = report ? parseRunReport(stdout + decoder.end()) : null;
    if (result?.outcome === 'failure') reason ||= result.reason || 'Command reported failure';
    if (code !== 0) reason ||= `Runner exited with code ${code ?? 'unknown'}`;
    const outcome = reason?.startsWith('Runtime limit exceeded') ? 'timeout'
      : reason?.startsWith('Cancelled by owner') ? 'cancelled'
      : reason?.startsWith('Service shutting down') ? 'interrupted'
      : reason || code !== 0 ? 'failure' : result?.outcome || 'success';
    if (fd !== undefined) {
      try { closeSync(fd); } catch (error) { reason ||= `Log close failed: ${error.message}`; }
      fd = undefined;
    }
    if (promptFile && clean) {
      try { rmSync(promptFile, { force: true }); } catch (error) { reason ||= `Prompt cleanup failed: ${error.message}`; }
    }
    onFinish(code, reason, { ...result, ...(reason ? { reason } : {}), outcome: reason && !['timeout', 'cancelled', 'interrupted'].includes(outcome) ? 'failure' : outcome, clean, unit });
  };
  const output = chunk => {
    if (fd === undefined || written >= maxLogBytes) return;
    try {
      const limit = Math.min(chunk.length, maxLogBytes - written);
      let offset = 0;
      while (offset < limit) offset += writeSync(fd, chunk, offset, limit - offset);
      written += limit;
      if (written === maxLogBytes) writeSync(fd, '\n[output truncated at 2 MiB]\n');
    } catch (error) { stop(`Log write failed: ${error.message}`); }
  };
  try {
    fd = openSync(log, 'wx', 0o600);
    if (promptFile) {
      mkdirSync(dirname(promptFile), { recursive: true, mode: 0o700 });
      writeFileSync(promptFile, prompt, { flag: 'wx', mode: 0o600 });
    }
    const gate = join(dirname(fileURLToPath(import.meta.url)), 'run-exec.mjs');
    child = spawn('systemd-run', runUnitArgs({ unit, command: process.execPath, args: [gate, command, ...args], cwd, env, timeoutMs }), { cwd, env, stdio: ['pipe', 'pipe', 'pipe'] });
    child.stdout.on('data', chunk => {
      if (report) stdout = (stdout + decoder.write(chunk)).slice(-64 * 1024);
      output(chunk);
    });
    child.stderr.on('data', output);
    child.on('error', error => { reason ||= `Spawn failed: ${error.message}`; });
    child.on('close', (code, signal) => { childClosed = true; void complete(code, signal); });
    child.stdin.on('error', error => {
      if (error.code !== 'EPIPE') stop(`Prompt delivery failed: ${error.message}`);
    });
    const releaseGate = async () => {
      const deadline = Date.now() + 5000;
      try {
        while (!childClosed && !reason) {
          const current = await readRunUnit(unit);
          if (childClosed || reason) return;
          if (current.LoadState === 'loaded' && ['active', 'activating'].includes(current.ActiveState)) {
            child.stdin.end(`scheduler-owned-unit-ready\n${promptFile ? '' : prompt || ''}`);
            return;
          }
          if (Date.now() >= deadline) throw Error('Owned unit registration timed out');
          await delay(50);
        }
      } catch (error) { if (!childClosed) stop(`Launch gate failed: ${error.message}`); }
    };
    void releaseGate();
    timeout = setTimeout(() => stop('Runtime limit exceeded'), timeoutMs);
  } catch (error) {
    reason = `Launch failed: ${error.message}`;
    if (child) stop(reason);
    else queueMicrotask(() => { void complete(null, null); });
  }
  return { stop, unit, get pid() { return child?.pid; } };
}
