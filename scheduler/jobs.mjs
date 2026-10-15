import { CronExpressionParser } from 'cron-parser';
import { statSync } from 'node:fs';

export function nextOccurrence(schedule, timezone, after = new Date()) {
  return CronExpressionParser.parse(schedule, { currentDate: after, tz: timezone }).next().toDate().toISOString();
}

export function validateJob(input) {
  if (!input || typeof input !== 'object' || Array.isArray(input)) throw Error('job must be a JSON object');
  const { id, name, schedule, timezone, cwd, runner, model = '', timeoutMinutes = 30, enabled = true, permissionMode = 'dontAsk' } = input;
  const prompt = input.prompt;
  const argv = input.argv;
  const { allowedTools, additionalDirectories } = input;
  if (typeof id !== 'string' || !/^[a-z0-9][a-z0-9-]{0,39}$/.test(id)) throw Error('id must be lowercase letters, digits, hyphens (1-40 characters)');
  if (typeof name !== 'string' || !name.trim() || name.length > 100) throw Error('name must be 1-100 characters');
  if (typeof schedule !== 'string' || schedule.trim().split(/\s+/).length !== 5) throw Error('schedule must be a five-field cron expression');
  if (/\bH\b/i.test(schedule)) throw Error('hashed H cron fields are unsupported; use explicit numeric times');
  if (typeof timezone !== 'string') throw Error('timezone must be an IANA time zone');
  try { new Intl.DateTimeFormat('en', { timeZone: timezone }); } catch { throw Error('invalid IANA time zone'); }
  try { nextOccurrence(schedule, timezone); } catch (error) { throw Error(`invalid schedule: ${error.message}`); }
  if (prompt !== undefined && (typeof prompt !== 'string' || prompt.length > 200_000 || prompt.includes('\0'))) throw Error('prompt must be a string of at most 200000 characters without NUL');
  if (runner !== 'command' && (typeof prompt !== 'string' || !prompt.trim())) throw Error('prompt must be 1-200000 characters for agent runners');
  if (argv !== undefined && (!Array.isArray(argv) || !argv.length || argv.some(value => typeof value !== 'string' || value.includes('\0')) || !argv[0].trim())) throw Error('argv must be a nonempty string array with a nonempty executable and no NUL');
  if (runner === 'command' && argv === undefined) throw Error('command runner requires argv');
  if (typeof cwd !== 'string' || !cwd.startsWith('/') || !statSync(cwd).isDirectory()) throw Error('cwd must be an existing absolute directory');
  if (!['omp', 'claude-print', 'claude-sdk', 'command'].includes(runner)) throw Error('runner must be omp, claude-print, claude-sdk, or command');
  if (typeof model !== 'string' || model.length > 100) throw Error('model must be at most 100 characters');
  if (!Number.isInteger(timeoutMinutes) || timeoutMinutes < 1 || timeoutMinutes > 1440) throw Error('timeoutMinutes must be 1-1440');
  if (typeof enabled !== 'boolean') throw Error('enabled must be boolean');
  if (!['dontAsk', 'acceptEdits', 'bypassPermissions'].includes(permissionMode)) throw Error('invalid permissionMode');
  if (allowedTools !== undefined && (!Array.isArray(allowedTools) || allowedTools.some(tool => typeof tool !== 'string' || !tool.trim() || tool.startsWith('-') || tool.includes('\0')))) throw Error('allowedTools must be a string array of nonempty tool rules without NUL or leading hyphens');
  if (additionalDirectories !== undefined && (!Array.isArray(additionalDirectories) || additionalDirectories.some(directory => typeof directory !== 'string' || !directory.startsWith('/') || directory.includes('\0') || !statSync(directory).isDirectory()))) throw Error('additionalDirectories must be an array of existing absolute directories');
  // Inactive runner settings remain configuration, never execution flags.
  return { id, name: name.trim(), schedule: schedule.trim(), timezone, ...(prompt === undefined ? {} : { prompt }), ...(argv === undefined ? {} : { argv: argv.slice() }), ...(allowedTools === undefined ? {} : { allowedTools: allowedTools.slice() }), ...(additionalDirectories === undefined ? {} : { additionalDirectories: additionalDirectories.slice() }), cwd, runner, model, timeoutMinutes, enabled, permissionMode };
}

const patchFields = new Set(['name', 'schedule', 'timezone', 'prompt', 'argv', 'cwd', 'runner', 'model', 'timeoutMinutes', 'enabled', 'permissionMode', 'allowedTools', 'additionalDirectories']);

export function configuredJob(input, previous, now = new Date()) {
  const inherited = previous ? Object.fromEntries(['argv', 'prompt', 'model', 'permissionMode', 'allowedTools', 'additionalDirectories'].filter(key => previous[key] !== undefined).map(key => [key, previous[key]])) : {};
  const job = validateJob({ ...inherited, ...input });
  const unchanged = previous && previous.schedule === job.schedule && previous.timezone === job.timezone && previous.enabled === job.enabled;
  job.nextRun = !job.enabled ? null : unchanged ? previous.nextRun : nextOccurrence(job.schedule, job.timezone, now);
  return job;
}

export function patchJob(previous, changes, now = new Date()) {
  if (!changes || typeof changes !== 'object' || Array.isArray(changes) || !Object.keys(changes).length) throw Error('changes must be a nonempty JSON object');
  for (const key of Object.keys(changes)) {
    if (!patchFields.has(key)) throw Error(`cannot patch field: ${key}`);
    if (changes[key] === undefined || changes[key] === null) throw Error(`invalid patch value: ${key}`);
  }
  return configuredJob({ ...previous, ...changes }, previous, now);
}

export function jobStats(runs) {
  return {
    total: runs.length,
    success: runs.filter(run => run.status === 'success').length,
    failed: runs.filter(run => run.status === 'failed').length,
    running: runs.filter(run => run.status === 'running').length,
    lastDurationMs: runs[0]?.durationMs ?? null,
  };
}
