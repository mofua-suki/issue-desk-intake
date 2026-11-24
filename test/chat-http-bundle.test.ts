import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import { spawn } from 'node:child_process';
import {
  deskContractHeader,
  deskContractVersion,
} from '../scripts/lib/desk-contract.generated.ts';

function cli(args, env) {
  return new Promise<any>((resolve, reject) => {
    const child = spawn(process.execPath, args, {
      env,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let stdout = '',
      stderr = '';
    child.stdout.on('data', (b) => (stdout += b));
    child.stderr.on('data', (b) => (stderr += b));
    child.on('error', reject);
    child.on('close', (code) => resolve({ code, stdout, stderr }));
  });
}
test('HTTP専用チャットCLIはPCtokenで読取し、NAS用moduleやDBなしで動く', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'chat-http-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const token = 'idk_' + 'a'.repeat(43);
  const file = path.join(dir, 'token');
  fs.writeFileSync(file, token, { mode: 0o600 });
  const requests = [];
  const server = http.createServer((r, w) => {
    requests.push(r.url);
    assert.equal(r.headers.authorization, 'Bearer ' + token);
    assert.equal(r.headers[deskContractHeader], deskContractVersion);
    assert.equal(r.headers['x-desk-key'], undefined);
    w.setHeader(deskContractHeader, deskContractVersion);
    w.setHeader('content-type', 'application/json');
    if (r.url.startsWith('/api/issues/search')) {
      w.end('{"total":0,"issues":[]}');
      return;
    }
    if (r.url.startsWith('/api/issue?')) {
      w.end(
        JSON.stringify({
          project: 'owner/app',
          number: 1,
          desk: 99,
          title: 'fixture',
          status: 'closed',
          state: 'closed',
          body: '本文',
          comments: [],
        }),
      );
      return;
    }
    w.statusCode = 404;
    w.end('{}');
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const base =
    'http://127.0.0.1:' +
    (server.address() as import('node:net').AddressInfo).port;
  const env = { ...process.env };
  delete env.NODE_TEST_CONTEXT;
  delete env.ISSUE_DESK_URL;
  delete env.ISSUE_DESK_TOKEN_FILE;
  const list = await cli(
    [
      'scripts/desk-get.ts',
      'GET',
      '/api/issues/search?project=owner%2Fapp&page=1&limit=100',
      '--url',
      base,
      '--token-file',
      file,
    ],
    env,
  );
  assert.equal(list.code, 0, list.stderr);
  assert.deepEqual(JSON.parse(list.stdout), { total: 0, issues: [] });
  const read = await cli(
    [
      'scripts/desk-get.ts',
      'GET',
      '/api/issue?project=owner%2Fapp&number=1',
      '--url',
      base,
      '--token-file',
      file,
    ],
    env,
  );
  assert.equal(read.code, 0, read.stderr);
  assert.equal(JSON.parse(read.stdout).desk, 99);
  const summary = await cli(
    [
      'scripts/desk-get.ts',
      'GET',
      '/api/issue?desk=99&view=summary',
      '--url',
      base,
      '--token-file',
      file,
    ],
    env,
  );
  assert.equal(summary.code, 0, summary.stderr);
  assert.match(requests.at(-1), /view=summary/);
  const invalid = await cli(
    [
      'scripts/desk-get.ts',
      'POST',
      '/api/issue?desk=99',
      '--url',
      base,
      '--token-file',
      file,
    ],
    env,
  );
  assert.equal(invalid.code, 1);
  assert.equal(requests.length, 3);
  assert.match(list.stdout, /\n {2}/);
  assert.doesNotMatch(
    list.stdout + list.stderr + read.stdout + read.stderr,
    new RegExp(token),
  );
});
