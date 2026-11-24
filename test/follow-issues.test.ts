import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import {
  follow,
  followIssue,
  parseFollowArgs,
  type Issue,
} from '../scripts/follow-issues.ts';

const hour = 3600000;
const issue = (status: string, extra: Partial<Issue> = {}): Issue => ({
  desk: 7,
  title: '題',
  state: 'open',
  status,
  updatedAt: 't1',
  ...extra,
});

test('保留・失敗・未知の状態は要対処として理由つきで1回だけ出す', () => {
  const first = followIssue(
    undefined,
    issue('review_held', { result: { error: '削除の承認待ち' } }),
    0,
    hour,
  );
  assert.match(
    first.line!,
    /^【要対処】共通番号7: 状態=review_held 理由: 削除の承認待ち/,
  );
  assert.equal(
    followIssue(
      first.next,
      issue('review_held', {
        updatedAt: 't2',
        result: { error: '削除の承認待ち' },
      }),
      1,
      hour,
    ).line,
    null,
  );
  assert.match(
    followIssue(first.next, issue('failed'), 2, hour).line!,
    /^【要対処】/,
  );
  for (const status of ['waiting', 'stopped', 'new_unknown_status', ''])
    assert.match(
      followIssue(undefined, issue(status), 0, hour).line!,
      /^【要対処】/,
      status,
    );
});

test('未了の最終承認はIDの変更を通知し、順序や更新時刻だけでは通知しない', () => {
  const a = {
    id: 'merge',
    headSha: '12345678aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
  };
  const b = {
    id: 'deploy',
    headSha: 'abcdef0122222222222222222222222222222222',
  };
  const held = (pendingFinalApprovals: (typeof a)[], updatedAt = 't1') =>
    issue('review_held', {
      result: { message: '承認待ち' },
      pendingFinalApprovals,
      updatedAt,
    });
  const first = followIssue(undefined, held([a, b]), 0, hour);
  assert.match(first.line!, /未了の最終承認: 2件（abcdef01, 12345678）/);
  assert.equal(followIssue(first.next, held([b, a], 't2'), 1, hour).line, null);
  assert.match(
    followIssue(first.next, held([b, { ...a, id: 'other' }]), 2, hour).line!,
    /^【要対処】/,
  );
});

test('旧状態ファイルから再開し、空または未指定の承認では通知しない', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-approval-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'state.json');
  fs.writeFileSync(
    file,
    JSON.stringify({
      issues: { '7': { mark: 'open/review_held/', since: 0 } },
    }),
  );
  const empty = runtime([
    () => issue('review_held', { pendingFinalApprovals: [] }),
  ]);
  assert.equal(await follow(options(file), empty.io), 3);
  assert.equal(
    empty.lines.filter((line) => line.startsWith('【要対処】')).length,
    0,
  );
  const absent = runtime([() => issue('review_held')]);
  assert.equal(await follow(options(file), absent.io), 3);
  assert.equal(
    absent.lines.filter((line) => line.startsWith('【要対処】')).length,
    0,
  );
});

test('見張りの連続読取と張り直しで最終承認のhead変更だけを通知する', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-head-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'state.json');
  const old = {
    id: 'merge',
    headSha: 'aaaaaaaa11111111111111111111111111111111',
  };
  const updated = {
    ...old,
    headSha: 'aaaaaaaa22222222222222222222222222222222',
  };
  let reads = 0;
  const run = runtime([
    () =>
      issue('review_held', {
        pendingFinalApprovals: [reads++ === 0 ? old : updated],
      }),
  ]);
  assert.equal(await follow(options(file), run.io), 3);
  const notices = run.lines.filter((line) => line.startsWith('【要対処】'));
  assert.equal(notices.length, 2);
  assert.match(notices[0], /1件（aaaaaaaa）/);
  assert.match(notices[1], /1件（aaaaaaaa）/);
  const resumed = runtime([
    () => issue('review_held', { pendingFinalApprovals: [updated] }),
  ]);
  assert.equal(await follow(options(file), resumed.io), 3);
  assert.equal(
    resumed.lines.filter((line) => line.startsWith('【要対処】')).length,
    0,
  );
});

