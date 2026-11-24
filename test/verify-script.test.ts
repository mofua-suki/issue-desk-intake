import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const source = fileURLToPath(new URL('../scripts/verify.sh', import.meta.url));
function fixture(t, fail = '') {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-script-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  for (const name of [
    'scripts',
    'server',
    'client/test',
    'test',
    'bin',
    'events',
  ])
    fs.mkdirSync(path.join(dir, name), { recursive: true });
  fs.copyFileSync(source, path.join(dir, 'scripts/verify.sh'));
  fs.writeFileSync(
    path.join(dir, 'scripts/verify-node-test-targets.list'),
    fail === 'invalid-node-targets'
      ? 'client test/client-*.test.ts\t--import utility.js\nroot test/root-*.test.ts\n'
      : 'client test/client-*.test.ts\nroot test/root-*.test.ts\n',
  );
  fs.copyFileSync(
    path.join(path.dirname(source), 'check-go-quality.sh'),
    path.join(dir, 'scripts/check-go-quality.sh'),
  );
  fs.copyFileSync(
    path.join(path.dirname(source), 'with-run-tmpdir.sh'),
    path.join(dir, 'scripts/with-run-tmpdir.sh'),
  );
  fs.writeFileSync(path.join(dir, 'client/test/client-a.test.ts'), '');
  fs.writeFileSync(path.join(dir, 'test/root-a.test.ts'), '');
  fs.writeFileSync(path.join(dir, 'client/test/root-b.test.ts'), '');
  fs.writeFileSync(path.join(dir, 'test/client-b.test.ts'), '');
  const mock = path.join(dir, 'bin/mock.cjs');
  fs.writeFileSync(
    mock,
    `const fs = require('fs'), path = require('path');
const args = process.argv.slice(2), exe = path.basename(process.argv[1]);
if (exe === 'node' && args[0] === '-e') process.exit(0);
if (exe === 'go' && args[0] === 'list') {
 if (process.env.FAIL_STAGE === 'list') process.exit(3);
 if (process.env.FAIL_STAGE !== 'empty') console.log('issue-desk-intake/server/internal/consultations\\nissue-desk-intake/server/internal/other\\nissue-desk-intake/server/internal/consultations/sub');
 else console.log('issue-desk-intake/server/internal/consultations');
 process.exit(0);
}
if (exe === 'node' && args[0].endsWith('verify-review-notes.ts') && args[1] === '--normal-go') { require('child_process').spawnSync(process.env.GO, ['test', '-json', '-count=1', '-timeout', '8m', ...args.slice(2)], {stdio:'inherit'}); process.exit(process.env.FAIL_STAGE === 'go-test' ? 3 : 0); }
if (exe === 'node' && args[0].endsWith('verify-review-notes.ts') && args[1] === '--consultations-race') { const race = require('child_process').spawnSync(process.env.GO, ['test', '-json', '-count=1', '-race', './internal/consultations'], {stdio:'inherit'}); process.exit(race.status ?? 1); }
const key = exe === 'node' ? (args[0] === '--test' ? (process.cwd().endsWith('/client') ? 'client-test' : 'root-test') : args[0].endsWith('check-ts-quality.ts') ? 'ts-' + args[1] : path.basename(args[0])) : exe === 'go' ? (args[0] === 'run' ? (args[1].includes('/deadcode@') ? 'deadcode' : 'staticcheck') : args[0] === 'vet' ? 'vet' : args.includes('-race') ? 'consultations-race' : args.includes('^TestNASCaddyHandoff$') ? 'nas-caddy' : 'go-test') : exe;
const dir = process.env.EVENTS, start = Date.now();
fs.writeFileSync(path.join(dir, key + '.start'), JSON.stringify({args, cwd: process.cwd(), env: {GODEBUG: process.env.GODEBUG, CGO_ENABLED: process.env.CGO_ENABLED, CC: process.env.CC}, start}));
if ((key === 'api-manifest.ts' || key === 'pnpm') && process.env.FAIL_STAGE !== 'invalid-node-targets') {
 const other = key === 'pnpm' ? 'api-manifest.ts' : 'pnpm';
 const limit = Date.now() + 10000;
 while (!fs.existsSync(path.join(dir, other + '.start')) && Date.now() < limit) Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 10);
 if (!fs.existsSync(path.join(dir, other + '.start'))) process.exit(9);
}
if (['go-test', 'consultations-race'].includes(key) && !['list', 'empty'].includes(process.env.FAIL_STAGE)) {
 const other = key === 'go-test' ? 'consultations-race' : 'go-test';
 const limit = Date.now() + 10000;
 while (!fs.existsSync(path.join(dir, other + '.start')) && Date.now() < limit) Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 10);
 if (!fs.existsSync(path.join(dir, other + '.start'))) process.exit(9);
}
if (key === 'deadcode' && process.env.FAIL_STAGE === 'deadcode-report') { console.log('fixture.go:1:1: unreachable func: abandoned'); fs.writeFileSync(path.join(dir, key + '.end'), String(Date.now())); process.exit(0); }
if (key === 'client-test' && process.env.FAIL_STAGE === 'client-hang') setInterval(() => {}, 1000);
if (key === process.env.FAIL_STAGE) { console.log('STAGE-FAIL-MARKER'); fs.writeFileSync(path.join(dir, key + '.end'), String(Date.now())); process.exit(3); }
if (!['gofmt', 'deadcode'].includes(key)) console.log('MARKER-' + key);
fs.writeFileSync(path.join(dir, key + '.end'), String(Date.now()));`,
  );
  for (const name of ['node', 'go', 'gofmt', 'pnpm']) {
    // Give the mock the invoked tool's name without depending on symlink resolution.
    fs.writeFileSync(
      path.join(dir, 'bin', name),
      `#!/bin/sh\nexec "${process.execPath}" "${path.join(dir, 'bin', name + '.cjs')}" "$@"\n`,
      { mode: 0o755 },
    );
    fs.copyFileSync(mock, path.join(dir, 'bin', name + '.cjs'));
    const content = fs
      .readFileSync(path.join(dir, 'bin', name + '.cjs'), 'utf8')
      .replace('path.basename(process.argv[1])', JSON.stringify(name));
    fs.writeFileSync(path.join(dir, 'bin', name + '.cjs'), content);
  }
  const env = {
    ...process.env,
    ISSUE_DESK_TEST_POSTGRES_DSN:
      'postgresql://fixture@localhost/issue_desk_test',
    EVENTS: path.join(dir, 'events'),
    FAIL_STAGE: fail,
    ISSUE_DESK_CLIENT_TEST_TIMEOUT_SECONDS:
      fail === 'client-hang' ? '1' : '1200',
    GODEBUG: 'tlskyber=0',
    CC: 'fixture-cc',
    ...Object.fromEntries(
      ['node', 'go', 'gofmt', 'pnpm'].map((n) => [
        n.toUpperCase(),
        path.join(dir, 'bin', n),
      ]),
    ),
  };
  const run = spawnSync('sh', [path.join(dir, 'scripts/verify.sh')], {
    env,
    encoding: 'utf8',
  });
  const event = (name) =>
    JSON.parse(
      fs.readFileSync(path.join(dir, 'events', name + '.start'), 'utf8'),
    );
  const end = (name) =>
    Number(fs.readFileSync(path.join(dir, 'events', name + '.end')));
  return { dir, run, event, end };
}

