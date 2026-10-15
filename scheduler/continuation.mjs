import { readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { stripVTControlCharacters } from 'node:util';

export function terminalText(text) {
  return stripVTControlCharacters(text).replace(/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\x9f]/g, '');
}

export async function continuation(run) {
  if (run.status === 'running') throw Error('Run is still active; use scheduler follow to observe it');
  if (run.continuationBlocked) throw Error(run.continuationBlocked);
  if (run.runner === 'command') {
    const saved = run.continuation;
    if (!saved || saved.kind !== 'claude' || saved.cwd !== run.cwd || !/^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/i.test(saved.sessionId)) throw Error('Command runs have no valid saved agent session; use scheduler logs or scheduler follow');
    run = { ...run, runner: 'claude-print', sessionId: saved.sessionId };
  }
  if (!run.cwd) throw Error('This older run has no recorded working directory; exact continuation is unavailable. Start a new run.');
  if (!statSync(run.cwd).isDirectory()) throw Error(`Run working directory is unavailable: ${run.cwd}`);
  if (run.runner === 'omp') {
    if (!run.sessionDir) throw Error('This run has no recorded omp session directory; exact continuation is unavailable. Start a new run.');
    let sessions;
    try { sessions = readdirSync(run.sessionDir, { withFileTypes: true }).filter(file => file.isFile() && file.name.endsWith('.jsonl')); }
    catch (error) { if (error.code === 'ENOENT') throw Error('The omp session was not saved or has been removed. Inspect scheduler logs, or start a new run.'); throw error; }
    if (sessions.length !== 1) throw Error(`Expected one exact omp transcript, found ${sessions.length}; inspect ${run.sessionDir} instead of resuming an unrelated session`);
    return { command: 'omp', args: ['--allow-home', '--session-dir', run.sessionDir, '--resume', join(run.sessionDir, sessions[0].name)] };
  }
  if (!['claude-print', 'claude-sdk'].includes(run.runner)) throw Error(`Unknown runner: ${run.runner}`);
  if (!run.sessionId) throw Error('This older Claude run has no persisted session ID; exact continuation is unavailable. Start a new run.');
  const { getSessionInfo } = await import('@anthropic-ai/claude-agent-sdk');
  if (!await getSessionInfo(run.sessionId, { dir: run.cwd })) throw Error(`Claude session ${run.sessionId} was not saved or has been removed. Inspect scheduler logs, or start a new run.`);
  return { command: 'claude', args: ['--resume', run.sessionId] };
}
