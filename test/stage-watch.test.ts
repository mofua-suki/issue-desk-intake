import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  parseArgs,
  resultFiles,
  runStageWatch,
  type Deps,
} from '../scripts/stage-watch.ts';

// 時計・ファイル数え・子の起動・タイマー・送信を差し替え、実ネットワークと長い待ちを使わない。
function rig({
  files = 5,
  failSend = false,
  start = '2026-10-20T10:00:00Z',
} = {}) {
  let clock = Date.parse(start);
  let finish: (code: number | null) => void = () => {};
  const timers: { fn: () => void; ms: number; cleared: boolean }[] = [];
  const out = {
    sent: [] as any[],
    logs: [] as string[],
    warns: [] as string[],
    results: [] as any[],
    removed: [] as string[],
    spawned: [] as any[],
  };
  const deps: Deps = {
    now: () => clock,
    countFiles: () => files,
    spawnChild: (command, args) => {
      out.spawned.push([command, args]);
      return { done: new Promise((resolve) => (finish = resolve)) };
    },
    setTimer: (fn, ms) => {
      const t = { fn, ms, cleared: false };
      timers.push(t);
      return t;
    },
    clearTimer: (h) => {
      (h as any).cleared = true;
    },
    send: async (notice) => {
      if (failSend)
        throw Object.assign(Error('https://secret.example/ token-abc'), {
          status: 503,
        });
      out.sent.push(notice);
    },
    writeResult: (name, result) => out.results.push({ name, ...result }),
    removeResult: (name) => out.removed.push(name),
    log: (l) => out.logs.push(l),
    warn: (l) => out.warns.push(l),
  };
  return {
    deps,
    out,
    timers,
    advance: (ms: number) => (clock += ms),
    end: async (code: number | null) => {
      finish(code);
    },
  };
}
const tick = () => new Promise((resolve) => setImmediate(resolve));

test('引数: 必須・形式・対・-- の後のコマンドを検査する', () => {
  const ok = parseArgs(['--name', 'night5', '--', 'node', 'x.js']);
  assert.deepEqual(ok, {
    name: 'night5',
    expectEnd: null,
    graceMin: 10,
    outputs: null,
    minOutputs: null,
    command: ['node', 'x.js'],
  });
  const full = parseArgs([
    '--name',
    'a.b_c-1',
    '--expect-end',
    '2026-10-20T10:05:00+09:00',
    '--grace-min',
    '0',
    '--outputs',
    'o',
    '--min-outputs',
    '3',
    '--',
    'x',
  ]);
  assert.equal(full.graceMin, 0);
  assert.equal(full.minOutputs, 3);
  for (const bad of [
    ['--', 'x'],
    ['--name', 'a'],
    ['--name', 'a', '--'],
    ['--name', 'a b', '--', 'x'],
    ['--name', 'a'.repeat(65), '--', 'x'],
    ['--name', 'a', '--expect-end', '2026-10-20', '--', 'x'],
    ['--name', 'a', '--expect-end', '2026-13-45T10:00:00Z', '--', 'x'],
    ['--name', 'a', '--grace-min', '-1', '--', 'x'],
    ['--name', 'a', '--grace-min', '1.5', '--', 'x'],
    ['--name', 'a', '--outputs', 'o', '--', 'x'],
    ['--name', 'a', '--min-outputs', '1', '--', 'x'],
    ['--name', 'a', '--min-outputs', 'x', '--outputs', 'o', '--', 'x'],
    ['--name', 'a', '--name', 'b', '--', 'x'],
    ['--name', 'a', '--unknown', '1', '--', 'x'],
    ['--name'],
  ])
    assert.throws(() => parseArgs(bad), Error, bad.join(' '));
});

test('引数の誤りは使い方を出して終了コード2で、子を起動しない', async () => {
  const r = rig();
  assert.equal(await runStageWatch(['--name', 'a'], r.deps), 2);
  assert.match(r.out.warns[0], /使い方/);
  assert.equal(r.out.spawned.length, 0);
});

test('通し: 子が失敗（終了コード1）すると exit を通知し、結果ファイルを残して1を返す', async () => {
  const r = rig({ files: 0 });
  const run = runStageWatch(
    ['--name', 'night5', '--', 'node', 'missing-config.js'],
    r.deps,
  );
  await tick();
  r.advance(3_000);
  await r.end(1);
  assert.equal(await run, 1);
  assert.deepEqual(r.out.spawned, [['node', ['missing-config.js']]]);
  assert.deepEqual(r.out.sent, [
    {
      kind: 'stage-watch',
      name: 'night5',
      anomaly: 'exit',
      exitCode: 1,
      outputs: null,
      lateMin: null,
      startedAt: '2026-10-20T10:00:00.000Z',
    },
  ]);
  assert.match(r.out.logs[0], /終了コード 1/);
  assert.equal(r.out.results[0].name, 'night5');
  assert.deepEqual(r.out.results[0].kinds, ['exit']);
  assert.equal(r.out.results[0].exitCode, 1);
  assert.equal(r.out.results[0].sent, true);
  assert.equal(r.out.results[0].endedAt, '2026-10-20T10:00:03.000Z');
});

