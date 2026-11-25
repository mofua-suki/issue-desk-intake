import { strict as assert } from 'node:assert';
import { spawnSync } from 'node:child_process';
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';

const operation = 'ai-auth-status';
const policy = JSON.parse(
  readFileSync(new URL('../host-ssh.json', import.meta.url), 'utf8'),
);

// 接続先の docker を偽物に置き換え、Workerの容器の中で動く部分だけを手元の node で流す。
function fixture(
  claudeScript: string,
  codexScript = "printf 'Logged in using ChatGPT\\n' >&2",
  dockerPrefix = '',
) {
  const dir = mkdtempSync(join(tmpdir(), 'ai-auth-status-'));
  const bin = join(dir, 'bin');
  const claudeHome = join(dir, 'claude');
  const codexHome = join(dir, 'codex');
  for (const path of [bin, claudeHome, codexHome]) mkdirSync(path);
  const docker = [
    '#!/bin/sh',
    dockerPrefix,
    '[ "$1" = compose ] && [ "$2" = -f ] && [ "$3" = "$BOOTSTRAP_DIR/compose.yaml" ] && [ "$4" = exec ] && [ "$5" = -T ] && [ "$6" = issue-desk-client ] || exit 42',
    'shift 6',
    `export CLAUDE_CONFIG_DIR='${claudeHome}' CODEX_HOME='${codexHome}'`,
    'exec "$@"',
  ].join('\n');
  writeFileSync(join(bin, 'docker'), docker + '\n', { mode: 0o700 });
  writeFileSync(join(bin, 'claude'), '#!/bin/sh\n' + claudeScript + '\n', {
    mode: 0o700,
  });
  writeFileSync(join(bin, 'codex'), '#!/bin/sh\n' + codexScript + '\n', {
    mode: 0o700,
  });
  writeFileSync(
    join(claudeHome, '.credentials.json'),
    JSON.stringify({
      claudeAiOauth: {
        accessToken: 'claude-access-value',
        refreshToken: 'claude-refresh-value',
        expiresAt: Date.UTC(2000, 0, 2),
        refreshTokenExpiresAt: Date.UTC(2000, 0, 9),
      },
    }),
  );
  writeFileSync(
    join(codexHome, 'auth.json'),
    JSON.stringify({
      OPENAI_API_KEY: 'codex-key-value',
      tokens: { access_token: 'codex-access-value' },
      last_refresh: '2000-01-01T00:00:00Z',
    }),
  );
  const result = spawnSync('/bin/sh', ['-c', policy.operations[operation]], {
    encoding: 'utf8',
    env: {
      ...process.env,
      PATH: `${bin}:${join(process.execPath, '..')}:/usr/bin:/bin`,
      BOOTSTRAP_DIR: dir,
    },
  });
  rmSync(dir, { recursive: true, force: true });
  return result;
}

test('認証状態の固定操作は5台に限った個別許可の操作で、自動承認の対象にしない', () => {
  assert.deepEqual(policy.operationHosts[operation], [
    'gt12-pro',
    'a9-max',
    'x1-pro1',
    'x1-pro2',
    'nab-9s',
  ]);
  assert.ok(!policy.readOnlyOperations.includes(operation));
});

test('Claude・Codex のログイン状態と期限だけを返し、資格情報の値を出さない', () => {
  const result = fixture(
    'printf \'{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max","orgId":"org-value"}\\n\'',
  );
  assert.equal(result.status, 0, result.stderr);
  const lines = result.stdout.trim().split('\n');
  assert.match(lines[0], /^host: /);
  assert.deepEqual(lines.slice(1), [
    'claude: loggedIn=true method=claude.ai plan=max',
    'claude: accessExpiresAt=2000-01-02T00:00:00.000Z refreshExpiresAt=2000-01-09T00:00:00.000Z',
    'codex: ChatGPTでログイン',
    'codex: lastRefresh=2000-01-01T00:00:00Z',
    'verify=0',
  ]);
  for (const secret of [
    'claude-access-value',
    'claude-refresh-value',
    'codex-key-value',
    'codex-access-value',
    'org-value',
  ]) {
    assert.ok(!result.stdout.includes(secret), secret);
  }
});

