import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import http from 'node:http';
import { spawn } from 'node:child_process';
import {
  deskContractHeader,
  deskContractVersion,
} from '../scripts/lib/desk-contract.generated.ts';

const markdown = fs.readFileSync(
  new URL(
    '../skills/issue-desk-issue-control/refs/close-issue.md',
    import.meta.url,
  ),
  'utf8',
);
// 掲載した固定呼出しのJSを実行し、別実装で置き換えない。コンテナ内のimport先だけローカルへ解決する。
const example = markdown
  .match(/--input-type=module -e '([\s\S]*?)' < "\$CLOSE_BODY_FILE"/)[1]
  .replaceAll(
    '"/app/shared/src/io-redact.ts"',
    JSON.stringify(path.resolve('shared/src/io-redact.ts')),
  );
// コンテナでは稼働イメージの読み込み役 chat-local.ts だけで node を動かす（改名前の chat-local.mjs へは倒さない）。
const invocation = markdown.match(
  /intake\.sh" exec -T (.*) --input-type=module -e '/,
)[1];

test('Skillの一時キー終了例は、イメージの chat-local.ts と io-redact.ts だけを読み、改名前の .mjs の名前を使わない', () => {
  assert.equal(invocation, 'node --import /app/deploy/nas/chat-local.ts');
  assert.match(example, /import \{ redactAll \} from "[^"]*io-redact\.ts";/);
  assert.doesNotMatch(markdown, /chat-local\.mjs|io-redact\.mjs/);
});

for (const status of [200, 403])
  test(
    `Skillの一時キー終了例はHTTP ${status}でも固定送信とキー回収を行い秘密を表示しない`,
    { timeout: 15000 },
    async (t) => {
      fs.mkdirSync('.tmp', { recursive: true });
      const directory = fs.mkdtempSync(path.resolve('.tmp/close-skill-'));
      t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
      const key = 'k'.repeat(43);
      const log = path.join(directory, 'admin.json');
      const binary = path.join(directory, 'intake');
      fs.writeFileSync(
        binary,
        `#!${process.execPath}\nconst fs=require('node:fs');\nconst entries=fs.existsSync(process.env.ADMIN_LOG)?JSON.parse(fs.readFileSync(process.env.ADMIN_LOG, 'utf8')):[];\nconst action=process.argv.at(-1);\nentries.push({action,input:fs.readFileSync(0,'utf8')});\nfs.writeFileSync(process.env.ADMIN_LOG,JSON.stringify(entries));\nif(action==='issue')console.log('${key}');\n`,
        { mode: 0o700 },
      );
      const requests = [];
      const server = http.createServer(async (request, response) => {
        let raw = '';
        for await (const chunk of request) raw += chunk;
        requests.push({
          method: request.method,
          path: request.url,
          headers: request.headers,
          body: JSON.parse(raw),
        });
        response.writeHead(status, {
          'Content-Type': 'application/json',
          [deskContractHeader]: deskContractVersion,
        });
        response.end(
          JSON.stringify(
            status === 200
              ? {
                  project: 'owner/repo',
                  number: 12,
                  state: 'closed',
                  internal: '非表示',
                }
              : { error: 'この操作は許可されていません' },
          ),
        );
      });
      await new Promise<void>((resolve) =>
        server.listen(0, '127.0.0.1', resolve),
      );
      t.after(() => new Promise((resolve) => server.close(resolve)));
      const port = (server.address() as import('node:net').AddressInfo).port;
      const config = path.join(directory, 'config.json');
      fs.writeFileSync(
        config,
        JSON.stringify({ workerId: 'nas', intake: { port } }),
      );
      const body = {
        project: 'owner/repo',
        number: 12,
        reason: 'voluntary',
        comment: '利用者の明示指示',
        force: true,
      };
      const env = { ...process.env };
      delete env.GODEBUG;
      const child = spawn(
        process.execPath,
        [
          '--import',
          path.resolve('deploy/nas/chat-local.ts'),
          '--input-type=module',
          '-e',
          example,
        ],
        {
          env: {
            ...env,
            ISSUE_DESK_CONFIG: config,
            ISSUE_DESK_APPLICATION_DIR: directory,
            ISSUE_DESK_INTAKE_BINARY: binary,
            ADMIN_LOG: log,
          },
          stdio: ['pipe', 'pipe', 'pipe'],
        },
      );
      let stdout = '',
        stderr = '';
      child.stdout.on('data', (chunk) => (stdout += chunk));
      child.stderr.on('data', (chunk) => (stderr += chunk));
      const exited = new Promise((resolve, reject) => {
        child.once('error', reject);
        child.once('close', resolve);
      });
      child.stdin.end(JSON.stringify(body));
      const code = await exited;
      assert.equal(code, status === 200 ? 0 : 1, stderr);
      assert.equal(requests.length, 1);
      assert.deepEqual(
        {
          method: requests[0].method,
          path: requests[0].path,
          body: requests[0].body,
        },
        { method: 'POST', path: '/api/issues/close', body },
      );
      assert.equal(requests[0].headers['x-desk-key'], key);
      assert.equal(requests[0].headers.origin, `http://127.0.0.1:${port}`);
      assert.equal(
        requests[0].headers[deskContractHeader.toLowerCase()],
        deskContractVersion,
      );
      assert.deepEqual(JSON.parse(fs.readFileSync(log, 'utf8')), [
        { action: 'issue', input: '' },
        { action: 'revoke', input: key },
      ]);
      assert.ok(!(stdout + stderr).includes(key));
      assert.ok(!stdout.includes('非表示'));
      if (status === 200)
        assert.deepEqual(JSON.parse(stdout), {
          project: 'owner/repo',
          number: 12,
          state: 'closed',
        });
    },
  );
