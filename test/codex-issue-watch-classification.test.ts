import { test } from 'node:test';
import assert from 'node:assert/strict';
import { classifyWatchTargets } from '../scripts/codex-issue-watch-classification.ts';
import type { WatchTarget } from '../scripts/codex-issue-watch-classification.ts';

const work = (state: string, reason: string) => ({
  state,
  reason,
  nextActor: '利用者',
  nextAction: '確認する',
  ...(state === 'waiting'
    ? { waitCondition: '依存先の完了', resumeBy: '自動再開' }
    : {}),
});
const response = () => ({
  counts: {
    attention: 1,
    running: 0,
    waiting: 1,
    stopped: 2,
    inProgress: 0,
    followups: 0,
  },
  checkedAt: '2026-11-23T00:00:00Z',
  scope: 'active',
  partial: false,
  unavailable: [] as string[],
  unreadProjects: [] as { project: string; reason: string }[],
  groups: {
    attention: [
      {
        kind: 'draft',
        id: 'parent',
        status: 'running',
        work: work('attention', '承認待ち'),
      },
    ],
    running: [],
    waiting: [
      {
        kind: 'issue',
        project: 'a/repo',
        number: 2,
        desk: 102,
        status: 'failed',
        work: work('waiting', '通常依存待ち'),
      },
    ],
    stopped: [
      {
        kind: 'issue',
        project: 'a/repo',
        number: 1,
        desk: 101,
        status: 'running',
        work: work('stopped', '明示停止'),
      },
      {
        kind: 'issue',
        project: 'a/repo',
        number: 3,
        desk: 103,
        status: 'running',
        work: work('stopped', '判断根拠なし'),
      },
      {
        kind: 'issue',
        project: 'b/repo',
        number: 1,
        desk: 201,
        status: 'running',
        work: work('stopped', '別会話'),
      },
    ],
  },
  families: [
    { kind: 'draft', id: 'other-parent', project: 'a/repo', number: 1 },
  ],
});
const targets: WatchTarget[] = [
  { type: 'draft', id: 'parent', received: true },
  { type: 'issue', project: 'a/repo', number: 1, received: true },
  { type: 'issue', desk: 102, received: true },
  { type: 'issue', desk: 103, project: 'a/repo', number: 3, received: true },
];

test('受領対象に画面の分類と次の行動を対応させる', () => {
  const data = response();
  const result = classifyWatchTargets(targets, data);
  assert.deepEqual(result, [
    {
      target: targets[0],
      status: 'confirmed',
      group: 'attention',
      work: data.groups.attention[0].work,
    },
    {
      target: targets[1],
      status: 'confirmed',
      group: 'stopped',
      work: data.groups.stopped[0].work,
    },
    {
      target: targets[2],
      status: 'confirmed',
      group: 'waiting',
      work: data.groups.waiting[0].work,
    },
    {
      target: targets[3],
      status: 'confirmed',
      group: 'stopped',
      work: data.groups.stopped[1].work,
    },
  ]);
  assert.equal(
    classifyWatchTargets(
      [{ type: 'draft', id: 'other-parent', received: true }],
      data,
    )[0].status,
    'unconfirmed',
  );
});

test('欠測、不一致、未受領、重複は未確認で入力を変更しない', () => {
  const data = response();
  const before = structuredClone(data);
  const missing: WatchTarget[] = [
    { type: 'issue', project: 'missing/repo', number: 1, received: true },
    { type: 'issue', desk: 1, received: true },
    { type: 'issue', desk: 101, project: 'b/repo', number: 1, received: true },
    { type: 'issue', desk: 102, received: false },
    { type: 'draft', id: '', received: true },
  ];
  assert.deepEqual(
    classifyWatchTargets(missing, data).map((r) => r.status),
    missing.map(() => 'unconfirmed'),
  );
  assert.deepEqual(data, before);
  for (const incomplete of [
    { ...data, partial: true },
    { ...data, unavailable: ['drafts'] },
    { ...data, unreadProjects: [{ project: 'a/repo', reason: '取得失敗' }] },
    {
      ...data,
      groups: { attention: [], running: [], waiting: [], stopped: [] },
    },
    {
      ...data,
      groups: {
        ...data.groups,
        stopped: [...data.groups.stopped, data.groups.stopped[0]],
      },
    },
  ]) {
    assert.equal(
      classifyWatchTargets([targets[1]], incomplete)[0].status,
      'unconfirmed',
    );
  }
  assert.equal(
    classifyWatchTargets([{ type: 'issue', desk: 101, received: true }], {
      ...data,
      unreadProjects: [{ project: 'other/repo', reason: '取得失敗' }],
    })[0].status,
    'unconfirmed',
  );
});
