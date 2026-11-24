import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer } from 'node:http';
import { consult, remoteDesk, requestArgs } from '../scripts/desk-client.ts';

test('通信失敗後の回答再試行は送信IDと本文と版を保持する', async () => {
  const draft = '11111111-1111-4111-8111-111111111111';
  const sendId = '22222222-2222-4222-8222-222222222222';
  const posts = [];
  const server = createServer(async (req, res) => {
    if (req.method === 'POST') {
      let body = '';
      for await (const chunk of req) body += chunk;
      posts.push(JSON.parse(body));
      if (posts.length === 1) {
        req.socket.destroy();
        return;
      }
      res
        .writeHead(200, { 'content-type': 'application/json' })
        .end(JSON.stringify({ id: draft }));
      return;
    }
    res.writeHead(200, { 'content-type': 'application/json' }).end(
      JSON.stringify({
        id: draft,
        revision: 8,
        status: 'draft',
        messages: [],
        questions: [],
      }),
    );
  });
  const dir = mkdtempSync(join(tmpdir(), 'desk-retry-test-'));
  try {
    writeFileSync(join(dir, 'token'), 'synthetic-test-token');
    await new Promise<void>((resolve) =>
      server.listen(0, '127.0.0.1', resolve),
    );
    const client = remoteDesk({
      base: `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`,
      tokenFile: join(dir, 'token'),
    });
    assert.equal(
      await consult(
        client.call,
        { draft, text: '画像についての回答', sendId, expectRevision: 7 },
        'x',
        '',
        0,
      ),
      0,
    );
    assert.equal(posts.length, 2);
    assert.deepEqual(posts[0], {
      id: sendId,
      messageId: sendId,
      text: '画像についての回答',
      expectedRevision: 7,
    });
    assert.deepEqual(posts[1], posts[0]);
  } finally {
    await new Promise((resolve) => server.close(resolve));
    rmSync(dir, { recursive: true, force: true });
  }
});

test('回答は明示版または直前GETの版を送り、取得失敗時は送らない', async () => {
  for (const version of [undefined, 3]) {
    const calls = [];
    const call = async (method, route, body) => {
      calls.push({ method, route, body });
      return method === 'POST'
        ? { id: 'draft' }
        : {
            id: 'draft',
            revision: 5,
            status: 'draft',
            messages: [],
            questions: [],
          };
    };
    assert.equal(
      await consult(
        call,
        { draft: 'draft', text: '回答', expectRevision: version },
        'x',
        '',
        0,
      ),
      0,
    );
    assert.equal(
      calls.find((c) => c.method === 'POST').body.expectedRevision,
      version ?? 5,
    );
    // 版だけを使う送信前の読取は要約で読み、明示版があれば読まない。
    assert.deepEqual(
      calls.slice(
        0,
        calls.findIndex((c) => c.method === 'POST'),
      ),
      version === undefined
        ? [
            {
              method: 'GET',
              route: '/api/drafts/draft?view=summary',
              body: undefined,
            },
          ]
        : [],
    );
  }
  let posts = 0;
  await assert.rejects(
    consult(
      async (method) => {
        if (method === 'POST') posts++;
        return {};
      },
      { draft: 'draft', text: '回答' },
      'x',
    ),
  );
  assert.equal(posts, 0);
  assert.equal(
    requestArgs(['--draft', 'abc', '--expect-revision', '0'], '')
      .expectRevision,
    0,
  );
  assert.throws(() =>
    requestArgs(['--draft', 'abc', '--expect-revision', '-1'], ''),
  );
});

test('409では理由と最新質問を表示して再送せず終了する', async () => {
  let posts = 0;
  const lines = [];
  const old = console.error;
  console.error = (...args) => lines.push(args.join(' '));
  try {
    const code = await consult(
      async (method) => {
        if (method === 'POST') {
          posts++;
          throw Object.assign(Error('版が古い'), { status: 409 });
        }
        return { revision: 9, questions: ['最新の質問'] };
      },
      { draft: 'draft', text: '回答', expectRevision: 2 },
      'x',
    );
    assert.equal(code, 1);
    assert.equal(posts, 1);
    assert.match(lines.join('\n'), /版が古い/);
    assert.match(lines.join('\n'), /最新の質問/);
  } finally {
    console.error = old;
  }
});

test('409後に質問が空または取得失敗でも再送しない', async () => {
  for (const failure of [false, true]) {
    let posts = 0;
    const lines = [];
    const old = console.error;
    console.error = (...args) => lines.push(args.join(' '));
    try {
      const code = await consult(
        async (method) => {
          if (method === 'POST') {
            posts++;
            throw Object.assign(Error('拒否'), { status: 409 });
          }
          if (failure) throw Error('通信失敗');
          return { revision: 3, questions: [] };
        },
        { draft: 'draft', text: '回答', expectRevision: 2 },
        'x',
      );
      assert.equal(code, 1);
      assert.equal(posts, 1);
      assert.match(lines.join('\n'), failure ? /未確認/ : /質問はありません/);
    } finally {
      console.error = old;
    }
  }
});
