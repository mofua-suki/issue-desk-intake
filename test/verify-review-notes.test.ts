import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import {
  requiredTests,
  verifyReviewNotes,
} from '../scripts/verify-review-notes.ts';
import {
  readPostgresCalls,
  writePostgresFixture,
} from './helpers/test-postgres.ts';

const env = {
  ISSUE_DESK_TEST_POSTGRES_DSN:
    'postgresql://fixture:private-value@localhost/issue_desk_test_notes',
  GO: '/fixture/go',
};
const passEvents = () =>
  requiredTests.map(([Package, Test]) => ({ Action: 'pass', Package, Test }));
const result = (events, status = 0) => ({
  status,
  stdout: events.map((event) => JSON.stringify(event)).join('\n'),
});

test('注意点の検証入口はDB未指定・本番名・不正URLを実行前に拒否し接続先を出力しない', () => {
  for (const dsn of [
    undefined,
    '',
    'postgresql://fixture:private-value@localhost/production',
    'private-value',
  ]) {
    const lines = [];
    const code = verifyReviewNotes({
      env: { ISSUE_DESK_TEST_POSTGRES_DSN: dsn },
      execute: () => assert.fail('Goを起動してはいけない'),
      output: (line) => lines.push(line),
    });
    assert.equal(code, 1);
    assert.ok(!lines.join('\n').includes('private-value'));
  }
});

test('注意点の検証入口は実際のGoテストの全件PASSを必要としキャッシュを使わない', () => {
  const lines = [];
  let calls = 0;
  const code = verifyReviewNotes({
    env,
    output: (line) => lines.push(line),
    execute: (exe, args, options) => {
      calls++;
      assert.equal(exe, env.GO);
      assert.equal(options.env, env);
      assert.ok(options.cwd.endsWith('/server'));
      assert.ok(args.includes('-json'));
      assert.ok(args.includes('-count=1'));
      assert.deepEqual(args.slice(-3), [
        './internal/reviewnotes',
        './internal/startup',
        './internal/ai',
      ]);
      const selected = new RegExp(args[args.indexOf('-run') + 1]);
      for (const [, name] of requiredTests) assert.ok(selected.test(name));
      return result(passEvents());
    },
  });
  assert.equal(code, 0);
  assert.equal(calls, 1);
  assert.equal(
    lines.filter((line) => line.startsWith('--- PASS:')).length,
    requiredTests.length,
  );
  assert.ok(!lines.join('\n').includes('private-value'));
});

test('注意点の検証入口はSKIP・消えたテスト・別パッケージ・不正出力・子テスト失敗を成功にしない', () => {
  const cases = [
    result([]),
    result(passEvents().slice(1)),
    result(
      passEvents().map((event, i) =>
        i === 0 ? { ...event, Action: 'skip' } : event,
      ),
    ),
    result(
      passEvents().map((event, i) =>
        i === 0 ? { ...event, Package: 'different/package' } : event,
      ),
    ),
    result([
      ...passEvents(),
      {
        Action: 'skip',
        Package: requiredTests[0][0],
        Test: `${requiredTests[0][1]}/child`,
      },
    ]),
    result([
      ...passEvents(),
      {
        Action: 'fail',
        Package: requiredTests[0][0],
        Test: `${requiredTests[0][1]}/child`,
      },
    ]),
    { status: 0, stdout: 'ok (no tests to run)' },
    result(passEvents(), 1),
    { error: new Error('起動失敗'), status: null },
  ];
  for (const response of cases) {
    assert.equal(
      verifyReviewNotes({ env, execute: () => response, output: () => {} }),
      1,
    );
  }
});

test('注意点の検証失敗は原因を残し接続文字列とパスワードを伏せる', () => {
  const lines = [];
  const response = result(
    [
      {
        Action: 'output',
        Output: `接続失敗 ${env.ISSUE_DESK_TEST_POSTGRES_DSN} private-value\n`,
      },
    ],
    1,
  );
  assert.equal(
    verifyReviewNotes({
      env,
      execute: () => response,
      output: (line) => lines.push(line),
    }),
    1,
  );
  assert.ok(lines.join('\n').includes('接続失敗'));
  assert.ok(!lines.join('\n').includes('private-value'));
  assert.ok(!lines.join('\n').includes(env.ISSUE_DESK_TEST_POSTGRES_DSN));
});