test('進行中の状態は黙り、更新が止まったら停滞を1回だけ出す', () => {
  const first = followIssue(undefined, issue('running'), 0, hour);
  assert.equal(first.line, null);
  assert.equal(
    followIssue(first.next, issue('running'), hour - 1, hour).line,
    null,
  );
  const stalled = followIssue(first.next, issue('running'), hour, hour);
  assert.match(stalled.line!, /^【停滞】共通番号7: 状態=running のまま 60 分/);
  assert.equal(
    followIssue(stalled.next, issue('running'), hour * 2, hour).line,
    null,
  );
  const moved = followIssue(
    stalled.next,
    issue('publishing', { updatedAt: 't2' }),
    hour * 2,
    hour,
  );
  assert.equal(moved.line, null);
  assert.equal(moved.next.stallNoticed, false);
});

test('マージ・クローズは完了として1回だけ出す', () => {
  const merged = followIssue(
    undefined,
    issue('running', { pr: { merged: true } }),
    0,
    hour,
  );
  assert.match(merged.line!, /^【完了】/);
  assert.equal(
    followIssue(merged.next, issue('completed'), 1, hour).line,
    null,
  );
  assert.match(
    followIssue(undefined, issue('queued', { state: 'closed' }), 0, hour).line!,
    /^【完了】/,
  );
});

function runtime(reads: (() => Issue)[], step = 60000) {
  let clock = 0;
  const lines: string[] = [];
  return {
    lines,
    io: {
      read: async () => reads[0](),
      print: (line: string) => lines.push(line),
      now: () => clock,
      sleep: async () => {
        clock += step;
      },
    },
  };
}
const options = (stateFile: string, runMs = 180000) => ({
  desks: [7],
  stateFile,
  intervalMs: 60000,
  stallMs: hour,
  runMs,
});

test('全件完了で終わり、時間切れは張り直しを出して終わる。張り直しても同じ通知を出さない', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'state.json');
  const held = runtime([() => issue('review_held')]);
  assert.equal(await follow(options(file), held.io), 3);
  assert.equal(
    held.lines.filter((line) => line.startsWith('【要対処】')).length,
    1,
  );
  assert.match(held.lines.at(-1)!, /^【張り直し】/);
  const again = runtime([() => issue('review_held')]);
  assert.equal(await follow(options(file), again.io), 3);
  assert.equal(
    again.lines.filter((line) => line.startsWith('【要対処】')).length,
    0,
  );
  const done = runtime([() => issue('completed')]);
  assert.equal(await follow(options(file), done.io), 0);
  assert.match(done.lines.at(-1)!, /^【見張り終了】/);
});

test('通知に次の担当と次にすることを添える。担当が不明なら添えない', () => {
  const line = followIssue(
    undefined,
    issue('review_held', { nextActor: 'user', nextAction: '削除を承認する' }),
    0,
    hour,
  ).line!;
  assert.match(line, /（題） 次=利用者: 削除を承認する$/);
  assert.match(
    followIssue(undefined, issue('failed'), 0, hour).line!,
    /（題）$/,
  );
  const first = followIssue(
    undefined,
    issue('running', { nextActor: 'system', nextAction: '実行の完了を待つ' }),
    0,
    hour,
  );
  assert.match(
    followIssue(
      first.next,
      issue('running', { nextActor: 'system', nextAction: '実行の完了を待つ' }),
      hour,
      hour,
    ).line!,
    /次=受付: 実行の完了を待つ$/,
  );
});

test('残り全件が利用者の判断待ちなら、続けて2回読んでから1回だけ知らせて終わる', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const held = () =>
    issue('review_held', { nextActor: 'user', nextAction: '削除を承認する' });
  const waiting = runtime([held]);
  assert.equal(
    await follow(options(path.join(dir, 'a.json'), hour), waiting.io),
    0,
  );
  assert.equal(waiting.lines.length, 2);
  assert.match(waiting.lines[0], /^【要対処】/);
  assert.match(
    waiting.lines[1],
    /^【判断待ち】.*共通番号7（題） 次=利用者: 削除を承認する/,
  );
  assert.ok(!waiting.lines.some((line) => line.startsWith('【張り直し】')));
});

