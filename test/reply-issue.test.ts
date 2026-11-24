import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { createServer } from 'node:http';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { runCommand, replyIssue } from '../scripts/reply-issue.ts';

const base = ['--project', 'owner/repo', '--number', '12'];
// 正本の操作例は文字列として扱い、実行しない。
const fixtures = [
  'git show <sha>:<path>',
  'git log',
  'git diff',
  'git cat-file',
  'git fetch',
  'curl http…',
  'wget https://example.invalid/a',
  'gh api repos/a/b',
];

function harness(fileText?) {
  const calls = [],
    logs = [];
  let opened = 0;
  const options = {
    env: {},
    configured: () => '',
    readFile: () => fileText,
    log: (line) => logs.push(line),
    open: async () => {
      opened++;
      return {
        call: async (...args) => {
          calls.push(args);
          return { status: 'queued' };
        },
        close() {},
      };
    },
  };
  return {
    options,
    calls,
    logs,
    get opened() {
      return opened;
    },
  };
}

test('操作例を両入力で接続前に拒否する', async () => {
  for (const fixture of fixtures) {
    for (const mode of ['text', 'file']) {
      const body = `実装AIは次を実行してください: \`${fixture}\``;
      const h = harness(body);
      await assert.rejects(
        runCommand(
          [...base, `--${mode}`, mode === 'text' ? body : 'body.md'],
          h.options,
        ),
        /履歴管理操作|外部取得/,
      );
      assert.equal(h.opened, 0);
      assert.deepEqual(h.calls, []);
      assert.deepEqual(h.logs, []);
    }
  }
});

test('説明と禁止は許可しコードブロックの実行要求は拒否する', async () => {
  for (const body of [
    '.git は読めないので差分を貼ります。',
    'git log は実行しないでください。',
    'git diff という語の説明です。',
    'git status を実行してください。',
  ]) {
    const h = harness();
    assert.equal(await runCommand([...base, '--text', body], h.options), 0);
    assert.equal(h.calls[1][2].text, body);
  }
  const h = harness();
  await assert.rejects(
    runCommand(
      [
        ...base,
        '--text',
        '次のコマンドを実行してください。\n```sh\ngit show abc:file\n```',
      ],
      h.options,
    ),
    /履歴管理操作/,
  );
  assert.equal(h.opened, 0);
});

test('禁止と実行要求の混在は接続前に拒否し過去の説明は送る', async () => {
  for (const mode of ['text', 'file']) {
    for (const body of [
      'git log を確認して',
      'git log を実行',
      'curl を使って。',
      'git log は実行しないで git diff を確認して',
      'git log は実行しないで、git diff を実行してください',
      'git log は実行しないで git diff を確認してください',
      'curl は使わないで、wget を実行してください',
      'curl は使わないで wget を実行してください',
      'git log は禁止ですが git diff を実行してください',
    ]) {
      const h = harness(body);
      await assert.rejects(
        runCommand(
          [...base, `--${mode}`, mode === 'text' ? body : 'body.md'],
          h.options,
        ),
        /履歴管理操作|外部取得/,
      );
      assert.equal(h.opened, 0);
      assert.deepEqual(h.calls, []);
    }
    for (const body of [
      'git log の結果を確認しました。差分を貼ります',
      'git diff を確認し、結果を貼りました。',
      'curl の結果を取得しました。',
      'git log を確認しています。',
      '差分を貼ってください。\ngit log の結果を確認しました。',
    ]) {
      const h = harness(body);
      assert.equal(
        await runCommand(
          [...base, `--${mode}`, mode === 'text' ? body : 'body.md'],
          h.options,
        ),
        0,
      );
      assert.equal(h.calls[1][2].text, body);
    }
  }
});

test('語尾なしの指示と曖昧な報告・見出しは拒否し理由付きで送れる', async () => {
  for (const mode of ['text', 'file']) {
    for (const body of [
      'git log を確認して、問題ありませんでした',
      'git log の確認',
    ]) {
      const args = [...base, `--${mode}`, mode === 'text' ? body : 'body.md'];
      const rejected = harness(body);
      await assert.rejects(runCommand(args, rejected.options), /履歴管理操作/);
      assert.equal(rejected.opened, 0);
      assert.deepEqual(rejected.calls, []);

      const allowed = harness(body);
      assert.equal(
        await runCommand(
          [...args, '--allow-worker-commands', '過去の報告または見出し'],
          allowed.options,
        ),
        0,
      );
      assert.equal(allowed.calls[1][2].text, body);
    }
  }
});

