import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { spawn, spawnSync } from 'node:child_process';
import { once } from 'node:events';
import { fileURLToPath } from 'node:url';
import {
  checkArchive,
  preparePostgres,
} from '../scripts/test-postgres-binaries.ts';
import {
  createSetupRecorder,
  isTestPostgresURL,
  POSTGRES_SETUP_STAGES,
  withTestPostgres,
} from '../scripts/with-test-postgres.ts';
import {
  readPostgresCalls,
  writePostgresFixture,
} from './helpers/test-postgres.ts';

function fixture(t) {
  const directory = fs.mkdtempSync(
    path.join(os.tmpdir(), 'test-postgres-lifecycle-'),
  );
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    ISSUE_DESK_TEST_POSTGRES_BIN: writePostgresFixture(directory),
    VERIFY_PG_CALLS: path.join(directory, 'calls'),
  };
  delete env.ISSUE_DESK_TEST_POSTGRES_DSN;
  let scratch;
  return {
    env,
    prepare: async (dir, variables, signal) => {
      scratch = dir;
      return preparePostgres(dir, variables, signal);
    },
    scratch: () => scratch,
  };
}

test('検証PostgreSQLの配布物は固定ハッシュと一致しないと使わない', () => {
  const bytes = Buffer.from('fixture archive');
  const hash = `sha512-${createHash('sha512').update(bytes).digest('base64')}`;
  assert.doesNotThrow(() => checkArchive(bytes, hash));
  assert.throws(
    () => checkArchive(Buffer.from('changed archive'), hash),
    /整合性/,
  );
});

test('配布クライアントは検証サーバー以上の版でdumpと復元の版が揃う', async (t) => {
  const directory = fs.mkdtempSync(
    path.join(os.tmpdir(), 'test-postgres-versions-'),
  );
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const bin = await preparePostgres(
    directory,
    {},
    new AbortController().signal,
  );
  const major = (name) => {
    const result = spawnSync(path.join(bin, name), ['--version'], {
      encoding: 'utf8',
    });
    assert.equal(result.status, 0, `${name}: ${result.error || result.stderr}`);
    const match = /\(PostgreSQL\) (\d+)\./.exec(result.stdout);
    assert.ok(match, `${name}: ${result.stdout}`);
    return Number(match[1]);
  };
  const server = major('postgres');
  const dump = major('pg_dump');
  assert.ok(dump >= server, 'pg_dumpはサーバー以上のメジャー版が必要');
  assert.equal(major('pg_restore'), dump);
  assert.ok(major('psql') >= server);
});

test('指定された専用DSNは再利用し不正な指定を自動DBへすり替えない', async () => {
  const dsn =
    'postgresql://fixture:private-value@localhost/issue_desk_test_explicit';
  const env = { ISSUE_DESK_TEST_POSTGRES_DSN: dsn };
  const prepare = () => assert.fail('指定済みのDBを置き換えてはいけない');
  assert.equal(
    await withTestPostgres(
      (actual) => {
        assert.equal(actual, env);
        return 7;
      },
      { env, prepare },
    ),
    7,
  );
  assert.equal(isTestPostgresURL(dsn), true);
  for (const value of [
    'invalid',
    'postgresql://fixture@localhost/production',
  ]) {
    await assert.rejects(
      withTestPostgres(() => assert.fail('実行してはいけない'), {
        env: { ISSUE_DESK_TEST_POSTGRES_DSN: value },
        prepare,
      }),
      /専用DB/,
    );
  }
});

