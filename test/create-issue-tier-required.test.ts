import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// The intake no longer judges the consultation tier, so a new request must carry
// --tier. Without it create-issue.ts stops before connecting; an answer to an
// existing draft keeps --tier optional and still reaches the intake.
test('新しい依頼は --tier が無いと何も送らず止まり、--draft の回答は --tier 無しで送る', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tier-required-'));
  fs.writeFileSync(path.join(dir, 'token'), 'test');
  const requests = [];
  const draft = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
  const server = http.createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    requests.push(`${req.method} ${req.url}`);
    res.setHeader('content-type', 'application/json');
    if (req.method === 'POST' && req.url === `/api/drafts/${draft}/chat`) {
      assert.equal('tier' in JSON.parse(body), false);
      return res.end(
        JSON.stringify({
          id: draft,
          status: 'draft',
          revision: 1,
          messages: [],
        }),
      );
    }
    if (req.url.split('?')[0] === `/api/drafts/${draft}`)
      return res.end(
        JSON.stringify({
          id: draft,
          status: 'draft',
          revision: 0,
          messages: [],
        }),
      );
    return res.end('[]');
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const run = (args) =>
    new Promise<any>((resolve) => {
      const p = spawn(process.execPath, ['scripts/create-issue.ts', ...args], {
        env: {
          ...process.env,
          HOME: dir,
          ISSUE_DESK_SENT_LEDGER: path.join(dir, 'ledger'),
          ISSUE_DESK_TOKEN_FILE: path.join(dir, 'token'),
          ISSUE_DESK_URL: `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`,
        },
      });
      let output = '';
      p.stdout.on('data', (x) => (output += x));
      p.stderr.on('data', (x) => (output += x));
      p.on('close', (status) => resolve({ status, output }));
    });
  try {
    for (const args of [
      ['--project', 'o/r', '--text', '依頼'],
      ['--project', 'o/r', '--hold', '--text', '依頼'],
    ]) {
      const r = await run(args);
      assert.equal(r.status, 1, r.output);
      assert.match(
        r.output,
        /--tier light\|standard\|heavy を必ず付けてください/,
      );
    }
    assert.deepEqual(
      requests,
      [],
      '--tier が無い新しい依頼は受付へ何も送らない',
    );
    const answer = await run([
      '--draft',
      draft,
      '--text',
      '回答',
      '--timeout',
      '1',
    ]);
    assert.ok(
      requests.includes(`POST /api/drafts/${draft}/chat`),
      answer.output,
    );
  } finally {
    server.close();
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
