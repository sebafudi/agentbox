import { query } from '@anthropic-ai/claude-agent-sdk';

const permissionMode = process.env.SCHEDULER_PERMISSION_MODE || 'dontAsk';
const allowedModes = new Set(['dontAsk', 'acceptEdits', 'bypassPermissions']);
const abortController = new AbortController();
let terminated = false;

process.on('SIGTERM', () => {
  terminated = true;
  process.exitCode = 143;
  abortController.abort();
  process.stdin.destroy();
});

async function main() {
  if (!allowedModes.has(permissionMode)) {
    throw new Error(`Invalid SCHEDULER_PERMISSION_MODE: ${permissionMode}`);
  }

  process.stdin.setEncoding('utf8');
  let prompt = '';
  for await (const chunk of process.stdin) prompt += chunk;
  if (terminated) return;
  if (!prompt.trim()) throw new Error('No prompt received on stdin');

  const options = {
    cwd: process.cwd(),
    permissionMode,
    abortController,
    persistSession: true,
    sessionId: process.env.SCHEDULER_SESSION_ID,
  };
  if (process.env.SCHEDULER_MODEL) options.model = process.env.SCHEDULER_MODEL;
  if (process.env.SCHEDULER_ALLOWED_TOOLS) options.allowedTools = JSON.parse(process.env.SCHEDULER_ALLOWED_TOOLS);
  if (process.env.SCHEDULER_ADDITIONAL_DIRECTORIES) options.additionalDirectories = JSON.parse(process.env.SCHEDULER_ADDITIONAL_DIRECTORIES);
  if (permissionMode === 'bypassPermissions') {
    options.allowDangerouslySkipPermissions = true;
  }

  let result;
  for await (const message of query({ prompt, options })) {
    if (message.type === 'result') result = message;
  }
  if (terminated) return;
  if (!result) throw new Error('Claude Agent SDK ended without a result');
  if (result.subtype !== 'success' || result.is_error) {
    const details = result.errors?.filter(Boolean).join('; ');
    throw new Error(`Claude Agent SDK failed (${result.subtype})${details ? `: ${details}` : ''}`);
  }
  process.stdout.write(`${result.result ?? ''}\n`);
  process.stdout.write(`${JSON.stringify({ outcome: 'success' })}\n`);
}

try {
  await main();
} catch (error) {
  if (!terminated) {
    console.error(`SDK runner: ${error instanceof Error ? error.message : String(error)}`);
    process.stdout.write(`${JSON.stringify({ outcome: 'failure', reason: `SDK error: ${error instanceof Error ? error.message : String(error)}` })}\n`);
    process.exitCode = 1;
  }
}
