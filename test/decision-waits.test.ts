import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  readDecisionDetails,
  aggregateDecisions,
  classifyDecision,
  decisionQuestions,
  recordDecisionAnswer,
} from '../shared/src/decision-waits.ts';

const row = (number, project = 'o/r') => ({
  kind: 'issue',
  project,
  number,
  desk: 100 + number,
  status: 'review_held',
});
const detail = (number, project = 'o/r', files = ['a', 'b']) => ({
  project,
  number,
  desk: 100 + number,
  status: 'review_held',
  generation: 2,
  retryable: true,
  permissionApproval: {
    project,
    head: String(number).repeat(40),
    files,
    pr: 7,
    generation: 2,
    epoch: 1,
  },
});

test('詳細取得を共有し権限の集合とprojectを分け全対象を保持する', async () => {
  const reason = ['a', 'b']
    .map(
      (file) =>
        `権限の範囲を定めるファイルの変更があります（利用者の承認が必要）: ${file}`,
    )
    .join('\n');
  const rows = [
    row(1),
    row(2),
    row(3),
    row(4),
    row(5, 'o/s'),
    {
      kind: 'attention',
      caseKind: 'review_held',
      project: 'o/r',
      number: 1,
      reason,
    },
    {
      kind: 'attention',
      caseKind: 'review_held',
      project: 'o/r',
      number: 1,
      reason: '別のレビュー判断',
    },
    { kind: 'attention', caseKind: 'ops_watch', project: 'o/r', number: 1 },
  ];
  const calls = [];
  const entries = await readDecisionDetails(rows, async (project, number) => {
    calls.push(`${project}#${number}`);
    return {
      ...detail(
        number,
        project,
        number === 2 ? ['b', 'a'] : number === 4 ? ['c'] : ['a', 'b'],
      ),
      result: { status: 'failed', summary: reason },
    };
  });
  const { groups, remaining } = aggregateDecisions(entries);
  assert.equal(calls.length, 5);
  assert.deepEqual(
    groups.map((g) => g.members.length),
    [3, 1, 1],
  );
  assert.deepEqual(
    groups[0].members.map((m) => m.desk),
    [101, 102, 103],
  );
  assert.equal(groups[0].entries.length, 3);
  assert.equal(remaining[0].row.reason, reason);
  assert.equal(remaining[1].row.reason, '別のレビュー判断');
  assert.equal(remaining[2].row.caseKind, 'ops_watch');
});

test('失敗と変化を個別に残し非候補は詳細を読まない', async () => {
  const rows = [
    row(1),
    { ...row(1), kind: 'attention', caseKind: 'review_held' },
    row(2),
    row(3),
    { ...row(4), status: 'failed' },
    { kind: 'attention', caseKind: 'usage_limit_wait', project: '' },
  ];
  let count = 0;
  const entries = await readDecisionDetails(rows, async (_p, n) => {
    count++;
    if (n === 1) throw Object.assign(Error('禁止'), { status: 403 });
    if (n === 2) return { ...detail(2), number: 9 };
    return { ...detail(3), status: 'queued' };
  });
  assert.equal(count, 3);
  assert.equal(entries[0].status, 403);
  assert.match(entries[1].failure, /禁止/);
  assert.match(entries[2].failure, /一致/);
  assert.match(entries[3].failure, /変わり/);
  assert.equal(entries[4].failure, undefined);
  assert.equal(entries[5].failure, undefined);
  assert.equal(aggregateDecisions(entries).remaining.length, 6);
});

test('対象のhead変更と詳細に見せかけたsummaryは集約せず理由を残す', async () => {
  const rows = [
    { ...row(1), permissionApproval: { head: 'old', files: ['a', 'b'] } },
    row(2),
    row(3),
  ];
  const entries = await readDecisionDetails(rows, async (_project, number) =>
    number === 1
      ? detail(1)
      : number === 2
        ? { ...detail(2), retryable: undefined, generation: undefined }
        : { ...detail(3), permissionApproval: { unavailable: '対象不明' } },
  );
  assert.match(entries[0].failure, /変わり/);
  // summaryにはフィールド自体が存在しない。
  const summary = await readDecisionDetails([row(2)], async () => ({
    project: 'o/r',
    number: 2,
    status: 'review_held',
  }));
  assert.match(summary[0].failure, /詳細応答/);
  assert.match(entries[2].failure, /照合/);
  assert.equal(aggregateDecisions(entries).groups.length, 0);
});

