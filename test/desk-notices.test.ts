import test from 'node:test';
import assert from 'node:assert/strict';
import { parseArgs, runCommand } from '../scripts/desk-ops.ts';

const notices = [
  {
    argv: [
      'remote-control',
      '--signature',
      'aabbccdd',
      '--instances',
      'worker@1,worker-2',
    ],
    body: {
      kind: 'remote-control',
      signature: 'aabbccdd',
      instances: ['worker@1', 'worker-2'],
    },
  },
  {
    argv: ['remote-control', '--signature', 'aabbccdd', '--instances', ''],
    body: { kind: 'remote-control', signature: 'aabbccdd', instances: [] },
  },
  {
    argv: [
      'boot-check',
      '--signature',
      'aabbccdd',
      '--items',
      'disk:free,container',
    ],
    body: {
      kind: 'boot-check',
      signature: 'aabbccdd',
      items: ['disk:free', 'container'],
    },
  },
  {
    argv: ['rc-update', '--timer', 'disabled'],
    body: { kind: 'rc-update', timer: 'disabled' },
  },
  {
    argv: ['rc-update', '--timer', 'enabled'],
    body: { kind: 'rc-update', timer: 'enabled' },
  },
];

for (const { argv, body } of notices)
  test(`機械通知 ${argv.join(' ')} は同じAPIへ送信し接続を解放する`, async () => {
    const calls = [];
    let closed = 0;
    const code = await runCommand(['notice', ...argv], {
      env: {},
      configured: () => null,
      open: async () => ({
        call: async (...args) => {
          calls.push(args);
          return { id: 'notice-id' };
        },
        close: () => closed++,
      }),
      log: () => {},
    });
    assert.equal(code, 0);
    assert.deepEqual(calls, [['POST', '/api/v1/notices', body]]);
    assert.equal(closed, 1);
  });

test('廃止した手動操作と不正な通知は接続する前に拒否する', async () => {
  for (const argv of [
    ['finish', 'draft'],
    ['grant-add'],
    ['hostd'],
    ['proposal-repost'],
    ['notice', 'unknown'],
    [
      'notice',
      'remote-control',
      '--signature',
      'aabbccdd',
      '--instances',
      'bad\nname',
    ],
    [
      'notice',
      'boot-check',
      '--signature',
      'aabbccdd',
      '--items',
      'disk',
      '--instances',
      'worker',
    ],
    ['notice', 'rc-update', '--timer', 'enabled', '--force'],
  ]) {
    let opened = false;
    await assert.rejects(
      runCommand(argv, {
        open: async () => {
          opened = true;
          throw Error('接続してはいけない');
        },
      }),
    );
    assert.equal(opened, false);
  }
  assert.throws(() =>
    parseArgs([
      'notice',
      'remote-control',
      '--signature',
      'aabbccdd',
      '--signature',
      'aabbccdd',
      '--instances',
      '',
    ]),
  );
});

test('通知拒否を別認証で再送せず接続を解放する', async () => {
  let sends = 0,
    closed = 0,
    opened = 0;
  const failure = Object.assign(Error('拒否'), { status: 403 });
  await assert.rejects(
    runCommand(['notice', 'rc-update', '--timer', 'disabled'], {
      env: {},
      configured: () => null,
      open: async () => {
        opened++;
        return {
          call: async () => {
            sends++;
            throw failure;
          },
          close: () => closed++,
        };
      },
    }),
    (error) => error === failure,
  );
  assert.deepEqual(
    { sends, closed, opened },
    { sends: 1, closed: 1, opened: 1 },
  );
});