test('隔離DBの初期化と単一ユーザーのDB作成でも同期書き込みを省く', async (t) => {
  // 一時ディレクトリの名前に start・stop を含めても、呼び出しの照合が経路に引きずられない。
  const parent = fs.mkdtempSync(
    path.join(os.tmpdir(), 'test-postgres-start-stop-'),
  );
  const tmpdir = process.env.TMPDIR;
  t.after(() => {
    if (tmpdir === undefined) delete process.env.TMPDIR;
    else process.env.TMPDIR = tmpdir;
    fs.rmSync(parent, { recursive: true, force: true });
  });
  process.env.TMPDIR = parent;
  const f = fixture(t);
  const bin = f.env.ISSUE_DESK_TEST_POSTGRES_BIN;
  for (const name of ['initdb', 'postgres']) {
    const original = path.join(bin, `${name}.original`);
    fs.renameSync(path.join(bin, name), original);
    fs.writeFileSync(
      path.join(bin, name),
      `#!/bin/sh\nprintf '%s\\n' '${name}' "$@" 'END' >> "$VERIFY_PG_CALLS"\nexec "$(dirname "$0")/${name}.original" "$@"\n`,
      { mode: 0o700 },
    );
  }
  await withTestPostgres(() => 0, { ...f, output: () => {} });
  const calls = fs.readFileSync(f.env.VERIFY_PG_CALLS, 'utf8');
  assert.match(calls, /initdb\n[\s\S]*?\n--no-sync\nEND/);
  assert.match(
    calls,
    /postgres\n--single\n-D\n[^\n]+\n-c\nfsync=off\ntemplate1\nEND/,
  );
  const lines = readPostgresCalls(f.env.VERIFY_PG_CALLS);
  // 隔離データの経路自体に start が入り、その経路が呼び出しの記録にも書かれていること。
  assert.ok(
    path.basename(parent).includes('start') &&
      f.scratch().startsWith(parent + path.sep),
    '経路に start を含む状態で確かめる',
  );
  assert.ok(
    lines.some((line) => line !== 'start' && line.startsWith(f.scratch())),
    '記録に start を含む経路がある',
  );
  const created = lines.indexOf('template1');
  assert.ok(created >= 0 && lines[created - 1] === 'fsync=off');
  assert.ok(created < lines.indexOf('start'));
});

test('DSN未設定時は隔離DBを準備し成功・失敗の終了値を保って停止と後片付けを行う', async (t) => {
  for (const exitCode of [0, 7]) {
    const f = fixture(t);
    const before = process.listenerCount('SIGTERM');
    const code = await withTestPostgres(
      (env) => {
        const dsn = new URL(env.ISSUE_DESK_TEST_POSTGRES_DSN);
        assert.equal(dsn.pathname, '/issue_desk_test_verify');
        assert.equal(dsn.password, '');
        assert.equal(dsn.hostname, '127.0.0.1');
        assert.ok(Number(dsn.port) > 0);
        assert.equal(fs.statSync(f.scratch()).mode & 0o777, 0o700);
        assert.equal(f.env.ISSUE_DESK_TEST_POSTGRES_DSN, undefined);
        return exitCode;
      },
      { ...f, output: () => {} },
    );
    assert.equal(code, exitCode);
    const calls = fs.readFileSync(f.env.VERIFY_PG_CALLS, 'utf8');
    const lines = readPostgresCalls(f.env.VERIFY_PG_CALLS);
    assert.ok(lines.includes('start'));
    assert.match(calls, /listen_addresses=127.0.0.1/);
    assert.match(calls, /unix_socket_directories=''/);
    assert.match(calls, /-c fsync=off(?:\s|$)/);
    assert.match(calls, /-c synchronous_commit=off(?:\s|$)/);
    assert.match(calls, /-c full_page_writes=off(?:\s|$)/);
    assert.ok(lines.includes('stop'));
    assert.equal(fs.existsSync(f.scratch()), false);
    assert.equal(process.listenerCount('SIGTERM'), before);
  }
});

test('DB準備途中と呼出先の例外でも隔離データを片付け部分起動を停止する', async (t) => {
  for (const stage of ['download', 'start', 'command']) {
    const f = fixture(t);
    if (stage === 'start') f.env.VERIFY_PG_START_EXIT = '1';
    let scratch;
    await assert.rejects(
      withTestPostgres(
        () => {
          throw new Error('呼出先が失敗');
        },
        {
          env: f.env,
          output: () => {},
          prepare: async (dir, env, signal) => {
            scratch = dir;
            if (stage === 'download') throw new Error('取得失敗');
            return f.prepare(dir, env, signal);
          },
        },
      ),
    );
    assert.equal(fs.existsSync(scratch), false);
    if (stage !== 'download')
      assert.ok(readPostgresCalls(f.env.VERIFY_PG_CALLS).includes('stop'));
  }
});