test('明示された上限と未知・停止・範囲拡大を区別する', () => {
  const d = {
    project: 'o/r',
    number: 1,
    status: 'waiting',
    generation: 100,
    retryable: true,
  };
  const classify = (text) =>
    classifyDecision({ ...d, result: { summary: text } }).operation;
  const final = classifyDecision({
    ...d,
    pendingFinalApprovals: [{ id: 'deploy', headSha: 'a'.repeat(40) }],
  });
  assert.equal(final.operation, undefined);
  assert.match(final.reason, /最終承認/);
  assert.equal(
    classify('実装の回数の上限（5回）に達しました。使用量は上限を超えていない'),
    'run_limit',
  );
  assert.equal(
    classify('累計使用量が上限を超えました。実装回数は上限を超えていない'),
    'usage_limit',
  );
  assert.equal(classify('実装の回数の上限（5回）に達しました。'), undefined);
  assert.equal(classify('累計使用量が上限を超えました'), undefined);
  assert.equal(
    classify('実装回数が上限を超えました。累計使用量が上限を超えました'),
    'run_and_usage',
  );
  assert.equal(classify('累計使用量が未知です'), undefined);
  assert.equal(classify('累計使用量が上限を超えていない'), undefined);
  assert.equal(
    classifyDecision({
      ...d,
      retryable: false,
      result: { summary: '実装回数が上限を超えました' },
    }).operation,
    undefined,
  );
  assert.equal(
    classifyDecision({ ...d, status: 'stopped' }).operation,
    'resume',
  );
  assert.equal(
    classifyDecision({
      ...d,
      status: 'stopped',
      result: { summary: '変更範囲が上限を超えました' },
    }).operation,
    undefined,
  );
});

test('異なる操作を混ぜず共通番号不明を補作しない', async () => {
  const rows = [
    row(1),
    { ...row(2), desk: null, status: 'waiting' },
    { ...row(3), status: 'waiting' },
    { ...row(4), status: 'stopped' },
  ];
  const entries = await readDecisionDetails(rows, async (_project, number) =>
    number === 1
      ? detail(1)
      : {
          project: 'o/r',
          number,
          desk: number === 2 ? null : 100 + number,
          status: number === 4 ? 'stopped' : 'waiting',
          generation: 4,
          retryable: true,
          result: {
            summary:
              number === 4
                ? '利用者が停止しました'
                : number === 2
                  ? '実装回数が上限を超えました。使用量は上限を超えていない'
                  : '累計使用量が上限を超えました。実装回数は上限を超えていない',
          },
        },
  );
  const groups = aggregateDecisions(entries).groups;
  assert.deepEqual(
    groups.map((g) => g.operation),
    ['permission', 'run_limit', 'usage_limit', 'resume'],
  );
  assert.equal(groups[1].members[0].desk, null);
  assert.deepEqual(decisionQuestions(groups[3]), []);
});

test('質問は全番号を保持し400 UTF-16単位を超える場合だけ分割する', () => {
  const group = {
    operation: 'permission',
    members: Array.from({ length: 12 }, (_, i) => ({
      project: 'o/r',
      number: i + 1,
      head: `h${i}`,
      files: ['a'],
    })),
  };
  const length = decisionQuestions(group)[0].question.length;
  const q400 = decisionQuestions(group, 'あ'.repeat(400 - length));
  assert.equal(q400.length, 1);
  assert.equal(q400[0].question.length, 400);
  const q401 = decisionQuestions(group, 'あ'.repeat(401 - length));
  assert.ok(q401.length > 1);
  assert.ok(q401.every((q) => q.question.length <= 400));
  assert.deepEqual(
    q401.flatMap((q) => q.members.map((m) => m.number)),
    group.members.map((m) => m.number),
  );
  group.members[0].head = 'changed';
  assert.equal(q401[0].members[0].head, 'h0');
  assert.throws(() => recordDecisionAnswer(q401[0], '別の質問', 'A'));
  const answered = recordDecisionAnswer(q401[0], q401[0].question, 'A');
  assert.deepEqual(answered.members, q401[0].members);
  assert.throws(() => answered.members.push(group.members[0]), TypeError);
  assert.throws(() => {
    q401[0].members[0].head = 'altered';
  }, TypeError);
  const emoji = decisionQuestions(group, '😀');
  assert.ok(emoji.every((q) => q.question.length <= 400));
});