test('出荷するverify.shは注意点テストのSKIPと未整形を拒否してから通常検証へ進む', (t) => {
  const root = path.resolve(fileURLToPath(new URL('../', import.meta.url)));
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-review-notes-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const postgresBin = writePostgresFixture(dir);
  const executable = (name, body) => {
    const file = path.join(dir, name);
    fs.writeFileSync(file, `#!/bin/sh\nset -eu\n${body}\n`, { mode: 0o700 });
    return file;
  };
  // Test the real shell entry and real JSON verifier; stand in only for tools
  // whose work belongs to their own tests (no client build or database here).
  const go = executable(
    'go',
    `printf 'go %s GODEBUG=%s umask=%s\\n' "$*" "\${GODEBUG-unset}" "$(umask)" >> "$VERIFY_CALLS"
if [ "$1" = list ]; then
  printf '%s\\n' issue-desk-intake/server/internal/consultations issue-desk-intake/server/internal/anomalies issue-desk-intake/server/internal/reviewnotes issue-desk-intake/server/internal/startup issue-desk-intake/server/internal/ai issue-desk-intake/server/internal/other
elif [ "$1" = test ] && [ "$2" = -json ]; then
  case "$*" in
    *TestNASBoundedHealthVerifierPreservesPipeAndStrictMeaning*) cat "$VERIFY_NAS_EVENTS";;
    *-race*./internal/consultations*) [ "\${VERIFY_RACE_FAIL:-}" != consultations ] || exit 1; cat "$VERIFY_CONSULTATION_EVENTS";;
    *) cat "$VERIFY_EVENTS";;
  esac
elif [ "$1" = test ] && [ "$2" = -race ]; then
  case "$*" in
    *./internal/httpapi*)
      [ "\${VERIFY_RACE_FAIL:-}" != openapi ] || exit 1
      printf '%s\\n' '{"Package":"issue-desk-intake/server/internal/httpapi","Action":"pass","Test":"TestOpenAPISpecConcurrentRequests"}' '{"Package":"issue-desk-intake/server/internal/httpapi","Action":"pass","Test":"TestOpenAPISpecConcurrentAuthorization"}';;
  esac
fi`,
  );
  const node = executable(
    'node',
    `case "$1" in
  "$VERIFY_ROOT/scripts/verify-review-notes.ts"|"$VERIFY_ROOT/scripts/verify-nas-node.ts"|"$VERIFY_ROOT/scripts/verify-openapi-race.ts"|"$VERIFY_ROOT/scripts/with-test-postgres.ts")
    printf 'node %s\\n' "$1" >> "$VERIFY_CALLS"
    exec "$VERIFY_REAL_NODE" "$@";;
  -e|"$VERIFY_ROOT/scripts/api-manifest.ts"|scripts/generate-desk-contract.ts|scripts/check-ts-quality.ts|client/scripts/typecheck.ts|scripts/typecheck-root.ts|client/scripts/build.ts|--test) exit 0;;
  *) printf '想定外のNode呼び出し: %s\\n' "$1" >&2; exit 1;;
esac`,
  );
  const unexpected = spawnSync(
    node,
    [path.join(root, 'scripts/unexpected.mjs')],
    {
      env: { ...process.env, VERIFY_ROOT: root },
      encoding: 'utf8',
    },
  );
  assert.equal(unexpected.status, 1, '未登録の実行を成功として扱わない');
  assert.match(unexpected.stderr, /想定外のNode呼び出し/);
  const gofmt = executable('gofmt', `printf '%s' "$VERIFY_UNFORMATTED"`);
  const pnpm = executable('pnpm', `printf 'pnpm\\n' >> "$VERIFY_CALLS"`);
  for (const scenario of [
    'pass',
    'skip',
    'no-dsn',
    'no-dsn-skip',
    'invalid-dsn',
    'gofmt',
    'nas-skip',
    'nas-missing',
    'race-fail',
    'consultations-race-fail',
    'consultations-race-missing',
  ]) {
    const events = passEvents();
    if (scenario === 'skip' || scenario === 'no-dsn-skip')
      events[0].Action = 'skip';
    const calls = path.join(dir, 'calls');
    const eventFile = path.join(dir, 'events');
    fs.writeFileSync(calls, '');
    fs.writeFileSync(eventFile, result(events).stdout);
    const consultationEvents = path.join(dir, 'consultation-events');
    fs.writeFileSync(
      consultationEvents,
      result(events.slice(scenario === 'consultations-race-missing' ? 1 : 0, 6))
        .stdout,
    );
    const nasEvents = path.join(dir, 'nas-events');
    const nasTest =
      'TestNASBoundedHealthVerifierPreservesPipeAndStrictMeaning/sh/node';
    fs.writeFileSync(
      nasEvents,
      result(
        scenario === 'nas-missing'
          ? []
          : [
              {
                Test: nasTest,
                Action: scenario === 'nas-skip' ? 'skip' : 'pass',
              },
            ],
      ).stdout,
    );
    const variables = {
      ...process.env,
      ...env,
      GO: go,
      NODE: node,
      GOFMT: gofmt,
      PNPM: pnpm,
      VERIFY_ROOT: root,
      VERIFY_REAL_NODE: process.execPath,
      VERIFY_CALLS: calls,
      VERIFY_EVENTS: eventFile,
      VERIFY_NAS_EVENTS: nasEvents,
      VERIFY_CONSULTATION_EVENTS: consultationEvents,
      ISSUE_DESK_TEST_POSTGRES_BIN: postgresBin,
      VERIFY_PG_CALLS: path.join(dir, 'pg-calls'),
      VERIFY_UNFORMATTED: scenario === 'gofmt' ? 'unformatted.go' : '',
      VERIFY_RACE_FAIL:
        scenario === 'race-fail'
          ? 'openapi'
          : scenario === 'consultations-race-fail'
            ? 'consultations'
            : '',
      CC: go,
      GODEBUG: 'tlskyber=0',
    };
    if (scenario.startsWith('no-dsn'))
      delete variables.ISSUE_DESK_TEST_POSTGRES_DSN;
    if (scenario === 'invalid-dsn')
      variables.ISSUE_DESK_TEST_POSTGRES_DSN =
        'postgresql://fixture:private-value@localhost/production';
    // チャットのシェルと同じく umask 077 から始める。
    const run = spawnSync(
      'sh',
      ['-c', 'umask 077; exec sh "$0"', path.join(root, 'scripts/verify.sh')],
      { env: variables, encoding: 'utf8' },
    );
    const passed = scenario === 'pass' || scenario === 'no-dsn';
    assert.equal(
      run.status,
      passed ? 0 : 1,
      `${scenario}: ${run.stdout} ${run.stderr}`,
    );
    const invoked = fs.readFileSync(calls, 'utf8');
    assert.equal(
      invoked.includes(
        `node ${path.join(root, 'scripts/with-test-postgres.ts')}\n`,
      ),
      scenario.startsWith('no-dsn'),
      scenario,
    );
    assert.equal(
      invoked.includes(
        `node ${path.join(root, 'scripts/verify-review-notes.ts')}\n`,
      ),
      scenario !== 'gofmt',
      scenario,
    );
    const notesSkip = scenario === 'skip' || scenario === 'no-dsn-skip';
    const nasInvoked =
      passed ||
      scenario === 'nas-skip' ||
      scenario === 'nas-missing' ||
      scenario === 'race-fail' ||
      scenario === 'consultations-race-fail' ||
      scenario === 'consultations-race-missing' ||
      notesSkip;
    assert.equal(
      invoked.includes(
        `node ${path.join(root, 'scripts/verify-nas-node.ts')}\n`,
      ),
      nasInvoked,
      scenario,
    );
    if (passed || notesSkip) {
      // 失敗の要約は前後の行を出し直すので、数えるのは要約より前の各段の出力だけにする。
      assert.equal(
        run.stdout
          .split('\n失敗の要約:\n')[0]
          .split('\n')
          .filter((line) => line.startsWith('--- PASS:')).length,
        notesSkip ? requiredTests.length : requiredTests.length + 1,
        scenario,
      );
      assert.equal(run.stdout.includes('--- SKIP:'), notesSkip, scenario);
      assert.ok(invoked.includes('go test -json -count=1'), scenario);
    }
    const raceInvoked =
      passed ||
      scenario === 'race-fail' ||
      scenario === 'consultations-race-fail' ||
      scenario === 'consultations-race-missing' ||
      notesSkip;
    assert.equal(
      invoked.includes(
        'go test -json -count=1 -timeout 8m issue-desk-intake/server/internal/anomalies',
      ),
      raceInvoked,
      scenario,
    );
    assert.equal(
      invoked.includes(
        `node ${path.join(root, 'scripts/verify-openapi-race.ts')}\n`,
      ),
      passed || scenario === 'race-fail',
      scenario,
    );
    assert.equal(
      invoked.includes(
        'go test -race -count=1 -json -run ^TestOpenAPISpecConcurrent ./internal/httpapi',
      ),
      passed || scenario === 'race-fail',
      scenario,
    );
    assert.equal(
      invoked.includes('go test -json -count=1 -race ./internal/consultations'),
      raceInvoked,
      scenario,
    );
    if (raceInvoked) {
      if (passed)
        assert.ok(
          invoked.indexOf('go test -json -count=1 -race ') <
            invoked.indexOf('go vet ./...'),
          scenario,
        );
    }
    if (scenario === 'consultations-race-missing')
      assert.match(
        run.stdout,
        /NOT RUN: TestSplitParentReconcileAfterLateFiling/,
      );
    assert.equal(invoked.includes('go vet ./...'), passed, scenario);
    assert.ok(invoked.includes('pnpm'), scenario);
    if (scenario.startsWith('no-dsn'))
      assert.ok(readPostgresCalls(variables.VERIFY_PG_CALLS).includes('stop'));
    assert.ok(!`${run.stdout}${run.stderr}`.includes('private-value'));
    assert.ok(
      !invoked.includes('GODEBUG=tlskyber'),
      `${scenario}: 削除済みの GODEBUG を go へ渡さない`,
    );
    assert.ok(
      !invoked.includes('umask=0077'),
      `${scenario}: go を配置と同じ umask 022 で呼ぶ`,
    );
    if (scenario === 'pass') {
      const goCalls = invoked
        .split('\n')
        .filter((line) => line.startsWith('go test -json -count=1'));
      const race = goCalls.filter((line) => line.includes(' -race '));
      assert.equal(race.length, 1);
      assert.match(race[0], /-race \.\/internal\/consultations /);
      const ordinary = goCalls.find((line) => line.includes(' -timeout 8m '));
      assert.ok(ordinary);
      for (const [pkg] of requiredTests) {
        const isConsultations = pkg.endsWith('/consultations');
        assert.equal(ordinary.includes(pkg), !isConsultations, pkg);
      }
      assert.ok(ordinary.includes('issue-desk-intake/server/internal/other'));
      const noDsn = { ...variables };
      delete noDsn.ISSUE_DESK_TEST_POSTGRES_DSN;
      for (const mode of ['--server', '--client']) {
        fs.writeFileSync(calls, '');
        const selected = spawnSync(
          'sh',
          [path.join(root, 'scripts/verify.sh'), mode],
          {
            env: noDsn,
            encoding: 'utf8',
          },
        );
        assert.equal(selected.status, 0, selected.stdout + selected.stderr);
        const selectedCalls = fs.readFileSync(calls, 'utf8');
        assert.ok(
          selectedCalls.includes('scripts/with-test-postgres.ts'),
          mode,
        );
        assert.equal(
          selectedCalls.includes('go vet ./...'),
          mode === '--server',
          mode,
        );
        assert.equal(selectedCalls.includes('pnpm'), mode === '--client', mode);
      }
    }
  }
});

