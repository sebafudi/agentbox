import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import https from 'node:https';
import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, openSync, closeSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
import { cleanupRunUnit } from './run-unit.mjs';

const root = dirname(fileURLToPath(import.meta.url));
function request(socketPath, path, body) {
  return new Promise((resolve, reject) => {
    const req = http.request({ socketPath, path, method: body ? 'POST' : 'GET', timeout: 5000 }, res => {
      let text = '';
      res.setEncoding('utf8');
      res.on('data', chunk => { text += chunk; });
      res.on('error', reject);
      res.on('end', () => {
        try { resolve({ status: res.statusCode, data: JSON.parse(text) }); }
        catch (error) { reject(error); }
      });
    });
    req.on('timeout', () => req.destroy(Error('test request timed out')));
    req.on('error', reject);
    req.end(body ? JSON.stringify(body) : undefined);
  });
}

async function daemon(dir, socket, port = 0) {
  const child = spawn(process.execPath, [join(root, 'server.mjs')], {
    cwd: root, env: { ...process.env, SCHEDULER_DATA_DIR: dir, SCHEDULER_SOCKET: socket, SCHEDULER_PORT: String(port) }, stdio: ['ignore', 'pipe', 'pipe'],
  });
  let diagnostic = '';
  child.stderr.on('data', chunk => { diagnostic += chunk; });
  const ended = new Promise(resolve => child.once('exit', resolve));
  try {
    await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(Error(`test daemon startup timed out: ${diagnostic}`)), 15000);
      child.once('error', error => { clearTimeout(timeout); reject(error); });
      child.once('exit', code => { clearTimeout(timeout); reject(Error(`test daemon exited ${code}: ${diagnostic}`)); });
      child.stdout.on('data', chunk => {
        if (String(chunk).includes('agent-scheduler listening')) { clearTimeout(timeout); resolve(); }
      });
    });
  } catch (error) { child.kill('SIGKILL'); await ended; throw error; }
  const stop = async () => { child.kill('SIGTERM'); await ended; };
  stop.crash = async () => { child.kill('SIGKILL'); await ended; };
  return stop;
}

function writerLock(path) {
  const fd = openSync(path, 'r');
  try { execFileSync('flock', ['--nonblock', '3'], { stdio: ['ignore', 'pipe', 'pipe', fd] }); }
  catch (error) { closeSync(fd); throw error; }
  return fd;
}

test('retention defers locked transcripts across restart and deletion refuses open continuations', { timeout: 25000 }, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'scheduler-retention-'));
  const socket = join(dir, 'api.sock');
  const locks = [];
  let stop;
  try {
    mkdirSync(join(dir, 'logs'));
    execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', join(dir, 'tls.key'), '-out', join(dir, 'tls.crt'), '-subj', '/CN=localhost', '-days', '1'], { stdio: 'ignore' });
    const runs = Array.from({ length: 201 }, (_, index) => {
      const id = `${1000000 - index}-1234abcd`;
      return { id, jobId: 'locked-job', runner: 'omp', status: 'success', startedAt: '2026-01-01T00:00:00.000Z', log: join(dir, 'logs', `${id}.log`) };
    });
    const [retained, pruned] = [runs[0], runs[200]];
    for (const run of [retained, pruned]) {
      writeFileSync(run.log, 'output');
      mkdirSync(run.log.replace(/\.log$/, '.session'));
      writeFileSync(join(run.log.replace(/\.log$/, '.session'), 'transcript.jsonl'), 'preserve this transcript');
      locks.push(writerLock(run.log));
    }
    const job = { id: 'locked-job', name: 'Locked job', schedule: '0 9 * * *', timezone: 'UTC', prompt: 'test', cwd: dir, runner: 'omp', model: '', timeoutMinutes: 1, enabled: false, permissionMode: 'dontAsk', nextRun: null };
    writeFileSync(join(dir, 'state.json'), JSON.stringify({ jobs: [job], runs, sessions: [], password: null }));
    stop = await daemon(dir, socket);
    assert.equal((await request(socket, '/snapshot')).data.runs.length, 200);
    assert.equal(existsSync(pruned.log), true);
    assert.equal(existsSync(pruned.log.replace(/\.log$/, '.session')), true);
    assert.equal((await request(socket, '/jobs/delete', { id: job.id })).status, 409);
    assert.equal((await request(socket, '/snapshot')).data.jobs.length, 1);
    closeSync(locks.shift());
    // Even after the retained writer closes, the pruned writer protects its job.
    assert.equal((await request(socket, '/jobs/delete', { id: job.id })).status, 409);
    assert.equal((await request(socket, '/jobs/patch', { id: job.id, changes: { model: 'kept-across-restart' } })).status, 200);
    await stop(); stop = undefined;
    const persisted = JSON.parse(readFileSync(join(dir, 'state.json'), 'utf8'));
    assert.deepEqual(persisted.pendingCleanup, [{ id: pruned.id, jobId: job.id }]);
    stop = await daemon(dir, socket);
    assert.equal(existsSync(pruned.log), true);
    assert.equal((await request(socket, '/snapshot')).data.jobs[0].model, 'kept-across-restart');
    closeSync(locks.shift());
    const deadline = Date.now() + 7000;
    while (existsSync(pruned.log) && Date.now() < deadline) await delay(50);
    assert.equal(existsSync(pruned.log), false, 'tick must clean after unlock without a new completion');
    assert.equal(existsSync(pruned.log.replace(/\.log$/, '.session')), false);
    assert.deepEqual(JSON.parse(readFileSync(join(dir, 'state.json'), 'utf8')).pendingCleanup, []);
    assert.equal((await request(socket, '/jobs/delete', { id: job.id })).status, 200);
    assert.equal(existsSync(retained.log), false);
    assert.equal(existsSync(retained.log.replace(/\.log$/, '.session')), false);
    assert.equal((await request(socket, '/snapshot')).data.jobs.length, 0);
  } finally {
    for (const fd of locks) closeSync(fd);
    if (stop) await stop();
    rmSync(dir, { recursive: true, force: true });
  }
});

