import test from 'node:test';
import assert from 'node:assert/strict';
import { privateRun } from './run-view.mjs';

test('stored continuation fields are never trusted; command runs stay log-only by default', () => {
  const session = '12345678-1234-4234-8234-123456789012';
  const run = { id: '1000000000000-abcdef12', jobId: 'ordinary', runner: 'command',
    continuation: { kind: 'claude', sessionId: session, cwd: '/tmp' }, continuationBlocked: 'stale', wrappedAgent: 'claude' };
  const before = structuredClone(run);
  assert.deepEqual(privateRun(run), { id: run.id, jobId: 'ordinary', runner: 'command' });
  assert.deepEqual(run, before);
  assert.deepEqual([run].map(privateRun), [{ id: run.id, jobId: 'ordinary', runner: 'command' }]);
});