test('注意点だけのCLIもDSN未設定時にDBを準備しSKIPを拒否して停止する', (t) => {
  const root = path.resolve(fileURLToPath(new URL('../', import.meta.url)));
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-notes-cli-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const postgresBin = writePostgresFixture(dir);
  const go = path.join(dir, 'go');
  fs.writeFileSync(
    go,
    '#!/bin/sh\nprintf "%s\\n" "$@" > "$VERIFY_GO_ARGS"\ncat "$VERIFY_EVENTS"\n',
    { mode: 0o700 },
  );
  for (const [skip, consultationsOnly] of [
    [false, false],
    [true, false],
    [false, true],
  ]) {
    const events = consultationsOnly ? passEvents().slice(0, 6) : passEvents();
    if (skip) events[0].Action = 'skip';
    const eventFile = path.join(dir, 'events');
    const calls = path.join(dir, 'calls');
    const goArgs = path.join(dir, 'go-args');
    fs.writeFileSync(eventFile, result(events).stdout);
    fs.writeFileSync(calls, '');
    const variables: NodeJS.ProcessEnv = {
      ...process.env,
      GO: go,
      ISSUE_DESK_TEST_POSTGRES_BIN: postgresBin,
      VERIFY_EVENTS: eventFile,
      VERIFY_PG_CALLS: calls,
      VERIFY_GO_ARGS: goArgs,
    };
    delete variables.ISSUE_DESK_TEST_POSTGRES_DSN;
    const run = spawnSync(
      process.execPath,
      [
        path.join(root, 'scripts/verify-review-notes.ts'),
        ...(consultationsOnly ? ['--consultations-only'] : []),
      ],
      {
        env: variables,
        encoding: 'utf8',
        timeout: 15_000,
      },
    );
    assert.equal(run.status, skip ? 1 : 0, `${run.stdout} ${run.stderr}`);
    assert.ok(run.stdout.includes(skip ? '--- SKIP:' : '--- PASS:'));
    if (consultationsOnly) {
      assert.equal(
        run.stdout.split('\n').filter((line) => line.startsWith('--- PASS:'))
          .length,
        6,
      );
      const args = fs.readFileSync(goArgs, 'utf8').split('\n');
      assert.deepEqual(
        args.filter((arg) => arg.startsWith('./internal/')),
        ['./internal/consultations'],
      );
      const selected = new RegExp(args[args.indexOf('-run') + 1]);
      for (const [pkg, name] of requiredTests)
        assert.equal(selected.test(name), pkg.endsWith('/consultations'), name);
    }
    const pgCalls = readPostgresCalls(calls);
    assert.ok(pgCalls.includes('start'));
    assert.ok(pgCalls.includes('stop'));
  }
});