test('判断待ち以外が残る間は止めない', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const cases: (() => Issue)[] = [
    () => issue('failed', { nextActor: 'ai', nextAction: '調べる' }),
    () => issue('running', { nextActor: 'user' }),
    () => issue('review_held'),
    () => {
      throw Error('受付に届きません');
    },
  ];
  for (const [index, read] of cases.entries()) {
    const run = runtime([read]);
    assert.equal(
      await follow(options(path.join(dir, `${index}.json`)), run.io),
      3,
      String(index),
    );
    assert.match(
      run.lines.at(-1)!,
      /^【張り直し】.*起動し直すだけで返答を終えて/,
    );
  }
  const mixed = runtime([() => issue('running')]);
  mixed.io.read = async (desk?: number) =>
    desk === 7
      ? issue('review_held', { nextActor: 'user' })
      : issue('running', { desk: 8, nextActor: 'system' });
  assert.equal(
    await follow(
      { ...options(path.join(dir, 'm.json')), desks: [7, 8] },
      mixed.io,
    ),
    3,
  );
});

test('読み取りの失敗は要対処なしとせず、続いたら読取失敗を1回出す', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const failing = runtime([
    () => {
      throw Error('受付に届きません');
    },
  ]);
  assert.equal(
    await follow(options(path.join(dir, 's.json'), 600000), failing.io),
    3,
  );
  assert.equal(
    failing.lines.filter((line) => line.startsWith('【読取失敗】')).length,
    1,
  );
  assert.ok(!failing.lines.some((line) => line.startsWith('【完了】')));
});

test('引数は共通番号と絶対パスを求める', () => {
  assert.deepEqual(
    parseFollowArgs(['--desk', '1,2', '--state', '/h/s']).desks,
    [1, 2],
  );
  for (const argv of [
    ['--state', '/h/s'],
    ['--desk', '1'],
    ['--desk', 'x', '--state', '/h/s'],
    ['--desk', '1', '--state', 'rel'],
  ])
    assert.throws(() => parseFollowArgs(argv));
});

test('交代終了前に通知状態を保存し、再開時に同じ通知を重ねない', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-handoff-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'state.json');
  const lines: string[] = [];
  const opts = { ...options(file), desks: [7, 8] };
  const first = {
    read: async (desk: number) => {
      if (desk === 8) throw Object.assign(Error('交代'), { nasHandoff: true });
      return issue('review_held');
    },
    print: (line: string) => lines.push(line),
    now: () => 0,
    sleep: async () => {},
  };
  assert.equal(await follow(opts, first), 75);
  assert.equal(lines.filter((line) => line.startsWith('【要対処】')).length, 1);
  assert.ok(JSON.parse(fs.readFileSync(file, 'utf8')).issues['7']);
  const next = {
    ...first,
    read: async (desk: number) => ({ ...issue('review_held'), desk }),
    now: () => 0,
    sleep: async () => {},
  };
  let reads = 0;
  const resumed = {
    ...next,
    read: async (desk: number) => {
      if (++reads > 2) return { ...issue('completed'), desk };
      return next.read(desk);
    },
  };
  assert.equal(await follow(opts, resumed), 0);
  assert.equal(
    lines.filter(
      (line) => line.includes('共通番号7') && line.startsWith('【要対処】'),
    ).length,
    1,
  );
});

test('窓口で渡された開始時刻からの期限を再開時に延長しない', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-deadline-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const oldMode = process.env.ISSUE_DESK_NAS_HANDOFF,
    oldStart = process.env.ISSUE_DESK_NAS_STARTED;
  t.after(() => {
    if (oldMode === undefined) delete process.env.ISSUE_DESK_NAS_HANDOFF;
    else process.env.ISSUE_DESK_NAS_HANDOFF = oldMode;
    if (oldStart === undefined) delete process.env.ISSUE_DESK_NAS_STARTED;
    else process.env.ISSUE_DESK_NAS_STARTED = oldStart;
  });
  const oldRestarts = process.env.ISSUE_DESK_NAS_RESTARTS;
  t.after(() => {
    if (oldRestarts === undefined) delete process.env.ISSUE_DESK_NAS_RESTARTS;
    else process.env.ISSUE_DESK_NAS_RESTARTS = oldRestarts;
  });
  process.env.ISSUE_DESK_NAS_HANDOFF = 'read';
  process.env.ISSUE_DESK_NAS_STARTED = '100';
  process.env.ISSUE_DESK_NAS_RESTARTS = '1';
  let reads = 0;
  const io = {
    read: async () => {
      reads++;
      return issue('running');
    },
    print: () => {},
    now: () => 160000,
    sleep: async () => {},
  };
  assert.equal(await follow(options(path.join(dir, 's'), 60000), io), 3);
  assert.equal(reads, 0);
  // 窓口の最初の子は、起動時点で期限が過ぎていても一度読んでから張り直しを知らせる。
  process.env.ISSUE_DESK_NAS_RESTARTS = '0';
  assert.equal(await follow(options(path.join(dir, 's2'), 60000), io), 3);
  assert.equal(reads, 1);
});

