import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { poll, privateDirectory } from '../scripts/codex-issue-watch.ts';
import { dispatchWatch } from '../scripts/codex-issue-watch-dispatch.ts';

const rootId = 'a0000000-0000-0000-0000-000000000000';
const childId = 'b0000000-0000-0000-0000-000000000000';
const threadId = 'c0000000-0000-0000-0000-000000000000';
const operation = 'd0000000-0000-0000-0000-000000000000';
const project = 'owner/app';

function fixture() {
  const root = {
    id: rootId,
    draftId: rootId,
    requestId: rootId,
    accepted: true,
    project,
    consultationProvider: 'codex',
    status: 'submitted',
    splitChildren: [childId],
  };
  const child = {
    id: childId,
    parentSplit: rootId,
    project,
    status: 'submitted',
    issue: { number: 3, project },
  };
  const issue = {
    project,
    number: 3,
    desk: 42,
    body: '対象依頼',
    comments: [],
    closedAt: '2026-11-23T00:00:00Z',
    status: 'waiting',
    state: 'open',
    result: {} as { status?: string },
    pr: {} as { number?: number; merged?: boolean },
    closeReason: {} as { reason?: string; source?: string },
    mergeEvidence: {} as {
      repo?: string;
      pr?: number;
      mergeCommitSha?: string;
    },
    pendingFinalApprovals: [] as object[],
  };
  const checkpoints = new Map();
  const states = new Map();
  const evidence = new Map();
  const deliveries: object[] = [];
  const notices: object[] = [];
  const settings = {
    now: 1700000000000,
    group: 'stopped',
    partial: false,
    delivery: 'delivered',
    operationNumber: 3,
  };
  const runtime: Parameters<typeof poll>[0] = {
    host: 'local',
    now: () => settings.now,
    records: [
      { sendId: rootId, threadId, host: 'local', receiptState: 'unreconciled' },
    ],
    get: async (route) => {
      if (route === '/api/list?view=work') {
        const groups = {
          attention: [],
          running: [],
          waiting: [],
          stopped: [],
        } as Record<string, object[]>;
        groups[settings.group].push({
          kind: 'issue',
          project,
          number: 3,
          desk: 42,
          work: {
            state: settings.group,
            nextActor: '受付',
            reason: '原因',
            nextAction: '原因を読む',
            waitCondition: '依存PR',
            resumeBy: '受付',
          },
        });
        return { groups, partial: settings.partial, unavailable: [] };
      }
      if (route === '/api/drafts/' + rootId) return root;
      if (route === '/api/drafts/' + childId + '?view=summary') return child;
      if (route === '/api/issue?project=owner%2Fapp&number=3&verifyMerge=true')
        return issue;
      if (route === '/api/comment?id=' + operation)
        return {
          id: operation,
          status: 'done',
          project,
          number: settings.operationNumber,
          accepted: true,
          action: 'reply',
          createdAt: new Date(settings.now + 1000).toISOString(),
          result: { id: 77, job: 90 },
        };
      throw Error('unexpected route');
    },
    stateDirectory: '',
    load: (key) => checkpoints.get(key),
    save: (key, value) => {
      checkpoints.set(key, value);
    },
    response: (key) => evidence.get(key),
    saveResponse: async (identity, response) => {
      const id = identity.notificationId;
      states.set(id, { ...states.get(id), response });
    },
    dispatch: async (_directory, identity, target, dependencies) =>
      dispatchWatch(identity, target, {
        ...dependencies,
        resume: () => states.get(identity.notificationId),
        save: (state) => {
          states.set(identity.notificationId, state);
        },
      }),
    deliver: async (message) => {
      deliveries.push(message);
      return settings.delivery;
    },
    notice: async (notice) => {
      notices.push(notice);
    },
  };
  return {
    runtime,
    root,
    child,
    issue,
    settings,
    checkpoints,
    states,
    evidence,
    deliveries,
    notices,
  };
}

