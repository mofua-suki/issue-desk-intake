import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import {
  binaries,
  preparePostgres,
} from '../scripts/test-postgres-binaries.ts';
import { withTestPostgres } from '../scripts/with-test-postgres.ts';
import { writePostgresFixture } from './helpers/test-postgres.ts';

test('隔離PostgreSQLのdumpを別DBへ復元して行を読み出す', async () => {
  const isolatedEnv = { ...process.env };
  delete isolatedEnv.ISSUE_DESK_TEST_POSTGRES_DSN;
  delete isolatedEnv.ISSUE_DESK_TEST_POSTGRES_BIN;
  await withTestPostgres(
    (env) => {
      const bin = env.ISSUE_DESK_TEST_POSTGRES_BIN;
      assert.ok(bin, 'クライアントのディレクトリが必要');
      const run = (name, args) => {
        const result = spawnSync(path.join(bin, name), args, {
          encoding: 'utf8',
          env,
        });
        assert.equal(
          result.status,
          0,
          `${name}: ${result.error || result.stderr}`,
        );
        return result.stdout;
      };
      const target = new URL(env.ISSUE_DESK_TEST_POSTGRES_DSN);
      const db = `issue_desk_test_restore_${process.pid}`;
      const table = `issue_desk_dump_test_${process.pid}`;
      run('psql', [
        target.href,
        '-v',
        'ON_ERROR_STOP=1',
        '-c',
        `CREATE DATABASE ${db}`,
      ]);
      try {
        run('psql', [
          target.href,
          '-v',
          'ON_ERROR_STOP=1',
          '-c',
          `CREATE TABLE ${table} (value text); INSERT INTO ${table} VALUES ('preserved')`,
        ]);
        const archive = path.join(
          fs.mkdtempSync(path.join(os.tmpdir(), 'issue-dump-')),
          'backup.dump',
        );
        try {
          run('pg_dump', ['-Fc', '-t', table, '-f', archive, target.href]);
          target.pathname = `/${db}`;
          run('pg_restore', ['--exit-on-error', '-d', target.href, archive]);
          assert.equal(
            run('psql', [
              target.href,
              '-At',
              '-c',
              `SELECT value FROM ${table}`,
            ]).trim(),
            'preserved',
          );
        } finally {
          fs.rmSync(path.dirname(archive), { recursive: true, force: true });
        }
      } finally {
        run('psql', [
          env.ISSUE_DESK_TEST_POSTGRES_DSN,
          '-v',
          'ON_ERROR_STOP=1',
          '-c',
          `DROP DATABASE ${db}`,
        ]);
        run('psql', [
          env.ISSUE_DESK_TEST_POSTGRES_DSN,
          '-v',
          'ON_ERROR_STOP=1',
          '-c',
          `DROP TABLE IF EXISTS ${table}`,
        ]);
      }
    },
    { env: isolatedEnv, output: () => {} },
  );
});

test('クライアントのSHA-512不一致では展開せず失敗する', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'issue-client-integrity-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const platform = `${process.platform}-${process.arch}`;
  const config = {
    ...binaries,
    clients: {
      ...binaries.clients,
      [platform]: {
        ...binaries.clients[platform],
        integrity: 'sha512-invalid',
      },
    },
  };
  await assert.rejects(
    preparePostgres(dir, {}, new AbortController().signal, config),
    /整合性.*一致しません/,
  );
  assert.deepEqual(fs.readdirSync(dir), []);
});

test('明示されたDSNとBINを保持し自動DBでも指定BINを渡す', async (t) => {
  const explicit = {
    ISSUE_DESK_TEST_POSTGRES_DSN:
      'postgresql://localhost/issue_desk_test_custom',
    ISSUE_DESK_TEST_POSTGRES_BIN: 'custom-bin',
  };
  await withTestPostgres((env) => assert.deepEqual(env, explicit), {
    env: explicit,
  });
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'issue-bin-env-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const bin = writePostgresFixture(dir);
  await withTestPostgres(
    (env) => assert.equal(env.ISSUE_DESK_TEST_POSTGRES_BIN, bin),
    {
      env: {
        ...process.env,
        ISSUE_DESK_TEST_POSTGRES_DSN: '',
        ISSUE_DESK_TEST_POSTGRES_BIN: bin,
        VERIFY_PG_CALLS: path.join(dir, 'calls'),
      },
      output: () => {},
    },
  );
});
