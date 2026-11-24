import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';

const id = '11111111-1111-4111-8111-111111111111';
// 受付の view=summary が返す欄。会話・失敗の原因・操作一覧は全文にだけある。
const summaryView = (draft) => ({
  view: 'summary',
  ...Object.fromEntries(
    [
      'id',
      'project',
      'status',
      'revision',
      'questions',
      'hold',
      'issue',
      'issues',
      'splitChildren',
    ]
      .filter((key) => key in draft)
      .map((key) => [key, draft[key]]),
  ),
  ...(draft.consultationBudget?.stopped
    ? { consultationBudget: { stopped: draft.consultationBudget.stopped } }
    : {}),
});
// legacy は view=summary を知らず、どの読取にも全文を返す受付。
async function runScenario(
  args,
  final,
  children = {},
  { legacy = false } = {},
) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'filing-result-'));
  fs.writeFileSync(path.join(dir, 'token'), 'test');
  let step = 0,
    summaryReads = 0,
    fullReads = 0,
    posts = 0;
  const revisionReads = [];
  const server = http.createServer((req, res) => {
    res.setHeader('content-type', 'application/json');
    if (req.method === 'POST') {
      posts++;
      return res.end(
        JSON.stringify({
          id,
          ...(args.includes('--submit')
            ? { accepted: true, status: 'auto_ready' }
            : {}),
        }),
      );
    }
    const url = new URL(req.url, 'http://127.0.0.1');
    if (url.pathname === `/api/drafts/${id}`) {
      const summary = url.searchParams.get('view') === 'summary';
      const shown = (draft) =>
        JSON.stringify(summary && !legacy ? summaryView(draft) : draft);
      // --draft may first read the revision before the POST.
      if (args.includes('--draft') && !posts) {
        revisionReads.push(req.url);
        return res.end(
          shown({
            id,
            project: 'o/r',
            revision: 1,
            status: 'draft',
            messages: [],
          }),
        );
      }
      // 待ちの読取（要約。旧受付では全文）ごとに状態が進み、待ちが解けた後の全文は同じ状態を返す。
      const status =
        ['thinking', 'auto_ready', 'submitting'][step] || final.status;
      if (summary) step++;
      if (summary && !legacy) summaryReads++;
      else fullReads++;
      return res.end(
        shown({
          id,
          project: 'o/r',
          revision: 1,
          messages: [],
          ...final,
          status,
        }),
      );
    }
    if (req.url.startsWith('/api/drafts/')) {
      const child = children[req.url.split('/').at(-1)];
      if (!child) {
        res.statusCode = 404;
        return res.end('{}');
      }
      return res.end(JSON.stringify(child));
    }
    if (req.url.startsWith('/api/issue?'))
      return res.end(JSON.stringify({ desk: 42, title: '受付の題名' }));
    if (req.url.startsWith('/api/todos'))
      return res.end(JSON.stringify({ items: [], total: 0, hasMore: false }));
    return res.end('[]');
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  try {
    const result = await new Promise<any>((resolve) => {
      const child = spawn(
        process.execPath,
        ['scripts/create-issue.ts', ...args, '--timeout', '20'],
        {
          env: {
            ...process.env,
            HOME: dir,
            ISSUE_DESK_SENT_LEDGER: path.join(dir, 'ledger'),
            ISSUE_DESK_TOKEN_FILE: path.join(dir, 'token'),
            ISSUE_DESK_URL: `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`,
          },
        },
      );
      let output = '';
      child.stdout.on('data', (x) => (output += x));
      child.stderr.on('data', (x) => (output += x));
      child.on('close', (code) => resolve({ code, output }));
    });
    return { ...result, summaryReads, fullReads, revisionReads, posts };
  } finally {
    await new Promise((resolve) => server.close(resolve));
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

test('新規・返信・A回答・確定は起票失敗まで待ち原因と操作を返す', async () => {
  const variants = [
    ['--project', 'o/r', '--tier', 'light', '--text', '依頼'],
    ['--draft', id, '--expect-revision', '1', '--text', '回答'],
    ['--draft', id, '--expect-revision', '1', '--text', 'A'],
    ['--draft', id, '--submit'],
  ];
  const results = await Promise.all(
    variants.map((args) =>
      runScenario(args, {
        status: 'submit_error',
        lastFailure: { cause: '接続先エラー' },
        actions: ['retry'],
      }),
    ),
  );
  for (const r of results) {
    assert.equal(r.code, 1, r.output);
    assert.equal(r.summaryReads, 4);
    assert.equal(r.fullReads, 1);
    assert.equal(r.posts, 1);
    assert.match(r.output, /接続先エラー/);
    assert.match(r.output, /retry/);
    assert.match(r.output, /起票されていない/);
  }
  // 版だけを使う送信前の読取（--expect-revision なしの回答と --submit）は要約で読む。
  assert.deepEqual(results[3].revisionReads, [
    `/api/drafts/${id}?view=summary`,
  ]);
});

test('view=summary を知らない受付が全文を返しても同じ結果を返し、読み直さない', async () => {
  const results = await Promise.all(
    [
      ['--project', 'o/r', '--tier', 'light', '--text', '依頼'],
      ['--draft', id, '--submit'],
    ].map((args) =>
      runScenario(
        args,
        {
          status: 'submit_error',
          lastFailure: { cause: '接続先エラー' },
          actions: ['retry'],
        },
        {},
        { legacy: true },
      ),
    ),
  );
  for (const r of results) {
    assert.equal(r.code, 1, r.output);
    assert.equal(r.summaryReads, 0);
    assert.equal(r.fullReads, 4);
    assert.equal(r.posts, 1);
    assert.match(r.output, /接続先エラー/);
    assert.match(r.output, /retry/);
    assert.match(r.output, /起票されていない/);
  }
});

test('checkは分割成功と部分失敗を読み送信しない', async () => {
  const [success, partial] = await Promise.all([
    runScenario(['--check', id], {
      status: 'submitted',
      issues: [
        { project: 'o/r', number: 1 },
        { project: 'o/r', number: 2 },
      ],
    }),
    runScenario(
      ['--check', id],
      { status: 'submit_error', splitChildren: ['a', 'b', 'c'], actions: [] },
      {
        a: {
          id: 'a',
          status: 'submitted',
          issue: { project: 'o/r', number: 1 },
        },
        b: {
          id: 'b',
          status: 'submit_error',
          lastFailure: { cause: '子の失敗' },
          actions: ['retry'],
        },
        c: { id: 'c', status: 'draft', actions: [] },
      },
    ),
  ]);
  assert.equal(success.code, 0, success.output);
  assert.equal(success.summaryReads, 4);
  assert.equal(success.fullReads, 1);
  assert.equal(success.posts, 0);
  assert.equal((success.output.match(/共通番号42/g) || []).length, 2);
  assert.match(success.output, /受付の題名/);
  assert.equal(partial.code, 1, partial.output);
  assert.equal(partial.posts, 0);
  assert.match(partial.output, /共通番号42/);
  assert.match(partial.output, /子の失敗/);
  assert.match(partial.output, /retry/);
  assert.match(partial.output, /未試行/);
  assert.match(partial.output, /全件の起票は完了していません/);
  assert.doesNotMatch(partial.output, /起票されていない/);
});

test('結果不明と読取不能は未起票と断定せず照合を案内する', async () => {
  const [unknown, missing] = await Promise.all([
    runScenario(['--check', id], { status: 'submit_unknown' }),
    runScenario(['--check', id], {
      status: 'submit_error',
      splitChildren: ['missing'],
      actions: [],
    }),
  ]);
  assert.equal(unknown.code, 0, unknown.output);
  assert.match(unknown.output, /未確認/);
  assert.match(unknown.output, /--check/);
  assert.doesNotMatch(unknown.output, /起票されていない/);
  assert.equal(missing.code, 1, missing.output);
  assert.match(missing.output, /missing 状態は未確認/);
  assert.match(missing.output, /原因: 未記録/);
  assert.match(missing.output, /操作: なし/);
  assert.doesNotMatch(missing.output, /起票されていない/);
});

test('処理中の各状態は同じ期限で停止し、読取喪失は未確認で終える', async () => {
  const { checkDraft, consult } = await import('../scripts/desk-client.ts');
  const oldLog = console.log,
    oldNow = Date.now;
  let now = 1000,
    lines = [];
  console.log = (line) => lines.push(line);
  Date.now = () => now;
  try {
    for (const status of ['thinking', 'auto_ready', 'submitting']) {
      now = 1000;
      lines = [];
      let reads = 0;
      const code = await checkDraft(
        async (method) => {
          assert.equal(method, 'GET');
          reads++;
          now += 400;
          return { id, status, messages: [] };
        },
        { check: id, timeout: 1 },
        'scripts/create-issue.ts',
        '',
        0,
      );
      assert.equal(code, 0);
      assert.equal(reads, 3);
      assert.match(lines.join('\n'), /未確認/);
      assert.match(lines.join('\n'), /--check/);
      assert.doesNotMatch(lines.join('\n'), /続きを回答/);
    }
    now = 1000;
    lines = [];
    let posts = 0;
    const code = await consult(
      async (method) => {
        if (method === 'POST') {
          posts++;
          return { id };
        }
        now += 400;
        throw Object.assign(Error('接続喪失'), { unreachable: true });
      },
      { project: 'o/r', provider: 'codex', text: '依頼', timeout: 1 },
      'scripts/create-issue.ts',
      '',
      0,
    );
    assert.equal(code, 2);
    assert.equal(posts, 1);
    assert.match(lines.join('\n'), /--check/);
    for (const draft of [
      { status: 'draft', questions: ['質問'] },
      { status: 'draft', hold: true },
      { status: 'thinking', consultationBudget: { stopped: true } },
    ]) {
      now = 1000;
      let reads = 0;
      await checkDraft(
        async () => {
          reads++;
          return { id, messages: [], ...draft };
        },
        { check: id, timeout: 1 },
        'scripts/create-issue.ts',
        '',
        0,
      );
      assert.equal(reads, 1);
    }
  } finally {
    console.log = oldLog;
    Date.now = oldNow;
  }
});

test('番号や結果の欠落と操作の欠落を実際の下書き確認で区別する', async () => {
  const { checkDraft } = await import('../scripts/desk-client.ts');
  const oldLog = console.log;
  const lines = [];
  console.log = (line) => lines.push(line);
  try {
    const draft = {
      id,
      status: 'submitted',
      messages: [],
      issues: [{ project: 'o/r', number: 9 }, null],
    };
    const code = await checkDraft(
      async (_method, route) =>
        route.startsWith('/api/drafts/') ? draft : { title: '題名' },
      { check: id, timeout: 1 },
      'scripts/create-issue.ts',
      '',
      0,
    );
    assert.equal(code, 0);
    assert.match(lines.join('\n'), /共通番号未確定/);
    assert.match(lines.join('\n'), /Gitea番号 9/);
    assert.match(lines.join('\n'), /一部未確認/);
    assert.match(lines.join('\n'), /--check/);
    assert.doesNotMatch(lines.join('\n'), /起票しました（2件）/);
    lines.length = 0;
    const failed = await checkDraft(
      async () => ({ id, status: 'submit_error', messages: [] }),
      { check: id, timeout: 1 },
      'scripts/create-issue.ts',
      '',
      0,
    );
    assert.equal(failed, 1);
    assert.match(lines.join('\n'), /原因: 未記録/);
    assert.match(lines.join('\n'), /操作: 未取得/);
    assert.doesNotMatch(lines.join('\n'), /続きを回答/);
  } finally {
    console.log = oldLog;
  }
});

test('最終読取が期限をまたいでも起票結果と分割の子の詳細を読む', async () => {
  const { checkDraft, consult } = await import('../scripts/desk-client.ts');
  const oldLog = console.log,
    oldNow = Date.now;
  let now, lines;
  console.log = (line) => lines.push(line);
  Date.now = () => now;
  try {
    for (const sent of [false, true]) {
      now = 1000;
      lines = [];
      const routes = [];
      const filed = {
        id,
        status: 'submitted',
        messages: [],
        issue: { project: 'o/r', number: 9 },
        splitChildren: ['child'],
      };
      const call = async (method, route) => {
        routes.push(`${method} ${route}`);
        if (method === 'POST') return { id };
        // 要約の読取が期限をまたぐ。期限切れでも全文を1回読んで結果を表示する。
        if (route === `/api/drafts/${id}?view=summary`) {
          now = 2001;
          return summaryView(filed);
        }
        if (route === `/api/drafts/${id}`) return filed;
        if (route === '/api/drafts/child')
          return {
            id: 'child',
            status: 'submitted',
            issue: { project: 'o/r', number: 10 },
          };
        if (route.startsWith('/api/issue?'))
          return { desk: 42, title: '受付の題名' };
        throw Error(`unexpected route ${route}`);
      };
      const code = sent
        ? await consult(
            call,
            { project: 'o/r', provider: 'codex', text: '依頼', timeout: 1 },
            'scripts/create-issue.ts',
            '',
            0,
          )
        : await checkDraft(
            call,
            { check: id, timeout: 1 },
            'scripts/create-issue.ts',
            '',
            0,
          );
      assert.equal(code, 0);
      assert.deepEqual(
        routes.filter((route) => route.startsWith(`GET /api/drafts/${id}`)),
        [`GET /api/drafts/${id}?view=summary`, `GET /api/drafts/${id}`],
      );
      // 起票結果の題名と共通番号は要約で読む。
      const issueReads = routes.filter((route) =>
        route.startsWith('GET /api/issue?'),
      );
      assert.equal(issueReads.length, 2);
      assert.ok(issueReads.every((route) => route.includes('view=summary')));
      assert.equal(
        (lines.join('\n').match(/共通番号42 受付の題名/g) || []).length,
        2,
      );
      assert.doesNotMatch(
        lines.join('\n'),
        /未確定|題名を読めませんでした|状態は未確認/,
      );
    }
  } finally {
    console.log = oldLog;
    Date.now = oldNow;
  }
});

test('応答待ちは要約で待ち、待ちが解けたら全文を1回読んで返事を表示する', async () => {
  const { checkDraft } = await import('../scripts/desk-client.ts');
  const oldLog = console.log;
  const lines = [];
  console.log = (line) => lines.push(line);
  try {
    const full = {
      id,
      status: 'draft',
      revision: 3,
      messages: [{ role: 'assistant', text: '受付AIの返事' }],
      questions: ['質問'],
      actions: ['chat'],
    };
    const routes = [];
    let summaryReads = 0;
    const code = await checkDraft(
      async (_method, route) => {
        routes.push(route);
        if (route !== `/api/drafts/${id}?view=summary`) return full;
        return summaryView({
          ...full,
          status: summaryReads++ < 2 ? 'thinking' : 'draft',
        });
      },
      { check: id, timeout: 30 },
      'scripts/create-issue.ts',
      '',
      0,
    );
    assert.equal(code, 0);
    assert.deepEqual(routes, [
      `/api/drafts/${id}?view=summary`,
      `/api/drafts/${id}?view=summary`,
      `/api/drafts/${id}?view=summary`,
      `/api/drafts/${id}`,
    ]);
    assert.match(lines.join('\n'), /受付AI:\n受付AIの返事/);
    assert.match(lines.join('\n'), /続きを回答/);
  } finally {
    console.log = oldLog;
  }
});

test('要約で待ちが解けても全文がまだ応答待ちなら待ちへ戻る', async () => {
  const { checkDraft } = await import('../scripts/desk-client.ts');
  const oldLog = console.log;
  const lines = [];
  console.log = (line) => lines.push(line);
  try {
    const routes = [];
    let fullReads = 0;
    const code = await checkDraft(
      async (_method, route) => {
        routes.push(route);
        if (route === `/api/drafts/${id}?view=summary`)
          return summaryView({ id, status: 'draft' });
        return {
          id,
          status: fullReads++ ? 'draft' : 'thinking',
          messages: [{ role: 'assistant', text: '次の返事' }],
        };
      },
      { check: id, timeout: 30 },
      'scripts/create-issue.ts',
      '',
      0,
    );
    assert.equal(code, 0);
    assert.deepEqual(routes, [
      `/api/drafts/${id}?view=summary`,
      `/api/drafts/${id}`,
      `/api/drafts/${id}?view=summary`,
      `/api/drafts/${id}`,
    ]);
    assert.match(lines.join('\n'), /次の返事/);
    assert.doesNotMatch(lines.join('\n'), /応答待ちのまま/);
  } finally {
    console.log = oldLog;
  }
});

test('期限切れでは全文を1回読み、読めなければ結果を未確認で終える', async () => {
  const { checkDraft } = await import('../scripts/desk-client.ts');
  const oldLog = console.log,
    oldNow = Date.now;
  let now, lines;
  console.log = (line) => lines.push(line);
  Date.now = () => now;
  try {
    for (const lost of [false, true]) {
      now = 1000;
      lines = [];
      const routes = [];
      const code = await checkDraft(
        async (_method, route) => {
          routes.push(route);
          now += 400;
          if (route === `/api/drafts/${id}?view=summary`)
            return summaryView({ id, status: 'thinking' });
          if (lost)
            throw Object.assign(Error('接続喪失'), { unreachable: true });
          return {
            id,
            status: 'thinking',
            messages: [{ role: 'assistant', text: '前の返事' }],
          };
        },
        { check: id, timeout: 1 },
        'scripts/create-issue.ts',
        '',
        0,
      );
      assert.deepEqual(routes, [
        `/api/drafts/${id}?view=summary`,
        `/api/drafts/${id}?view=summary`,
        `/api/drafts/${id}?view=summary`,
        `/api/drafts/${id}`,
      ]);
      assert.equal(code, lost ? 2 : 0);
      assert.match(lines.join('\n'), /--check/);
      if (lost) assert.match(lines.join('\n'), /接続喪失/);
      else assert.match(lines.join('\n'), /応答待ちのまま待機を終了しました/);
    }
  } finally {
    console.log = oldLog;
    Date.now = oldNow;
  }
});

test('応答待ちの読取は最初の30秒は3秒ごと、以後は6秒ごとにする', async () => {
  const { checkDraft } = await import('../scripts/desk-client.ts');
  const oldLog = console.log,
    oldNow = Date.now,
    oldTimeout = globalThis.setTimeout;
  let now = 1000;
  const delays = [];
  const routes = [];
  console.log = () => {};
  Date.now = () => now;
  // 待ち時間を記録して時計だけを進める。
  globalThis.setTimeout = ((done, ms) => {
    delays.push(ms);
    now += ms;
    return oldTimeout(done, 0);
  }) as typeof setTimeout;
  try {
    const code = await checkDraft(
      async (_method, route) => {
        routes.push(route);
        return route.endsWith('?view=summary')
          ? summaryView({ id, status: 'thinking' })
          : { id, status: 'thinking', messages: [] };
      },
      { check: id, timeout: 60 },
      'scripts/create-issue.ts',
      '',
    );
    assert.equal(code, 0);
    assert.deepEqual(delays, [...Array(10).fill(3000), ...Array(5).fill(6000)]);
    // 60秒の待ちで要約14回、期限切れの全文1回。
    assert.deepEqual(routes, [
      ...Array(14).fill(`/api/drafts/${id}?view=summary`),
      `/api/drafts/${id}`,
    ]);
  } finally {
    console.log = oldLog;
    Date.now = oldNow;
    globalThis.setTimeout = oldTimeout;
  }
});