test('理由付き解除は本文だけを既存の返信経路へ送る', async () => {
  const h = harness();
  const body = 'git log を実行してください';
  const id = '12345678-1234-1234-1234-123456789abc';
  assert.equal(
    await runCommand(
      [
        ...base,
        '--text',
        body,
        '--allow-worker-commands',
        '説明文の引用',
        '--id',
        id,
      ],
      h.options,
    ),
    0,
  );
  assert.equal(h.opened, 1);
  assert.equal(h.calls[1][0], 'POST');
  assert.equal(h.calls[1][1], '/api/comment');
  assert.deepEqual(h.calls[1][2], {
    async: true,
    id,
    project: 'owner/repo',
    number: 12,
    action: 'reply',
    text: body,
  });
  assert.match(h.logs[0], /送信ID/);
});

test('理由の欠落・空白・重複と解除できない本文不備を接続前に拒否する', async () => {
  for (const extra of [
    ['--text', '本文', '--allow-worker-commands'],
    ['--text', '本文', '--allow-worker-commands', '  '],
    [
      '--text',
      '本文',
      '--allow-worker-commands',
      '理由',
      '--allow-worker-commands',
      '別理由',
    ],
    ['--text', '  ', '--allow-worker-commands', '理由'],
    ['--text', '/codex run', '--allow-worker-commands', '理由'],
    ['--text', 'あ'.repeat(8001), '--allow-worker-commands', '理由'],
  ]) {
    const h = harness();
    await assert.rejects(runCommand([...base, ...extra], h.options));
    assert.equal(h.opened, 0);
    assert.deepEqual(h.logs, []);
  }
});

test('整形後のBMP文字と絵文字の境界を両入力で検査する', async () => {
  for (const body of ['あ'.repeat(8000), '😀'.repeat(4000)]) {
    for (const mode of ['text', 'file']) {
      for (const extra of ['', 'a']) {
        const text = `\ufeff ${body}${extra} `;
        const h = harness(text);
        const operation = runCommand(
          [...base, `--${mode}`, mode === 'text' ? text : 'body.md'],
          h.options,
        );
        if (extra) {
          await assert.rejects(operation, /長すぎ/);
          assert.equal(h.opened, 0);
        } else {
          assert.equal(await operation, 0);
          assert.equal(h.calls[1][2].text, body);
        }
      }
    }
  }
});

test('CLIは操作指示を非ゼロで拒否し送信IDを表示しない', () => {
  for (const [body, reason] of [
    ['curl http… を実行してください', /外部取得/],
    ['git show <sha>:<path> を実行してください', /履歴管理操作/],
  ] as [string, RegExp][]) {
    const result = spawnSync(
      process.execPath,
      ['scripts/reply-issue.ts', ...base, '--text', body],
      { encoding: 'utf8' },
    );
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, reason);
    assert.doesNotMatch(result.stdout + result.stderr, /送信ID/);
  }
});

test('既存の承認条件と403案内を維持する', async () => {
  const h = harness();
  await assert.rejects(
    runCommand(
      [...base, '--text', '本文', '--implementation-limit-retry'],
      h.options,
    ),
    /トークン接続が必要/,
  );
  assert.deepEqual(h.calls, []);
  assert.deepEqual(h.logs, []);
  const logs = [],
    calls = [];
  const options = {
    env: {},
    configured: () => 'https://desk.invalid',
    log: (s) => logs.push(s),
    open: async () => ({
      close() {},
      call: async (...args) => {
        calls.push(args);
        if (args[0] === 'POST')
          throw Object.assign(new Error('拒否'), { status: 403 });
        return { status: 'waiting' };
      },
    }),
  };
  await assert.rejects(
    runCommand(
      [...base, '--text', '本文', '--implementation-limit-retry'],
      options,
    ),
    /運用の代行を許す/,
  );
  assert.equal(calls[1][2].implementationLimitRetry, true);
  assert.match(logs[0], /送信ID/);
});

