import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

test('応答喪失後に同じ送信IDで照合し、異なる本文は409で拒否する', async () => {
  const conflict = '別の文面でも送信IDは使用済みです。';
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'replay-'));
  fs.writeFileSync(path.join(dir, 'token'), 'test');
  let saved;
  let posts = 0;
  const server = http.createServer(async (req, res) => {
    res.setHeader('content-type', 'application/json');
    if (req.method === 'POST' && req.url === '/api/issues') {
      let body = '';
      for await (const chunk of req) body += chunk;
      const data = JSON.parse(body);
      posts++;
      if (!saved) saved = data;
      if (posts <= 8) {
        req.socket.destroy();
        return;
      }
      if (data.text !== saved.text) {
        res.statusCode = 409;
        return res.end(
          JSON.stringify({ error: conflict, code: 'send_id_conflict' }),
        );
      }
      return res.end(JSON.stringify({ id: saved.id, status: 'draft' }));
    }
    if (req.url === '/api/drafts/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa') {
      res.statusCode = 404;
      return res.end('{}');
    }
    if (req.url === `/api/drafts/${saved?.id}`)
      return res.end(
        JSON.stringify({
          id: saved.id,
          status: 'draft',
          messages: [
            {
              role: 'user',
              id: saved.id,
              requestId: saved.id,
              text: saved.text,
            },
            { role: 'assistant', text: '質問' },
            { role: 'user', requestId: 'later', text: '回答' },
          ],
        }),
      );
    if (req.method === 'GET' && req.url.startsWith('/api/issues/search?'))
      return res.end(
        JSON.stringify({
          total: saved ? 1 : 0,
          hasMore: false,
          issues: saved
            ? [
                {
                  project: 'o/r',
                  state: 'open',
                  number: 7,
                  title: 'SQLiteからPostgreSQLへの移行の段階2から6',
                },
              ]
            : [],
        }),
      );
    if (req.url.startsWith('/api/issue?'))
      return res.end(
        JSON.stringify({
          project: 'o/r',
          state: 'open',
          number: 7,
          title: 'SQLiteからPostgreSQLへの移行の段階2から6',
          body: 'SQLiteからPostgreSQLへの移行の段階2から6',
        }),
      );
    if (req.url.startsWith('/api/todos'))
      return res.end(
        JSON.stringify({
          items: [],
          total: 0,
          page: 1,
          limit: 200,
          hasMore: false,
        }),
      );
    return res.end('[]');
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const run = (args) =>
    new Promise<any>((resolve) => {
      const p = spawn(
        process.execPath,
        [
          'scripts/create-issue.ts',
          '--project',
          'o/r',
          '--tier',
          'light',
          ...args,
        ],
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
      p.stdout.on('data', (x) => (output += x));
      p.stderr.on('data', (x) => (output += x));
      p.on('close', (status) => resolve({ status, output }));
    });
  try {
    const first = await run([
      '--text',
      'SQLiteからPostgreSQLへの移行の段階2から6',
    ]);
    const id = first.output.match(/送信ID: ([a-f0-9-]+)/)?.[1];
    assert.ok(id, first.output);
    assert.equal(first.status, 1);
    assert.match(first.output, new RegExp(`--send-id ${id}`));
    const failedReplay = await run([
      '--text',
      'SQLiteからPostgreSQLへの移行の段階2から6',
      '--send-id',
      id,
    ]);
    assert.equal(failedReplay.status, 1);
    assert.match(failedReplay.output, /同じコマンドで再実行/);
    assert.doesNotMatch(failedReplay.output, /同じ引数に --send-id/);
    const replay = await run([
      '--text',
      'SQLiteからPostgreSQLへの移行の段階2から6',
      '--send-id',
      id,
    ]);
    assert.equal(replay.status, 0, replay.output);
    assert.match(replay.output, new RegExp(`下書き: ${id}`));
    const other = await run([
      '--text',
      'SQLiteからPostgreSQLへの移行の段階2から6',
      '--send-id',
      'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
    ]);
    assert.equal(other.status, 1, other.output);
    assert.match(other.output, /二重起票の疑い/);
    assert.match(other.output, /未受領/);
    assert.doesNotMatch(other.output, /受領を確認してください:.*--check/);
    const mismatch = await run(['--text', '別の本文', '--send-id', id]);
    assert.equal(mismatch.status, 1);
    assert.ok(mismatch.output.includes(conflict));
    assert.doesNotMatch(mismatch.output, /受け付けられていません/);
    assert.equal(posts, 10);
  } finally {
    server.close();
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('送信IDはUUIDの新規相談だけに指定できる', async () => {
  const { requestArgs } = await import('../scripts/desk-client.ts');
  const id = '11111111-1111-4111-8111-111111111111';
  assert.equal(
    requestArgs(['--project', 'o/r', '--hold', '--send-id', id], '').sendId,
    id,
  );
  assert.equal(
    requestArgs(
      ['--project', 'o/r', '--send-id', 'ABCDEFAB-CDEF-4ABC-8ABC-ABCDEFABCDEF'],
      '',
    ).sendId,
    'abcdefab-cdef-4abc-8abc-abcdefabcdef',
  );
  for (const args of [
    ['--project', 'o/r', '--send-id', 'invalid'],
    ['--draft', id, '--send-id', id],
    ['--check', id, '--send-id', id],
    ['--draft', id, '--submit', '--send-id', id],
  ])
    assert.throws(() => requestArgs(args, ''));
});

test('受領確認の結果を表示し、確認済み再送だけ閉じた参照を越える', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'receipt-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  fs.writeFileSync(path.join(dir, 'token'), 'test');
  const id = 'abcdefab-cdef-4abc-8abc-abcdefabcdef';
  const missing = '22222222-2222-4222-8222-222222222222';
  const failed = '33333333-3333-4333-8333-333333333333';
  const mismatch = '44444444-4444-4444-8444-444444444444';
  let posts = 0;
  const server = http.createServer(async (req, res) => {
    res.setHeader('content-type', 'application/json');
    if (req.method === 'POST' && req.url === '/api/issues') {
      let body = '';
      for await (const chunk of req) body += chunk;
      assert.equal(JSON.parse(body).id, id);
      posts++;
      return res.end(JSON.stringify({ id, status: 'draft' }));
    }
    if (req.url === `/api/drafts/${id}`)
      return res.end(
        JSON.stringify({
          id,
          receiptId: id,
          requestId: id,
          accepted: true,
          status: 'draft',
          messages: [],
        }),
      );
    if (req.url === `/api/drafts/${mismatch}`)
      return res.end(JSON.stringify({ id: mismatch, messages: [] }));
    if (
      req.url === `/api/drafts/${missing}` ||
      req.url === `/api/drafts/${failed}`
    ) {
      res.statusCode = req.url.endsWith(missing) ? 404 : 500;
      return res.end('{}');
    }
    if (req.url.startsWith('/api/issue?'))
      return res.end(
        JSON.stringify({
          project: 'o/r',
          number: 42,
          state: 'closed',
          status: 'failed',
          title: '過去の依頼',
        }),
      );
    if (req.url.startsWith('/api/todos'))
      return res.end(
        JSON.stringify({
          items: [],
          total: 0,
          page: 1,
          limit: 200,
          hasMore: false,
        }),
      );
    return res.end('[]');
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const run = (sendId) =>
    new Promise<any>((resolve) => {
      const child = spawn(
        process.execPath,
        [
          'scripts/create-issue.ts',
          '--project',
          'o/r',
          '--tier',
          'light',
          '--text',
          '閉じた依頼 #42 の再検討',
          '--send-id',
          sendId,
        ],
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
      child.on('close', (status) => resolve({ status, output }));
    });
  for (const [sendId, message] of [
    [missing, /未受領/],
    [failed, /確認失敗/],
    [mismatch, /受領の内容が一致しません/],
  ] as [string, RegExp][]) {
    const result = await run(sendId);
    assert.equal(result.status, 1, result.output);
    assert.match(result.output, message);
    assert.match(result.output, /マージされずに閉じた/);
    if (sendId === missing) assert.doesNotMatch(result.output, /--check/);
    if (sendId === mismatch) assert.doesNotMatch(result.output, /確認失敗/);
  }
  const replay = await run(id.toUpperCase());
  assert.equal(replay.status, 0, replay.output);
  assert.match(replay.output, /受領確認済み/);
  assert.equal(posts, 1);
});

test('通常の下書きは一致する利用者の発言がある場合だけ再送と認める', async () => {
  const { confirmedReceipt } = await import(
    '../scripts/lib/consultation-replay.ts'
  );
  const id = '11111111-1111-4111-8111-111111111111';
  const check = (value) => confirmedReceipt(async () => value, id);
  assert.equal(
    await check({ accepted: true, receiptId: id, requestId: id }),
    'confirmed',
  );
  assert.equal(
    await check({ accepted: true, receiptId: id, requestId: 'other' }),
    'mismatch',
  );
  assert.equal(
    await check({ accepted: false, receiptId: id, requestId: id }),
    'mismatch',
  );
  assert.equal(
    await check({ id, messages: [{ role: 'user', requestId: id }] }),
    'confirmed',
  );
  assert.equal(
    await check({ id: 'other', messages: [{ role: 'user', requestId: id }] }),
    'mismatch',
  );
  assert.equal(
    await check({ id, messages: [{ role: 'assistant', requestId: id }] }),
    'mismatch',
  );
  assert.equal(await check({ id, messages: [] }), 'mismatch');
  assert.equal(
    await confirmedReceipt(async () => {
      throw Error('forbidden');
    }, id),
    'failed',
  );
  assert.equal(
    await confirmedReceipt(async () => {
      throw Object.assign(Error('missing'), { status: 404 });
    }, id),
    'missing',
  );
});

test('送信ID衝突は応答文ではなくコードで判定する', async () => {
  const { consult } = await import('../scripts/desk-client.ts');
  const args = {
    project: 'o/r',
    text: '本文',
    sendId: '11111111-1111-4111-8111-111111111111',
  };
  const run = (message, code) =>
    consult(
      async () => {
        throw Object.assign(Error(message), { status: 409, code });
      },
      args,
      'scripts/create-issue.ts',
    );
  await assert.rejects(
    run('変更された衝突文面', 'send_id_conflict'),
    (error: Error) => error.message === '変更された衝突文面',
  );
  await assert.rejects(
    run('送信IDは別の依頼で使用済みです。', 'other_conflict'),
    (error: Error) => /受け付けられていません/.test(error.message),
  );
});

test('--draft <ID> --retry は retry だけを送る、拒否と併用エラーを扱う', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'draft-retry-'));
  fs.writeFileSync(path.join(dir, 'token'), 'test');
  const id = '11111111-1111-4111-8111-111111111111';
  let requests = [],
    refuse = false,
    lost = false;
  const server = http.createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    requests.push({ method: req.method, url: req.url, body });
    res.setHeader('content-type', 'application/json');
    if (req.method === 'POST') {
      if (lost) {
        req.socket.destroy();
        return;
      }
      if (refuse) {
        res.statusCode = 409;
        return res.end(JSON.stringify({ error: '再試行できません' }));
      }
      return res.end(JSON.stringify({ id }));
    }
    return res.end(
      JSON.stringify({ id, status: 'draft', revision: 1, messages: [] }),
    );
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => {
    server.close();
    fs.rmSync(dir, { recursive: true, force: true });
  });
  const run = (args) =>
    new Promise<any>((resolve) => {
      const p = spawn(
        process.execPath,
        ['scripts/create-issue.ts', '--draft', id, '--retry', ...args],
        {
          env: {
            ...process.env,
            HOME: dir,
            ISSUE_DESK_TOKEN_FILE: path.join(dir, 'token'),
            ISSUE_DESK_URL: `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`,
          },
        },
      );
      let output = '';
      p.stdout.on('data', (x) => (output += x));
      p.stderr.on('data', (x) => (output += x));
      p.on('close', (status) => resolve({ status, output }));
    });
  const ok = await run(['--timeout', '1']);
  assert.equal(ok.status, 0, ok.output);
  assert.equal(requests[0].url, `/api/drafts/${id}/chat`);
  assert.equal(requests[0].method, 'POST');
  assert.deepEqual(JSON.parse(requests[0].body), { retry: true });
  requests = [];
  refuse = true;
  const rejected = await run([]);
  assert.equal(rejected.status, 1);
  assert.match(rejected.output, /再試行できません/);
  for (const args of [
    ['--text', '本文'],
    ['--submit'],
    ['--project', 'o/r'],
    ['--tier', 'light'],
  ]) {
    requests = [];
    const invalid = await run(args);
    assert.equal(invalid.status, 1, invalid.output);
    assert.equal(requests.length, 0);
  }
  requests = [];
  refuse = false;
  lost = true;
  const unknown = await run([]);
  assert.equal(unknown.status, 2, unknown.output);
  assert.match(unknown.output, /--check/);
});
