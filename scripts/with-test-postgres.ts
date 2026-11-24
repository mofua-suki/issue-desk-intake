import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import net from 'node:net';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { binaries, preparePostgres } from './test-postgres-binaries.ts';

export const POSTGRES_SETUP_STAGES = Object.freeze([
  'binary',
  'initdb',
  'create-db',
  'start',
]);

export function createSetupRecorder(start = performance.now()) {
  let stageStart = start;
  const timings = [];
  return {
    timings,
    record(name) {
      const expected = POSTGRES_SETUP_STAGES[timings.length];
      if (expected === undefined || name !== expected) {
        throw new Error(
          `PostgreSQL準備段階が不正です: ${name} (期待: ${expected ?? '記録終了'})`,
        );
      }
      const now = performance.now();
      timings.push(`${name}=${(now - stageStart).toFixed(1)}ms`);
      stageStart = now;
    },
  };
}

export function isTestPostgresURL(value) {
  try {
    const url = new URL(value);
    return (
      ['postgres:', 'postgresql:'].includes(url.protocol) &&
      decodeURIComponent(url.pathname.slice(1)).startsWith('issue_desk_test')
    );
  } catch {
    return false;
  }
}

function availablePort() {
  return new Promise<number>((resolve, reject) => {
    const server = net.createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address() as net.AddressInfo;
      server.close((error) => (error ? reject(error) : resolve(port)));
    });
  });
}