test('登録からの巡回と再開', async () => {
  const f = fixture();
  f.root.accepted = false;
  assert.equal((await poll(f.runtime)).unconfirmed, 1);
  f.root.accepted = true;
  f.root.requestId = childId;
  await poll(f.runtime);
  f.root.requestId = rootId;
  f.child.parentSplit = childId;
  await poll(f.runtime);
  f.child.parentSplit = rootId;
  f.runtime.records[0].host = 'another-host';
  await poll(f.runtime);
  f.runtime.records[0].host = 'local';
  f.settings.group = 'waiting';
  await poll(f.runtime);
  assert.equal(f.deliveries.length, 0);
  f.settings.group = 'stopped';
  f.settings.partial = true;
  await poll(f.runtime);
  assert.equal(f.deliveries.length, 0);
  f.settings.partial = false;
  await poll(f.runtime);
  await poll({ ...f.runtime });
  assert.equal(f.deliveries.length, 1);
  assert.deepEqual(f.deliveries[0], {
    conversationId: threadId,
    notificationId: [...f.states.keys()][0],
    target: 'desk:42',
    state: 'held',
  });
});

test('巡回の期限に達したら次の登録を後続の回へ送る', async () => {
  const f = fixture();
  const following = { ...f.runtime.records[0], sendId: childId };
  let completed = 0;
  f.runtime.records.push(following);
  f.runtime.deadline = f.settings.now + 40_000;
  f.runtime.processed = () => {
    completed++;
    f.settings.now += 6000;
  };

  await poll(f.runtime);

  assert.equal(completed, 1);
  assert.equal(f.deliveries.length, 1);
});

test('子相談の読取が遅くても確認済みの停止中へ通知する時間を確保する', async () => {
  const f = fixture();
  const slow = 'e0000000-0000-0000-0000-000000000000';
  f.root.splitChildren.push(slow);
  f.runtime.deadline = f.settings.now + 80_000;
  const get = f.runtime.get;
  let childReads = 0;
  f.runtime.get = async (route) => {
    if (route === '/api/drafts/' + childId + '?view=summary') {
      f.settings.now += ++childReads === 1 ? 44_000 : 12_000;
    } else if (route === '/api/drafts/' + slow + '?view=summary') {
      f.settings.now += 12_000;
      return { id: slow, parentSplit: rootId, project, status: 'submitted' };
    } else if (route.startsWith('/api/issue?')) {
      f.settings.now += 12_000;
    }
    return get(route);
  };

  await poll(f.runtime);

  assert.equal(f.deliveries.length, 1);
});

test('配信と対処の照合', async () => {
  const f = fixture();
  f.settings.delivery = 'offline';
  await poll(f.runtime);
  assert.equal(f.notices.length, 1);
  const id = [...f.states.keys()][0];
  assert.equal(f.states.get(id).notification, 'pending');
  f.settings.delivery = 'delivered';
  await poll(f.runtime);
  assert.equal(f.states.get(id).notification, 'delivered');
  assert.deepEqual(f.states.get(id).response, { kind: 'pending' });
  f.settings.now += 11 * 60 * 1000;
  await poll(f.runtime);
  assert.equal(f.notices.length, 2);
  assert.notEqual((f.notices[1] as { id: string }).id, id);
  f.evidence.set(id, { action: 'waiting', reference: '不明' });
  await poll(f.runtime);
  assert.deepEqual(f.states.get(id).response, { kind: 'pending' });
  f.evidence.set(id, { action: 'read' });
  await poll(f.runtime);
  assert.deepEqual(f.states.get(id).response, {
    kind: 'investigated',
    evidenceId: id,
  });
  f.evidence.set(id, { action: 'operation', reference: operation });
  f.settings.operationNumber = 4;
  await poll(f.runtime);
  assert.equal(f.states.get(id).response.kind, 'investigated');
  f.settings.operationNumber = 3;
  f.settings.group = 'running';
  await poll(f.runtime);
  assert.deepEqual(f.states.get(id).response, {
    kind: 'operation',
    evidenceId: operation,
  });
  assert.equal(f.deliveries.length, 2);
});