function preparedHarness() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'reply-test-'));
  const file = path.join(dir, 'prepared.json');
  const calls = [],
    logs = [];
  const detail: any = {
    project: 'owner/repo',
    number: 12,
    desk: 42,
    body: '現在の受入条件',
    status: 'waiting',
    comments: [
      {
        id: 1,
        createdAt: '2026-01-01',
        updatedAt: '2026-01-01',
        author: 'user',
        body: '8ファイルまで',
      },
      {
        id: 2,
        createdAt: '2026-01-02',
        updatedAt: '2026-01-02',
        author: 'user',
        body: '10ファイルへ拡大',
      },
      {
        id: 3,
        createdAt: '2026-01-03',
        updatedAt: '2026-01-03',
        author: 'user',
        body: '8ファイル制限を取消',
      },
    ],
  };
  let failGet = false,
    lose = false,
    applied = 0;
  const options = {
    env: {},
    configured: () => '',
    log: (s) => logs.push(s),
    open: async () => ({
      close() {},
      call: async (...a) => {
        calls.push(a);
        if (a[0] === 'GET') {
          if (failGet) throw Error('token=secretvalue');
          // 要約（view=summary）は本文とコメントを返さない。
          if (a[1].includes('view=summary')) {
            const summary = structuredClone(detail);
            delete summary.body;
            delete summary.comments;
            return { view: 'summary', ...summary };
          }
          return structuredClone(detail);
        }
        if (!applied) {
          applied++;
          detail.comments.push({
            id: 4,
            createdAt: '2026-01-04',
            updatedAt: '2026-01-04',
            body: a[2].text,
          });
        }
        if (lose) {
          lose = false;
          throw Error('応答喪失');
        }
        return {};
      },
    }),
  };
  return {
    file,
    detail,
    calls,
    logs,
    options,
    prepare: () =>
      runCommand(
        [...base, '--desk', '42', '--prepare', '--prepared', file],
        options,
      ),
    send: (extra = []) =>
      runCommand(
        [...base, '--prepared', file, '--text', '返信本文', ...extra],
        options,
      ),
    fail: () => {
      failGet = true;
    },
    lose: () => {
      lose = true;
    },
    get applied() {
      return applied;
    },
    cleanup: () => fs.rmSync(dir, { recursive: true, force: true }),
  };
}

test('準備で最新条件と取消しを取得しPOSTしない', async () => {
  const h = preparedHarness();
  try {
    assert.equal(await h.prepare(), 0);
    const output = h.logs.join('\n');
    for (const value of [
      '現在の受入条件',
      '8ファイルまで',
      '10ファイルへ拡大',
      '8ファイル制限を取消',
      'コメント 3',
      '共通番号 #42',
    ])
      assert.ok(output.includes(value));
    assert.equal(h.calls.filter((c) => c[0] === 'POST').length, 0);
  } finally {
    h.cleanup();
  }
});

test('準備後の本文とコメントの変更を送信前に拒否する', async () => {
  for (const change of [
    (d) => (d.body += '変更'),
    (d) =>
      d.comments.push({ id: 5, createdAt: 'x', updatedAt: 'x', body: '追加' }),
    (d) => (d.comments[0].body += '編集'),
    (d) => d.comments.pop(),
    (d) => (d.project = 'other/repo'),
  ]) {
    const h = preparedHarness();
    try {
      await h.prepare();
      change(h.detail);
      await assert.rejects(h.send());
      assert.equal(h.calls.filter((c) => c[0] === 'POST').length, 0);
    } finally {
      h.cleanup();
    }
  }
  const h = preparedHarness();
  try {
    await h.prepare();
    assert.equal(await h.send(), 0);
    assert.equal(h.calls.filter((c) => c[0] === 'POST')[0][2].text, '返信本文');
    // 準備と送信前の照合は全文、送信後の状態の読み直しだけ要約を読む。
    assert.deepEqual(
      h.calls
        .filter((c) => c[0] === 'GET')
        .map((c) => new URLSearchParams(c[1].split('?')[1]).get('view')),
      [null, null, 'summary'],
    );
  } finally {
    h.cleanup();
  }
});