test('follow waits rather than spinning when live UTF-8 cursor cannot advance', { timeout: 5000 }, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'scheduler-follow-'));
  const socket = join(dir, 'api.sock');
  const received = [];
  const server = http.createServer((req, res) => {
    received.push(Date.now());
    const done = received.length === 3;
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ run: { id: 'partial', status: done ? 'success' : 'running' }, output: done ? '€' : '', offset: done ? 3 : 0, size: done ? 3 : 1 }));
  });
  let child;
  try {
    await new Promise(resolve => server.listen(socket, resolve));
    child = spawn(process.execPath, [join(root, 'scheduler.mjs'), 'follow', 'partial'], { env: { ...process.env, SCHEDULER_SOCKET: socket }, stdio: ['ignore', 'pipe', 'pipe'] });
    let output = '';
    child.stdout.setEncoding('utf8');
    child.stdout.on('data', chunk => { output += chunk; });
    child.stderr.resume();
    const code = await new Promise((resolve, reject) => { child.once('error', reject); child.once('exit', resolve); });
    assert.equal(code, 0);
    assert.equal(output, '€');
    assert.equal(received.length, 3);
    assert.ok(received[1] - received[0] >= 500);
    assert.ok(received[2] - received[1] >= 500);
  } finally {
    child?.kill('SIGKILL');
    await new Promise(resolve => server.close(resolve));
    rmSync(dir, { recursive: true, force: true });
  }
});

