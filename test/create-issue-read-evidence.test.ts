import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

test('新規相談の証拠を本文と分けて送信し、不正入力は送信しない', async (t) => {
  const dir = mkdtempSync(path.join(tmpdir(), 'evidence-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  writeFileSync(path.join(dir, 'token'), 'test');
  const file = path.join(dir, 'evidence.json');
  const evidence = {
    version: 1,
    documents: [
      {
        id: 'a',
        project: 'o/r',
        path: 'a',
        revision: 'a'.repeat(40),
        start: 1,
        end: 1,
        text: 'one\r\n',
      },
    ],
    facts: [{ statement: 'fact', documents: ['a'] }],
  };
  const posts = [];
  let postCount = 0;
  const server = http.createServer(async (req, res) => {
    res.setHeader('content-type', 'application/json');
    if (req.method === 'POST') postCount++;
    if (req.method === 'POST' && req.url === '/api/issues') {
      let body = '';
      for await (const chunk of req) body += chunk;
      const value = JSON.parse(body);
      posts.push(value);
      return res.end(JSON.stringify({ id: value.id, status: 'draft' }));
    }
    if (req.url.startsWith('/api/drafts/'))
      return res.end(
        JSON.stringify({ id: posts.at(-1)?.id, status: 'draft', messages: [] }),
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
    res.end('[]');
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const run = (args) =>
    new Promise<any>((resolve) => {
      const child = spawn(
        process.execPath,
        ['scripts/create-issue.ts', ...args],
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
      child.stdout.on('data', (v) => (output += v));
      child.stderr.on('data', (v) => (output += v));
      child.on('close', (status) => resolve({ status, output }));
    });
  writeFileSync(file, JSON.stringify(evidence));
  let r = await run([
    '--project',
    'o/r',
    '--tier',
    'light',
    '--text',
    '独立した本文',
    '--read-evidence-file',
    file,
  ]);
  assert.equal(r.status, 0, r.output);
  assert.equal(posts[0].text, '独立した本文');
  assert.equal(posts[0].readEvidence.documents[0].text, 'one\n');
  r = await run(['--project', 'o/r', '--tier', 'light', '--text', '別の依頼']);
  assert.equal(r.status, 0, r.output);
  assert.equal('readEvidence' in posts[1], false);
  for (const contents of [
    'not json',
    JSON.stringify({ ...evidence, version: 2 }),
    JSON.stringify({
      ...evidence,
      documents: [{ ...evidence.documents[0], text: 'a'.repeat(65536) }],
    }),
  ]) {
    writeFileSync(file, contents);
    r = await run([
      '--project',
      'o/r',
      '--tier',
      'light',
      '--text',
      '本文',
      '--read-evidence-file',
      file,
    ]);
    assert.equal(r.status, 1, r.output);
  }
  writeFileSync(file, JSON.stringify(evidence));
  for (const [args, message] of [
    [
      ['--draft', 'abc', '--text', '本文'],
      '--read-evidence-file は新規相談にだけ指定します。',
    ],
    [
      ['--check', 'abc'],
      '--check は下書きを読むだけなので、--timeout 以外と同時に指定できません。',
    ],
    [
      ['--draft', 'abc', '--submit'],
      '--submit は --draft <下書きID> とだけ使えます（本文・--tier・--hold などとは同時に指定できません）。',
    ],
    [
      ['--project', 'o/r', '--check-only', '--text', '本文'],
      '--read-evidence-file は --check-only と併用できません。',
    ],
  ]) {
    const count = postCount;
    r = await run([...args, '--read-evidence-file', file]);
    assert.equal(r.status, 1, r.output);
    assert.ok(r.output.startsWith(message), r.output);
    assert.equal(postCount, count);
  }
  assert.equal(posts.length, 2);
});