test('準備と確認の失敗では送信しない', async () => {
  for (const mode of ['initial', 'later', 'invalid', 'missing', 'large']) {
    const h = preparedHarness();
    try {
      if (mode === 'initial') h.fail();
      else if (mode === 'missing') delete h.detail.body;
      else if (mode === 'large') h.detail.body = 'a'.repeat(1000001);
      else {
        await h.prepare();
        if (mode === 'later') h.fail();
        else fs.writeFileSync(h.file, '{}');
      }
      await assert.rejects(
        mode === 'later' || mode === 'invalid' ? h.send() : h.prepare(),
      );
      assert.equal(h.calls.filter((c) => c[0] === 'POST').length, 0);
    } finally {
      h.cleanup();
    }
  }
});

test('取消対象と維持条件を確認して本文を送る', async () => {
  const h = preparedHarness();
  try {
    await h.prepare();
    const body =
      'コメント1の8ファイル制限を取消し、10ファイルへ変更します。他の受入条件は維持します。';
    assert.equal(
      await runCommand(
        [...base, '--prepared', h.file, '--text', body],
        h.options,
      ),
      0,
    );
    const post = h.calls.find((c) => c[0] === 'POST')[2];
    assert.equal(post.text, body);
    assert.equal(post.overLimitStatement, undefined);
  } finally {
    h.cleanup();
  }
});