test('全検査のコマンドと条件を保ち独立系統を並列に実行する', (t) => {
  const { dir, run, event, end } = fixture(t);
  assert.equal(run.status, 0, run.stdout + run.stderr);
  const checks = [
    'api-manifest.ts',
    'gofmt',
    'verify-review-notes.ts',
    'verify-nas-node.ts',
    'nas-caddy',
    'deadcode',
    'staticcheck',
    'go-test',
    'verify-openapi-race.ts',
    'consultations-race',
    'vet',
    'pnpm',
    'generate-desk-contract.ts',
    'ts-format',
    'ts-lint',
    'ts-exports',
    'typecheck.ts',
    'typecheck-root.ts',
    'client-test',
    'build.ts',
    'root-test',
  ];
  for (const key of checks) {
    assert.ok(end(key));
    assert.equal(event(key).env.GODEBUG, undefined);
  }
  for (const key of [
    'gofmt',
    'verify-review-notes.ts',
    'verify-nas-node.ts',
    'nas-caddy',
    'deadcode',
    'staticcheck',
    'go-test',
    'verify-openapi-race.ts',
    'consultations-race',
    'vet',
  ])
    assert.equal(event(key).cwd, path.join(dir, 'server'));
  for (const key of [
    'pnpm',
    'generate-desk-contract.ts',
    'ts-format',
    'ts-lint',
    'ts-exports',
    'typecheck.ts',
    'typecheck-root.ts',
    'build.ts',
    'root-test',
  ])
    assert.equal(event(key).cwd, dir);
  assert.equal(event('client-test').cwd, path.join(dir, 'client'));
  assert.deepEqual(event('nas-caddy').args, [
    'test',
    './internal/startup',
    '-run',
    '^TestNASCaddyHandoff$',
    '-count=1',
    '-v',
  ]);
  assert.deepEqual(event('go-test').args, [
    'test',
    '-json',
    '-count=1',
    '-timeout',
    '8m',
    'issue-desk-intake/server/internal/other',
    'issue-desk-intake/server/internal/consultations/sub',
  ]);
  assert.deepEqual(event('deadcode').args, [
    'run',
    'golang.org/x/tools/cmd/deadcode@v0.52.0',
    '-test',
    './...',
  ]);
  assert.deepEqual(event('staticcheck').args, [
    'run',
    'honnef.co/go/tools/cmd/staticcheck@v0.8.2',
    './...',
  ]);
  for (const mode of ['format', 'lint', 'exports'])
    assert.deepEqual(event('ts-' + mode).args, [
      'scripts/check-ts-quality.ts',
      mode,
    ]);
  assert.deepEqual(event('consultations-race').args, [
    'test',
    '-json',
    '-count=1',
    '-race',
    './internal/consultations',
  ]);
  assert.equal(event('consultations-race').env.CGO_ENABLED, '1');
  assert.equal(event('consultations-race').env.CC, 'fixture-cc');
  assert.deepEqual(event('vet').args, ['vet', './...']);
  assert.deepEqual(event('pnpm').args, [
    '--dir',
    'client',
    'install',
    '--frozen-lockfile',
    '--ignore-scripts',
    '--ignore-pnpmfile',
    '--config.minimum-release-age=20160',
    '--config.confirmModulesPurge=false',
  ]);
  assert.deepEqual(event('gofmt').args, ['-l', '.']);
  assert.deepEqual(event('client-test').args, [
    '--test',
    '--test-concurrency=4',
    'test/client-a.test.ts',
  ]);
  assert.deepEqual(event('root-test').args, [
    '--test',
    '--test-concurrency=4',
    'test/root-a.test.ts',
  ]);
  for (const key of [
    'api-manifest.ts',
    'verify-review-notes.ts',
    'verify-nas-node.ts',
    'verify-openapi-race.ts',
  ])
    assert.deepEqual(event(key).args, [
      path.join(dir, 'scripts', key),
      ...(key === 'verify-review-notes.ts' ? ['--check-dsn'] : []),
    ]);
  for (const key of ['typecheck.ts', 'build.ts'])
    assert.deepEqual(event(key).args, ['client/scripts/' + key]);
  assert.deepEqual(event('generate-desk-contract.ts').args, [
    'scripts/generate-desk-contract.ts',
  ]);
  assert.deepEqual(event('typecheck-root.ts').args, [
    'scripts/typecheck-root.ts',
  ]);
  assert.ok(
    event('api-manifest.ts').start <= end('pnpm') &&
      event('pnpm').start <= end('api-manifest.ts'),
  );
  for (const [before, after] of [
    ['pnpm', 'generate-desk-contract.ts'],
    ['generate-desk-contract.ts', 'ts-format'],
    ['ts-format', 'ts-lint'],
    ['ts-lint', 'ts-exports'],
    ['ts-exports', 'typecheck.ts'],
    ['typecheck.ts', 'typecheck-root.ts'],
    ['typecheck-root.ts', 'client-test'],
    ['client-test', 'build.ts'],
    ['build.ts', 'root-test'],
    ['nas-caddy', 'go-test'],
    ['nas-caddy', 'consultations-race'],
    ['go-test', 'verify-openapi-race.ts'],
  ])
    assert.ok(end(before) <= event(after).start, `${before} -> ${after}`);
  assert.ok(event('consultations-race').start <= end('go-test'));
  assert.ok(event('go-test').start <= end('consultations-race'));
  assert.ok(end('consultations-race') <= event('verify-openapi-race.ts').start);
  assert.equal(
    (run.stdout.match(/stage=.* end=.* duration=\d+s exit=0/g) || []).length,
    checks.length - 1,
  ); // go-qualityの1段で2つの道具を実行する
});

