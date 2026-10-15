import { spawn } from 'node:child_process';

// systemd may finish a queued launch after the daemon crashes. Never start the
// actual workload until the live daemon has observed this owned unit. EOF before
// the gate is a harmless aborted launch, not an invitation to run anything.
let buffer = Buffer.alloc(0);
let child;
let gated = false;
// systemd delivers TERM to the whole cgroup. Keep the leader alive until the
// workload reports its actual exit; systemd's five-second KILL still bounds it.
process.on('SIGTERM', () => { if (!gated) process.exit(143); });
process.stdin.on('data', chunk => {
  if (gated) return;
  buffer = Buffer.concat([buffer, chunk]);
  const newline = buffer.indexOf(10);
  if (newline < 0) {
    if (buffer.length > 64) process.exit(1);
    return;
  }
  if (buffer.subarray(0, newline).toString() !== 'scheduler-owned-unit-ready') process.exit(1);
  gated = true;
  process.stdin.pause();
  const [command, ...args] = process.argv.slice(2);
  child = spawn(command, args, { stdio: ['pipe', 'inherit', 'inherit'] });
  child.on('error', error => { console.error(`Spawn failed: ${error.message}`); process.exitCode = 1; process.stdin.destroy(); });
  child.on('exit', (code, signal) => {
    // Do not wait for inherited pipes: systemd kills any remaining descendants.
    process.exit(code ?? (signal === 'SIGTERM' ? 143 : signal === 'SIGKILL' ? 137 : 1));
  });
  child.stdin.on('error', error => { if (error.code !== 'EPIPE') console.error(`Input delivery failed: ${error.message}`); });
  child.stdin.write(buffer.subarray(newline + 1));
  buffer = null;
  process.stdin.pipe(child.stdin);
  process.stdin.resume();
});
process.stdin.on('end', () => { if (!gated) process.exit(1); });
process.stdin.on('error', () => { if (!gated) process.exit(1); });
