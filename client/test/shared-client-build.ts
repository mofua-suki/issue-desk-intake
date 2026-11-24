import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildClient } from '../scripts/build.ts';

let cached:
  | (ReturnType<typeof buildClient> & { destination: string })
  | undefined;

export function sharedClientBuild() {
  if (cached) return cached;
  const run = process.env.ISSUE_DESK_RUN_TMPDIR;
  // Only the verification wrapper provides a run-scoped directory.
  if (!run || run !== process.env.TMPDIR || !existsSync(run)) {
    const destination = mkdtempSync(join(tmpdir(), 'intake-client-shared-'));
    try {
      cached = { destination, ...buildClient(destination) };
    } catch (error) {
      rmSync(destination, { recursive: true, force: true });
      throw error;
    }
    process.once('exit', () =>
      rmSync(destination, { recursive: true, force: true }),
    );
    return cached;
  }

  const destination = join(run, 'client-build');
  const lock = join(run, 'client-build.lock');
  const deadline = Date.now() + 30000;
  for (;;) {
    if (existsSync(destination)) {
      const result = JSON.parse(
        readFileSync(join(destination, '.build-result.json'), 'utf8'),
      ) as ReturnType<typeof buildClient>;
      cached = { destination, ...result };
      return cached;
    }
    try {
      mkdirSync(lock);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'EEXIST') throw error;
      if (Date.now() >= deadline)
        throw new Error('Timed out waiting for shared client build');
      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 50);
      continue;
    }
    let staging: string | undefined;
    try {
      // Another builder may have published between our check and lock acquisition.
      if (existsSync(destination)) continue;
      staging = mkdtempSync(join(run, 'client-build-staging-'));
      const result = buildClient(staging);
      writeFileSync(
        join(staging, '.build-result.json'),
        JSON.stringify(result),
      );
      renameSync(staging, destination);
      staging = undefined;
      cached = { destination, ...result };
      return cached;
    } finally {
      if (staging) rmSync(staging, { recursive: true, force: true });
      rmSync(lock, { recursive: true, force: true });
    }
  }
}