test(
  '検証中のSIGTERMで子処理を終了させ隔離PostgreSQLも停止する',
  { timeout: 15_000 },
  async (t) => {
    const f = fixture(t);
    const root = fileURLToPath(new URL('../', import.meta.url));
    const probe = path.join(path.dirname(f.env.VERIFY_PG_CALLS), 'wait.mjs');
    const marker = path.join(path.dirname(probe), 'socket-root');
    fs.writeFileSync(
      probe,
      `console.log('検証の子処理が開始'); setInterval(() => {}, 1000);`,
    );
    const child = spawn(
      process.execPath,
      [
        path.join(root, 'scripts/with-test-postgres.ts'),
        '--',
        process.execPath,
        probe,
      ],
      {
        env: { ...f.env, VERIFY_PG_DIRECTORY_MARKER: marker },
        stdio: ['ignore', 'pipe', 'pipe'],
      },
    );
    t.after(() => {
      if (child.exitCode === null) child.kill('SIGKILL');
    });
    const closed = once(child, 'close');
    let output = '';
    await new Promise<void>((resolve, reject) => {
      child.on('error', reject);
      child.stdout.on('data', (data) => {
        output += data;
        if (output.includes('検証の子処理が開始')) resolve();
      });
      child.once('close', () => reject(new Error('子処理の開始前に終了')));
    });
    child.kill('SIGTERM');
    const [code] = await closed;
    assert.equal(code, 1);
    assert.ok(readPostgresCalls(f.env.VERIFY_PG_CALLS).includes('stop'));
    assert.equal(fs.existsSync(fs.readFileSync(marker, 'utf8').trim()), false);
  },
);

test('準備時間を段階別と合計で記録し指定済みDBでは記録しない', async (t) => {
  const f = fixture(t);
  const messages = [];
  await withTestPostgres(() => 0, {
    ...f,
    output: (message) => messages.push(message),
  });
  const timing = messages.filter((message) =>
    message.startsWith('検証用PostgreSQL準備時間:'),
  );
  assert.equal(timing.length, 1);
  const entries = timing[0]
    .slice('検証用PostgreSQL準備時間:'.length)
    .trim()
    .split(/\s+/);
  const parsed = entries.map((entry) => {
    const match = /^([^=]+)=(\d+\.\d)ms$/.exec(entry);
    assert.ok(match, `不正な計測値: ${entry}`);
    return [match[1], Number(match[2])];
  });
  assert.deepEqual(
    parsed.map(([name]) => name),
    [...POSTGRES_SETUP_STAGES, 'total'],
  );
  const values = parsed.map(([, value]) => value);
  assert.ok(
    Math.abs(
      values.slice(0, -1).reduce((sum, value) => sum + value, 0) -
        values.at(-1),
    ) < 1,
  );
  messages.length = 0;
  await withTestPostgres(() => 0, {
    env: {
      ISSUE_DESK_TEST_POSTGRES_DSN:
        'postgresql://localhost/issue_desk_test_existing',
    },
    output: (message) => messages.push(message),
  });
  assert.deepEqual(messages, []);
});

test('準備段階の順序違いと記録終了後の呼び出しを拒否する', () => {
  const { record, timings } = createSetupRecorder();
  assert.throws(() => record('initdb'), /準備段階が不正/);
  assert.deepEqual(timings, []);
  for (const stage of POSTGRES_SETUP_STAGES) record(stage);
  assert.throws(() => record('extra'), /準備段階が不正/);
  assert.equal(timings.length, 4);
  assert.doesNotMatch(timings.join(' '), /undefined/);
});