test('テスト対象に指定したNodeオプションをテスト起動前に拒否する', (t) => {
  const { dir, run } = fixture(t, 'invalid-node-targets');
  assert.notEqual(run.status, 0);
  assert.match(run.stdout + run.stderr, /不正なNodeテスト対象/);
  assert.equal(
    fs.existsSync(path.join(dir, 'events/client-test.start')),
    false,
  );
  assert.equal(fs.existsSync(path.join(dir, 'events/root-test.start')), false);
});

test('失敗段の出力と終了コードを分離して残し起動済みの全プロセスを回収する', (t) => {
  for (const failed of ['go-test', 'consultations-race']) {
    const { dir, run } = fixture(t, failed);
    assert.notEqual(run.status, 0);
    assert.match(run.stdout, new RegExp(`stage=${failed} end=.* exit=3`));
    assert.match(run.stdout, /^STAGE-FAIL-MARKER$/m);
    assert.match(run.stdout, /^MARKER-pnpm$/m);
    for (const file of fs
      .readdirSync(path.join(dir, 'events'))
      .filter((x) => x.endsWith('.start')))
      assert.ok(
        fs.existsSync(
          path.join(dir, 'events', file.replace(/\.start$/, '.end')),
        ),
        file,
      );
    const go = run.stdout.indexOf('stage=go-test start=');
    const race = run.stdout.indexOf('stage=consultations-race start=');
    assert.ok(
      go < run.stdout.indexOf('stage=go-test end=') &&
        run.stdout.indexOf('stage=go-test end=') < race,
    );
    assert.ok(race < run.stdout.indexOf('stage=consultations-race end='));
  }
});