test('restart reconciles owned escaped descendants before accepting another manual run', { timeout: 45000 }, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'scheduler-restart-'));
  const socket = join(dir, 'api.sock');
  let stop;
  try {
    execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', join(dir, 'tls.key'), '-out', join(dir, 'tls.crt'), '-subj', '/CN=localhost', '-days', '1'], { stdio: 'ignore' });
    stop = await daemon(dir, socket);
    const code = `const {spawn}=require('node:child_process'); const c=spawn(process.execPath,['-e',\"process.on('SIGTERM',()=>{});setInterval(()=>{},100)\"],{detached:true,stdio:'ignore'});require('node:fs').writeFileSync('escaped-pid',String(c.pid));setInterval(()=>{},100);`;
    const job = { id: 'fake-watcher', name: 'Fake watcher', schedule: '*/10 * * * *', timezone: 'UTC', cwd: dir, runner: 'command', argv: [process.execPath, '-e', code], timeoutMinutes: 1, enabled: false };
    assert.equal((await request(socket, '/jobs', job)).status, 200);
    const started = await request(socket, '/run', { id: job.id });
    assert.equal(started.status, 200);
    const deadline = Date.now() + 5000;
    while (!existsSync(join(dir, 'escaped-pid')) && Date.now() < deadline) await delay(25);
    assert.equal(existsSync(join(dir, 'escaped-pid')), true);
    const pid = readFileSync(join(dir, 'escaped-pid'), 'utf8');
    const persisted = JSON.parse(readFileSync(join(dir, 'state.json'), 'utf8'));
    assert.deepEqual(persisted.ownedUnits, [{ unit: started.data.run.unit, runId: started.data.run.id }]);
    assert.equal((await request(socket, '/run', { id: job.id })).status, 409);
    await stop.crash(); stop = undefined;
    stop = await daemon(dir, socket);
    try { assert.match(readFileSync(`/proc/${pid}/stat`, 'utf8'), /\) [ZX] /); }
    catch (error) { if (error.code !== 'ENOENT') throw error; }
    const snapshot = (await request(socket, '/snapshot')).data;
    assert.equal(snapshot.runs[0].status, 'failed');
    assert.equal(snapshot.runs[0].outcome, 'interrupted');
    assert.equal(snapshot.jobs[0].enabled, false);
    assert.deepEqual(snapshot.jobs[0].argv, job.argv);
    assert.deepEqual(JSON.parse(readFileSync(join(dir, 'state.json'), 'utf8')).ownedUnits, []);
    await request(socket, '/jobs/patch', { id: job.id, changes: { argv: [process.execPath, '-e', "console.log(JSON.stringify({outcome:'idle'}))"] } });
    assert.equal((await request(socket, '/run', { id: job.id })).status, 200);
    const doneDeadline = Date.now() + 5000;
    let completed;
    do { completed = (await request(socket, '/snapshot')).data.runs[0]; if (completed.status === 'running') await delay(25); }
    while (completed.status === 'running' && Date.now() < doneDeadline);
    assert.equal(completed.status, 'success');
    assert.equal(completed.outcome, 'idle');
  } finally {
    if (stop) await stop();
    if (existsSync(join(dir, 'state.json'))) {
      for (const owned of JSON.parse(readFileSync(join(dir, 'state.json'), 'utf8')).ownedUnits || []) await cleanupRunUnit(owned.unit);
    }
    rmSync(dir, { recursive: true, force: true });
  }
});

