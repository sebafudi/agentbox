import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { execFile, execFileSync } from 'node:child_process';
import { promisify } from 'node:util';
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, rmSync, openSync, closeSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
const exec = promisify(execFile);
const CLI = fileURLToPath(new URL('./scheduler.mjs', import.meta.url));
const SESSION = '12345678-1234-4234-8234-123456789012';

async function fixture(t, change = () => {}) {
  const dir = mkdtempSync(join(tmpdir(), 'scheduler-join-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  mkdirSync(join(dir, 'bin'));
  const log = join(dir, 'run.log');
  writeFileSync(log, 'saved output\n');
  const run = { id: 'fixture-run', jobId: 'fixture', status: 'success', runner: 'command', cwd: dir, log,
    continuation: { kind: 'claude', sessionId: SESSION, cwd: dir } };
  const captured = join(dir, 'captured.json');
  writeFileSync(join(dir, 'bin', 'claude'), `#!${process.execPath}\nimport { writeFileSync } from 'node:fs';\nimport { execFileSync } from 'node:child_process';\nlet locked = false; try { execFileSync('flock', ['--nonblock', ${JSON.stringify(log)}, 'true']); } catch { locked = true; }\nwriteFileSync(${JSON.stringify(captured)}, JSON.stringify({ args: process.argv.slice(2), cwd: process.cwd(), locked }));\n`, { mode: 0o700 });
  // This isolated CLI fixture replaces only transcript lookup, never production code.
  // Its Claude executable is a local recorder, not a model or real session writer.
  const sdk = `export async function getSessionInfo(id, options) { return id === ${JSON.stringify(SESSION)} && options.dir === ${JSON.stringify(dir)} ? { sessionId: id } : undefined; }`;
  const hook = join(dir, 'hook.mjs');
  writeFileSync(hook, `import { registerHooks } from 'node:module'; registerHooks({ resolve(specifier, context, next) { return specifier === '@anthropic-ai/claude-agent-sdk' ? { url: ${JSON.stringify('data:text/javascript,' + encodeURIComponent(sdk))}, shortCircuit: true } : next(specifier, context); } });`);
  let requests = 0;
  const server = http.createServer((req, res) => {
    assert.equal(req.method, 'GET');
    assert.ok(req.url.startsWith('/runs/fixture-run'));
    const current = structuredClone(run);
    change(current, ++requests);
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ run: current, output: 'saved output\n', offset: 13, size: 13 }));
  });
  const socket = join(dir, 'api.sock');
  await new Promise(resolve => server.listen(socket, resolve));
  t.after(() => new Promise(resolve => server.close(resolve)));
  const invoke = async () => {
    try {
      return { code: 0, ...await exec(process.execPath, ['--import', hook, CLI, 'join', run.id], { env: { ...process.env, SCHEDULER_SOCKET: socket, PATH: `${join(dir, 'bin')}:${process.env.PATH}` }, timeout: 10000 }) };
    } catch (error) { return { code: error.code, stdout: error.stdout, stderr: error.stderr }; }
  };
  return { run, invoke, captured };
}

test('wrapped CLI resumes only saved Claude ID/cwd while holding the existing log inode lock', async t => {
  const f = await fixture(t);
  const result = await f.invoke();
  assert.equal(result.code, 0, result.stderr);
  assert.deepEqual(JSON.parse(readFileSync(f.captured, 'utf8')), { args: ['--resume', SESSION], cwd: f.run.cwd, locked: true });
});

test('ordinary completed commands and active wrapped runs remain read-only', async t => {
  for (const active of [false, true]) {
    const f = await fixture(t, (run, count) => {
      if (!active) delete run.continuation;
      else if (count === 1) run.status = 'running';
    });
    const result = await f.invoke();
    assert.equal(result.code, 0, result.stderr);
    assert.match(result.stdout, /saved output/);
    assert.equal(existsSync(f.captured), false);
  }
});

test('metadata changes or unavailable session evidence between locked reads prevent agent spawn', async t => {
  for (const unavailable of [false, true]) {
    const f = await fixture(t, (run, count) => {
      if (count > 1) {
        if (unavailable) run.continuationBlocked = 'Saved session evidence unavailable';
        else run.continuation.sessionId = '87654321-1234-4234-8234-123456789012';
      }
    });
    const result = await f.invoke();
    assert.equal(result.code, 1);
    assert.notEqual(result.stderr, '');
    assert.equal(existsSync(f.captured), false);
  }
});

test('invalid IDs and missing exact transcript never fall back to continue', async t => {
  for (const id of ['--continue', '87654321-1234-4234-8234-123456789012']) {
    const f = await fixture(t, run => { run.continuation.sessionId = id; });
    const result = await f.invoke();
    assert.equal(result.code, 1);
    assert.match(result.stderr, /no valid saved agent session|not saved or has been removed/);
    assert.equal(existsSync(f.captured), false);
  }
});

test('a concurrent writer prevents a second wrapped continuation', async t => {
  const f = await fixture(t);
  const fd = openSync(f.run.log, 'r');
  try {
    execFileSync('flock', ['--nonblock', '3'], { stdio: ['ignore', 'pipe', 'pipe', fd] });
    const result = await f.invoke();
    assert.equal(result.code, 1);
    assert.match(result.stderr, /already open/);
    assert.equal(existsSync(f.captured), false);
  } finally { closeSync(fd); }
});