test('品質検査の失敗を無視せず後続の同系統を開始しない', (t) => {
  for (const failed of [
    'deadcode',
    'staticcheck',
    'nas-caddy',
    'generate-desk-contract.ts',
    'ts-format',
    'ts-lint',
    'ts-exports',
  ]) {
    const { dir, run } = fixture(t, failed);
    assert.notEqual(run.status, 0, failed);
    assert.match(run.stdout, /STAGE-FAIL-MARKER/);
    if (failed === 'nas-caddy') {
      assert.match(run.stdout, /stage=nas-caddy end=.* exit=3/);
      assert.equal(
        fs.existsSync(path.join(dir, 'events/consultations-race.start')),
        false,
      );
    }
    const downstream =
      failed.startsWith('ts-') || failed === 'generate-desk-contract.ts'
        ? 'build.ts'
        : 'go-test';
    assert.equal(
      fs.existsSync(path.join(dir, 'events', downstream + '.start')),
      false,
      failed,
    );
  }
});

test('パッケージ列挙の失敗と空の対象を失敗として回収する', (t) => {
  for (const failure of ['list', 'empty']) {
    const { dir, run } = fixture(t, failure);
    assert.notEqual(run.status, 0, failure);
    assert.match(run.stdout, /stage=go-test end=.* exit=[1-9]/);
    assert.match(run.stdout, /stage=consultations-race end=.* exit=0/);
    assert.equal(fs.existsSync(path.join(dir, 'events/go-test.start')), false);
  }
});

