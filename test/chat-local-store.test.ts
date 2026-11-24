import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import '../deploy/nas/chat-local.ts';

// 削除済みの GODEBUG（tlskyber=0 など）が残ったシェルでは go と組み立てた受付が起動時に落ちるので、子へ渡さない。
const baseEnv = { ...process.env };
delete baseEnv.GODEBUG;
// 共通プロセスの他のCLI試験にNAS接続を残さない。検証対象は各子プロセスでpreloadする。
delete globalThis.issueDeskLocal;

function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'chat-local-store-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const config = path.join(dir, 'config.json');
  fs.writeFileSync(
    config,
    JSON.stringify({
      workerId: 'nas',
      giteaUrl: 'http://gitea:3000',
      runtimeDir: 'runtime',
      tokenFile: 'private/admin-token',
      codex: 'tools/codex',
      repositories: [{ name: 'owner/app' }],
    }),
  );
  const binary = path.join(dir, 'intake');
  const log = path.join(dir, 'calls.json');
  fs.writeFileSync(
    binary,
    `#!${process.execPath}
const fs = require('node:fs');
const action = process.argv.at(-1);
const calls = fs.existsSync(process.env.ADMIN_LOG) ? JSON.parse(fs.readFileSync(process.env.ADMIN_LOG, 'utf8')) : [];
const input = fs.readFileSync(0, 'utf8');
calls.push({ action, input });
fs.writeFileSync(process.env.ADMIN_LOG, JSON.stringify(calls));
if (process.env.FAIL_ACTION === action) { console.error(process.env.ADMIN_ERROR); process.exit(1); }
if (action === 'issue') console.log(String(calls.length).padStart(43, 'a'));
`,
  );
  fs.chmodSync(binary, 0o700);
  const run = (script, env = {}) =>
    spawnSync(
      process.execPath,
      [
        '--import',
        path.resolve('deploy/nas/chat-local.ts'),
        '--input-type=module',
        '-e',
        script,
      ],
      {
        env: {
          ...baseEnv,
          HOME: dir,
          ISSUE_DESK_CONFIG: config,
          ISSUE_DESK_APPLICATION_DIR: dir,
          ISSUE_DESK_INTAKE_BINARY: binary,
          ADMIN_LOG: log,
          ...env,
        },
        encoding: 'utf8',
        timeout: 15000,
      },
    );
  return { run, log, dir };
}

test('一時キー発行の保存先診断を実際のpreloadから利用者へ届け、任意のstderrは隠す', (t) => {
  const f = fixture(t);
  const messages = [
    '受付のPostgreSQLを開けません',
    '窓口が確認した受付の保存先がPostgreSQLではありません。受付はPostgreSQL専用です。NASの受付と一時キー窓口を更新してください',
    '保存先のPostgreSQLを指定してください。ISSUE_DESK_DATABASE_URL（またはPGHOST・PGUSER・PGDATABASE）が必要です',
    '切替の完了を確認できません',
    'ISSUE_DESK_APP_STOREにsqliteは使えません。保存先はPostgreSQLだけです（postgresまたは未設定にしてください）',
    '受付の保存先の確認値が不正です。NASの一時キー窓口を更新してください',
  ];
  for (const action of ['issue']) {
    for (const message of [
      ...messages,
      'postgres://user:secret-password@host/db',
    ]) {
      // cmd/intakeのslogは空白の有無に応じて引用する。どちらの実出力形式も通す。
      for (const value of [message, JSON.stringify(message)]) {
        const r = f.run(
          `
          globalThis.fetch = async () => new Response('{}');
          try { await globalThis.issueDeskLocal().call('GET', '/api/issues'); process.exit(9); }
          catch (error) { console.error(error.message); process.exit(1); }
        `,
          {
            FAIL_ACTION: action,
            ADMIN_ERROR:
              '2026/11/04 16:00:00 ERROR Serverの操作に失敗しました error=' +
              value,
          },
        );
        assert.equal(r.status, 1, r.stderr);
        assert.ok(!r.stderr.includes('secret-password'));
        if (messages.includes(message)) assert.equal(r.stderr.trim(), message);
        else assert.match(r.stderr, /NASの受付へ接続できません/);
      }
    }
  }
});

