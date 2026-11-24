import { test } from 'node:test';
import assert from 'node:assert/strict';
import { runCommand } from '../scripts/record-execution-check.ts';

const id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
test('共通番号の条件を読んでから確認記録だけを送る', async () => {
  const calls = [];
  const code = await runCommand(
    ['--desk', '42', '--file', 'fixture', '--id', id],
    {
      read: () =>
        JSON.stringify({
          requirement: 'deploy',
          planSHA: 'a'.repeat(64),
          head: 'b'.repeat(40),
          generation: 1,
          status: 'passed',
          source: 'host-ssh:status',
          observedAt: '2026-11-13T08:00:00+09:00',
          detail: '稼働版が一致',
        }),
      connection: () => null,
      log: () => {},
      error: assert.fail,
      open: async () => ({
        close: async () => {},
        call: async (method, route, body) => {
          calls.push({ method, route, body });
          return method === 'GET'
            ? { executionProgress: { checks: [{ id: 'deploy' }] } }
            : { id, executionProgress: { pending: 0 } };
        },
      }),
    },
  );
  assert.equal(code, 0);
  assert.deepEqual(
    calls.map((c) => [c.method, c.route]),
    [
      ['GET', '/api/issue?desk=42'],
      ['POST', '/api/comment'],
    ],
  );
  assert.equal(calls[1].body.action, 'record-check');
  assert.equal(calls[1].body.desk, 42);
  assert.equal(calls[1].body.executionCheck.id, id);
  assert.equal(calls[1].body.action === 'reply', false);
});
test('別の条件には送信しない', async () => {
  let posts = 0;
  const code = await runCommand(
    ['--desk', '42', '--file', 'fixture', '--id', id],
    {
      read: () => '{"requirement":"other"}',
      connection: () => null,
      log: () => {},
      error: () => {},
      open: async () => ({
        close: async () => {},
        call: async (method) => {
          if (method === 'POST') posts++;
          return { executionProgress: { checks: [{ id: 'deploy' }] } };
        },
      }),
    },
  );
  assert.equal(code, 1);
  assert.equal(posts, 0);
});
