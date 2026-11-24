import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import {
  duplicateStop,
  recordSent,
  strongDuplicates,
  similarTitle,
} from '../scripts/lib/duplicate-guard.ts';

const text =
  '# SQLiteからPostgreSQLへの移行の段階2から6\n\n対象: server/internal/store/a.go と server/internal/store/b.go と shared/src/c.mjs を直す。';
const ledger = () => ({
  file: path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'dup-')), 'sent.json'),
});

test('題名が同じ既存の依頼・相談・TODOは二重起票として止める', () => {
  for (const kind of ['依頼', '相談', 'TODO']) {
    const message = duplicateStop({
      text,
      project: 'o/r',
      rows: [
        { kind, title: 'SQLiteからPostgreSQLへの移行の段階2から6', number: 9 },
      ],
      ledger: ledger(),
    });
    assert.match(message, /二重起票の疑い/, kind);
  }
});

test('対象ファイルの大半が重なる既存は題名が違っても止める', () => {
  const rows = [
    {
      kind: '依頼',
      title: '別の題',
      body: '直す: server/internal/store/a.go server/internal/store/b.go',
    },
  ];
  assert.match(
    duplicateStop({ text, project: 'o/r', rows, ledger: ledger() }),
    /既存の依頼/,
  );
});

test('無関係な既存・ファイル1つだけの重なりは止めない', () => {
  const rows = [
    {
      kind: '依頼',
      title: 'Stopフックの謝罪の検査',
      body: 'scripts/chat-stop-hook.ts を直す',
    },
    { kind: '依頼', title: '画面の色', body: 'shared/src/c.mjs だけ直す' },
  ];
  assert.equal(
    duplicateStop({ text, project: 'o/r', rows, ledger: ledger() }),
    null,
  );
});

test('--not-duplicate の理由があれば通す', () => {
  const rows = [
    { kind: '依頼', title: 'SQLiteからPostgreSQLへの移行の段階2から6' },
  ];
  assert.equal(
    duplicateStop({
      text,
      project: 'o/r',
      rows,
      notDuplicate: '段階7が別',
      ledger: ledger(),
    }),
    null,
  );
});

test('自分が直近に送った依頼は台帳で止め、別プロジェクト・24時間後は止めない', () => {
  const l = ledger();
  const now = Date.parse('2026-11-03T00:00:00Z');
  recordSent(text, 'o/r', { file: l.file, now, draft: 'abc' });
  const message = duplicateStop({
    text,
    project: 'o/r',
    ledger: { ...l, now: now + 3600_000 },
  });
  assert.match(message, /自分が.*に送った依頼/);
  assert.match(message, /abc/);
  assert.equal(
    duplicateStop({
      text,
      project: 'o/other',
      ledger: { ...l, now: now + 3600_000 },
    }),
    null,
  );
  assert.equal(
    duplicateStop({
      text,
      project: 'o/r',
      ledger: { ...l, now: now + 25 * 3600_000 },
    }),
    null,
  );
});

test('壊れた台帳でも落ちない', () => {
  const l = ledger();
  fs.writeFileSync(l.file, '{broken');
  assert.equal(duplicateStop({ text, project: 'o/r', ledger: l }), null);
});

test('題名の類似判定', () => {
  assert.equal(
    similarTitle('SQLite移行の段階2から6', 'SQLite移行の段階2〜6'),
    true,
  );
  assert.equal(similarTitle('Stopフックの謝罪', '画面の色の変更'), false);
  assert.equal(strongDuplicates(text, []).length, 0);
});