test('0枚の出力は empty を通知して終了コード3、足りていれば何も知らせない', async () => {
  const r = rig({ files: 0 });
  const run = runStageWatch(
    ['--name', 'n', '--outputs', 'out', '--min-outputs', '2', '--', 'x'],
    r.deps,
  );
  await tick();
  await r.end(0);
  assert.equal(await run, 3);
  assert.equal(r.out.sent.length, 1);
  assert.equal(r.out.sent[0].anomaly, 'empty');
  assert.equal(r.out.sent[0].outputs, 0);
  assert.equal(r.out.sent[0].exitCode, 0);
  assert.match(r.out.logs[0], /0枚/);

  const ok = rig({ files: 2 });
  const run2 = runStageWatch(
    ['--name', 'n', '--outputs', 'out', '--min-outputs', '2', '--', 'x'],
    ok.deps,
  );
  await tick();
  await ok.end(0);
  assert.equal(await run2, 0);
  assert.deepEqual([ok.out.sent, ok.out.logs, ok.out.results], [[], [], []]);
  assert.deepEqual(ok.out.removed, ['n']);
  assert.deepEqual(r.out.removed, []);
});

test('異常なしの終了は前回の結果ファイルを消し、消せなくても終了コードを変えず警告する', async () => {
  const r = rig();
  r.deps.removeResult = () => {
    throw Error('EACCES');
  };
  const run = runStageWatch(['--name', 'n', '--', 'x'], r.deps);
  await tick();
  await r.end(0);
  assert.equal(await run, 0);
  assert.equal(r.out.warns.length, 1);
});

test('既定の結果ファイル: 削除は無くても成功し、rename に失敗したら一時ファイルを残さない', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'stage-watch-'));
  try {
    const files = resultFiles(dir);
    files.removeResult('none');
    files.writeResult('a', { exitCode: 1 });
    assert.deepEqual(
      JSON.parse(fs.readFileSync(path.join(dir, 'a.json'), 'utf8')),
      { exitCode: 1 },
    );
    files.removeResult('a');
    assert.deepEqual(fs.readdirSync(dir), []);

    fs.mkdirSync(path.join(dir, 'b.json'));
    assert.throws(() => files.writeResult('b', { exitCode: 1 }));
    assert.deepEqual(fs.readdirSync(dir), ['b.json']);
    assert.throws(() => files.removeResult('b'));
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('予定時刻+猶予を過ぎて動く子には overdue を1回だけ知らせ、子は止めず、終了後は late も知らせる', async () => {
  const r = rig();
  const run = runStageWatch(
    ['--name', 'n', '--expect-end', '2026-10-20T10:05:00Z', '--', 'x'],
    r.deps,
  );
  await tick();
  assert.equal(r.timers.length, 1);
  assert.equal(r.timers[0].ms, 15 * 60_000);
  r.advance(15 * 60_000);
  r.timers[0].fn();
  r.timers[0].fn();
  await tick();
  assert.deepEqual(
    r.out.sent.map((n) => [n.anomaly, n.lateMin, n.exitCode]),
    [['overdue', 10, null]],
  );
  r.advance(5 * 60_000);
  await r.end(0);
  assert.equal(await run, 0);
  assert.deepEqual(
    r.out.sent.map((n) => n.anomaly),
    ['overdue', 'late'],
  );
  assert.equal(r.out.sent[1].lateMin, 15);
  assert.equal(r.timers[0].cleared, true);
});

test('期限前にタイマーが発火しても overdue を通知せず掛け直し、期限で1回だけ通知する', async () => {
  const r = rig();
  const day = 24 * 60 * 60_000;
  const run = runStageWatch(
    [
      '--name',
      'n',
      '--expect-end',
      '2026-11-19T10:00:00Z',
      '--grace-min',
      '0',
      '--',
      'x',
    ],
    r.deps,
  );
  await tick();
  assert.equal(r.timers.length, 1);
  r.advance(23 * day);
  r.timers[0].fn();
  await tick();
  assert.equal(r.out.sent.length, 0);
  assert.equal(r.timers.length, 2);
  assert.equal(r.timers[1].ms, 7 * day);
  r.advance(7 * day);
  r.timers[1].fn();
  r.timers[1].fn();
  await tick();
  assert.deepEqual(
    r.out.sent.map((n) => [n.anomaly, n.lateMin]),
    [['overdue', 0]],
  );
  await r.end(0);
  await run;
  assert.equal(r.timers[1].cleared, true);
});

test('猶予内の終了は何も知らせず、終了時にタイマーを外す。送信失敗は終了コードを変えず秘匿して警告1行', async () => {
  const r = rig();
  const run = runStageWatch(
    ['--name', 'n', '--expect-end', '2026-10-20T10:05:00Z', '--', 'x'],
    r.deps,
  );
  await tick();
  r.advance(14 * 60_000);
  await r.end(0);
  assert.equal(await run, 0);
  assert.deepEqual(
    [r.out.sent, r.out.logs, r.timers[0].cleared],
    [[], [], true],
  );

  const f = rig({ failSend: true });
  const run2 = runStageWatch(['--name', 'n', '--', 'x'], f.deps);
  await tick();
  await f.end(7);
  assert.equal(await run2, 7);
  assert.equal(f.out.warns.length, 1);
  assert.doesNotMatch(f.out.warns[0], /secret|token/);
  assert.equal(f.out.results[0].sent, false);
});

test('子を起動できないときは127で exit を通知する', async () => {
  const r = rig();
  r.deps.spawnChild = () => ({ done: Promise.reject(Error('ENOENT')) });
  assert.equal(await runStageWatch(['--name', 'n', '--', 'nope'], r.deps), 127);
  assert.equal(r.out.sent[0].anomaly, 'exit');
  assert.equal(r.out.sent[0].exitCode, 127);
});
