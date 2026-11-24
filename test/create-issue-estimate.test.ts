import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';

const id = '11111111-1111-4111-8111-111111111111';
async function run(
  text,
  {
    estimate = { files: 3, lines: 90 } as { files?: number; lines?: number },
    request = '変更ファイル 3本・変更行 約100行',
    explicit = false,
    conflict = false,
    fail = false,
  } = {},
) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'estimate-'));
  fs.writeFileSync(path.join(dir, 'token'), 'test');
  let posted,
    outputAtPost = '',
    output = '',
    fullReads = 0;
  const draft = {
    id,
    project: 'o/r',
    status: 'draft',
    revision: 7,
    estimate,
    messages: [{ role: 'user', text: request }],
  };
  const server = http.createServer((req, res) => {
    res.setHeader('content-type', 'application/json');
    if (req.method === 'POST') {
      outputAtPost = output;
      let body = '';
      req.on('data', (part) => (body += part));
      req.on('end', () => {
        posted = JSON.parse(body);
        if (conflict) {
          res.statusCode = 409;
          res.end(JSON.stringify({ error: '競合' }));
        } else res.end(JSON.stringify({ id }));
      });
      return;
    }
    if (req.url.startsWith(`/api/drafts/${id}`)) {
      if (!req.url.includes('?')) {
        fullReads++;
        if (fail && fullReads === 1) {
          res.statusCode = 500;
          return res.end('{}');
        }
        return res.end(JSON.stringify(draft));
      }
      return res.end(
        JSON.stringify({
          id,
          project: 'o/r',
          status: 'draft',
          revision: 7,
          view: 'summary',
        }),
      );
    }
    res.end('[]');
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  try {
    const code = await new Promise((resolve) => {
      const child = spawn(
        process.execPath,
        [
          'scripts/create-issue.ts',
          '--draft',
          id,
          '--text',
          text,
          '--timeout',
          '1',
          ...(explicit ? ['--expect-revision', '5'] : []),
        ],
        {
          env: {
            ...process.env,
            HOME: dir,
            ISSUE_DESK_TOKEN_FILE: path.join(dir, 'token'),
            ISSUE_DESK_URL: `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`,
          },
        },
      );
      child.stdout.on('data', (x) => (output += x));
      child.stderr.on('data', (x) => (output += x));
      child.on('close', resolve);
    });
    return { code, posted, output, outputAtPost, fullReads };
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

test('了承の4形式で見込みを送信前に表示し超過を警告して送信する', async () => {
  for (const text of ['A', '1-A', '承認します', 'この計画を承認します']) {
    const r = await run(text, {
      estimate: { files: 5, lines: 90 },
      request: '変更ファイル 10本・変更行 約100行',
    });
    assert.equal(r.code, 0, r.output);
    assert.match(r.outputAtPost, /受付の見込み.*5/);
    assert.match(r.outputAtPost, /警告/);
    assert.equal(r.posted.text, text);
    assert.equal(r.posted.expectedRevision, 7);
  }
});

test('依頼文とのファイル数と行数の超過、範囲内、欠落と読取失敗を区別する', async () => {
  for (const estimate of [
    { files: 3, lines: 90 },
    { files: 2, lines: 101 },
  ]) {
    const r = await run('A', {
      estimate,
      request: '変更ファイル 2本・変更行 約100行',
    });
    assert.equal(r.code, 0, r.output);
    assert.match(r.outputAtPost, /警告/);
    assert.ok(r.posted);
  }
  const within = await run('A', {});
  assert.equal(within.code, 0, within.output);
  assert.doesNotMatch(within.outputAtPost, /警告|未確認/);
  const range = await run('A', {
    estimate: { files: 3, lines: 101 },
    request: '変更ファイル 1-2本・変更行 80-100行',
  });
  assert.equal(range.code, 0, range.output);
  assert.match(range.outputAtPost, /ファイル数: 3、依頼文: 2/);
  assert.match(range.outputAtPost, /行数: 101、依頼文: 100/);
  assert.equal((range.outputAtPost.match(/警告/g) || []).length, 2);
  const missing = await run('A', {
    estimate: { files: 2 },
    request: '見込みなし',
  });
  assert.equal(missing.code, 0, missing.output);
  assert.match(missing.outputAtPost, /未確認/);
  assert.ok(missing.posted);
  const failed = await run('A', { fail: true });
  assert.equal(failed.code, 0, failed.output);
  assert.match(failed.outputAtPost, /読取失敗.*未確認/);
  assert.equal(failed.posted.expectedRevision, 7);
});

test('通常回答は見込みを読まず明示版を保ち、競合は送信失敗として最新質問を表示する', async () => {
  const normal = await run('A（補足）', { explicit: true });
  assert.equal(normal.code, 0, normal.output);
  assert.doesNotMatch(normal.outputAtPost, /受付の見込み/);
  assert.equal(normal.posted.expectedRevision, 5);
  const explicit = await run('1-A', { explicit: true });
  assert.equal(explicit.code, 0, explicit.output);
  assert.equal(explicit.posted.expectedRevision, 5);
  const conflict = await run('A', { conflict: true });
  assert.equal(conflict.code, 1, conflict.output);
  assert.equal(conflict.posted.expectedRevision, 7);
  assert.match(conflict.output, /最新版: 7/);
});
