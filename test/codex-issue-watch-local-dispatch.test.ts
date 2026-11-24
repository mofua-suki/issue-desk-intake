import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { dispatchWatchLocal } from '../scripts/codex-issue-watch-local-dispatch.ts';
import {
  resumeWatchState,
  saveWatchState,
} from '../scripts/codex-issue-watch-state.ts';

const identity = {
  sendId: 'send/1',
  conversationId: 'conversation-1',
  targetId: 'desk:101',
  notificationId: 'notice-1',
};
const target = { type: 'issue', desk: 101, received: true } as const;
const classification = {
  partial: false,
  unavailable: [],
  groups: {
    attention: [
      {
        kind: 'issue',
        desk: 101,
        work: {
          state: 'attention',
          reason: 'reason',
          nextActor: 'user',
          nextAction: 'check',
        },
      },
    ],
    stopped: [],
    waiting: [],
    running: [],
  },
};
function setup(t: any) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'local-watch-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const messages: string[] = [];
  const deps = {
    readClassification: async () => classification,
    deliver: async (message: { notificationId: string }) => {
      messages.push(message.notificationId);
      return 'delivered';
    },
  };
  return {
    dir,
    messages,
    deps,
    stateFile: () =>
      path.join(
        dir,
        fs.readdirSync(dir).find((name) => name.endsWith('.json'))!,
      ),
  };
}

test('巡回のロック中に通知固有の別ロックを取得できる', (t) => {
  const { dir } = setup(t);
  const module = new URL(
    '../scripts/codex-issue-watch-local-dispatch.ts',
    import.meta.url,
  );
  const result = spawnSync(
    process.execPath,
    [
      '--input-type=module',
      '--eval',
      `import { withWatchLock } from ${JSON.stringify(module.href)};
       console.log(await withWatchLock('poll.lock', () =>
         withWatchLock('notice.lock', async () => 'acquired')));`,
    ],
    { cwd: dir, encoding: 'utf8', timeout: 3000 },
  );
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), 'acquired');
  assert.deepEqual(fs.readdirSync(dir).sort(), ['notice.lock', 'poll.lock']);
});

test('並行開始と再開で同じ通知を一度だけ送達する', async (t) => {
  const f = setup(t);
  const results = await Promise.all(
    Array.from({ length: 8 }, () =>
      dispatchWatchLocal(f.dir, identity, target, f.deps),
    ),
  );
  assert.deepEqual(results, Array(8).fill('delivered'));
  assert.deepEqual(f.messages, ['notice-1']);
  assert.equal(
    await dispatchWatchLocal(f.dir, identity, target, f.deps),
    'delivered',
  );
  assert.deepEqual(f.messages, ['notice-1']);
  assert.equal(
    fs.readdirSync(f.dir).filter((name) => name.endsWith('.json')).length,
    1,
  );
  assert.equal(
    await dispatchWatchLocal(
      f.dir,
      identity,
      { ...target, received: false },
      f.deps,
    ),
    'invalid',
  );
});

test('送達失敗後も同じ通知IDで照合し対処結果を保持する', async (t) => {
  const f = setup(t);
  const failed = {
    ...f.deps,
    deliver: async (message: { notificationId: string }) => {
      f.messages.push(message.notificationId);
      throw Error('lost');
    },
  };
  assert.equal(
    await dispatchWatchLocal(f.dir, identity, target, failed),
    'unknown',
  );
  const file = f.stateFile();
  saveWatchState(file, {
    ...identity,
    notification: 'pending',
    response: { kind: 'investigated', evidenceId: 'evidence-1' },
  });
  assert.equal(
    await dispatchWatchLocal(f.dir, identity, target, f.deps),
    'delivered',
  );
  assert.deepEqual(f.messages, ['notice-1', 'notice-1']);
  assert.deepEqual(resumeWatchState(file, identity)?.response, {
    kind: 'investigated',
    evidenceId: 'evidence-1',
  });
});

test('破損、不一致、危険な状態は送達や上書きをしない', async (t) => {
  for (const kind of ['corrupt', 'mismatch', 'mode', 'symlink'] as const) {
    const f = setup(t);
    await dispatchWatchLocal(f.dir, identity, target, {
      ...f.deps,
      deliver: async () => 'unknown',
    });
    const file = f.stateFile();
    const other = { ...identity, sendId: 'other' };
    if (kind === 'corrupt') fs.writeFileSync(file, '{broken');
    if (kind === 'mismatch')
      saveWatchState(file, {
        ...other,
        notification: 'pending',
        response: { kind: 'waiting', decision: 'decision-1' },
      });
    if (kind === 'mode') fs.chmodSync(file, 0o644);
    if (kind === 'symlink') {
      fs.unlinkSync(file);
      fs.symlinkSync('missing', file);
    }
    const before =
      kind === 'symlink'
        ? fs.readlinkSync(file)
        : fs.readFileSync(file, 'utf8');
    await assert.rejects(dispatchWatchLocal(f.dir, identity, target, f.deps));
    assert.deepEqual(f.messages, []);
    assert.equal(
      kind === 'symlink'
        ? fs.readlinkSync(file)
        : fs.readFileSync(file, 'utf8'),
      before,
    );
  }
});

test('送達後の保存失敗でも対処結果を保ち同じIDで再照合する', async (t) => {
  const f = setup(t);
  await dispatchWatchLocal(f.dir, identity, target, {
    ...f.deps,
    deliver: async () => 'unknown',
  });
  const file = f.stateFile();
  saveWatchState(file, {
    ...identity,
    notification: 'pending',
    response: { kind: 'operation', evidenceId: 'evidence-1' },
  });
  await assert.rejects(
    dispatchWatchLocal(f.dir, identity, target, {
      ...f.deps,
      deliver: async (message) => {
        f.messages.push(message.notificationId);
        fs.chmodSync(f.dir, 0o755);
        return 'delivered';
      },
    }),
  );
  fs.chmodSync(f.dir, 0o700);
  assert.deepEqual(resumeWatchState(file, identity)?.response, {
    kind: 'operation',
    evidenceId: 'evidence-1',
  });
  assert.equal(
    await dispatchWatchLocal(f.dir, identity, target, f.deps),
    'delivered',
  );
  assert.deepEqual(f.messages, ['notice-1', 'notice-1']);
  assert.deepEqual(resumeWatchState(file, identity)?.response, {
    kind: 'operation',
    evidenceId: 'evidence-1',
  });
});
