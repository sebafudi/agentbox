import { execFile } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { randomUUID } from 'node:crypto';

export function newRunUnit() { return `agent-scheduler-run-${randomUUID()}.service`; }
export function assertOwnedUnit(unit) {
  if (typeof unit !== 'string' || !/^agent-scheduler-run-[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\.service$/.test(unit)) throw Error('Invalid owned run unit');
}
function control(args, timeout = 1500) {
  return new Promise((resolve, reject) => {
    execFile('systemctl', ['--user', '--no-pager', ...args], { timeout, maxBuffer: 64 * 1024 }, (error, stdout) => error ? reject(error) : resolve(stdout));
  });
}
export async function readRunUnit(unit) {
  assertOwnedUnit(unit);
  const output = await control(['show', unit, '--property=LoadState,ActiveState,ControlGroup,Result,ExecMainCode,ExecMainStatus']);
  return Object.fromEntries(output.trim().split('\n').map(line => { const index = line.indexOf('='); return [line.slice(0, index), line.slice(index + 1)]; }));
}
function emptyCgroup(group) {
  if (!group) return true;
  if (!group.startsWith('/') || group.split('/').includes('..')) throw Error('Invalid run control group');
  try { return /^populated 0$/m.test(readFileSync(`/sys/fs/cgroup${group}/cgroup.events`, 'utf8')); }
  catch (error) { if (error.code === 'ENOENT') return true; throw error; }
}
// A successful stop includes systemd's bounded TERM -> KILL control-group cleanup.
// An unloaded unit is safe only after its launching client has closed, or on restart.
export async function cleanupRunUnit(unit) {
  assertOwnedUnit(unit);
  const before = await readRunUnit(unit);
  if (before.LoadState === 'not-found') return before;
  if (['inactive', 'failed'].includes(before.ActiveState) && emptyCgroup(before.ControlGroup)) {
    await control(['reset-failed', unit]).catch(() => {});
    return before;
  }
  let stopError;
  try { await control(['stop', unit], 7000); } catch (error) { stopError = error; }
  const after = await readRunUnit(unit);
  const group = after.ControlGroup || before.ControlGroup;
  if (!['inactive', 'failed'].includes(after.ActiveState) && after.LoadState !== 'not-found') throw Error(`Run unit did not stop: ${unit}${stopError ? ` (${stopError.message})` : ''}`);
  if (!emptyCgroup(group)) throw Error(`Run control group still populated: ${unit}`);
  // Failed transient units otherwise accumulate indefinitely. Reset only this unit.
  if (after.LoadState !== 'not-found') await control(['reset-failed', unit]).catch(() => {});
  return before;
}
export async function stopRunUnit(unit) {
  assertOwnedUnit(unit);
  await control(['stop', unit], 7000);
}
export function runUnitArgs({ unit, command, args, cwd, env, timeoutMs }) {
  assertOwnedUnit(unit);
  return ['--user', '--wait', '--pipe', '--quiet', '--service-type=exec', '--expand-environment=no', `--unit=${unit}`, `--working-directory=${cwd}`,
    '--property=KillMode=control-group', '--property=TimeoutStopSec=5s', '--property=SendSIGKILL=yes', '--property=ExitType=main',
    `--property=RuntimeMaxSec=${Math.ceil(timeoutMs / 1000)}s`,
    ...Object.entries(env).filter(([, value]) => value !== undefined).map(([key, value]) => `--setenv=${key}=${value}`), '--', command, ...args];
}
