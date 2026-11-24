import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { dispatchWatch } from '../scripts/codex-issue-watch-dispatch.ts';
import {
  resumeWatchState,
  saveWatchState,
  type WatchState,
} from '../scripts/codex-issue-watch-state.ts';

const identity = {
  sendId: 'send-1',
  conversationId: 'conversation-1',
  targetId: 'desk:101',
  notificationId: 'notice-1',
};
const target = { type: 'issue', desk: 101, received: true } as const;
const response = (
  group: 'attention' | 'stopped' | 'waiting' | 'running',
  desk = 101,
) => ({
  partial: false,
  unavailable: [] as string[],
  groups: {
    attention: [],
    stopped: [],
    waiting: [],
    running: [],
    [group]: [
      {
        kind: 'issue',
        desk,
        work: {
          state: group,
          reason: 'reason',
          nextActor: 'user',
          nextAction: 'check',
          waitCondition: 'wait',
          resumeBy: 'later',
        },
      },
    ],
  },
});
function fixture(t: any) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'watch-dispatch-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'state');
  let data = response('attention');
  const events: any[] = [];
  const deps = {
    readClassification: async () => {
      events.push('classify');
      return data;
    },
    resume: (id: typeof identity) => resumeWatchState(file, id),
    save: (state: WatchState) => {
      events.push(state.notification);
      saveWatchState(file, state);
    },
    deliver: async (message: any) => {
      events.push(message);
      return 'delivered';
    },
  };
  return {
    file,
    deps,
    events,
    setData: (value: typeof data) => {
      data = value;
    },
  };
}

test('対象の最新分類に応じて送達し、それ以外は操作しない', async (t) => {
  for (const group of ['attention', 'stopped'] as const) {
    const f = fixture(t);
    f.setData(response(group));
    assert.equal(await dispatchWatch(identity, target, f.deps), 'delivered');
    assert.deepEqual(f.events, [
      'classify',
      'pending',
      {
        notificationId: 'notice-1',
        conversationId: 'conversation-1',
        target: 'desk:101',
        state: group === 'attention' ? 'decision' : 'held',
      },
      'delivered',
    ]);
  }
  for (const data of [
    response('waiting'),
    response('running'),
    response('attention', 102),
    { ...response('attention'), partial: true },
    { ...response('attention'), unavailable: ['issues'] },
    { ...response('attention'), groups: {} as any },
  ]) {
    const f = fixture(t);
    f.setData(data);
    assert.equal(await dispatchWatch(identity, target, f.deps), 'unconfirmed');
    assert.deepEqual(f.events, ['classify']);
    assert.equal(fs.existsSync(f.file), false);
  }
  const f = fixture(t);
  assert.equal(
    await dispatchWatch({ ...identity, targetId: 'desk:102' }, target, f.deps),
    'invalid',
  );
  assert.deepEqual(f.events, []);
});

test('不明な送達を同じIDで照合し、対処回答を再起動と保存失敗後も保持する', async (t) => {
  const f = fixture(t);
  saveWatchState(f.file, {
    ...identity,
    notification: 'pending',
    response: { kind: 'investigated', evidenceId: 'evidence-1' },
  });
  const messages: any[] = [];
  f.deps.deliver = async (message) => {
    messages.push(message);
    throw Error('response lost');
  };
  assert.equal(await dispatchWatch(identity, target, f.deps), 'unknown');
  assert.equal(
    resumeWatchState(f.file, identity)?.response.kind,
    'investigated',
  );
  const restarted = spawnSync(
    process.execPath,
    [
      '--input-type=module',
      '-e',
      `
    import { dispatchWatch } from './scripts/codex-issue-watch-dispatch.ts';
    import { resumeWatchState, saveWatchState } from './scripts/codex-issue-watch-state.ts';
    const id = JSON.parse(process.argv[2]);
    const target = { type: 'issue', desk: 101, received: true };
    const data = JSON.parse(process.argv[3]);
    const status = await dispatchWatch(id, target, {
      readClassification: async () => data,
      resume: identity => resumeWatchState(process.argv[1], identity),
      save: state => saveWatchState(process.argv[1], state),
      deliver: async message => { console.log(JSON.stringify(message)); return 'unknown'; },
    });
    console.log(status);
  `,
      f.file,
      JSON.stringify(identity),
      JSON.stringify(response('attention')),
    ],
    { encoding: 'utf8' },
  );
  assert.equal(restarted.status, 0, restarted.stderr);
  const [message, status] = restarted.stdout.trim().split('\n');
  assert.equal(JSON.parse(message).notificationId, identity.notificationId);
  assert.equal(status, 'unknown');
  assert.deepEqual(resumeWatchState(f.file, identity)?.response, {
    kind: 'investigated',
    evidenceId: 'evidence-1',
  });
  f.deps.deliver = async (message) => {
    messages.push(message);
    return 'unknown';
  };
  assert.equal(await dispatchWatch(identity, target, f.deps), 'unknown');
  f.deps.deliver = async (message) => {
    messages.push(message);
    return 'delivered';
  };
  assert.equal(await dispatchWatch(identity, target, f.deps), 'delivered');
  assert.deepEqual(
    messages.map((m) => m.notificationId),
    ['notice-1', 'notice-1', 'notice-1'],
  );
  assert.deepEqual(resumeWatchState(f.file, identity)?.response, {
    kind: 'investigated',
    evidenceId: 'evidence-1',
  });
  f.events.length = 0;
  assert.equal(await dispatchWatch(identity, target, f.deps), 'delivered');
  assert.deepEqual(f.events, []);

  saveWatchState(f.file, {
    ...identity,
    notification: 'pending',
    response: { kind: 'waiting', decision: 'decision-1' },
  });
  f.deps.save = () => {
    throw Error('save failed');
  };
  const count = messages.length;
  await assert.rejects(dispatchWatch(identity, target, f.deps), /save failed/);
  assert.equal(messages.length, count);
  assert.deepEqual(resumeWatchState(f.file, identity)?.response, {
    kind: 'waiting',
    decision: 'decision-1',
  });
});

test('破損記録と送達後の保存失敗は対処回答を上書きしない', async (t) => {
  const f = fixture(t);
  fs.writeFileSync(f.file, '{broken', { mode: 0o600 });
  await assert.rejects(dispatchWatch(identity, target, f.deps));
  assert.deepEqual(f.events, []);
  assert.equal(fs.readFileSync(f.file, 'utf8'), '{broken');
  fs.writeFileSync(
    f.file,
    JSON.stringify({
      ...identity,
      notification: 'pending',
      response: { kind: 'operation', evidenceId: 'operation-1' },
    }),
  );
  const save = f.deps.save;
  f.deps.save = (state) => {
    if (state.notification === 'delivered') throw Error('save failed');
    save(state);
  };
  await assert.rejects(dispatchWatch(identity, target, f.deps), /save failed/);
  assert.deepEqual(resumeWatchState(f.file, identity)?.response, {
    kind: 'operation',
    evidenceId: 'operation-1',
  });
  assert.equal(resumeWatchState(f.file, identity)?.notification, 'pending');
  f.deps.save = save;
  f.events.length = 0;
  assert.equal(await dispatchWatch(identity, target, f.deps), 'delivered');
  assert.equal(f.events[2].notificationId, identity.notificationId);
  assert.deepEqual(resumeWatchState(f.file, identity)?.response, {
    kind: 'operation',
    evidenceId: 'operation-1',
  });
});
