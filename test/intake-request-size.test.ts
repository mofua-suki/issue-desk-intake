import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  missingChangeFilesWarning,
  namedRequestFiles,
  requestSizeWarning,
} from '../shared/src/intake-request-size.ts';

const files = (n) =>
  Array.from({ length: n }, (_, i) => `src/f${i}.js`).join(' ');

test('変更対象の目安は重複を除き複数の変更範囲を合算して警告する', () => {
  for (const n of [4, 5, 12]) {
    const text = `## 変更範囲\n${files(n)} src/f0.js\n## 資料\n${files(20)}`;
    if (n <= 4) assert.equal(requestSizeWarning(text), null);
    else
      assert.match(
        requestSizeWarning(text),
        new RegExp(`${n}件.*送信は続けます`),
      );
  }
  assert.match(
    requestSizeWarning(
      `## 変更範囲\n${files(4)}\n## 資料\n${files(20)}\n## 変更範囲\nsrc/f4.js src/f0.js`,
    ),
    /5件/,
  );
  assert.equal(requestSizeWarning(`## 変更範囲\n## 資料\n${files(20)}`), null);
  assert.match(requestSizeWarning(files(5)), /5件/);
  assert.equal(
    namedRequestFiles('src/a.js src/a.js https://example.com/a.js').length,
    1,
  );
});

test('フェンス内の見出しは変更範囲の境界に使わない', () => {
  for (const fence of ['```', '~~~']) {
    assert.equal(
      requestSizeWarning(
        `## 変更範囲\n${files(4)}\n## 資料\n${fence}md\n## 変更範囲\n${files(8)}\n${fence}`,
      ),
      null,
    );
    assert.match(
      requestSizeWarning(
        `## 変更範囲\n${files(4)}\n${fence}md\n## 資料\n${fence}\nsrc/f4.js`,
      ),
      /5件/,
    );
  }
  assert.equal(
    requestSizeWarning(
      `## 変更範囲\n${files(4)}\n\`\`\`bad\`info\n## 資料\n${files(8)}`,
    ),
    null,
  );
});

test('ファイルがない依頼を警告する', () => {
  assert.match(
    missingChangeFilesWarning('割り込みで直して'),
    /変更するファイルが1件もありません/,
  );
  assert.equal(
    missingChangeFilesWarning('server/internal/filing/plan.go を直す'),
    null,
  );
});

test('CLIは新規相談の目安超過を警告し送信し、検査専用では送信しない', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'size-cli-'));
  fs.writeFileSync(path.join(dir, 'token'), 'test');
  const posted = [];
  const server = http.createServer(async (req, res) => {
    res.setHeader('content-type', 'application/json');
    if (req.method === 'POST' && req.url === '/api/issues') {
      let body = '';
      for await (const chunk of req) body += chunk;
      const data = JSON.parse(body);
      posted.push(data);
      return res.end(JSON.stringify({ id: data.id, status: 'draft' }));
    }
    if (req.url.startsWith('/api/drafts/'))
      return res.end(
        JSON.stringify({
          status: 'draft',
          messages: [{ role: 'assistant', text: '質問' }],
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
  const run = (text, extra = []) =>
    new Promise<any>((resolve) => {
      const p = spawn(
        process.execPath,
        [
          'scripts/create-issue.ts',
          '--project',
          'o/r',
          '--tier',
          'light',
          '--text',
          text,
          ...extra,
        ],
        {
          env: {
            ...process.env,
            HOME: dir,
            ISSUE_DESK_SENT_LEDGER: path.join(dir, `ledger-${Math.random()}`),
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
    for (const n of [4, 5, 12]) {
      const text = `## 変更範囲\n${files(n)} src/f0.js\n## 資料\n${files(20)}`;
      const result = await run(text);
      assert.equal(result.status, 0, result.output);
      if (n > 4) assert.match(result.output, new RegExp(`警告:.*${n}件`));
      else assert.doesNotMatch(result.output, /警告: 名指ししたファイル/);
      assert.equal(posted.at(-1).text, text);
    }
    const check = await run(files(5), ['--check-only']);
    assert.equal(check.status, 0, check.output);
    assert.match(check.output, /警告:.*5件/);
    assert.match(check.output, /over_spec/);
    assert.equal(posted.length, 3);
  } finally {
    await new Promise<any>((resolve) => server.close(resolve));
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