test('受領確認済みの再送だけ台帳の重複停止を越え、未受領と確認失敗は止まる', async (t) => {
  const { default: http } = await import('node:http');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dup-replay-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const token = path.join(dir, 'token');
  const ledgerFile = path.join(dir, 'sent.json');
  fs.writeFileSync(token, 'test');
  recordSent(text, 'o/r', { file: ledgerFile });
  const id = '11111111-1111-4111-8111-111111111111';
  const missing = '22222222-2222-4222-8222-222222222222';
  const failed = '33333333-3333-4333-8333-333333333333';
  let posts = 0;
  const server = http.createServer((req, res) => {
    res.setHeader('content-type', 'application/json');
    if (req.method === 'POST' && req.url === '/api/issues') {
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
    if (
      req.url === `/api/drafts/${missing}` ||
      req.url === `/api/drafts/${failed}`
    ) {
      res.statusCode = req.url.endsWith(missing) ? 404 : 500;
      return res.end('{}');
    }
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
      const args = [
        'scripts/create-issue.ts',
        '--project',
        'o/r',
        '--tier',
        'light',
        '--text',
        text,
      ];
      if (sendId) args.push('--send-id', sendId);
      const child = spawn(process.execPath, args, {
        env: {
          ...process.env,
          HOME: dir,
          ISSUE_DESK_SENT_LEDGER: ledgerFile,
          ISSUE_DESK_TOKEN_FILE: token,
          ISSUE_DESK_URL: `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`,
        },
      });
      let output = '';
      child.stdout.on('data', (b) => (output += b));
      child.stderr.on('data', (b) => (output += b));
      child.on('close', (status) => resolve({ status, output }));
    });
  for (const sendId of [undefined, missing, failed]) {
    const result = await run(sendId);
    assert.equal(result.status, 1, result.output);
    assert.match(result.output, /二重起票の疑い/);
    if (sendId === failed)
      assert.ok(result.output.includes(`--check ${sendId}`), result.output);
    else assert.doesNotMatch(result.output, /受領を確認してください:/);
  }
  assert.equal(posts, 0);
  const replay = await run(id);
  assert.equal(replay.status, 0, replay.output);
  assert.match(replay.output, new RegExp(`下書き: ${id}`));
  assert.equal(posts, 1);
});

test('create-issue.ts は重複の候補があると何も送らず終了コード1（再現: 受付の模擬サーバー）', async () => {
  const http = await import('node:http');
  const calls = [];
  const server = http.createServer((req, res) => {
    calls.push(`${req.method} ${req.url}`);
    res.setHeader('content-type', 'application/json');
    if (
      req.url.startsWith('/api/issues') &&
      req.method === 'GET' &&
      !req.url.includes('number')
    )
      return res.end(
        JSON.stringify({
          total: 1,
          hasMore: false,
          issues: [
            {
              project: 'o/r',
              state: 'open',
              number: 7,
              title: 'SQLiteからPostgreSQLへの移行の段階2から6',
            },
          ],
        }),
      );
    if (req.url.startsWith('/api/issue?'))
      return res.end(
        JSON.stringify({
          project: 'o/r',
          state: 'open',
          number: 7,
          title: 'SQLiteからPostgreSQLへの移行の段階2から6',
          body: 'x',
        }),
      );
    if (req.url.startsWith('/api/drafts') && req.method === 'GET')
      return res.end('[]');
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
    res.statusCode = 500;
    res.end('{}');
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'dup-cli-'));
  fs.writeFileSync(path.join(dir, 'req.md'), text);
  fs.writeFileSync(path.join(dir, 'token'), 'x\n');
  const child = spawn(
    process.execPath,
    [
      'scripts/create-issue.ts',
      '--project',
      'o/r',
      '--tier',
      'light',
      '--file',
      path.join(dir, 'req.md'),
    ],
    {
      env: {
        ...process.env,
        ISSUE_DESK_SENT_LEDGER: path.join(dir, 'l.json'),
        ISSUE_DESK_TOKEN_FILE: path.join(dir, 'token'),
        ISSUE_DESK_URL: `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`,
        HOME: dir,
      },
    },
  );
  let stderr = '';
  child.stderr.on('data', (chunk) => (stderr += chunk));
  const run = await new Promise<any>((resolve) =>
    child.on('close', (status) => resolve({ status, stderr })),
  );
  server.close();
  assert.ok(!calls.some((c) => c.startsWith('POST')), calls.join(','));
  assert.equal(run.status, 1, run.stderr);
  assert.match(run.stderr, /二重起票の疑い/);
});