test('CLI の文面は決まった語に置き換え、メール・組織ID・APIキーを出さない', () => {
  const result = fixture(
    "printf 'Login method: user@example.com org-secret-id\\n'",
    "printf 'Logged in using an API key - sk-proj-***abcde\\n'",
  );
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /^claude: 状態をJSONで読めません$/m);
  assert.match(result.stdout, /^codex: APIキーでログイン$/m);
  for (const secret of [
    'user@example.com',
    'org-secret-id',
    'sk-proj',
    'abcde',
  ]) {
    assert.ok(!result.stdout.includes(secret), secret);
  }
});

test('CLI が失敗したら終了コードだけを返し、未ログインは未ログインと出す', () => {
  const result = fixture(
    "printf 'token leaked-value\\n' >&2\nexit 1",
    "printf 'Not logged in\\n' >&2\nexit 1",
  );
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /^claude: 失敗\(1\)$/m);
  assert.match(result.stdout, /^codex: 未ログイン$/m);
  assert.ok(!result.stdout.includes('leaked-value'));
});

test('未ログインで claude が非0で終わっても、JSON が読めれば loggedIn=false を出す', () => {
  const result = fixture(
    '[ "$3" = --json ] || exit 9\nprintf \'{"loggedIn":false}\\n\'\nprintf \'update available\\n\' >&2\nexit 1',
  );
  assert.equal(result.status, 0, result.stderr);
  assert.match(
    result.stdout,
    /^claude: loggedIn=false method=不明 plan=不明$/m,
  );
});

test('容器に入れないときは docker の文面と終了コードを返し、秘匿の行だけを除く', () => {
  const result = fixture(
    'exit 0',
    'exit 0',
    'printf \'service "issue-desk-client" is not running\\nopen /home/u/issue-desk-client/compose.yaml: no such file\\ntoken docker-secret\\nkey sk-docker-secret\\nidk_docker-secret\\n\' >&2; exit 7',
  );
  assert.equal(result.status, 7, result.stderr);
  assert.match(result.stdout, /^service "issue-desk-client" is not running$/m);
  assert.match(
    result.stdout,
    /^open \/home\/u\/issue-desk-client\/compose\.yaml: no such file$/m,
  );
  assert.match(result.stdout, /\nverify=7\n$/);
  assert.ok(!result.stdout.includes('docker-secret'));
});

test('時間切れでも最後の行は verify=124 で、入口が接続成功と見分けられる', () => {
  const result = fixture('exit 0', 'exit 0', 'exit 124');
  assert.equal(result.status, 124, result.stderr);
  assert.match(result.stdout, /\nverify=124\n$/);
});

test('長い docker の出力は行の単位で8KiBまでに切り、最後の行は verify= のまま', () => {
  const result = fixture(
    'exit 0',
    'exit 0',
    "i=0; while [ $i -lt 400 ]; do printf 'ordinary docker line %s\\n' $i; i=$((i+1)); done; exit 3",
  );
  assert.equal(result.status, 3, result.stderr);
  const lines = result.stdout.trimEnd().split('\n');
  assert.equal(lines.at(-1), 'verify=3');
  assert.match(lines.at(-2) ?? '', /^ordinary docker line [0-9]+$/);
  assert.ok(Buffer.byteLength(result.stdout) < 8192 + 200);
});

test('長期トークンでのログインも秘匿の語に当たらない決まった語で出す', () => {
  const result = fixture(
    'printf \'{"loggedIn":true,"authMethod":"oauth_token","subscriptionType":"max"}\\n\'',
  );
  assert.equal(result.status, 0, result.stderr);
  assert.match(
    result.stdout,
    /^claude: loggedIn=true method=長期OAuth plan=max$/m,
  );
});
