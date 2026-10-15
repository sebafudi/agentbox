import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, writeFileSync, appendFileSync, readFileSync, mkdirSync, existsSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { startProcess, readLog, maxLogBytes, logTailBytes, parseRunReport } from './run-process.mjs';
import { continuation, terminalText } from './continuation.mjs';
import { setTimeout as delay } from 'node:timers/promises';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

function directory(t) {
  const dir = mkdtempSync(join(tmpdir(), 'scheduler-test-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}
function execute(dir, code, overrides = {}) {
  let handle;
  const done = new Promise(resolve => {
    handle = startProcess({ command: process.execPath, args: ['-e', code], cwd: dir, env: process.env, prompt: '', log: join(dir, 'run.log'), timeoutMs: 5000, ...overrides, onFinish: (code, reason, result) => resolve({ code, reason, ...result }) });
  });
  return { done, get handle() { return handle; } };
}

test('log tail is bounded and byte cursors drain output without UTF-8 splits', t => {
  const dir = directory(t);
  const path = join(dir, 'output');
  const text = 'hello € world\n'.repeat(4000);
  writeFileSync(path, text);
  assert.ok(Buffer.byteLength(readLog(path).output) <= logTailBytes + 3);
  let offset = 0;
  let output = '';
  do {
    const chunk = readLog(path, offset);
    assert.ok(chunk.offset > offset);
    output += chunk.output;
    offset = chunk.offset;
  } while (offset < Buffer.byteLength(text));
  assert.equal(output, text);
});

test('live log cursor retains a split character at temporary EOF and final EOF drains invalid bytes', t => {
  const dir = directory(t);
  const path = join(dir, 'output');
  writeFileSync(path, Buffer.from([0x61, 0xe2]));
  const first = readLog(path, 0, false);
  assert.deepEqual(first, { output: 'a', offset: 1, size: 2 });
  assert.deepEqual(readLog(path, first.offset, false), { output: '', offset: 1, size: 2 });
  appendFileSync(path, Buffer.from([0x82]));
  assert.deepEqual(readLog(path, first.offset, false), { output: '', offset: 1, size: 3 });
  appendFileSync(path, Buffer.from([0xac]));
  assert.deepEqual(readLog(path, first.offset, false), { output: '€', offset: 4, size: 4 });
  appendFileSync(path, Buffer.from([0xf0, 0x9f]));
  assert.deepEqual(readLog(path, 4, false), { output: '', offset: 4, size: 6 });
  assert.deepEqual(readLog(path, 4, true), { output: '�', offset: 6, size: 6 });
});

test('terminal logs remove OSC, CSI and unsafe controls while preserving readable spacing', () => {
  assert.equal(terminalText('\x1b]52;c;c2VjcmV0\x07a\x1b[2Jb\x00\t\r\n'), 'ab\t\r\n');
});

test('spawn and log-open failure settle without crashing the caller', async t => {
  const dir = directory(t);
  const missing = execute(dir, '', { command: join(dir, 'missing') });
  const failed = await missing.done;
  assert.notEqual(failed.code, 0);
  assert.match(failed.reason, /Runner exited/);
  assert.match(readLog(join(dir, 'run.log')).output, /Spawn failed/);
  const blocked = execute(dir, '', { log: dir });
  assert.match((await blocked.done).reason, /Launch failed/);
});

test('timeout remains a failure reason when runner handles TERM and exits zero', async t => {
  const dir = directory(t);
  const run = execute(dir, "process.on('SIGTERM', () => process.exit(0)); setInterval(() => {}, 100);", { timeoutMs: 1000 });
  const result = await run.done;
  assert.equal(result.code, 0);
  assert.equal(result.reason, 'Runtime limit exceeded');
});

test('cancellation and SDK diagnostics keep explicit failure reasons', async t => {
  const dir = directory(t);
  const run = execute(dir, "process.on('SIGTERM', () => process.exit(0)); require('node:fs').writeFileSync('ready', ''); setInterval(() => {}, 100);");
  while (!existsSync(join(dir, 'ready'))) await delay(25);
  run.handle.stop('Cancelled by owner');
  const cancelled = await run.done;
  assert.equal(cancelled.reason, 'Cancelled by owner');
  assert.equal(cancelled.outcome, 'cancelled');
  const sdk = execute(dir, `console.log(JSON.stringify({outcome:'failure',reason:'SDK error: result_error'})); process.exitCode=1;`, { report: true, log: join(dir, 'sdk.log') });
  assert.equal((await sdk.done).reason, 'SDK error: result_error');
});

test('runner output is capped and accepted large file prompts survive intact', async t => {
  const dir = directory(t);
  const prompt = '€'.repeat(100_000);
  const promptFile = join(dir, 'session', 'prompt.txt');
  const code = `const fs = require('node:fs'); if (fs.readFileSync(process.argv[1], 'utf8').length !== 100000) process.exit(2); process.stdout.write('x'.repeat(${maxLogBytes + 10000}));`;
  const run = execute(dir, code, { args: ['-e', code, promptFile], prompt, promptFile });
  assert.equal((await run.done).code, 0);
  assert.equal(existsSync(promptFile), false);
  assert.ok(statSync(join(dir, 'run.log')).size <= maxLogBytes + 100);
  assert.match(readLog(join(dir, 'run.log')).output, /output truncated/);
});

test('leader exit kills descendants that keep inherited output pipes open', async t => {
  const dir = directory(t);
  const code = "const {spawn}=require('node:child_process'); const child=spawn(process.execPath,['-e','setInterval(()=>{},100)'],{stdio:['ignore',1,2]}); require('node:fs').writeFileSync('pid',String(child.pid)); child.unref();";
  const result = await execute(dir, code).done;
  assert.equal(result.code, 0);
  const pid = readFileSync(join(dir, 'pid'), 'utf8');
  try {
    const status = readFileSync(`/proc/${pid}/stat`, 'utf8');
    assert.match(status, /\) [ZX] /, 'descendant must be dead or awaiting reap');
  } catch (error) { if (error.code !== 'ENOENT') throw error; }
});

test('continuation never guesses a missing, live, or ambiguous session', async t => {
  const dir = directory(t);
  await assert.rejects(continuation({ status: 'running' }), /still active/);
  await assert.rejects(continuation({ status: 'success', runner: 'command', cwd: dir }), /no valid saved agent session/);
  await assert.rejects(continuation({ status: 'success' }), /working directory/);
  await assert.rejects(continuation({ status: 'success', cwd: dir, runner: 'claude-print' }), /session ID/);
  const sessionDir = join(dir, 'session');
  mkdirSync(sessionDir);
  const run = { status: 'success', cwd: dir, runner: 'omp', sessionDir };
  await assert.rejects(continuation(run), /found 0/);
  writeFileSync(join(sessionDir, 'selected.jsonl'), '{}\n');
  assert.deepEqual(await continuation(run), { command: 'omp', args: ['--allow-home', '--session-dir', sessionDir, '--resume', join(sessionDir, 'selected.jsonl')] });
  writeFileSync(join(sessionDir, 'other.jsonl'), '{}\n');
  await assert.rejects(continuation(run), /found 2/);
});

test('explicit bounded final reports are conservative and tolerate preceding logs', () => {
  assert.equal(parseRunReport('{"watched": [], "pending": [], "uncertain": []}'), null);
  assert.equal(parseRunReport('{"outcome":"unknown"}'), null);
  assert.deepEqual(parseRunReport('log with unmatched " and {\\n' + JSON.stringify({ outcome: 'idle', detail: 'escaped " }' })), { outcome: 'idle' });
  assert.deepEqual(parseRunReport('log\n{\n "status": "skipped"\n}\n'), { outcome: 'skipped' });
});

test('workflow reports retain only bounded text and safe publication links', () => {
  const workflow = { runId: 'run-123', outcome: 'blocked', phase: 'verification', scope: 'private scope',
    githubUrls: ['https://github.com/example/repo/pull/123'], blocker: 'Required check still running' };
  assert.deepEqual(parseRunReport(JSON.stringify({ outcome: 'failure', workflow, secret: 'discard' })),
    { outcome: 'failure', workflow });
  const oversized = parseRunReport(JSON.stringify({ outcome: 'processed', reason: 'Completed normally',
    workflow: { ...workflow, runId: 'r'.repeat(200), outcome: 'o'.repeat(100), phase: 'p'.repeat(100),
      scope: '\u001bsecret\u202e' + 's'.repeat(600), blocker: 'b'.repeat(5000), secret: 'discard',
      githubUrls: ['javascript:alert(1)', 'https://github.com.evil/repo/pull/1',
        'https://user:password@github.com/example/repo/issues/1',
        'https://github.com/example/repo/issues/1?secret=hidden',
        ...Array.from({ length: 15 }, (_, index) => `https://github.com/example/repo/issues/${index + 1}`)] } }));
  assert.equal(oversized.reason, 'Completed normally');
  assert.equal(oversized.workflow.runId.length, 128);
  assert.equal(oversized.workflow.outcome.length, 64);
  assert.equal(oversized.workflow.phase.length, 64);
  assert.ok(oversized.workflow.scope.length <= 512);
  assert.doesNotMatch(oversized.workflow.scope, /[\u001b\u202e]/);
  assert.equal(oversized.workflow.blocker.length, 4096);
  assert.equal(oversized.workflow.githubUrls.length, 10);
  assert.equal(oversized.workflow.githubUrls[0], 'https://github.com/example/repo/issues/1');
  assert.equal('secret' in oversized.workflow, false);
  for (const invalid of [null, [], 'text', { runId: 'missing-fields' }]) {
    assert.deepEqual(parseRunReport(JSON.stringify({ outcome: 'idle', workflow: invalid })), { outcome: 'idle' });
  }
});

test('workflow completion preserves metadata and failure takes precedence over positive reports', async t => {
  const dir = directory(t);
  const workflow = { runId: 'current-run', outcome: 'no_finding', phase: 'finish', scope: null, githubUrls: [], blocker: null };
  const positive = { outcome: 'processed', reason: 'Review complete', workflow };
  const completed = await execute(dir, `console.log(${JSON.stringify(JSON.stringify(positive))})`, { report: true }).done;
  assert.equal(completed.code, 0);
  assert.equal(completed.outcome, 'processed');
  assert.equal(completed.reason, 'Review complete');
  assert.deepEqual(completed.workflow, workflow);
  const failed = await execute(dir, `console.log(${JSON.stringify(JSON.stringify(positive))}); process.exitCode=7;`,
    { report: true, log: join(dir, 'nonzero.log') }).done;
  assert.equal(failed.code, 7);
  assert.equal(failed.outcome, 'failure');
  assert.match(failed.reason, /Runner exited with code 7/);
  assert.deepEqual(failed.workflow, workflow);
  const blocked = { outcome: 'failure', reason: 'Verification incomplete',
    workflow: { ...workflow, outcome: 'incomplete', phase: 'verification', blocker: 'Background check unfinished' } };
  const reported = await execute(dir, `console.log(${JSON.stringify(JSON.stringify(blocked))})`,
    { report: true, log: join(dir, 'reported.log') }).done;
  assert.equal(reported.outcome, 'failure');
  assert.equal(reported.reason, blocked.reason);
  assert.deepEqual(reported.workflow, blocked.workflow);
});

test('argv is literal, final report survives noisy stdout, and application failure overrides idle', async t => {
  const dir = directory(t);
  const code = `const fs=require('node:fs'); fs.writeFileSync('argv.json', JSON.stringify(process.argv.slice(1))); process.stdout.write('x'.repeat(${maxLogBytes + 1000})); console.log('\\n'+JSON.stringify({outcome:'processed'}));`;
  const args = ['two words', '', '$HOME', 'x;touch forbidden', 'quote"back\\\\slash'];
  const literal = await execute(dir, code, { args: ['-e', code, '--', ...args], report: true }).done;
  assert.equal(literal.code, 0);
  assert.equal(literal.outcome, 'processed');
  assert.deepEqual(JSON.parse(readFileSync(join(dir, 'argv.json'), 'utf8')), args);
  assert.equal(existsSync(join(dir, 'forbidden')), false);
  const failure = await execute(dir, `console.log(JSON.stringify({outcome:'idle'})); process.exitCode=7;`, { report: true, log: join(dir, 'failure.log') }).done;
  assert.equal(failure.code, 7);
  assert.equal(failure.outcome, 'failure');
});

test('control group kills detached setsid descendants on leader completion and cancellation', { timeout: 20000 }, async t => {
  const dir = directory(t);
  const childCode = "process.on('SIGTERM',()=>{});setInterval(()=>{},100)";
  for (const cancel of [false, true]) {
    const pidFile = cancel ? 'cancel-pid' : 'exit-pid';
    const code = `const {spawn}=require('node:child_process'); const c=spawn(process.execPath,['-e',${JSON.stringify(childCode)}],{detached:true,stdio:'ignore'}); require('node:fs').writeFileSync(${JSON.stringify(pidFile)},String(c.pid)); c.unref(); ${cancel ? 'setInterval(()=>{},100);' : ''}`;
    const run = execute(dir, code, { timeoutMs: 15000, log: join(dir, `${pidFile}.log`) });
    while (!existsSync(join(dir, pidFile))) await delay(25);
    if (cancel) run.handle.stop('Cancelled by owner');
    const result = await run.done;
    assert.equal(result.clean, true);
    const pid = readFileSync(join(dir, pidFile), 'utf8');
    try { assert.match(readFileSync(`/proc/${pid}/stat`, 'utf8'), /\) [ZX] /); }
    catch (error) { if (error.code !== 'ENOENT') throw error; }
  }
});

test('EOF before the owned-unit execution gate never starts the configured workload', async t => {
  const dir = directory(t);
  const child = spawn(process.execPath, [fileURLToPath(new URL('./run-exec.mjs', import.meta.url)), process.execPath, '-e', "require('node:fs').writeFileSync('must-not-run','')"], { cwd: dir, stdio: ['pipe', 'ignore', 'ignore'] });
  child.stdin.end();
  const code = await new Promise((resolve, reject) => { child.once('error', reject); child.once('exit', resolve); });
  assert.notEqual(code, 0);
  assert.equal(existsSync(join(dir, 'must-not-run')), false);
});