test('一時キー回収の診断で受信済み応答を失わずstderrを出さない', (t) => {
  const f = fixture(t);
  for (const message of [
    '受付のPostgreSQLを開けません',
    '窓口が確認した受付の保存先がPostgreSQLではありません。受付はPostgreSQL専用です。NASの受付と一時キー窓口を更新してください',
    '保存先のPostgreSQLを指定してください。ISSUE_DESK_DATABASE_URL（またはPGHOST・PGUSER・PGDATABASE）が必要です',
    '切替の完了を確認できません',
    'ISSUE_DESK_APP_STOREにsqliteは使えません。保存先はPostgreSQLだけです（postgresまたは未設定にしてください）',
    '受付の保存先の確認値が不正です。NASの一時キー窓口を更新してください',
    'postgres://user:secret-password@host/db',
  ])
    for (const value of [message, JSON.stringify(message)]) {
      const r = f.run(
        `
      import { deskContractHeader, deskContractVersion } from './scripts/lib/desk-contract.generated.ts';
      globalThis.fetch = async () => new Response('{"ok":true}', {headers:{[deskContractHeader]:deskContractVersion}});
      const result = await globalThis.issueDeskLocal().call('GET', '/api/issues');
      if (!result.ok) process.exit(9);
    `,
        {
          FAIL_ACTION: 'revoke',
          ADMIN_ERROR:
            '2026/11/04 16:00:00 ERROR Serverの操作に失敗しました error=' +
            value,
        },
      );
      assert.equal(r.status, 0, r.stderr);
      assert.ok(!r.stderr.includes(message));
    }
});

test('出荷用intakeバイナリの保存先診断をpreloadが秘密を出さずに伝える', (t) => {
  const f = fixture(t);
  const binary = path.join(f.dir, 'intake-built');
  const localGo = path.join(os.homedir(), '.local/bin/go');
  const go = process.env.GO || (fs.existsSync(localGo) ? localGo : 'go');
  const build = spawnSync(go, ['build', '-o', binary, './cmd/intake'], {
    cwd: path.resolve('server'),
    encoding: 'utf8',
    timeout: 120000,
    env: baseEnv,
  });
  assert.equal(build.status, 0, build.stderr);
  const cases = [
    {
      expected: 'sqlite',
      store: 'postgres',
      dsn: '',
      message: /受付の保存先がPostgreSQLではありません/,
    },
    {
      expected: 'postgres',
      store: 'sqlite',
      dsn: '',
      message: /ISSUE_DESK_APP_STOREにsqliteは使えません/,
    },
    {
      expected: 'postgres',
      store: 'postgres',
      dsn: '',
      message: /ISSUE_DESK_DATABASE_URL.*必要です/,
    },
    {
      expected: 'postgres',
      store: 'postgres',
      dsn: 'postgres://user:secret-password@127.0.0.1:1/private-db?connect_timeout=1',
      message: /受付のPostgreSQLを開けません/,
    },
  ];
  for (const { expected, store, dsn, message } of cases) {
    const r = f.run(
      `
      try { await globalThis.issueDeskLocal().call('GET', '/api/issues'); process.exit(9); }
      catch (error) { console.error(error.message); process.exit(1); }
    `,
      {
        ISSUE_DESK_INTAKE_BINARY: binary,
        ISSUE_DESK_EXPECTED_APP_STORE: expected,
        ISSUE_DESK_APP_STORE: store,
        ISSUE_DESK_DATABASE_URL: dsn,
      },
    );
    assert.equal(r.status, 1, r.stderr);
    assert.match(r.stderr, message);
    assert.ok(!(r.stdout + r.stderr).includes('secret-password'));
  }
});

test('実際のpreloadはHTTP再試行ごとに別の一時キーを発行して未消費キーを回収する', (t) => {
  const f = fixture(t);
  const r = f.run(`
    import assert from 'node:assert/strict';
    const nativeTimeout = globalThis.setTimeout;
    globalThis.setTimeout = callback => nativeTimeout(callback, 0);
    const keys = [];
    globalThis.fetch = async (_url, init) => {
      keys.push(init.headers['x-desk-key']);
      return new Response(JSON.stringify(keys.length === 1 ? { code: 'TEMPORARY_UNAVAILABLE' } : { ok: true }), { status: keys.length === 1 ? 503 : 200 });
    };
    const result = await globalThis.issueDeskLocal().call('GET', '/api/issues');
    assert.equal(result.ok, true);
    assert.equal(keys.length, 2);
    assert.notEqual(keys[0], keys[1]);
  `);
  assert.equal(r.status, 0, r.stderr);
  const calls = JSON.parse(fs.readFileSync(f.log, 'utf8'));
  assert.deepEqual(
    calls.map((c) => c.action),
    ['issue', 'revoke', 'issue', 'revoke'],
  );
  assert.equal(calls[1].input, '1'.padStart(43, 'a'));
  assert.equal(calls[3].input, '3'.padStart(43, 'a'));
});