test('SIGTERMで並列検証の両系統とその子プロセスを終了する', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-term-'));
  const root = path.resolve(fileURLToPath(new URL('../', import.meta.url)));
  const executable = path.join(dir, 'tool');
  fs.writeFileSync(
    executable,
    `#!/bin/sh
if [ "$1" = -e ]; then exit 0; fi
case "$1" in
  */api-manifest.ts) name=server ;;
  --dir) name=client ;;
  *) exit 0 ;;
esac
sleep 300 &
printf '%s %s\\n' "$$" "$!" > "$EVENTS/$name"
wait
`,
    { mode: 0o755 },
  );
  const env = {
    ...process.env,
    HOME: dir,
    ISSUE_DESK_TEST_POSTGRES_DSN:
      'postgresql://fixture@localhost/issue_desk_test',
    EVENTS: dir,
    NODE: executable,
    PNPM: executable,
    GO: executable,
    GOFMT: executable,
    TMPDIR: dir,
    ISSUE_DESK_RUN_TMPDIR: '',
  };
  const run = spawn('sh', [path.join(root, 'scripts/verify.sh')], {
    env,
    stdio: 'ignore',
  });
  const closed = new Promise<any>((resolve) =>
    run.once('close', (code, signal) => resolve({ code, signal })),
  );
  const pids = [];
  t.after(() => {
    if (run.exitCode === null && run.signalCode === null) run.kill('SIGKILL');
    for (const pid of pids) {
      try {
        process.kill(pid, 'SIGKILL');
      } catch {}
    }
    fs.rmSync(dir, { recursive: true, force: true });
  });
  const until = async (condition, message) => {
    const deadline = Date.now() + 10000;
    while (!condition()) {
      if (Date.now() > deadline) assert.fail(message);
      await new Promise<any>((resolve) => setTimeout(resolve, 20));
    }
  };
  await until(
    () =>
      ['server', 'client'].every((name) => {
        try {
          return /^\d+ \d+\s*$/.test(
            fs.readFileSync(path.join(dir, name), 'utf8'),
          );
        } catch (error) {
          if (error.code === 'ENOENT') return false;
          throw error;
        }
      }),
    '両系統が起動しなかった',
  );
  for (const name of ['server', 'client'])
    pids.push(
      ...fs
        .readFileSync(path.join(dir, name), 'utf8')
        .trim()
        .split(' ')
        .map(Number),
    );
  run.kill('SIGTERM');
  const result = await closed;
  assert.notEqual(result.code, 0);
  const alive = (pid) => {
    try {
      const stat = fs.readFileSync(`/proc/${pid}/stat`, 'utf8');
      return !/^\d+ \(.*\) Z /.test(stat);
    } catch (error) {
      if (error.code === 'ENOENT') return false;
      throw error;
    }
  };
  await until(
    () => pids.every((pid) => !alive(pid)),
    `子プロセスが残った: ${pids.join(', ')}`,
  );
  assert.deepEqual(
    fs.readdirSync(dir).filter((name) => name.startsWith('issue-desk-run-')),
    [],
    '作業ツリー専用の一時ディレクトリが残った',
  );
});

test('Go並列2段のTERMで両段と子孫を回収する', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-go-term-'));
  const root = path.resolve(fileURLToPath(new URL('../', import.meta.url)));
  const tool = path.join(dir, 'tool');
  fs.writeFileSync(
    tool,
    `#!/bin/sh
case "$*" in
  *"--normal-go"*) name=go ;;
  "list ./...") printf '%s\\n' issue-desk-intake/server/internal/other; exit 0 ;;
  *"test -json -count=1 -timeout 8m"*) name=go ;;
  *"--consultations-race"*) name=race ;;
  *) exit 0 ;;
esac
sleep 300 &
printf '%s %s\n' "$$" "$!" > "$EVENTS/$name"
wait
`,
    { mode: 0o755 },
  );
  const env = {
    ...process.env,
    HOME: dir,
    ISSUE_DESK_TEST_POSTGRES_DSN:
      'postgresql://fixture@localhost/issue_desk_test',
    EVENTS: dir,
    NODE: tool,
    PNPM: tool,
    GO: tool,
    GOFMT: tool,
    CC: tool,
    TMPDIR: dir,
    ISSUE_DESK_RUN_TMPDIR: dir,
  };
  const run = spawn('sh', [path.join(root, 'scripts/verify.sh')], { env });
  let output = '';
  run.stdout.on('data', (chunk) => {
    output += chunk;
  });
  run.stderr.on('data', (chunk) => {
    output += chunk;
  });
  const closed = new Promise<any>((resolve) =>
    run.once('close', (code) => resolve(code)),
  );
  const pids = [];
  t.after(() => {
    if (run.exitCode === null && run.signalCode === null) run.kill('SIGKILL');
    for (const pid of pids) {
      try {
        process.kill(pid, 'SIGKILL');
      } catch {}
    }
    fs.rmSync(dir, { recursive: true, force: true });
  });
  const until = async (predicate) => {
    const deadline = Date.now() + 10000;
    while (!predicate()) {
      if (Date.now() > deadline) assert.fail('待機が完了しない');
      await new Promise<any>((resolve) => setTimeout(resolve, 20));
    }
  };
  await until(() =>
    ['go', 'race'].every((name) => fs.existsSync(path.join(dir, name))),
  );
  for (const name of ['go', 'race'])
    pids.push(
      ...fs
        .readFileSync(path.join(dir, name), 'utf8')
        .trim()
        .split(' ')
        .map(Number),
    );
  run.kill('SIGTERM');
  assert.notEqual(await closed, 0);
  assert.match(output, /stage=go-test interrupted/);
  assert.match(output, /stage=consultations-race interrupted/);
  await until(() =>
    pids.every((pid) => {
      try {
        return /^\d+ \(.*\) Z /.test(
          fs.readFileSync(`/proc/${pid}/stat`, 'utf8'),
        );
      } catch (error) {
        if (error.code === 'ENOENT') return true;
        throw error;
      }
    }),
  );
  assert.equal(
    fs.readdirSync(dir).filter((name) => name.startsWith('issue-desk-verify.'))
      .length,
    0,
  );
});