test('uncertain persisted ownership fails closed before API or catch-up launch', { timeout: 10000 }, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'scheduler-closed-'));
  const socket = join(dir, 'api.sock');
  try {
    execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', join(dir, 'tls.key'), '-out', join(dir, 'tls.crt'), '-subj', '/CN=localhost', '-days', '1'], { stdio: 'ignore' });
    const state = { jobs: [], runs: [], sessions: [], password: null, pendingCleanup: [], ownedUnits: [{ unit: 'unrelated-owner.service', runId: 'old' }] };
    writeFileSync(join(dir, 'state.json'), JSON.stringify(state));
    await assert.rejects(daemon(dir, socket), /Invalid owned run unit/);
    assert.equal(existsSync(socket), false);
    assert.deepEqual(JSON.parse(readFileSync(join(dir, 'state.json'), 'utf8')), state);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test('workflow metadata reaches snapshots and authenticated history but never public status', { timeout: 30000 }, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'scheduler-workflow-'));
  const socket = join(dir, 'api.sock');
  const reservation = http.createServer();
  let stop;
  try {
    await new Promise(resolve => reservation.listen(0, '127.0.0.1', resolve));
    const port = reservation.address().port;
    await new Promise(resolve => reservation.close(resolve));
    execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', join(dir, 'tls.key'), '-out', join(dir, 'tls.crt'), '-subj', '/CN=localhost', '-days', '1'], { stdio: 'ignore' });
    stop = await daemon(dir, socket, port);
    const web = (path, body, cookie) => new Promise((resolve, reject) => {
      // This isolated loopback server uses the throwaway certificate above.
      const req = https.request({ hostname: '127.0.0.1', port, path, rejectUnauthorized: false,
        method: body ? 'POST' : 'GET', timeout: 5000,
        headers: { Origin: `https://127.0.0.1:${port}`, 'Content-Type': 'application/json', ...(cookie ? { Cookie: cookie } : {}) } }, res => {
        let text = '';
        res.setEncoding('utf8');
        res.on('data', chunk => { text += chunk; });
        res.on('error', reject);
        res.on('end', () => {
          try { resolve({ status: res.statusCode, data: JSON.parse(text), headers: res.headers }); }
          catch (error) { reject(error); }
        });
      });
      req.on('timeout', () => req.destroy(Error('test web request timed out')));
      req.on('error', reject);
      req.end(body ? JSON.stringify(body) : undefined);
    });
    const workflow = { outcome: 'no_finding', phase: 'finish', scope: 'private-review-scope',
      githubUrls: ['https://github.com/example/private-repo/issues/42'], blocker: null };
    const code = `if(process.env.SCHEDULER_JOB_ID!=='workflow-job')process.exit(8); console.log(JSON.stringify({outcome:'processed',reason:'Review complete',workflow:{...${JSON.stringify(workflow)},runId:process.env.SCHEDULER_RUN_ID}}));`;
    const job = { id: 'workflow-job', name: 'Workflow job', schedule: '0 3 * * *', timezone: 'UTC',
      cwd: dir, runner: 'command', argv: [process.execPath, '-e', code], timeoutMinutes: 1, enabled: false };
    assert.equal((await request(socket, '/jobs', job)).status, 200);
    const complete = async () => {
      const started = await request(socket, '/run', { id: job.id });
      assert.equal(started.status, 200);
      const deadline = Date.now() + 7000;
      let run;
      do {
        run = (await request(socket, '/snapshot')).data.runs.find(run => run.id === started.data.run.id);
        if (run.status === 'running') await delay(25);
      } while (run.status === 'running' && Date.now() < deadline);
      assert.notEqual(run.status, 'running');
      return run;
    };
    const success = await complete();
    assert.equal(success.status, 'success', 'a positive diagnostic must not fail the run');
    assert.equal(success.outcome, 'processed');
    assert.equal(success.reason, 'Review complete');
    assert.deepEqual(success.workflow, { ...workflow, runId: success.id });
    const blocked = { ...workflow, outcome: 'incomplete', phase: 'verification', blocker: 'Private check unfinished' };
    const failureCode = `console.log(JSON.stringify({outcome:'failure',reason:'Verification incomplete',workflow:{...${JSON.stringify(blocked)},runId:process.env.SCHEDULER_RUN_ID}}));`;
    assert.equal((await request(socket, '/jobs/patch', { id: job.id, changes: { argv: [process.execPath, '-e', failureCode] } })).status, 200);
    const failed = await complete();
    assert.equal(failed.exitCode, 0);
    assert.equal(failed.status, 'failed', 'an explicit failure must fail even at exit zero');
    assert.equal(failed.outcome, 'failure');
    assert.deepEqual(failed.workflow, { ...blocked, runId: failed.id });
    await stop(); stop = undefined;
    // Frequent idle checks must not hide a less frequent job beyond the old
    // hundred-run web window while its history is still retained.
    const saved = JSON.parse(readFileSync(join(dir, 'state.json'), 'utf8'));
    saved.runs.find(run => run.id === success.id).jobId = 'infrequent-job';
    saved.runs.splice(1, 0, ...Array.from({ length: 100 }, (_, index) => ({
      id: `idle-check-${index}`, jobId: job.id, jobName: job.name, runner: 'command',
      status: 'success', outcome: 'idle', startedAt: failed.startedAt,
      log: join(dir, `idle-check-${index}.log`),
    })));
    writeFileSync(join(dir, 'state.json'), JSON.stringify(saved));
    stop = await daemon(dir, socket, port);
    assert.deepEqual((await request(socket, '/snapshot')).data.runs[0].workflow, failed.workflow);
    assert.deepEqual((await web('/api/status')).data, { jobs: [{ name: job.name, status: 'failed' }] });
    assert.equal((await web('/api/runs')).status, 401);
    assert.equal((await web('/api/jobs')).status, 401);
    assert.equal((await request(socket, '/password', { password: 'isolated-workflow-password' })).status, 200);
    const login = await web('/api/login', { password: 'isolated-workflow-password' });
    assert.equal(login.status, 200);
    const cookie = login.headers['set-cookie'][0].split(';')[0];
    const history = (await web('/api/runs', undefined, cookie)).data.runs;
    assert.deepEqual(history[0].workflow, failed.workflow);
    const infrequent = history.find(run => run.id === success.id);
    assert.equal(infrequent?.jobId, 'infrequent-job', 'retained infrequent history must survive frequent idle checks');
    assert.deepEqual(infrequent.workflow, success.workflow);
    assert.deepEqual((await web('/api/jobs', undefined, cookie)).data.jobs[0].lastRun.workflow, failed.workflow);
    assert.deepEqual((await web('/api/status', undefined, cookie)).data, { jobs: [{ name: job.name, status: 'failed' }] });
  } finally {
    if (reservation.listening) await new Promise(resolve => reservation.close(resolve));
    if (stop) await stop();
    rmSync(dir, { recursive: true, force: true });
  }
});