test('保留解除と終了の照合', async () => {
  const f = fixture();
  await poll(f.runtime);
  f.settings.group = 'running';
  await poll(f.runtime);
  f.settings.group = 'stopped';
  await poll(f.runtime);
  assert.equal(f.deliveries.length, 2);
  f.issue.state = 'closed';
  assert.equal((await poll(f.runtime)).merged, 0);
  f.issue.result.status = 'merged';
  assert.equal((await poll(f.runtime)).merged, 0);
  f.issue.pr.number = 12;
  assert.equal((await poll(f.runtime)).merged, 0);
  f.issue.mergeEvidence = {
    repo: project,
    pr: 12,
    mergeCommitSha: 'a'.repeat(40),
  };
  assert.equal((await poll(f.runtime)).merged, 1);
  assert.equal(
    [...f.checkpoints.values()].some((state) => state.terminal === 'merged'),
    true,
  );
  f.issue.result = {};
  f.issue.pr = {};
  f.issue.closeReason = { reason: 'voluntary', source: 'token' };
  assert.equal((await poll(f.runtime)).withdrawn, 0);
  f.issue.mergeEvidence = {};
  assert.equal((await poll(f.runtime)).withdrawn, 1);
  Object.assign(f.issue, { pr: undefined });
  assert.equal((await poll(f.runtime)).withdrawn, 1);
  f.issue.pr = { number: 12 };
  assert.equal((await poll(f.runtime)).withdrawn, 0);
  f.issue.pr.merged = false;
  assert.equal((await poll(f.runtime)).withdrawn, 1);
  f.issue.pr.merged = true;
  assert.equal((await poll(f.runtime)).merged, 0);
  f.issue.pr.merged = false;
  f.issue.mergeEvidence = {
    repo: project,
    pr: 12,
    mergeCommitSha: 'a'.repeat(40),
  };
  assert.equal((await poll(f.runtime)).withdrawn, 0);
  assert.equal((await poll(f.runtime)).merged, 0);
  Object.assign(f.issue.closeReason, { route: { repo: project, pr: 12 } });
  f.issue.pr = { number: 13 };
  assert.equal((await poll(f.runtime)).merged, 0);
  f.issue.pr = {};
  assert.equal((await poll(f.runtime)).merged, 1);
});

test('読み取り上限に達した登録の後も別の停止中を通知する', async () => {
  const f = fixture();
  const slow = 'e0000000-0000-0000-0000-000000000000';
  f.runtime.records.unshift({ ...f.runtime.records[0], sendId: slow });
  const get = f.runtime.get;
  f.runtime.get = async (route) => {
    if (route !== '/api/drafts/' + slow) return get(route);
    f.settings.now += 46000;
    return { ...f.root, id: slow, requestId: slow, draftId: slow };
  };
  const completed: number[] = [];
  f.runtime.processed = () => {
    completed.push(f.settings.now);
  };
  const result = await poll(f.runtime);
  assert.equal(completed.length, 2);
  assert.ok(result.unconfirmed >= 1);
  assert.equal(f.deliveries.length, 1);
  assert.equal((f.deliveries[0] as { target: string }).target, 'desk:42');
});

test('分割子の読取上限で残った対象を次の巡回で個別に再照合する', async () => {
  const f = fixture();
  const slow = 'e0000000-0000-0000-0000-000000000000';
  f.root.splitChildren.unshift(slow);
  const get = f.runtime.get;
  let reads = 0;
  f.runtime.get = async (route) => {
    if (route !== '/api/drafts/' + slow + '?view=summary') return get(route);
    reads++;
    f.settings.now += 46000;
    return { id: slow, parentSplit: rootId, project, status: 'submitted' };
  };
  let rotation = 0;
  f.runtime.rotation = (_, draftId) => (draftId === rootId ? rotation++ : 0);
  await poll(f.runtime);
  assert.equal(f.deliveries.length, 0);
  await poll(f.runtime);
  assert.equal(f.deliveries.length, 1);
  assert.equal(reads, 2);
});

test('作成時にNASが広い権限を継承しても新規の保存先だけを私有にする', (t) => {
  const base = fs.mkdtempSync(
    path.join(os.tmpdir(), 'codex-watch-permissions-'),
  );
  t.after(() => fs.rmSync(base, { recursive: true, force: true }));
  const mkdir = fs.mkdirSync;
  t.mock.method(fs, 'mkdirSync', (directory, options) => {
    const created = mkdir(directory, options);
    if (created) fs.chmodSync(directory, 0o777);
    return created;
  });
  const state = path.join(base, 'state');
  privateDirectory(state, true);
  assert.equal(fs.statSync(state).mode & 0o777, 0o700);
  fs.chmodSync(state, 0o755);
  assert.throws(() => privateDirectory(state, true), /unsafe watch directory/);
  assert.equal(fs.statSync(state).mode & 0o777, 0o755);
});