test('判断待ち確認中の交代は保存して終了し、再開後に2回確認する', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-wait-handoff-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'state.json');
  const lines: string[] = [];
  let count = 0;
  const held = () => issue('review_held', { nextActor: 'user' });
  const io = {
    read: async () => {
      if (++count === 2)
        throw Object.assign(Error('交代'), { nasHandoff: true });
      return held();
    },
    print: (line: string) => lines.push(line),
    now: () => 0,
    sleep: async () => {},
  };
  assert.equal(await follow(options(file), io), 75);
  assert.equal(count, 2);
  assert.ok(!lines.some((line) => line.startsWith('【判断待ち】')));
  let resumedReads = 0;
  assert.equal(
    await follow(options(file), {
      ...io,
      read: async () => {
        resumedReads++;
        return held();
      },
    }),
    0,
  );
  assert.equal(resumedReads, 2);
  assert.equal(lines.filter((line) => line.startsWith('【要対処】')).length, 1);
  assert.equal(
    lines.filter((line) => line.startsWith('【判断待ち】')).length,
    1,
  );
});

test('既定は30分ごとに読み、変化が無ければ張り直しまで何も出さない', async (t) => {
  const parsed = parseFollowArgs(['--desk', '7', '--state', '/h/s']);
  assert.equal(parsed.intervalMs, 1800000);
  assert.equal(parsed.runMs, 28 * 60000);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-default-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 's.json');
  const sleeps: number[] = [];
  let clock = 0;
  let reads = 0;
  const lines: string[] = [];
  const io = {
    read: async () => {
      reads++;
      return issue('running');
    },
    print: (line: string) => lines.push(line),
    now: () => clock,
    sleep: async (ms: number) => {
      sleeps.push(ms);
      clock += ms;
    },
  };
  assert.equal(await follow({ ...parsed, stateFile: file }, io), 3);
  assert.equal(reads, 1);
  assert.deepEqual(sleeps, [28 * 60000]);
  assert.equal(lines.length, 1);
  assert.match(
    lines[0],
    /^【張り直し】共通番号7 に新しい変化はありません。返答の文は一切書かず/,
  );
});

test('読み取りの失敗と判断待ちの確かめ直しは、長い間隔を待たず60秒後に読み直す', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-recheck-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const run = async (file: string, read: () => Issue) => {
    const sleeps: number[] = [];
    let clock = 0;
    const lines: string[] = [];
    const io = {
      read: async () => read(),
      print: (line: string) => lines.push(line),
      now: () => clock,
      sleep: async (ms: number) => {
        sleeps.push(ms);
        clock += ms;
      },
    };
    const code = await follow(
      {
        desks: [7],
        stateFile: file,
        intervalMs: 1800000,
        stallMs: hour,
        runMs: 28 * 60000,
      },
      io,
    );
    return { code, sleeps, lines };
  };
  const waiting = await run(path.join(dir, 'w.json'), () =>
    issue('review_held', { nextActor: 'user' }),
  );
  assert.equal(waiting.code, 0);
  assert.deepEqual(waiting.sleeps, [60000]);
  assert.match(waiting.lines.at(-1)!, /^【判断待ち】/);
  const failing = await run(path.join(dir, 'f.json'), () => {
    throw Error('受付に届きません');
  });
  assert.equal(failing.code, 3);
  assert.ok(failing.sleeps.every((ms) => ms <= 60000));
  assert.equal(
    failing.lines.filter((line) => line.startsWith('【読取失敗】')).length,
    1,
  );
});

