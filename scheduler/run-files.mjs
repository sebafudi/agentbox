import { openSync, closeSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';

// Hold every log-inode lock until the state mutation and physical cleanup finish.
// The CLI uses the same inode to protect an interactive continuation.
export function lockRunFiles(dataDir, runs) {
  const locked = [];
  const release = () => {
    for (const fd of locked.splice(0)) closeSync(fd);
  };
  const paths = [];
  try {
    for (const run of runs) {
      if (!/^[0-9]+-[a-f0-9]{8}$/.test(run.id)) continue;
      const log = join(dataDir, 'logs', `${run.id}.log`);
      const session = join(dataDir, 'logs', `${run.id}.session`);
      let fd;
      try { fd = openSync(log, 'r'); }
      catch (error) { if (error.code !== 'ENOENT') throw error; }
      if (fd !== undefined) {
        locked.push(fd);
        try { execFileSync('flock', ['--nonblock', '3'], { stdio: ['ignore', 'pipe', 'pipe', fd] }); }
        catch (error) {
          if (error.status !== 1) throw error;
          const busy = Error(`Run ${run.id} is open in scheduler join; close its continuation before deleting the job`);
          busy.code = 'RUN_JOINED';
          throw busy;
        }
      }
      paths.push({ log, session });
    }
    return {
      release,
      remove() {
        for (const { log, session } of paths) {
          // Keep the lock inode available until its transcript is removed.
          rmSync(session, { recursive: true, force: true });
          rmSync(log, { force: true });
        }
      },
    };
  } catch (error) { release(); throw error; }
}
