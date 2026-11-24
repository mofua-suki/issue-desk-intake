import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';

type WatchIdentity = {
  sendId: string;
  conversationId: string;
  targetId: string;
  notificationId: string;
};
export type WatchState = WatchIdentity & {
  notification: 'pending' | 'delivered';
  response:
    | { kind: 'pending' }
    | { kind: 'investigated'; evidenceId: string }
    | { kind: 'operation'; evidenceId: string }
    | { kind: 'waiting'; decision: string };
};

// Values are identifiers, not arbitrary issue text or logs.
const identifier = (value: unknown): value is string =>
  typeof value === 'string' &&
  /^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,255}$/.test(value);
const keys = (value: any, expected: string[]) =>
  value &&
  typeof value === 'object' &&
  !Array.isArray(value) &&
  Object.keys(value).sort().join(',') === expected.slice().sort().join(',');
const identityKeys = [
  'sendId',
  'conversationId',
  'targetId',
  'notificationId',
] as const;
function validIdentity(value: any): boolean {
  return value && identityKeys.every((key) => identifier(value[key]));
}
function validate(value: any): asserts value is WatchState {
  if (
    !keys(value, [...identityKeys, 'notification', 'response']) ||
    !validIdentity(value) ||
    !['pending', 'delivered'].includes(value.notification)
  )
    throw Error('invalid watch state');
  const response = value.response;
  if (response?.kind === 'pending' && keys(response, ['kind'])) return;
  if (
    ['investigated', 'operation'].includes(response?.kind) &&
    keys(response, ['kind', 'evidenceId']) &&
    identifier(response.evidenceId)
  )
    return;
  if (
    response?.kind === 'waiting' &&
    keys(response, ['kind', 'decision']) &&
    identifier(response.decision)
  )
    return;
  throw Error('invalid watch response');
}
function checkDirectory(dir: string) {
  const stat = fs.lstatSync(dir);
  if (
    !stat.isDirectory() ||
    stat.uid !== process.getuid?.() ||
    stat.mode & 0o077
  )
    throw Error('unsafe watch state directory');
}
function read(file: string): WatchState | null {
  let fd: number;
  try {
    fd = fs.openSync(
      file,
      fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK,
    );
  } catch (error: any) {
    if (error.code === 'ENOENT') return null;
    throw error;
  }
  try {
    const stat = fs.fstatSync(fd);
    if (!stat.isFile() || stat.uid !== process.getuid?.() || stat.mode & 0o077)
      throw Error('unsafe watch state file');
    const value: unknown = JSON.parse(fs.readFileSync(fd, 'utf8'));
    validate(value);
    return value;
  } finally {
    fs.closeSync(fd);
  }
}
/** Returns null only for a missing file or a different identity; invalid records throw. */
export function resumeWatchState(
  file: string,
  identity: WatchIdentity,
): WatchState | null {
  if (!validIdentity(identity)) throw Error('invalid watch identity');
  checkDirectory(path.dirname(file));
  const state = read(file);
  return state && identityKeys.every((key) => state[key] === identity[key])
    ? state
    : null;
}

/** Caller holds the existing single-watch lock for the whole read/update cycle. */
export function saveWatchState(file: string, state: WatchState): void {
  validate(state);
  const dir = path.dirname(file);
  checkDirectory(dir);
  // Refuse to replace malformed or unsafe files, including dangling symlinks.
  read(file);
  const temporary = path.join(dir, `.watch-${crypto.randomUUID()}`);
  let created = false;
  try {
    const fd = fs.openSync(
      temporary,
      fs.constants.O_WRONLY |
        fs.constants.O_CREAT |
        fs.constants.O_EXCL |
        fs.constants.O_NOFOLLOW,
      0o600,
    );
    created = true;
    try {
      fs.writeFileSync(fd, `${JSON.stringify(state)}\n`);
      fs.fsyncSync(fd);
    } finally {
      fs.closeSync(fd);
    }
    fs.renameSync(temporary, file);
    created = false;
    const dirFd = fs.openSync(
      dir,
      fs.constants.O_RDONLY |
        fs.constants.O_DIRECTORY |
        fs.constants.O_NOFOLLOW,
    );
    try {
      fs.fsyncSync(dirFd);
    } finally {
      fs.closeSync(dirFd);
    }
  } finally {
    if (created) fs.unlinkSync(temporary);
  }
}