test('下書きと共通番号の引数を単独・併用できる', () => {
  for (const args of [
    ['--draft', 'abc'],
    ['--desk', '7'],
    ['--draft', 'abc', '--desk', '7'],
  ])
    assert.doesNotThrow(() =>
      parseFollowArgs([...args, '--state', '/tmp/state']),
    );
  assert.throws(() => parseFollowArgs(['--state', '/tmp/state']), /使い方/);
});

test('相談の状態を保存して再開し、同じ要対処を重ねない', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-draft-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  for (const [i, draft] of [
    { status: 'draft', questions: ['質問'] },
    { status: 'submit_error' },
    { status: 'error' },
    { status: 'stalled' },
    { status: 'draft', ready: false },
  ].entries()) {
    const file = path.join(dir, String(i));
    const opts = { ...options(file), desks: [], drafts: ['d'], stallMs: 60000 };
    const run = () => {
      const r = runtime([() => issue('running')]);
      return {
        ...r,
        io: {
          ...r.io,
          readDraft: async () => draft,
          resolve: async () => issue('running'),
        },
      };
    };
    const first = run();
    assert.equal(await follow(opts, first.io), 3);
    assert.equal(
      first.lines.filter((x) => x.startsWith('【要対処】')).length,
      1,
    );
    const again = run();
    assert.equal(await follow(opts, again.io), 3);
    assert.equal(
      again.lines.filter((x) => x.startsWith('【要対処】')).length,
      0,
    );
  }
});

test('分割と複数起票を照合し未確定を再試行して完了まで追う', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-split-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  let attempts = 0;
  const resolved: string[] = [];
  const r = runtime([() => issue('completed')]);
  const io = {
    ...r.io,
    read: async (desk: number) => issue('completed', { desk }),
    readDraft: async (id: string) =>
      id === 'p'
        ? {
            status: 'submitted',
            project: 'a/b',
            splitChildren: ['c'],
            splitIssues: [{}],
            issues: [{ number: 1 }, { number: 2 }],
          }
        : {
            status: 'submitted',
            project: 'wrong/repo',
            issue: { project: 'c/d', number: 3 },
          },
    resolve: async (project: string, number: number) => {
      resolved.push(`${project}#${number}`);
      if (number === 2 && attempts++ === 0) throw Error('未確認');
      return issue('running', { desk: number });
    },
  };
  assert.equal(
    await follow(
      { ...options(path.join(dir, 'state')), desks: [], drafts: ['p'] },
      io,
    ),
    0,
  );
  assert.equal(r.lines.filter((x) => x.startsWith('【完了】')).length, 3);
  assert.ok(
    resolved.includes('a/b#1') &&
      resolved.includes('a/b#2') &&
      resolved.includes('c/d#3'),
  );
  assert.equal(resolved.filter((x) => x === 'a/b#2').length, 2);
  assert.match(r.lines.at(-1)!, /【見張り終了】/);
});

test('起票結果の照合失敗を再開後も一度だけ通知し、回復後の再発は再通知する', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-resolve-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'state');
  const opts = {
    ...options(file, 180000),
    desks: [],
    drafts: ['d'],
    intervalMs: 1800000,
  };
  const run = (resolve: () => Promise<Issue>) => {
    const r = runtime([() => issue('running')]);
    return {
      ...r,
      io: {
        ...r.io,
        readDraft: async () => ({
          status: 'submitted',
          project: 'a/b',
          issue: { number: 1 },
        }),
        resolve,
      },
    };
  };
  const fail = async (): Promise<Issue> => {
    throw Error('照合不可');
  };
  const first = run(fail);
  assert.equal(await follow(opts, first.io), 3);
  assert.equal(
    first.lines.filter((x) => x.startsWith('【読取失敗】')).length,
    0,
  );
  const resumed = run(fail);
  assert.equal(await follow(opts, resumed.io), 3);
  assert.equal(
    resumed.lines.filter((x) =>
      x.includes('起票結果の照合が5回続けてできません'),
    ).length,
    1,
  );
  const again = run(fail);
  assert.equal(await follow(opts, again.io), 3);
  assert.equal(
    again.lines.filter((x) => x.startsWith('【読取失敗】')).length,
    0,
  );
  // 一度照合できても未完了の分割子が残る場合、その後の未確認は新たな連続失敗になる。
  const recovered = run(async () => issue('running'));
  recovered.io.readDraft = async () => ({
    status: 'draft',
    project: 'a/b',
    issue: { number: 1 },
  });
  assert.equal(await follow({ ...opts, runMs: 60000 }, recovered.io), 3);
  const relapsed = run(fail);
  assert.equal(await follow({ ...opts, runMs: 300000 }, relapsed.io), 3);
  assert.equal(
    relapsed.lines.filter((x) => x.startsWith('【読取失敗】')).length,
    1,
  );
  assert.ok(!relapsed.lines.some((x) => x.startsWith('【見張り終了】')));
});

