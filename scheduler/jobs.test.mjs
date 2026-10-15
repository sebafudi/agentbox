import test from 'node:test';
import assert from 'node:assert/strict';
import { nextOccurrence, validateJob, configuredJob, patchJob, jobStats } from './jobs.mjs';

const base = {
  id: 'morning-report', name: 'Morning report', schedule: '30 9 * * 1-5',
  timezone: 'Europe/Warsaw', prompt: 'Summarize changes', cwd: process.cwd(), runner: 'omp',
};

test('a weekday schedule uses its configured timezone across the weekend', () => {
  assert.equal(nextOccurrence(base.schedule, base.timezone, new Date('2026-09-25T08:00:00Z')), '2026-09-28T07:30:00.000Z');
});

test('spring daylight-saving gap produces a real later instant', () => {
  assert.equal(nextOccurrence('30 2 * * *', 'Europe/Warsaw', new Date('2026-03-28T23:00:00Z')), '2026-03-29T01:30:00.000Z');
});

test('rejects invalid schedule, directory, and unsafe runner settings before saving', () => {
  assert.throws(() => validateJob({ ...base, schedule: '* * * * * *' }), /five-field/);
  assert.throws(() => validateJob({ ...base, timezone: 'Not/AZone' }), /time zone/);
  assert.throws(() => validateJob({ ...base, cwd: '/not-a-real-job-path' }), /ENOENT/);
  assert.throws(() => validateJob({ ...base, permissionMode: 'invalid' }), /permissionMode/);
});

test('patches merge current configuration and preserve an overdue occurrence', () => {
  const job = { ...validateJob(base), nextRun: '2026-01-01T00:00:00.000Z' };
  const updated = patchJob(job, { model: 'new-model', timeoutMinutes: 45 });
  assert.equal(updated.prompt, job.prompt);
  assert.equal(updated.cwd, job.cwd);
  assert.equal(updated.nextRun, job.nextRun);
  assert.equal(updated.timeoutMinutes, 45);
  assert.equal(job.model, '');
  assert.equal(patchJob(job, { enabled: true }).nextRun, job.nextRun);
});

test('invalid or immutable patch fields never mutate the original job', () => {
  const job = configuredJob(base);
  const before = structuredClone(job);
  for (const changes of [{}, { id: 'other' }, { nextRun: null }, { timeoutMinutes: 0 }, { model: null }, { enabled: 'false' }, { model: 'valid', timezone: 'Not/AZone' }]) {
    assert.throws(() => patchJob(job, changes));
    assert.deepEqual(job, before);
  }
});

test('pause and changed scheduling recalculate only from the supplied clock', () => {
  const now = new Date('2026-09-25T08:00:00Z');
  const job = configuredJob(base, undefined, now);
  const paused = patchJob(job, { enabled: false }, now);
  assert.equal(paused.nextRun, null);
  assert.equal(patchJob(paused, { enabled: true }, now).nextRun, '2026-09-28T07:30:00.000Z');
  assert.equal(patchJob(job, { timezone: 'UTC' }, now).nextRun, '2026-09-25T09:30:00.000Z');
  assert.equal(configuredJob({ ...base, name: 'Renamed' }, job, now).nextRun, job.nextRun);
});

test('retained statistics count failures and do not invent missing durations', () => {
  assert.deepEqual(jobStats([{ status: 'running' }, { status: 'success', durationMs: 12 }, { status: 'failed' }]), {
    total: 3, running: 1, success: 1, failed: 1, lastDurationMs: null,
  });
  assert.equal(jobStats([{ status: 'success', durationMs: 12 }]).lastDurationMs, 12);
});

test('randomized hashed fields are refused but named weekdays remain valid', () => {
  assert.throws(() => validateJob({ ...base, schedule: 'H * * * *' }), /hashed/);
  assert.equal(validateJob({ ...base, schedule: '0 9 * * THU' }).schedule, '0 9 * * THU');
});

test('autumn daylight-saving repetition advances to a strictly later occurrence', () => {
  const first = nextOccurrence('30 2 * * *', 'Europe/Warsaw', new Date('2026-10-24T22:00:00Z'));
  assert.equal(first, '2026-10-25T00:30:00.000Z');
  const second = nextOccurrence('30 2 * * *', 'Europe/Warsaw', new Date(first));
  assert.ok(Date.parse(second) > Date.parse(first));
});

test('command argv validates every element even while inactive and never mutates input', () => {
  const input = { ...base, runner: 'command', argv: ['/usr/bin/printf', '%s', 'two words', ''] };
  const job = validateJob(input);
  assert.deepEqual(job.argv, input.argv);
  assert.notEqual(job.argv, input.argv);
  assert.equal(validateJob({ ...input, prompt: undefined }).prompt, undefined);
  for (const argv of [undefined, [], 'echo hello', [''], ['   '], ['echo', 1], ['echo', null], ['echo', 'a\0b']]) {
    assert.throws(() => validateJob({ ...input, argv }), /argv/);
  }
  assert.throws(() => validateJob({ ...base, argv: ['echo', false] }), /argv/);
});

test('apply, patch, and runner switches preserve inactive settings without applying them', () => {
  const original = configuredJob({ ...base, runner: 'claude-print', model: 'saved-model', permissionMode: 'acceptEdits', argv: ['/usr/bin/printf', 'two words'] });
  const command = patchJob(original, { runner: 'command', timeoutMinutes: 5 });
  assert.equal(command.prompt, base.prompt);
  assert.equal(command.permissionMode, 'acceptEdits');
  const omp = patchJob(command, { runner: 'omp' });
  assert.deepEqual(omp.argv, original.argv);
  assert.equal(omp.model, original.model);
  assert.equal(omp.permissionMode, original.permissionMode);
  const { argv, prompt, model, permissionMode, ...replacement } = command;
  const applied = configuredJob(replacement, command);
  assert.deepEqual(applied.argv, argv);
  assert.equal(applied.prompt, prompt);
  assert.equal(applied.model, model);
  assert.equal(applied.permissionMode, permissionMode);
  const before = structuredClone(command);
  assert.throws(() => patchJob(command, { argv: ['echo', false], enabled: true }));
  assert.deepEqual(command, before);
});

test('scoped Claude tools and directories validate atomically and survive inactive runners', () => {
  const job = configuredJob({ ...base, allowedTools: ['Read', 'Bash(git status:*)'], additionalDirectories: [process.cwd()] });
  for (const allowedTools of ['Read', [null], [''], ['a\0b'], ['--bypass-permissions']]) assert.throws(() => patchJob(job, { allowedTools }), /allowedTools/);
  for (const additionalDirectories of ['/', ['relative'], [null], ['/not-a-real-job-path']]) assert.throws(() => patchJob(job, { additionalDirectories }));
  const command = patchJob(job, { runner: 'command', argv: ['/usr/bin/true'] });
  const applied = configuredJob({ ...base, runner: 'claude-print' }, command);
  assert.deepEqual(applied.allowedTools, job.allowedTools);
  assert.deepEqual(applied.additionalDirectories, job.additionalDirectories);
  assert.equal(applied.permissionMode, 'dontAsk');
  assert.deepEqual(patchJob(applied, { allowedTools: [], additionalDirectories: [] }).allowedTools, []);
});