test('deadcodeが終了0でも未使用レポートがあれば後続を止める', (t) => {
  const { dir, run } = fixture(t, 'deadcode-report');
  assert.notEqual(run.status, 0);
  assert.match(run.stdout, /unreachable func: abandoned/);
  assert.match(run.stdout, /stage=go-quality end=.* exit=1/);
  assert.equal(
    fs.existsSync(path.join(dir, 'events/staticcheck.start')),
    false,
  );
  assert.equal(fs.existsSync(path.join(dir, 'events/go-test.start')), false);
});

test('client-testの応答がない場合は期限切れを診断して失敗する', (t) => {
  const { run } = fixture(t, 'client-hang');
  assert.notEqual(run.status, 0);
  assert.match(run.stdout, /client-test: テスト結果待機が1秒で期限切れ/);
  assert.match(run.stdout, /stage=client-test end=.* exit=124/);
});

test('ブラウザ結果待ちの期限と失敗理由を保ちHTTPと子を回収する', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'browser-wait-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const preload = path.join(dir, 'clock.mjs');
  fs.writeFileSync(
    preload,
    `const original = globalThis.setTimeout;
globalThis.setTimeout = (fn, ms, ...args) => original(fn, ms === 180000 ? 1000 : ms, ...args);
`,
  );
  const browser = path.join(dir, 'browser');
  fs.writeFileSync(
    browser,
    `#!${process.execPath}
const fs = require('node:fs');
const url = process.argv.find(x => x.startsWith('http://127.0.0.1:'));
fs.writeFileSync(process.env.BROWSER_RECORD, JSON.stringify({pid: process.pid, url}));
console.error('DevTools listening on ws://127.0.0.1:12345/devtools/browser/fake');
if (process.env.BROWSER_FAIL === '1') fetch(new URL('/__result', url), {method:'POST', body:JSON.stringify({success:false, message:'original browser failure'})});
setInterval(() => {}, 1000);
`,
    { mode: 0o755 },
  );
  const root = path.resolve(fileURLToPath(new URL('../', import.meta.url)));
  for (const [file, name] of [
    [
      'stall-cause.test.ts',
      '配布画面は停止原因と理由を表示し原因のない一覧を断定しない',
    ],
    [
      'repository-display.test.ts',
      '配布画面のリポジトリ表示と内部完全名を検証し直接表示の漏れを検出する',
    ],
  ]) {
    for (const failure of [false, true]) {
      const record = path.join(dir, 'record.json');
      fs.rmSync(record, { force: true });
      const env = { ...process.env };
      delete env.NODE_TEST_CONTEXT;
      const run = spawnSync(
        process.execPath,
        [
          '--import',
          preload,
          '--test',
          '--test-name-pattern',
          `^${name}$`,
          path.join(root, 'client/test', file),
        ],
        {
          env: {
            ...env,
            ISSUE_DESK_TEST_CHROMIUM: browser,
            BROWSER_RECORD: record,
            BROWSER_FAIL: failure ? '1' : '0',
          },
          encoding: 'utf8',
        },
      );
      const output = run.stdout + run.stderr;
      assert.notEqual(run.status, 0, output);
      assert.match(output, new RegExp(name));
      assert.match(
        output,
        failure
          ? /original browser failure/
          : /ブラウザの結果POST待機が期限切れ/,
      );
      const { pid, url } = JSON.parse(fs.readFileSync(record, 'utf8'));
      assert.throws(() => process.kill(pid, 0), { code: 'ESRCH' });
      await assert.rejects(fetch(url));
    }
  }
});