test('準備出力とエラーに資格情報を出さない', async () => {
  const h = preparedHarness();
  try {
    h.detail.body = 'Authorization: Bearer fake-secret';
    await assert.rejects(h.prepare(), /未確認.*Issue本文/);
    assert.doesNotMatch(h.logs.join('\n'), /fake-secret/);
    assert.equal(fs.existsSync(h.file), false);
    h.fail();
    await assert.rejects(
      h.prepare(),
      (e: Error) => !e.message.includes('secretvalue'),
    );
    assert.equal(h.calls.filter((c) => c[0] === 'POST').length, 0);
  } finally {
    h.cleanup();
  }

  for (const mode of ['get', 'post', 'hidden']) {
    const h = preparedHarness();
    try {
      await h.prepare();
      h.logs.length = 0;
      const originalOpen = h.options.open;
      h.options.open = async () => {
        const desk = await originalOpen();
        return {
          ...desk,
          call: async (...args) => {
            if (
              (mode === 'get' && args[0] === 'GET') ||
              (mode === 'post' && args[0] === 'POST')
            ) {
              h.calls.push(args);
              throw Object.assign(Error('token=fake-send-secret'), {
                status: 403,
              });
            }
            return desk.call(...args);
          },
        };
      };
      if (mode === 'hidden')
        h.detail.body = 'Authorization: Bearer fake-send-secret';
      await assert.rejects(h.send(), (e: Error) => {
        assert.match(
          e.message,
          mode === 'get'
            ? /送信前の詳細取得/
            : mode === 'post'
              ? /送信結果/
              : /秘匿/,
        );
        assert.doesNotMatch(e.message, /fake-send-secret/);
        return true;
      });
      assert.doesNotMatch(h.logs.join('\n'), /fake-send-secret/);
      assert.equal(
        h.calls.filter((c) => c[0] === 'POST').length,
        mode === 'post' ? 1 : 0,
      );
    } finally {
      h.cleanup();
    }
  }

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'reply-cli-test-'));
  const tokenFile = path.join(dir, 'token');
  fs.writeFileSync(tokenFile, 'fake-cli-token');
  const detail = {
    project: 'owner/repo',
    number: 12,
    desk: 42,
    body: '条件',
    comments: [],
    status: 'waiting',
  };
  let mode = 'prepare',
    posts = 0;
  const server = createServer((req, res) => {
    if (req.method === 'POST') posts++;
    if (
      (mode === 'get' && req.method === 'GET') ||
      (mode === 'post' && req.method === 'POST')
    ) {
      res.statusCode = 400;
      return res.end(JSON.stringify({ error: 'token=fake-response-secret' }));
    }
    res.end(
      JSON.stringify(
        mode === 'hidden'
          ? { ...detail, body: 'Authorization: Bearer fake-response-secret' }
          : detail,
      ),
    );
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const cli = (args) =>
    new Promise<any>((resolve, reject) => {
      const child = spawn(process.execPath, [
        'scripts/reply-issue.ts',
        '--url',
        `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`,
        '--token-file',
        tokenFile,
        ...base,
        ...args,
      ]);
      let stdout = '',
        stderr = '';
      child.stdout.on('data', (data) => (stdout += data));
      child.stderr.on('data', (data) => (stderr += data));
      child.on('error', reject);
      child.on('close', (code, signal) =>
        resolve({ code, signal, stdout, stderr }),
      );
    });
  try {
    for (const scenario of ['get', 'post', 'hidden']) {
      const file = path.join(dir, `${scenario}.json`);
      mode = 'prepare';
      posts = 0;
      const prepared = await cli([
        '--desk',
        '42',
        '--prepare',
        '--prepared',
        file,
      ]);
      assert.equal(prepared.code, 0, prepared.stderr);
      mode = scenario;
      const result = await cli(['--prepared', file, '--text', '返信本文']);
      assert.equal(result.signal, null);
      assert.equal(result.code, 1);
      assert.match(
        result.stderr,
        scenario === 'get'
          ? /送信前の詳細取得/
          : scenario === 'post'
            ? /送信結果/
            : /秘匿/,
      );
      for (const output of [
        prepared.stdout,
        prepared.stderr,
        result.stdout,
        result.stderr,
      ]) {
        assert.doesNotMatch(output, /fake-cli-token|fake-response-secret/);
      }
      assert.equal(posts, scenario === 'post' ? 1 : 0);
    }
  } finally {
    await new Promise<any>((resolve) => server.close(resolve));
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('応答喪失後は同じIDと本文で照合する', async () => {
  const h = preparedHarness();
  try {
    await h.prepare();
    h.lose();
    const id = '12345678-1234-1234-1234-123456789abc';
    await assert.rejects(h.send(['--id', id]));
    assert.equal(await h.send(['--id', id]), 0);
    assert.equal(h.applied, 1);
    assert.deepEqual(
      h.calls.filter((c) => c[0] === 'POST').map((c) => c[2].id),
      [id, id],
    );
  } finally {
    h.cleanup();
  }
});

test('非同期返信は受理IDで結果を照会し適用後にIssueを読む', async () => {
  const id = '11111111-1111-4111-8111-111111111115';
  const calls: any[] = [];
  const logs: string[] = [];
  const call = async (method, route, data?) => {
    calls.push([method, route, data]);
    if (method === 'POST') {
      assert.equal(data.async, true);
      return {
        accepted: true,
        id,
        project: 'owner/repo',
        number: 12,
        status: 'queued',
      };
    }
    if (route.startsWith('/api/comment?'))
      return {
        id,
        project: 'owner/repo',
        number: 12,
        status: 'done',
        result: { job: 1 },
      };
    return { project: 'owner/repo', number: 12, desk: 42, status: 'queued' };
  };
  assert.equal(
    await replyIssue(
      call,
      { id, project: 'owner/repo', number: '12' },
      '指示',
      (s) => logs.push(s),
    ),
    0,
  );
  assert.deepEqual(
    calls.map((c) => c[0]),
    ['GET', 'POST', 'GET', 'GET'],
  );
  assert.match(calls[2][1], new RegExp('id=' + id));
  // 準備なしの返信は、送信前後のIssueを要約で読む。
  for (const i of [0, 3])
    assert.equal(
      new URL(calls[i][1], 'http://desk').searchParams.get('view'),
      'summary',
    );
  assert.ok(logs.some((s) => s.includes('受付済み') && s.includes(id)));
});

test('非同期返信の失敗を成功にせず送信は1回だけにする', async () => {
  const id = '11111111-1111-4111-8111-111111111116';
  let posts = 0;
  const call = async (method, route) => {
    if (method === 'POST') {
      posts++;
      return {
        accepted: true,
        id,
        project: 'owner/repo',
        number: 12,
        status: 'queued',
      };
    }
    if (route.startsWith('/api/comment?'))
      return {
        id,
        project: 'owner/repo',
        number: 12,
        status: 'failed',
        error: '送り手が失効',
      };
    return { status: 'queued' };
  };
  await assert.rejects(
    replyIssue(
      call,
      { id, project: 'owner/repo', number: '12' },
      '指示',
      () => {},
    ),
    /送り手が失効/,
  );
  assert.equal(posts, 1);
});