test('相談の終了と404は追跡を終え、他の失敗は読取失敗を出して続ける', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-end-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  for (const [id, response] of Object.entries({
    finished: { status: 'submitted', finishedWithoutIssue: true },
    todo: { status: 409, code: 'draft_returned_to_todo' },
    missing: { status: 404 },
  })) {
    const r = runtime([() => issue('running')]);
    const io = {
      ...r.io,
      readDraft: async () => {
        if (id !== 'finished') throw response;
        return response as any;
      },
      resolve: async () => issue('running'),
    };
    const opts = { ...options(path.join(dir, id)), desks: [], drafts: [id] };
    assert.equal(await follow(opts, io), 0);
    assert.match(r.lines.at(-1)!, /【見張り終了】/);
    if (id === 'missing') {
      assert.equal(
        r.lines.filter((x) =>
          x.includes(
            '相談が見つからない（破棄されたか、この接続では読めない）',
          ),
        ).length,
        1,
      );
      assert.equal(await follow(opts, io), 0);
      assert.equal(
        r.lines.filter((x) =>
          x.includes(
            '相談が見つからない（破棄されたか、この接続では読めない）',
          ),
        ).length,
        1,
      );
    }
  }
  const r = runtime([() => issue('running')]);
  assert.equal(
    await follow(
      {
        ...options(path.join(dir, 'failure'), 420000),
        desks: [],
        drafts: ['d'],
      },
      {
        ...r.io,
        readDraft: async () => {
          throw Error('通信失敗');
        },
        resolve: async () => issue('running'),
      },
    ),
    3,
  );
  assert.equal(r.lines.filter((x) => x.startsWith('【読取失敗】')).length, 1);
});

test('古い未確定相談は初回に停滞を知らせ、欠けた分割子は完了としない', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-incomplete-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const lines: string[] = [];
  let now = Date.parse('2026-11-11T12:00:00Z');
  let reads = 0;
  const io = {
    read: async () => issue('running'),
    readDraft: async (id: string) =>
      id === 'old'
        ? { status: 'draft', ready: false, updatedAt: '2026-11-11T10:00:00Z' }
        : ++reads === 1
          ? { status: 'submitted', splitIssues: [{}] }
          : {
              status: 'submitted',
              splitIssues: [{}],
              splitChildren: ['child'],
            },
    resolve: async () => issue('running'),
    print: (line: string) => lines.push(line),
    now: () => now,
    sleep: async () => {
      now += 60000;
    },
  };
  assert.equal(
    await follow(
      { ...options(path.join(dir, 'old'), 60000), desks: [], drafts: ['old'] },
      io,
    ),
    3,
  );
  assert.match(lines[0], /【要対処】下書きold: 未確定のまま停滞/);
  lines.length = 0;
  const incomplete = {
    ...io,
    readDraft: async (id: string) =>
      id === 'child'
        ? { status: 'submitted', finishedWithoutIssue: true }
        : io.readDraft(id),
  };
  assert.equal(
    await follow(
      { ...options(path.join(dir, 'split')), desks: [], drafts: ['parent'] },
      incomplete,
    ),
    0,
  );
  assert.equal(reads, 2);
  assert.match(lines.at(-1)!, /【見張り終了】/);
});
