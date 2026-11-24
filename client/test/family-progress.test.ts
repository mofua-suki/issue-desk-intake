import test from 'node:test';
import assert from 'node:assert/strict';
import {
  requestRows,
  rowGroup,
  familyProgressText,
  familyProgressNote,
} from '../src/ui/request-classification.ts';
import {
  requestRows as sharedRows,
  rowGroup as sharedGroup,
} from '../../shared/src/desk-attention.mts';
import { sharedClientBuild } from './shared-client-build.ts';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

test('起票済みの分割親は仕事に二重計上せず、子Issueを残す', () => {
  const progress = {
    total: 2,
    completed: 1,
    closed: 0,
    remaining: 1,
    stopped: [
      {
        project: 'owner/repo',
        number: 3,
        desk: 42,
        status: 'failed',
        reason: '失敗',
      },
    ],
  };
  const draft = {
    id: 'parent',
    project: 'owner/repo',
    status: 'submitted',
    familyProgress: progress,
    issue: { number: 3 },
  };
  const rows = requestRows(
    [{ project: 'owner/repo', number: 3, status: 'failed' }],
    [draft],
  );
  assert.equal(
    rows.some((r) => r.kind === 'draft'),
    false,
  );
  assert.equal(rows.filter((r) => r.kind === 'issue').length, 1);
  assert.match(
    familyProgressText(progress),
    /完了 1\/2、残り 1、止まっている: 共通番号42 owner\/repo#3 failed（失敗）/,
  );
  assert.equal(
    requestRows(
      [],
      [{ ...draft, familyProgress: { ...progress, remaining: 0 } }],
    ).some((r) => r.kind === 'draft'),
    false,
  );
});

test('起票済みの分割親の注記は残りを示す', () => {
  const draft: any = {
    id: 'p',
    project: 'owner/repo',
    status: 'submitted',
    splitChildren: ['c'],
    issue: { number: 3 },
    issues: [{ number: 3 }],
    familyProgress: {
      total: 2,
      completed: 1,
      closed: 0,
      remaining: 1,
      stopped: [],
    },
  };
  const note = familyProgressNote(draft)!;
  assert.doesNotMatch(note, /起票済み|完了。/);
  assert.match(note, /完了 1\/2、残り 1/);
  assert.match(
    familyProgressNote({
      ...draft,
      familyProgress: { ...draft.familyProgress, remaining: 0 },
    })!,
    / · 完了$/,
  );
  assert.doesNotMatch(
    familyProgressNote({ ...draft, familyProgress: undefined })!,
    /起票済み|完了/,
  );
});

test('引き継ぎ相談は起票状態に関係なく両方の分類で完了し、記録として案内する', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroups } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const drafts = [
    {
      id: 'filed',
      status: 'project_handoff',
      projectHandoff: { id: 'target-1', status: 'registered', filing: 'filed' },
    },
    {
      id: 'unfiled',
      status: 'project_handoff',
      projectHandoff: {
        id: 'target-2',
        status: 'registered',
        filing: 'pending',
      },
    },
    {
      id: 'unknown',
      status: 'project_handoff',
      projectHandoff: { id: 'target-3' },
    },
    { id: 'missing', status: 'project_handoff' },
    {
      id: 'child',
      status: 'project_handoff',
      parentSplit: 'parent',
      projectHandoff: null,
    },
  ];
  const groups = requestGroups([], drafts);
  assert.deepEqual(
    groups.completed.map((row: { id: string }) => row.id),
    drafts.map((draft) => draft.id),
  );
  assert.equal(groups.issues.length, 0);
  for (const row of groups.completed) {
    assert.match(row.guidance, /引き継いだ記録/);
    assert.doesNotMatch(row.guidance, /起票済み|起票成功/);
  }
  const shared = sharedRows([], drafts);
  assert.deepEqual(
    shared.map((row) => row.id),
    drafts.map((draft) => draft.id),
  );
  for (const row of shared) assert.equal(sharedGroup(row), 'completed');
});

test('状態不明の子を完了と表示しない', () => {
  const progress = {
    total: 2,
    completed: 0,
    closed: 1,
    remaining: 0,
    unknown: 1,
    stopped: [],
  };
  assert.match(familyProgressText(progress), /確認不能 1/);
  assert.doesNotMatch(familyProgressText(progress), / · 完了/);
  assert.equal(
    requestRows(
      [],
      [
        {
          id: 'p',
          status: 'submitted',
          splitChildren: ['a', 'b'],
          familyProgress: progress,
        },
      ],
    ).length,
    0,
  );
});

test('旧分割親は照合済みの全子完了時だけ履歴に残る', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroups } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  const progress = {
    total: 2,
    completed: 1,
    closed: 1,
    remaining: 0,
    unknown: 0,
    stopped: [],
  };
  for (const status of ['draft', 'submit_error']) {
    const parent = {
      id: 'p',
      status,
      splitIssues: [],
      familyProgress: progress,
      actions: ['finish'],
    };
    const row = requestRows([], [parent])[0];
    assert.equal(rowGroup(row), 'completed');
    assert.equal(row.id, 'p');
    assert.equal(row.kind, 'draft');
    const groups = requestGroups([], [parent]);
    assert.equal(groups.issues.length, 0);
    assert.equal(groups.completed[0].id, 'p');
    assert.equal(groups.completed[0].kind, 'draft');
    for (const patch of [
      { actions: undefined },
      { actions: 'finish' },
      { actions: [] },
      { familyProgress: undefined },
      { familyProgress: { ...progress, remaining: 1 } },
      { familyProgress: { ...progress, unknown: undefined } },
      { familyProgress: { ...progress, unknown: 1 } },
      { familyProgress: { ...progress, completed: 0 } },
      { splitIssues: [{}] },
    ]) {
      const protectedRow = requestRows([], [{ ...parent, ...patch } as any])[0];
      assert.notEqual(rowGroup(protectedRow), 'completed');
    }
  }
});

test('マージ後も未確認条件があれば完了タブへ移さない', async () => {
  const { rowGroup, monitoringRequired } = await import(
    '../../shared/src/desk-attention.mts'
  );
  for (const executionProgress of [
    { pending: 1 },
    { unavailable: '証拠を読めません' },
  ]) {
    const row = { kind: 'issue', status: 'completed', executionProgress };
    assert.equal(rowGroup(row), 'stopped');
    assert.equal(monitoringRequired(row), true);
  }
  assert.equal(
    rowGroup({
      kind: 'issue',
      status: 'completed',
      executionProgress: { pending: 0 },
    }),
    'completed',
  );
});

test('クローズされたIssueの報告保留を一覧と監視に残す', async () => {
  const { destination, ui } = sharedClientBuild();
  const { requestGroups } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  for (const executionProgress of [
    { pending: 1 },
    { unavailable: '読取失敗' },
  ]) {
    const row = {
      project: 'owner/repo',
      number: 7,
      state: 'closed',
      status: 'review_held',
      executionProgress,
    };
    const groups = requestGroups([row], []);
    assert.equal(groups.stopped.length, 1);
    assert.equal(groups.monitoring.length, 1);
    assert.equal(groups.closed.length, 0);
    const approval = requestGroups(
      [{ ...row, requiresUser: true, nextAction: '上限超えを判断する' }],
      [],
    );
    assert.equal(approval.attention.length, 1);
    assert.equal(approval.monitoring.length, 0);
    const closed = requestGroups([{ ...row, status: 'closed' }], []);
    assert.equal(closed.closed.length, 1);
    assert.equal(closed.stopped.length, 0);
  }
});