test('OpenAPI race の固定検証は並行テスト2件の個別の合格を必要とする', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'openapi-race-cli-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  fs.writeFileSync(path.join(dir, 'clang'), '#!/bin/sh\nexit 0\n', {
    mode: 0o700,
  });
  const go = path.join(dir, 'go');
  fs.writeFileSync(
    go,
    '#!/bin/sh\n[ "$CC" = clang ] || exit 90\nprintf "%s\\n" "$GO_EVENTS"\nexit "$GO_STATUS"\n',
    { mode: 0o700 },
  );
  const script = fileURLToPath(
    new URL('../scripts/verify-openapi-race.ts', import.meta.url),
  );
  const Package = 'issue-desk-intake/server/internal/httpapi';
  const requests = {
    Package,
    Test: 'TestOpenAPISpecConcurrentRequests',
    Action: 'pass',
  };
  const authorization = {
    Package,
    Test: 'TestOpenAPISpecConcurrentAuthorization',
    Action: 'pass',
  };
  const output = { Package, Action: 'output', Output: 'race diagnostic\n' };
  const run = (events, status = 0) =>
    spawnSync(
      process.execPath,
      [
        '--import',
        'data:text/javascript,globalThis.fetch=()=>{throw Error("unexpected compiler download")}',
        script,
      ],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          PATH: dir,
          CC: '',
          GO: go,
          GO_EVENTS: events.map((event) => JSON.stringify(event)).join('\n'),
          GO_STATUS: String(status),
        },
      },
    );
  const complete = run([output, requests, authorization]);
  assert.equal(complete.status, 0, complete.stderr);
  assert.match(complete.stdout, /race diagnostic/);
  for (const events of [
    [{ Package, Action: 'pass' }],
    [requests],
    [requests, { ...authorization, Action: 'skip' }],
  ]) {
    const result = run(events);
    assert.equal(result.status, 1);
    assert.match(result.stderr, /TestOpenAPISpecConcurrentAuthorization/);
  }
  assert.equal(run([requests, authorization], 1).status, 1);
});

