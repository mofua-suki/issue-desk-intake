import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { randomUUID } from 'node:crypto';
import { spawn } from 'node:child_process';

export const MAX_BYTES = 1024 * 1024;
// Unique contender directories allow dead owners to be removed without touching
// another contender's lock. A live or unknown owner always wins over recovery.
export function withRecordLock(file, action) {
  const prefix = `${path.basename(file)}.lock-`;
  const host = Buffer.from(os.hostname()).toString('hex');
  const lock = path.join(
    path.dirname(file),
    `${prefix}${host}-${process.pid}-${randomUUID()}`,
  );
  fs.mkdirSync(lock, { mode: 0o700 });
  try {
    for (const name of fs.readdirSync(path.dirname(file))) {
      if (
        !name.startsWith(prefix) ||
        path.join(path.dirname(file), name) === lock
      )
        continue;
      const owner = name
        .slice(prefix.length)
        .match(/^([0-9a-f]+)-(\d+)-[0-9a-f-]+$/);
      if (!owner || owner[1] !== host) throw Error('record lock busy');
      let alive = true;
      try {
        process.kill(Number(owner[2]), 0);
      } catch (error) {
        if (error.code === 'ESRCH') alive = false;
      }
      if (alive) throw Error('record lock busy');
      try {
        fs.rmdirSync(path.join(path.dirname(file), name));
      } catch (error) {
        if (error.code !== 'ENOENT') throw error;
      }
    }
    return action();
  } finally {
    fs.rmdirSync(lock);
  }
}
export function projectName(cwd, home = os.homedir()) {
  if (typeof cwd !== 'string' || !path.isAbsolute(cwd)) return 'other';
  const parts = path
    .relative(path.join(home, 'rc'), path.resolve(cwd))
    .split(path.sep);
  return parts.length &&
    /^[A-Za-z0-9._-]{1,100}$/.test(parts[0]) &&
    parts[0] !== '..' &&
    parts[0] !== '.'
    ? parts[0]
    : 'other';
}
export function replaceLines(file, lines) {
  const tmp = `${file}.${process.pid}.${randomUUID()}.tmp`;
  try {
    fs.writeFileSync(tmp, lines.join(''), { mode: 0o600, flag: 'wx' });
    fs.renameSync(tmp, file);
  } finally {
    try {
      fs.unlinkSync(tmp);
    } catch {}
  }
}
export function recordFires(fires, cwd, home = os.homedir(), now = new Date()) {
  if (!fires.length) return;
  const dir = path.join(home, '.issue-desk');
  const file = path.join(dir, 'stop-check-fires.jsonl');
  try {
    withRecordLock(file, () => {
      for (const fire of fires) {
        const line =
          JSON.stringify({
            id: randomUUID(),
            checkId: fire.id,
            result: fire.result,
            at: now.toISOString(),
            project: projectName(cwd, home),
          }) + '\n';
        fs.appendFileSync(file, line, { mode: 0o600 });
      }
      if (fs.statSync(file).size > MAX_BYTES) {
        const lines = fs.readFileSync(file, 'utf8').match(/[^\n]*\n/g) || [];
        let size = 0;
        const kept = [];
        for (let i = lines.length - 1; i >= 0; i--) {
          size += Buffer.byteLength(lines[i]);
          if (size > MAX_BYTES / 2) break;
          kept.push(lines[i]);
        }
        replaceLines(file, kept.reverse());
      }
    });
  } catch {} // Recording must never affect the decision.
  try {
    const child = spawn(path.join(dir, 'desk'), ['stop-check-send.ts'], {
      detached: true,
      stdio: 'ignore',
      cwd: home,
    });
    child.on('error', () => {});
    child.unref();
  } catch {}
}
