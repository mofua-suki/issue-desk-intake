import test from 'node:test';
import assert from 'node:assert/strict';
import { waitReplyRequest } from '../src/ui/reply-request.ts';
const expected = {
  id: '11111111-1111-4111-8111-111111111118',
  project: 'owner/repo',
  number: 1,
};
const receipt = { ...expected, accepted: true, status: 'queued' };

test('返信の受理後は同IDをGETして適用結果を待つ', async () => {
  const paths: string[] = [];
  let reads = 0;
  const result = await waitReplyRequest(
    receipt,
    expected,
    async (path) => {
      paths.push(path);
      return ++reads === 1
        ? receipt
        : { ...receipt, status: 'done', result: { job: 4 } };
    },
    { sleep: async () => {} },
  );
  assert.deepEqual(result, { job: 4 });
  assert.deepEqual(paths, [
    `/api/comment?id=${expected.id}`,
    `/api/comment?id=${expected.id}`,
  ]);
});

test('返信の失敗・未知の状態・対象違い・結果欠測を成功と扱わない', async () => {
  for (const state of [
    { ...receipt, status: 'failed', error: '上限で拒否' },
    { ...receipt, status: 'unknown' },
    { ...receipt, id: '別の送信' },
    { ...receipt, status: 'done' },
  ])
    await assert.rejects(
      waitReplyRequest(
        state,
        expected,
        async () => {
          throw Error('送信しない');
        },
        { sleep: async () => {} },
      ),
    );
});

test('待機期限は返信を重ねず未確認として同じIDを残す', async () => {
  let at = 0,
    reads = 0;
  await assert.rejects(
    waitReplyRequest(
      receipt,
      expected,
      async () => {
        reads++;
        return receipt;
      },
      {
        sleep: async (ms) => {
          at += ms;
        },
        now: () => at,
        timeout: 1,
      },
    ),
    new RegExp(expected.id),
  );
  assert.equal(reads, 1);
});
