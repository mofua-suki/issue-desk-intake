import assert from 'node:assert/strict';
import { test } from 'node:test';
import { requestPayload } from '../shared/src/request-rules.ts';

const head = 'a'.repeat(40);
// 利用者がチャットで指示した日時（今の1分前）。
const at = new Date(Date.now() - 60_000).toISOString();

test('merge_manual keeps a trimmed approval only when given', () => {
  assert.deepEqual(
    requestPayload('merge_manual', 'o/r', {
      number: 30,
      head,
      approval: ' 30はマージして ',
      instructionAt: '2026-11-11T11:58:00+09:00',
    }),
    {
      payload: {
        number: 30,
        head,
        approval: '30はマージして',
        instructionAt: '2026-11-11T11:58:00+09:00',
      },
    },
  );
  // 承認発言には指示の日時が要り、発言の無い日時は断る。
  assert.ok(
    requestPayload('merge_manual', 'o/r', {
      number: 30,
      head,
      approval: '30はマージして',
    }).problem,
  );
  assert.ok(
    requestPayload('merge_manual', 'o/r', {
      number: 30,
      head,
      approval: '30はマージして',
      instructionAt: '2026-11-11T11:58:00',
    }).problem,
  );
  assert.ok(
    requestPayload('merge_manual', 'o/r', {
      number: 30,
      head,
      instructionAt: '2026-11-11T11:58:00+09:00',
    }).problem,
  );
  assert.deepEqual(
    requestPayload('merge_manual', 'o/r', { number: 30, head }),
    { payload: { number: 30, head } },
  );
  assert.ok(
    requestPayload('merge_manual', 'o/r', { number: 30, head, approval: 3 })
      .problem,
  );
});

test('pr_create は事前承認を指示日時付きで残し、merge_manual は approvalRequest を承認と別に受け付ける', () => {
  const instructionAt = '2026-11-11T11:58:00+09:00';
  assert.deepEqual(
    requestPayload('pr_create', 'o/r', {
      branch: 'chat/x',
      title: 't',
      body: 'b',
      approval: ' マージまで進めて ',
      instructionAt,
    }),
    {
      payload: {
        branch: 'chat/x',
        title: 't',
        body: 'b',
        approval: 'マージまで進めて',
        instructionAt,
      },
    },
  );
  assert.ok(
    requestPayload('pr_create', 'o/r', {
      branch: 'chat/x',
      title: 't',
      body: 'b',
      approval: 'マージまで進めて',
    }).problem,
  );
  assert.deepEqual(
    requestPayload('merge_manual', 'o/r', {
      number: 30,
      head,
      approvalRequest: '123-aabbccdd',
    }),
    { payload: { number: 30, head, approvalRequest: '123-aabbccdd' } },
  );
  assert.ok(
    requestPayload('merge_manual', 'o/r', {
      number: 30,
      head,
      approvalRequest: '123-AABBCCDD',
    }).problem,
  );
  assert.ok(
    requestPayload('merge_manual', 'o/r', {
      number: 30,
      head,
      approvalRequest: '123-aabbccdd',
      approval: '30はマージして',
      instructionAt,
    }).problem,
  );
});

test('上限超過の返信承認発言は指示の日時を必須にして送る', async () => {
  const { parseArgs, replyIssue } = await import('../scripts/reply-issue.ts');
  const text = '止まった原因を直したので、この条件で再試行してください';
  const base = [
    '--project',
    'o/r',
    '--number',
    '3',
    '--text',
    text,
    '--over-limit-statement',
    '共通番号3を上限を超えて再試行して',
  ];
  assert.throws(() => parseArgs(base), /--instruction-at/);
  assert.throws(
    () =>
      parseArgs([
        '--project',
        'o/r',
        '--number',
        '3',
        '--text',
        text,
        '--instruction-at',
        at,
      ]),
    /--over-limit-statement/,
  );
  assert.equal(
    parseArgs([...base, '--instruction-at', at])['instruction-at'],
    at,
  );
  const bodies = [];
  const call = async (method, _route, body) => {
    if (method === 'POST') bodies.push(body);
    return { status: 'failed', project: 'o/r', number: 3 };
  };
  await replyIssue(
    call,
    parseArgs([...base, '--instruction-at', at]),
    text,
    () => {},
    { remote: true },
  );
  assert.equal(
    bodies[0].overLimitStatement,
    '共通番号3を上限を超えて再試行して',
  );
  assert.equal(bodies[0].instructionAt, at);
  await assert.rejects(
    replyIssue(
      call,
      {
        ...parseArgs([...base, '--instruction-at', at]),
        'instruction-at': undefined,
      },
      text,
      () => {},
      { remote: true },
    ),
    /--instruction-at/,
  );
});
