import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import {
  resumeWatchState,
  saveWatchState,
  type WatchState,
} from '../scripts/codex-issue-watch-state.ts';

function fixture(t: any) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'watch-state-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'state.json');
  const state: WatchState = {
    sendId: 'send-1',
    conversationId: 'conversation-1',
    targetId: 'draft:1',
    notificationId: 'notification-1',
    notification: 'delivered',
    response: { kind: 'pending' },
  };
  return { dir, file, state };
}

test('再読込は四つの識別子を照合し通知と対処を分けて保持する', (t) => {
  const { file, state } = fixture(t);
  assert.equal(resumeWatchState(file, state), null);
  saveWatchState(file, state);
  assert.deepEqual(resumeWatchState(file, state), state);
  const restarted = spawnSync(
    process.execPath,
    [
      '--input-type=module',
      '-e',
      `import { resumeWatchState } from './scripts/codex-issue-watch-state.ts';
     console.log(JSON.stringify(resumeWatchState(process.argv[1], JSON.parse(process.argv[2]))));`,
      file,
      JSON.stringify(state),
    ],
    { cwd: process.cwd(), encoding: 'utf8' },
  );
  assert.equal(restarted.status, 0, restarted.stderr);
  assert.deepEqual(JSON.parse(restarted.stdout), state);
  for (const key of [
    'sendId',
    'conversationId',
    'targetId',
    'notificationId',
  ] as const)
    assert.equal(
      resumeWatchState(file, { ...state, [key]: 'different' }),
      null,
    );
  for (const response of [
    { kind: 'investigated', evidenceId: 'evidence-1' },
    { kind: 'operation', evidenceId: 'receipt-1' },
    { kind: 'waiting', decision: 'approve-restart' },
  ] as const) {
    const updated = { ...state, response };
    saveWatchState(file, updated);
    assert.deepEqual(resumeWatchState(file, state), updated);
  }
});

test('非公開ファイルへの更新は書込失敗時に確定値を保持する', (t) => {
  const { file, state, dir } = fixture(t);
  saveWatchState(file, state);
  assert.equal(fs.statSync(file).mode & 0o777, 0o600);
  const original = fs.readFileSync(file, 'utf8');
  const realWrite = fs.writeFileSync;
  const write = t.mock.method(fs, 'writeFileSync', (fd: any) => {
    realWrite(fd, '{partial');
    throw Error('write failed');
  });
  assert.throws(
    () => saveWatchState(file, { ...state, notification: 'pending' }),
    /write failed/,
  );
  write.mock.restore();
  assert.equal(fs.readFileSync(file, 'utf8'), original);
  assert.deepEqual(fs.readdirSync(dir), ['state.json']);
  assert.deepEqual(resumeWatchState(file, state), state);
});

test('危険な所有者・権限・リンク・破損形式を拒否し上書きしない', (t) => {
  const { file, state, dir } = fixture(t);
  saveWatchState(file, state);
  const original = fs.readFileSync(file, 'utf8');
  const realStat = fs.fstatSync;
  const stat = t.mock.method(
    fs,
    'fstatSync',
    (...args: Parameters<typeof fs.fstatSync>) => {
      const result = realStat(...args);
      return Object.assign(result, { uid: Number(result.uid) + 1 });
    },
  );
  assert.throws(() => resumeWatchState(file, state), /unsafe/);
  assert.throws(() => saveWatchState(file, state), /unsafe/);
  stat.mock.restore();
  assert.equal(fs.readFileSync(file, 'utf8'), original);
  fs.chmodSync(file, 0o644);
  assert.throws(() => saveWatchState(file, state), /unsafe/);
  fs.chmodSync(file, 0o600);
  fs.writeFileSync(file, '{broken');
  assert.throws(() => resumeWatchState(file, state));
  assert.throws(() => saveWatchState(file, state));
  assert.equal(fs.readFileSync(file, 'utf8'), '{broken');
  fs.unlinkSync(file);
  fs.symlinkSync(path.join(dir, 'missing'), file);
  assert.throws(() => saveWatchState(file, state));
  assert.throws(() => resumeWatchState(file, state));
  fs.unlinkSync(file);
  fs.chmodSync(dir, 0o755);
  assert.throws(() => saveWatchState(file, state), /unsafe/);
  assert.equal(fs.existsSync(file), false);
});

test('保存形式は追加の本文・ログ・資格情報を受け付けない', (t) => {
  const { file, state } = fixture(t);
  saveWatchState(file, state);
  const original = fs.readFileSync(file, 'utf8');
  for (const field of ['body', 'log', 'credential'])
    assert.throws(() =>
      saveWatchState(file, { ...state, [field]: 'secret text' }),
    );
  assert.throws(() =>
    saveWatchState(file, {
      ...state,
      response: { kind: 'waiting', decision: 'secret text' },
    }),
  );
  assert.equal(fs.readFileSync(file, 'utf8'), original);
});
