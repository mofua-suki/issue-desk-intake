import assert from 'node:assert/strict';
import test from 'node:test';
import {
  approvalInstructionAt,
  choiceApprovalStatement,
  meaningfulApprovalStatement,
} from '../shared/src/approval-statement.ts';

test('記号の回答は質問文とまとめて承認の文になる', () => {
  const text = choiceApprovalStatement(
    '共通番号4541を上限を超えて再試行しますか',
    'A',
  );
  assert.match(text, /4541/);
  assert.match(text, /回答: A$/);
  assert.ok(meaningfulApprovalStatement(text));
  assert.ok(choiceApprovalStatement('PR 15 をマージしますか', '1-A'));
});

test('番号の無い質問と記号でない回答は断る', () => {
  assert.throws(() =>
    choiceApprovalStatement('上限を超えて再試行しますか', 'A'),
  );
  assert.throws(() =>
    choiceApprovalStatement('共通番号4541を再試行しますか', 'いいと思う'),
  );
  assert.throws(() =>
    choiceApprovalStatement('共通番号4541を再試行しますか', ''),
  );
});

test('利用者がチャットで指示した日時はUTCオフセット付きで、未来でなく7日以内に限る', () => {
  const now = Date.parse('2026-11-11T03:00:00Z');
  assert.equal(
    approvalInstructionAt('2026-11-11T11:58:00+09:00', now),
    '2026-11-11T11:58:00+09:00',
  );
  assert.equal(
    approvalInstructionAt('2026-11-11T02:58:00.123Z', now),
    '2026-11-11T02:58:00.123Z',
  );
  for (const value of [
    undefined,
    '',
    '2026-11-11T11:58:00',
    '2026-11-11T11:58+09:00',
    '2026-11-11 11:58:00+09:00',
    '2026-02-30T11:58:00+09:00',
    1,
  ])
    assert.throws(
      () => approvalInstructionAt(value, now),
      /UTCオフセット付き|不正/,
    );
  assert.throws(
    () => approvalInstructionAt('2026-11-11T12:01:00+09:00', now),
    /未来/,
  );
  assert.throws(
    () => approvalInstructionAt('2026-11-04T11:59:00+09:00', now),
    /7日より前/,
  );
});
