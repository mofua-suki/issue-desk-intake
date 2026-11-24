import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { dispatchWatch } from './codex-issue-watch-dispatch.ts';
import { resumeWatchState, saveWatchState } from './codex-issue-watch-state.ts';

type Identity = Parameters<typeof dispatchWatch>[0];
type Target = Parameters<typeof dispatchWatch>[1];
type Dependencies = Pick<
  Parameters<typeof dispatchWatch>[2],
  'readClassification' | 'deliver'
>;

/** Dispatch under a process-wide lock shared by all callers using this directory. */
export async function dispatchWatchLocal(
  directory: string,
  identity: Identity,
  target: Target,
  dependencies: Dependencies,
): Promise<string> {
  const dir = fs.lstatSync(directory);
  if (!dir.isDirectory() || dir.uid !== process.getuid?.() || dir.mode & 0o077)
    throw Error('unsafe watch state directory');
  const digest = crypto
    .createHash('sha256')
    .update(
      JSON.stringify([
        identity.sendId,
        identity.conversationId,
        identity.targetId,
        identity.notificationId,
      ]),
    )
    .digest('hex');
  const file = path.join(directory, `${digest}.json`);
  return withWatchLock(path.join(directory, `${digest}.lock`), async () => {
    return dispatchWatch(identity, target, {
      ...dependencies,
      resume: (id) => {
        const state = resumeWatchState(file, id);
        if (!state && fs.existsSync(file))
          throw Error('watch state identity mismatch');
        return state;
      },
      save: (state) => saveWatchState(file, state),
    });
  });
}

export async function withWatchLock<T>(
  lockFile: string,
  run: () => Promise<T>,
): Promise<T> {
  const fd = fs.openSync(
    lockFile,
    fs.constants.O_CREAT |
      fs.constants.O_RDWR |
      fs.constants.O_NOFOLLOW |
      fs.constants.O_NONBLOCK,
    0o600,
  );
  try {
    const stat = fs.fstatSync(fd);
    if (
      !stat.isFile() ||
      stat.uid !== process.getuid?.() ||
      stat.mode & 0o077 ||
      stat.nlink !== 1
    )
      throw Error('unsafe watch lock file');
    // Descriptor mode takes no command argument. The shell retains descriptor 3
    // for the duration of the lock, independently of the working directory.
    const child = spawn(
      'sh',
      ['-c', 'flock -x -w 60 3 && printf ready && cat >/dev/null'],
      { stdio: ['pipe', 'pipe', 'ignore', fd] },
    );
    const exited = new Promise<void>((resolve, reject) => {
      child.once('error', reject);
      child.once('close', (code) =>
        code === 0 ? resolve() : reject(Error('watch lock failed')),
      );
    });
    try {
      await Promise.race([
        new Promise<void>((resolve) =>
          child.stdout.once('data', () => resolve()),
        ),
        exited.then(() => {
          throw Error('watch lock ended early');
        }),
      ]);
      return await run();
    } finally {
      child.stdin.end();
      await exited;
    }
  } finally {
    fs.closeSync(fd);
  }
}
