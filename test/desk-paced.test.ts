import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import path from 'node:path';
import { executePaced, parsePaced, runPaced } from '../scripts/desk-paced.ts';

const args = [
  '--method',
  'POST',
  '--path',
  '/api/comment',
  '--body-file',
  '/home/u/work/.tmp/retry.json',
];
const options = () =>
  parsePaced([
    '--sequence',
    'test',
    '--interval',
    '2',
    '--',
    'operation',
    ...args,
  ]);

test('間隔制御は同じK2の汎用窓口へ送り拒否の終了コードも保持する', async () => {
  const calls: unknown[] = [];
  const spawnChild: any = (binary: string, argv: string[], config: object) => {
    calls.push([binary, argv, config]);
    const child = new EventEmitter();
    queueMicrotask(() => child.emit('close', 1, null));
    return child;
  };
  const o = options();
  assert.deepEqual(
    await executePaced(o.command, o.args, undefined, spawnChild),
    { code: 1, signal: null },
  );
  assert.deepEqual(calls, [
    [
      '/usr/local/bin/desk',
      ['operation', '--config', '/state/config.local.json', ...args],
      { shell: false, stdio: 'inherit' },
    ],
  ]);
  for (const bad of [
    args.slice(0, -2),
    ['--method', 'GET', ...args.slice(2)],
    [...args.slice(0, 4), '--body-file', 'relative.json'],
    ['--method', 'POST', '--path', '/api/other', ...args.slice(4)],
  ])
    assert.throws(() =>
      parsePaced([
        '--sequence',
        'test',
        '--interval',
        '2',
        '--',
        'operation',
        ...bad,
      ]),
    );
});

test('API操作への置換後も前回終了からの待機と結果永続化を保つ', async () => {
  let state: any = null;
  let ns = 0n;
  let locked = false;
  const starts: bigint[] = [];
  const store = {
    acquire: async () => {
      assert.equal(locked, false);
      locked = true;
    },
    read: async () => state,
    write: async (s: unknown) => {
      state = structuredClone(s);
    },
    release: async () => {
      locked = false;
    },
  };
  const deps = {
    store,
    time: {
      mono: () => ns,
      wall: () => new Date(Number(ns / 1_000_000n)).toISOString(),
      sleep: async (ms: number) => {
        ns += BigInt(ms) * 1_000_000n;
      },
    },
    execute: async () => {
      starts.push(ns);
      return { code: 0, signal: null };
    },
    protect: async () => ({ release: async () => {} }),
  };
  await runPaced(options(), deps);
  await runPaced(options(), deps);
  assert.deepEqual(starts, [0n, 2_000_000_000n]);
  assert.equal(state.phase, 'finished');
  assert.equal(state.waitedNs, '2000000000');
  assert.equal(locked, false);
});

test('入口は .ts の名前だけを受け付け、改名前の .mjs の名前は断る', async () => {
  const name = (command: string, ...rest: string[]) =>
    parsePaced([
      '--sequence',
      'test',
      '--interval',
      '2',
      '--',
      command,
      ...rest,
    ]);
  assert.equal(
    name('create-issue.ts', '--draft', 'd-1').command,
    'create-issue.ts',
  );
  assert.throws(() => name('create-issue.ts', '--project', 'o/r'), /新規相談/);
  assert.throws(() => name('create-issue.ts'), /--draft/);
  assert.equal(name('reply-issue.ts').command, 'reply-issue.ts');
  for (const old of ['create-issue.mjs', 'reply-issue.mjs', 'desk-ops.ts'])
    assert.throws(() => name(old, '--draft', 'd-1'), /対象は/);
  const calls: [string, string[]][] = [];
  const spawnChild: any = (binary: string, argv: string[]) => {
    calls.push([binary, argv]);
    const child = new EventEmitter();
    queueMicrotask(() => child.emit('close', 0, null));
    return child;
  };
  await executePaced(
    'reply-issue.ts',
    ['--number', '1'],
    undefined,
    spawnChild,
  );
  assert.deepEqual(calls, [
    [
      process.execPath,
      [path.resolve('scripts/reply-issue.ts'), '--number', '1'],
    ],
  ]);
});