test('通常Go段はパッケージ別の必須PASSと配下のskipを照合し無関係なskipを許す', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'normal-go-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const go = path.join(dir, 'go');
  fs.writeFileSync(go, '#!/bin/sh\ncat "$VERIFY_EVENTS"\n', { mode: 0o700 });
  const normal = passEvents().slice(6);
  const [first] = normal;
  const scenarios = [
    {
      events: [
        ...normal,
        { Action: 'skip', Package: first.Package, Test: 'TestOther' },
      ],
      status: 0,
    },
    { events: normal.slice(1), status: 1 },
    {
      events: [
        {
          Action: 'build-output',
          ImportPath: first.Package,
          Output: 'コンパイル失敗 private-value',
        },
        { Action: 'build-fail', ImportPath: first.Package },
        ...normal.slice(1),
      ],
      status: 1,
      diagnostic: /コンパイル失敗 \[検証DB接続情報\]/,
    },
    { events: [{ ...first, Action: 'skip' }, ...normal.slice(1)], status: 1 },
    { events: [{ ...first, Action: 'fail' }, ...normal.slice(1)], status: 1 },
    {
      events: [
        ...normal,
        { ...first, Test: first.Test + '/child', Action: 'skip' },
      ],
      status: 1,
    },
    {
      events: [{ ...first, Package: 'other/package' }, ...normal.slice(1)],
      status: 1,
    },
  ];
  for (const scenario of scenarios) {
    const file = path.join(dir, 'events');
    fs.writeFileSync(file, result(scenario.events).stdout);
    const run = spawnSync(
      process.execPath,
      [
        fileURLToPath(
          new URL('../scripts/verify-review-notes.ts', import.meta.url),
        ),
        '--normal-go',
        first.Package,
      ],
      { env: { ...env, GO: go, VERIFY_EVENTS: file }, encoding: 'utf8' },
    );
    assert.equal(run.status, scenario.status, run.stdout + run.stderr);
    if (scenario.diagnostic) assert.match(run.stdout, scenario.diagnostic);
    assert.doesNotMatch(run.stdout, /private-value/);
  }
});