export async function withTestPostgres(
  run,
  { env = process.env, prepare = preparePostgres, output = console.log } = {},
) {
  if (
    env.ISSUE_DESK_TEST_POSTGRES_DSN &&
    !isTestPostgresURL(env.ISSUE_DESK_TEST_POSTGRES_DSN)
  ) {
    throw new Error(
      'issue_desk_testで始まる専用DBのPostgreSQL URLが必要です。',
    );
  }
  const controller = new AbortController();
  const abort = () => controller.abort();
  process.on('SIGINT', abort);
  process.on('SIGTERM', abort);
  let directory,
    data,
    bin,
    stopped = true;
  const command = (name: string, args: string[], input?: string) => {
    controller.signal.throwIfAborted();
    const result = spawnSync(path.join(bin, name), args, {
      env,
      input,
      encoding: 'utf8',
      timeout: 60_000,
    });
    if (result.error || result.status !== 0) {
      // Only setup diagnostics from this new cluster, never the caller's DSN.
      const log = path.join(directory, 'postgres.log');
      const diagnostic =
        name === 'pg_ctl' && fs.existsSync(log)
          ? fs.readFileSync(log, 'utf8')
          : result.stderr || '';
      throw new Error(
        `検証用PostgreSQLの${name}が失敗しました。\n${diagnostic.slice(-4000)}`,
      );
    }
    return result;
  };
  try {
    if (env.ISSUE_DESK_TEST_POSTGRES_DSN)
      return await run(env, controller.signal);
    if (process.getuid?.() === 0)
      throw new Error('検証用PostgreSQLは一般ユーザーで実行してください。');
    directory = fs.mkdtempSync(path.join(os.tmpdir(), 'issue-desk-test-pg-'));
    fs.chmodSync(directory, 0o700);
    data = path.join(directory, 'data');
    output('固定検証用の隔離PostgreSQLを準備します。');
    const setupStart = performance.now();
    const { timings, record } = createSetupRecorder(setupStart);
    bin = await prepare(directory, env, controller.signal);
    record('binary');
    const version = command('postgres', ['--version'])
      .stdout.trim()
      .split(/\s+/)
      .at(-1);
    if (version !== binaries.postgresVersion)
      throw new Error(
        `検証用PostgreSQLは${binaries.postgresVersion}が必要です。`,
      );
    command('initdb', [
      '-D',
      data,
      '--username=issue_desk_test',
      '--auth=trust',
      '--no-locale',
      '--encoding=UTF8',
      '--no-sync',
    ]);
    record('initdb');
    // Use the real PostgreSQL engine to create the dedicated DB before daemon
    // startup. No psql/createdb dependency, credentials or existing DB is needed.
    command(
      'postgres',
      ['--single', '-D', data, '-c', 'fsync=off', 'template1'],
      'CREATE DATABASE issue_desk_test_verify;\n',
    );
    record('create-db');
    // Sandboxed controllers may prohibit Unix sockets. Bind only loopback on
    // an OS-selected free port, never a public interface or a fixed host port.
    const port = await availablePort();
    fs.writeFileSync(path.join(directory, '.managed-postgres'), '', {
      flag: 'wx',
    });
    stopped = false;
    command('pg_ctl', [
      '-D',
      data,
      '-l',
      path.join(directory, 'postgres.log'),
      '-o',
      `-c listen_addresses=127.0.0.1 -c unix_socket_directories='' -c shared_buffers=16MB -c fsync=off -c synchronous_commit=off -c full_page_writes=off -p ${port}`,
      '-w',
      '-t',
      '30',
      'start',
    ]);
    record('start');
    output(
      `検証用PostgreSQL準備時間: ${timings.join(' ')} total=${(performance.now() - setupStart).toFixed(1)}ms`,
    );
    const dsn = new URL(
      `postgresql://issue_desk_test@127.0.0.1:${port}/issue_desk_test_verify`,
    );
    dsn.searchParams.set('sslmode', 'disable');
    output(
      '隔離PostgreSQLを起動しました。指定された検証コマンドを実行します。',
    );
    return await run(
      {
        ...env,
        ISSUE_DESK_TEST_POSTGRES_DSN: dsn.href,
        ISSUE_DESK_TEST_POSTGRES_BIN: env.ISSUE_DESK_TEST_POSTGRES_BIN || bin,
      },
      controller.signal,
      directory,
    );
  } finally {
    if (!stopped) {
      // A failed start may still have created a server. Never remove live data.
      stopped = !fs.existsSync(path.join(data, 'postmaster.pid'));
      for (const mode of ['fast', 'immediate']) {
        if (stopped) break;
        const result = spawnSync(
          path.join(bin, 'pg_ctl'),
          ['-D', data, '-m', mode, '-w', '-t', '30', 'stop'],
          { env, encoding: 'utf8', timeout: 40_000 },
        );
        stopped = !result.error && result.status === 0;
      }
    }
    process.off('SIGINT', abort);
    process.off('SIGTERM', abort);
    if (stopped && directory)
      fs.rmSync(directory, { recursive: true, force: true });
    if (!stopped)
      // eslint-disable-next-line no-unsafe-finally -- 停止失敗を優先して知らせ、稼働中の検証データを保持する。
      throw new Error(
        `検証用PostgreSQLを停止できませんでした。データは保持しています: ${directory}`,
      );
  }
}

export function runCommand(command, args, env, signal) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      resolve(1);
      return;
    }
    const child = spawn(command, args, {
      env,
      stdio: 'inherit',
      detached: process.platform !== 'win32',
    });
    let escalation;
    const stop = () => {
      try {
        process.kill(
          process.platform === 'win32' ? child.pid : -child.pid,
          'SIGTERM',
        );
      } catch {}
      escalation = setTimeout(() => {
        try {
          process.kill(
            process.platform === 'win32' ? child.pid : -child.pid,
            'SIGKILL',
          );
        } catch {}
      }, 5_000);
      escalation.unref();
    };
    signal?.addEventListener('abort', stop, { once: true });
    child.once('error', reject);
    child.once('close', (code) => {
      clearTimeout(escalation);
      signal?.removeEventListener('abort', stop);
      resolve(code ?? 1);
    });
  });
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  const [separator, command, ...args] = process.argv.slice(2);
  try {
    if (separator !== '--' || !command)
      throw new Error(
        '使い方: node scripts/with-test-postgres.ts -- <コマンド> [引数...]',
      );
    process.exitCode = await withTestPostgres((env, signal) =>
      runCommand(command, args, env, signal),
    );
  } catch (error) {
    console.error(
      error instanceof Error && error.name !== 'AbortError'
        ? error.message
        : '固定検証を中止しました。',
    );
    process.exitCode = 1;
  }
}
