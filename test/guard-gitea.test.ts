import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn, spawnSync } from 'node:child_process';
import type { SpawnSyncReturns } from 'node:child_process';
import type { TestContext } from 'node:test';

const repoRoot = path.resolve(import.meta.dirname, '..');
const guard = path.join(repoRoot, 'scripts/guard-gitea.sh');
const INTAKE = 'gitea-nas:mofua-suki/issue-desk-intake.git';
const WORKER = 'gitea-nas:mofua-suki/issue-desk-worker.git';
// 実在の資格情報の書式をまねない合成値（32文字以上・数字を含む英数字の並びとして扱われる）
const SECRET = '0123456789abcdefghijklmnopqrstuvwxyzABCD';

// ガードとフィクスチャが使う最小限の道具だけを置いた PATH。実機の git・go・gofmt は見えない。
const system = fs.mkdtempSync(path.join(os.tmpdir(), 'guard-gitea-system-'));
test.after(() => fs.rmSync(system, { recursive: true, force: true }));
for (const name of [
  'bash',
  'sh',
  'cat',
  'env',
  'grep',
  'ls',
  'mkdir',
  'mv',
  'rm',
  'rmdir',
  'sleep',
]) {
  const found = ['/usr/bin', '/bin']
    .map((dir) => path.join(dir, name))
    .find((file) => fs.existsSync(file));
  assert.ok(found, `${name} が必要です`);
  fs.symlinkSync(found, path.join(system, name));
}
const bash = path.join(system, 'bash');
// 実際の git（PATH には置かない）。Gitのフィクスチャが内容の要約（hash-object）だけを任せ、実際のリポジトリを使う試験が直接使う。
const realGit = (process.env.PATH ?? '')
  .split(path.delimiter)
  .map((dir) => path.join(dir, 'git'))
  .find((file) => fs.existsSync(file));
assert.ok(realGit, '実際の git が必要です');
// Gitのフィクスチャが返す同一性の既定値（先頭コミットと内容の木）。
const HEAD_ID = '1'.repeat(40);
const TREE_ID = '2'.repeat(40);

interface EntryOptions {
  cwd?: string;
  env?: Record<string, string>;
  input?: string;
}
interface Closed {
  status: number | null;
  signal?: NodeJS.Signals | null;
  stdout?: string;
  stderr?: string;
}
type Entry = (
  args?: string[],
  options?: EntryOptions,
) => SpawnSyncReturns<string>;
interface Box {
  root: string;
  bin: string;
  home: string;
  repo: string;
  state: string;
  tmp: string;
  set: (name: string, value: unknown) => void;
  calls: () => string[];
  tool: (name: string, body: string, dir?: string) => void;
  env: (extra?: Record<string, string>) => Record<string, string>;
  verify: Entry;
  store: Entry;
}

// 隔離した一時ディレクトリに、Gitの固定応答と検証コマンドのフィクスチャを置く。
// 実際のリポジトリの検証・Git・Giteaは呼ばない。フィクスチャの挙動は state/ のファイルで決める。
function sandbox(t: TestContext, origin?: string | null): Box {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'guard-gitea-test-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const dirs = Object.fromEntries(
    ['bin', 'home', 'repo', 'state', 'tmp'].map((name) => [
      name,
      path.join(root, name),
    ]),
  );
  for (const dir of Object.values(dirs)) fs.mkdirSync(dir);
  fs.mkdirSync(path.join(dirs.repo, 'scripts'));
  fs.mkdirSync(path.join(dirs.repo, 'sub'));
  const box = { root, ...dirs } as Box;
  box.set = (name, value) =>
    fs.writeFileSync(path.join(dirs.state, name), String(value));
  box.calls = () =>
    fs.existsSync(path.join(dirs.state, 'calls'))
      ? fs
          .readFileSync(path.join(dirs.state, 'calls'), 'utf8')
          .split('\n')
          .filter(Boolean)
      : [];
  box.tool = (name, body, dir = dirs.bin) => {
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(
      path.join(dir, name),
      `#!/bin/sh\nS='${dirs.state}'\n${body}\n`,
      { mode: 0o755 },
    );
  };
  // Gitの固定メタデータ応答。
  box.tool(
    'git',
    [
      'env > "$S/env-git"',
      'if [ "$*" = "rev-parse --show-toplevel" ]; then [ -f "$S/no-toplevel" ] && exit 128; cat "$S/toplevel"; exit 0; fi',
      'if [ "$*" = "remote get-url origin" ]; then [ -f "$S/origin" ] || exit 2; cat "$S/origin"; exit 0; fi',
      // 同一性の固定応答（state/ の id-* で差し替える。ファイルを消せば読めない応答になる）。
      'if [ "$*" = "rev-parse --absolute-git-dir" ]; then cat "$S/id-gitdir"; exit $?; fi',
      'if [ "$*" = "rev-parse --path-format=absolute --git-common-dir" ]; then cat "$S/id-commondir"; exit $?; fi',
      'if [ "$*" = "rev-parse --verify HEAD^{commit}" ]; then cat "$S/id-head"; exit $?; fi',
      'if [ "$*" = "symbolic-ref -q HEAD" ]; then [ -f "$S/id-branch-broken" ] && exit 128; [ -s "$S/id-branch" ] || exit 1; cat "$S/id-branch"; exit 0; fi',
      'if [ "$*" = "remote get-url --push --all origin" ]; then if [ -f "$S/id-push" ]; then cat "$S/id-push"; else cat "$S/origin"; fi; exit $?; fi',
      'if [ "$*" = "read-tree HEAD" ] || [ "$*" = "add -A" ]; then printf \'%s %s\\n\' "$1" "$GIT_INDEX_FILE" >> "$S/index-calls"; [ -f "$S/id-index-broken" ] && exit 1; [ -f "$S/id-index-leave" ] && : > "$GIT_INDEX_FILE"; exit 0; fi',
      'if [ "$*" = "write-tree" ]; then cat "$S/id-tree"; exit $?; fi',
      // 内容の要約だけは実際の git に任せる（フィクスチャの道具だけでは要約を計算できないため）。
      `if [ "$1" = hash-object ]; then [ -f "$S/id-hash-broken" ] && exit 1; exec '${realGit}' "$@"; fi`,
      'exit 1',
    ].join('\n'),
  );
  box.set('id-gitdir', path.join(dirs.repo, '.git') + '\n');
  box.set('id-commondir', path.join(dirs.repo, '.git') + '\n');
  box.set('id-head', HEAD_ID + '\n');
  box.set('id-branch', 'refs/heads/main\n');
  box.set('id-tree', TREE_ID + '\n');
  // 検証コマンドのフィクスチャ: 呼び出し・環境・標準入力を記録し、state/ の出力と終了コードを返す。
  const fixture = (label, name) =>
    [
      `name=${name}`,
      `printf '%s\\n' "${label}\${*:+ $*}" >> "$S/calls"`,
      'env > "$S/env-$name"',
      'cat > "$S/stdin-$name"',
      '[ -f "$S/out-$name" ] && cat "$S/out-$name"',
      'exit "$(cat "$S/exit-$name" 2>/dev/null || echo 0)"',
    ].join('\n');
  fs.writeFileSync(
    path.join(dirs.repo, 'scripts/verify.sh'),
    `S='${dirs.state}'\n${fixture('verify', 'verify')}\n`,
  );
  box.tool('go', fixture('go', '"go-$1"'));
  box.tool('gofmt', fixture('gofmt', 'gofmt'));
  box.set('toplevel', dirs.repo + '\n');
  if (origin) box.set('origin', origin + '\n');
  box.env = (extra = {}) => ({
    HOME: dirs.home,
    PATH: `${dirs.bin}:${system}`,
    TMPDIR: dirs.tmp,
    LANG: 'C.UTF-8',
    ...extra,
  });
  const entry =
    (name: string) =>
    (args: string[] = [], options: EntryOptions = {}) => {
      const result = spawnSync(bash, [guard, name, ...args], {
        cwd: options.cwd ?? path.join(dirs.repo, 'sub'),
        env: box.env(options.env),
        input: options.input ?? '',
        encoding: 'utf8',
      });
      assert.equal(result.stderr, '');
      return result;
    };
  box.verify = entry('verify');
  box.store = entry('verify-store');
  return box;
}

function hook(input, env = process.env) {
  const result = spawnSync(bash, [guard], {
    input: JSON.stringify(input),
    encoding: 'utf8',
    env,
  });
  assert.equal(result.status, 0);
  assert.equal(result.stderr, '');
  return result.stdout;
}

function tree(dir) {
  return fs.readdirSync(dir, { recursive: true }).map(String).sort();
}

const STARTUP_REFUSED =
  '必須検証（段1）: 実行不能。ガードの起動環境に BASH_ENV・SHELLOPTS・BASHOPTS または取り込まれたシェル関数があり、検証を差し替え得るため実行しない。これらを外して実行する。\n';

// フィクスチャが記録した子プロセスの環境値を、名前→値で返す。
function childEnv(box, file) {
  return Object.fromEntries(
    fs
      .readFileSync(path.join(box.state, file), 'utf8')
      .split('\n')
      .filter(Boolean)
      .map((line) => {
        const at = line.indexOf('=');
        return [line.slice(0, at), line.slice(at + 1)];
      }),
  );
}

// 合格の行。合格は、結果を世代 generation として記録できたときだけ出る。
const passed = (generation = 1, repo = 'mofua-suki/issue-desk-intake') =>
  `必須検証（段1）: 合格 repo=${repo}。結果を世代 ${generation} として記録し、検証の前後で対象が同じことを確かめた。この記録だけでは送信の許可にならない（送信直前の照合は未完成）。\n`;

function shownNames(stdout) {
  return stdout.split('\n').filter((line) => line.startsWith('- '));
}

test('固定入口: Intakeは作業場所の最上位で sh scripts/verify.sh だけを実行して合格する', (t) => {
  const box = sandbox(t, INTAKE);
  const result = box.verify();
  assert.equal(result.status, 0);
  assert.equal(result.stdout, passed());
  assert.deepEqual(box.calls(), ['verify']);
  assert.ok(
    fs
      .readFileSync(path.join(box.state, 'env-verify'), 'utf8')
      .split('\n')
      .includes(`PWD=${box.repo}`),
  );
});

test('固定入口: Workerは go test・go vet・gofmt の3つを実行し、すべて成功したときだけ合格する', (t) => {
  const box = sandbox(t, WORKER);
  const result = box.verify();
  assert.equal(result.status, 0);
  assert.equal(result.stdout, passed(1, 'mofua-suki/issue-desk-worker'));
  assert.deepEqual(box.calls(), [
    'go test ./...',
    'go vet ./...',
    'gofmt -l .',
  ]);
});

test('対応表: originの書き方が違っても同じリポジトリとして扱う', (t) => {
  for (const origin of [
    'ssh://git@nas.example:2222/mofua-suki/issue-desk-intake.git',
    'http://nas.example:3000/mofua-suki/issue-desk-intake',
    'http://nas.example:3000/mofua-suki/issue-desk-intake/',
  ]) {
    const box = sandbox(t, origin);
    assert.equal(box.verify().status, 0, origin);
    assert.deepEqual(box.calls(), ['verify']);
  }
});

test('対応表: ~/.local/bin の go・gofmt を PATH より優先する', (t) => {
  const box = sandbox(t, WORKER);
  box.set('exit-go-test', 1);
  const local = path.join(box.home, '.local/bin');
  box.tool('go', 'printf \'%s\\n\' "local-go $*" >> "$S/calls"', local);
  box.tool('gofmt', 'printf \'%s\\n\' "local-gofmt $*" >> "$S/calls"', local);
  assert.equal(box.verify().status, 0);
  assert.deepEqual(box.calls(), [
    'local-go test ./...',
    'local-go vet ./...',
    'local-gofmt -l .',
  ]);
});

test('差替え・成功申告・任意フラグ: 引数付きの呼び出しは何も実行せず拒否する', (t) => {
  const cases = [
    ['--', 'true'],
    ['true'],
    ['--command', 'true'],
    ['--result', 'pass'],
    ['passed'],
    ['--verified'],
    ['--force'],
    ['--skip'],
    ['--no-verify'],
    ['--allow-preexisting'],
    ['mofua-suki/issue-desk-worker'],
    [''],
  ];
  for (const args of cases) {
    const box = sandbox(t, INTAKE);
    box.set('exit-verify', 1);
    const result = box.verify(args);
    assert.equal(result.status, 2, JSON.stringify(args));
    assert.equal(
      result.stdout,
      '必須検証（段1）: 拒否。引数は受け付けない（検証コマンドの指定・結果の申告・フラグでは合格にならない）。作業場所で引数なしの verify を実行する。\n',
    );
    assert.deepEqual(box.calls(), []);
  }
});

test('成功申告: 標準入力や環境値で成功を申告しても、失敗した検証は不合格になる', (t) => {
  const box = sandbox(t, INTAKE);
  box.set('exit-verify', 1);
  const claim = JSON.stringify({
    verified: true,
    result: 'pass',
    command: 'true',
    tool_name: 'Bash',
    tool_input: { command: 'true' },
  });
  const result = box.verify([], {
    input: claim,
    env: {
      GUARD_VERIFY_RESULT: 'pass',
      VERIFIED: '1',
      GUARD_VERIFY_COMMAND: 'true',
      VERIFY_COMMAND: 'true',
    },
  });
  assert.equal(result.status, 1);
  assert.match(result.stdout, /不合格 repo=mofua-suki\/issue-desk-intake。/);
  assert.equal(
    fs.readFileSync(path.join(box.state, 'stdin-verify'), 'utf8'),
    '',
  );
});

test('一部成功: Workerの3検証のうち1つでも失敗すれば不合格になり、残りも実行する', (t) => {
  for (const [name, label] of [
    ['go-test', 'go test ./...'],
    ['go-vet', 'go vet ./...'],
    ['gofmt', 'gofmt -l .（出力が空であること）'],
  ]) {
    const box = sandbox(t, WORKER);
    box.set(`exit-${name}`, 1);
    const result = box.verify();
    assert.equal(result.status, 1, name);
    assert.ok(
      result.stdout.includes(`失敗した検証: ${label}（終了コード 1）\n`),
      result.stdout,
    );
    assert.match(result.stdout, /不合格 repo=mofua-suki\/issue-desk-worker。/);
    assert.deepEqual(box.calls(), [
      'go test ./...',
      'go vet ./...',
      'gofmt -l .',
    ]);
  }
});

test('一部成功: gofmtが終了コード0でも未整形の出力があれば不合格になり、ファイル名は表示しない', (t) => {
  const box = sandbox(t, WORKER);
  box.set('out-gofmt', 'internal/secret-name.go\n');
  const result = box.verify();
  assert.equal(result.status, 1);
  assert.ok(
    result.stdout.includes(
      '失敗した検証: gofmt -l .（出力が空であること）（終了コード 1）\n',
    ),
  );
  assert.ok(!result.stdout.includes('secret-name'));
});

test('対応表不足: 未定義のリポジトリは固定の理由で拒否し、検証を実行しない', (t) => {
  for (const origin of [
    'gitea-nas:mofua-suki/other-project.git',
    'gitea-nas:someone/issue-desk-intake.git',
    'gitea-nas:Mofua-suki/Issue-Desk-Intake.git',
    'issue-desk-intake/issue-desk-intake',
  ]) {
    const box = sandbox(t, origin);
    const result = box.verify();
    assert.equal(result.status, 2, origin);
    assert.equal(
      result.stdout,
      '必須検証（段1）: 拒否。必須検証の対応表に無いリポジトリのため、検証済みにできない。\n',
    );
    assert.deepEqual(box.calls(), []);
  }
});

test('実行不能: Gitの作業場所を確認できなければ合格にしない', (t) => {
  const noRepo = sandbox(t, INTAKE);
  noRepo.set('no-toplevel', '');
  let result = noRepo.verify();
  assert.equal(result.status, 3);
  assert.equal(
    result.stdout,
    '必須検証（段1）: 実行不能。Gitの作業場所を確認できない。\n',
  );
  assert.deepEqual(noRepo.calls(), []);

  const noGit = sandbox(t, INTAKE);
  fs.rmSync(path.join(noGit.bin, 'git'));
  result = noGit.verify();
  assert.equal(result.status, 3);
  assert.equal(
    result.stdout,
    '必須検証（段1）: 実行不能。Gitの作業場所を確認できない。\n',
  );
  assert.deepEqual(noGit.calls(), []);
});

test('実行不能: originからリポジトリを特定できなければ合格にしない', (t) => {
  for (const origin of [
    null,
    '\n',
    'issue-desk-intake\n',
    'gitea-nas:mofua-suki/issue desk.git\n',
    'gitea-nas:mofua-suki/a\ngitea-nas:mofua-suki/issue-desk-intake\n',
  ]) {
    const box = sandbox(t, null);
    if (origin !== null) box.set('origin', origin);
    const result = box.verify();
    assert.equal(result.status, 3, JSON.stringify(origin));
    assert.equal(
      result.stdout,
      '必須検証（段1）: 実行不能。origin からリポジトリを特定できない。\n',
    );
    assert.deepEqual(box.calls(), []);
  }
});

test('実行不能: 必須の検証コマンドやファイルが無ければ合格にしない', (t) => {
  const noScript = sandbox(t, INTAKE);
  fs.rmSync(path.join(noScript.repo, 'scripts/verify.sh'));
  let result = noScript.verify();
  assert.equal(result.status, 3);
  assert.equal(
    result.stdout,
    '実行できない検証: sh scripts/verify.sh（必要なコマンドまたはファイルが無い）\n必須検証（段1）: 実行不能 repo=mofua-suki/issue-desk-intake。必須の検証をすべて実行できるまで合格にしない。\n',
  );

  // 検証のスクリプトがリンクなら、リンク先（無視されている場所に置けば同一性に入らない）を実行せず、実行不能にする。
  for (const kind of [
    '外のファイルへのリンク',
    '無視されている場所へのリンク',
    'ディレクトリ',
  ]) {
    const linked = sandbox(t, INTAKE);
    const script = path.join(linked.repo, 'scripts/verify.sh');
    const body = fs.readFileSync(script);
    fs.rmSync(script);
    if (kind === 'ディレクトリ') {
      fs.mkdirSync(script);
    } else {
      const target =
        kind === '外のファイルへのリンク'
          ? path.join(linked.tmp, 'elsewhere.sh')
          : path.join(linked.repo, '.tmp', 'verify.sh');
      fs.mkdirSync(path.dirname(target), { recursive: true });
      fs.writeFileSync(target, body, { mode: 0o755 });
      fs.symlinkSync(target, script);
    }
    result = linked.verify();
    assert.equal(result.status, 3, kind);
    assert.equal(
      result.stdout,
      '実行できない検証: sh scripts/verify.sh（必要なコマンドまたはファイルが無い）\n必須検証（段1）: 実行不能 repo=mofua-suki/issue-desk-intake。必須の検証をすべて実行できるまで合格にしない。\n',
      kind,
    );
    assert.deepEqual(linked.calls(), [], kind);
    assert.equal(readRecord(storePaths(linked)), record('invalid', 1), kind);
    assert.equal(resultsOf(storePaths(linked)), 'intake-verify:x', kind);
  }

  const noGo = sandbox(t, WORKER);
  fs.rmSync(path.join(noGo.bin, 'go'));
  result = noGo.verify();
  assert.equal(result.status, 3);
  assert.ok(result.stdout.includes('実行できない検証: go test ./...（'));
  assert.ok(result.stdout.includes('実行できない検証: go vet ./...（'));
  assert.ok(!result.stdout.includes('合格 repo'));
  assert.deepEqual(noGo.calls(), ['gofmt -l .']);

  const noGoAndFail = sandbox(t, WORKER);
  fs.rmSync(path.join(noGoAndFail.bin, 'go'));
  noGoAndFail.set('exit-gofmt', 2);
  assert.equal(noGoAndFail.verify().status, 1);
});

test('環境上書き: 検証ツールを差し替える環境値は検証に流入しない', (t) => {
  const overrides = {
    NODE: '/evil/node',
    PNPM: '/evil/pnpm',
    GO: '/evil/go',
    GOFMT: '/evil/gofmt',
    GOFLAGS: '-run=^$',
    GODEBUG: 'x=1',
    GOTOOLCHAIN: 'evil',
    GOROOT: '/evil',
    GOENV: '/evil/env',
    GOWORK: '/evil/go.work',
    NODE_OPTIONS: '--require=/evil.js',
    NODE_PATH: '/evil',
    NODE_TEST_CONTEXT: 'child',
    ENV: '/evil/rc',
    VERIFY_LOG_DIR: '/evil',
    VERIFY_STAGE_FILE: '/evil/stage',
    ISSUE_DESK_TEST_POSTGRES_DSN: 'postgres://evil/issue_desk_test',
    ISSUE_DESK_TEST_POSTGRES_BIN: '/evil/bin',
    GOOS: 'plan9',
    GOARCH: 'wasm',
    CGO_ENABLED: '0',
    GOPATH: '/evil/gopath',
    GOCACHE: '/evil/cache',
    CC: '/evil/cc',
    XDG_CONFIG_HOME: '/evil/config',
    LD_LIBRARY_PATH: '/evil/lib',
    GIT_DIR: '/evil/.git',
    GIT_WORK_TREE: '/evil',
    GIT_CONFIG_COUNT: '1',
    GIT_CONFIG_KEY_0: 'remote.origin.url',
    GIT_CONFIG_VALUE_0: INTAKE,
    GIT_CONFIG_GLOBAL: '/evil/gitconfig',
    GIT_EXEC_PATH: '/evil/git-core',
    // 許可リストに無い任意の名前も渡さない。
    GUARD_ANYTHING: 'x',
    UNLISTED_NAME_123: 'y',
  };
  for (const [origin, names] of [
    [INTAKE, ['verify']],
    [WORKER, ['go-test', 'go-vet', 'gofmt']],
  ] as [string, string[]][]) {
    const box = sandbox(t, origin);
    assert.equal(box.verify([], { env: overrides }).status, 0);
    for (const file of ['env-git', ...names.map((name) => `env-${name}`)]) {
      const seen = childEnv(box, file);
      for (const name of Object.keys(overrides))
        assert.ok(!(name in seen), `${file}: ${name}`);
      // 渡るのは許可した4つだけ（PWD・OLDPWD・SHLVL・_ はフィクスチャのシェルが自分で置く値）。
      const passed = Object.keys(seen)
        .filter((name) => !['PWD', 'OLDPWD', 'SHLVL', '_'].includes(name))
        .sort();
      assert.deepEqual(passed, ['HOME', 'LANG', 'PATH', 'TMPDIR'], file);
      assert.equal(seen.HOME, box.home);
      assert.equal(seen.PATH, `${box.bin}:${system}`);
      assert.equal(seen.TMPDIR, box.tmp);
      assert.equal(seen.LANG, 'C.UTF-8');
    }
  }
});

test('環境上書き: XDG_CONFIG_HOME は検証の go・gofmt へ渡らない（go/env の GOFLAGS を読ませない）', (t) => {
  const box = sandbox(t, WORKER);
  const config = path.join(box.root, 'xdg');
  fs.mkdirSync(path.join(config, 'go'), { recursive: true });
  fs.writeFileSync(path.join(config, 'go/env'), 'GOFLAGS=-run=^$\n');
  box.set('exit-go-test', 1);
  const result = box.verify([], { env: { XDG_CONFIG_HOME: config } });
  assert.equal(result.status, 1);
  for (const file of ['env-go-test', 'env-go-vet', 'env-gofmt']) {
    assert.ok(
      !fs
        .readFileSync(path.join(box.state, file), 'utf8')
        .includes('XDG_CONFIG_HOME='),
      file,
    );
  }
});

test('環境上書き: BASH_ENV が設定されていれば、実在するファイルでも検証を実行せず実行不能にする', (t) => {
  for (const body of [
    'sh() { return 0; }\ngit() { return 0; }\n',
    'sh() { return 0; }\nexport -f sh\nunset BASH_ENV\n',
    ': 何もしない\n',
  ]) {
    const box = sandbox(t, INTAKE);
    box.set('exit-verify', 1);
    const rc = path.join(box.root, 'rc.sh');
    fs.writeFileSync(rc, body);
    const result = box.verify([], { env: { BASH_ENV: rc } });
    assert.equal(result.status, 3, body);
    assert.equal(result.stdout, STARTUP_REFUSED);
    assert.deepEqual(box.calls(), []);
  }
  const missing = sandbox(t, INTAKE);
  assert.equal(
    missing.verify([], { env: { BASH_ENV: '/nonexistent/rc' } }).status,
    3,
  );
  assert.deepEqual(missing.calls(), []);
});

test('環境上書き: 環境から取り込まれたシェル関数があれば、検証を実行せず実行不能にする', (t) => {
  const pass = '() { return 0; }';
  for (const name of [
    'sh',
    'git',
    'go',
    'gofmt',
    'env',
    'printf',
    'cd',
    'unset',
    'command',
    'verify_run_other',
    'anything',
  ]) {
    for (const origin of [INTAKE, WORKER]) {
      const box = sandbox(t, origin);
      for (const check of ['verify', 'go-test', 'go-vet', 'gofmt'])
        box.set(`exit-${check}`, 1);
      const result = box.verify([], { env: { [`BASH_FUNC_${name}%%`]: pass } });
      assert.equal(result.status, 3, name);
      assert.equal(result.stdout, STARTUP_REFUSED, name);
      assert.deepEqual(box.calls(), [], name);
    }
  }
  // 検出に使う組み込みコマンドを、何も返さない関数に差し替えても検証は実行されない（固定の文は出せないことがある）。
  for (const name of ['builtin', 'declare']) {
    const box = sandbox(t, INTAKE);
    const result = box.verify([], { env: { [`BASH_FUNC_${name}%%`]: pass } });
    assert.equal(result.status, 3, name);
    assert.ok(!result.stdout.includes('合格'), name);
    assert.deepEqual(box.calls(), [], name);
  }
  // ガード自身と同じ名前の関数を取り込ませても、ガードの定義が使われ、取り込みとして検出される。
  for (const name of [
    'verify_run',
    'verify_checks',
    'verify_report_failure',
    'verify_startup_clean',
    'verify_clean_env',
    'verify_has',
    'verify_main',
  ]) {
    const box = sandbox(t, INTAKE);
    const result = box.verify([], { env: { [`BASH_FUNC_${name}%%`]: pass } });
    assert.equal(result.status, 3, name);
    assert.equal(result.stdout, STARTUP_REFUSED, name);
    assert.deepEqual(box.calls(), [], name);
  }
});

test('環境上書き: 環境から渡した SHELLOPTS・BASHOPTS があれば、検証を実行せず実行不能にする', (t) => {
  for (const env of [
    { SHELLOPTS: 'xtrace' },
    { SHELLOPTS: 'errexit' },
    { BASHOPTS: 'extglob' },
  ]) {
    const box = sandbox(t, INTAKE);
    box.set('out-verify', `SECRET_TOKEN=${SECRET}\n`);
    box.set('exit-verify', 1);
    // xtrace は標準エラーへ追跡を書くので、標準エラーは検証の出力が漏れていないことだけを確かめる。
    const result = spawnSync(bash, [guard, 'verify'], {
      cwd: box.repo,
      env: box.env(env),
      input: '',
      encoding: 'utf8',
    });
    assert.equal(result.status, 3, JSON.stringify(env));
    assert.equal(result.stdout, STARTUP_REFUSED);
    assert.ok(!result.stderr.includes(SECRET));
    assert.deepEqual(box.calls(), []);
  }
});

test('環境上書き: 実際の git でも GIT_DIR・GIT_CONFIG_COUNT で別のリポジトリの origin に見せかけられない', (t) => {
  const realGit = (process.env.PATH ?? '')
    .split(path.delimiter)
    .map((dir) => path.join(dir, 'git'))
    .find((file) => fs.existsSync(file));
  assert.ok(realGit, '実際の git が必要です');
  const box = sandbox(t, null);
  fs.rmSync(path.join(box.bin, 'git'));
  fs.symlinkSync(realGit, path.join(box.bin, 'git'));
  const git = (cwd, ...args) => {
    const result = spawnSync(realGit, args, {
      cwd,
      env: { HOME: box.home, PATH: process.env.PATH },
      encoding: 'utf8',
    });
    assert.equal(result.status, 0, result.stderr);
  };
  // 作業場所は対応表に無いリポジトリ。別の場所に、origin が Intake のリポジトリを置く。
  git(box.repo, 'init', '-q');
  git(
    box.repo,
    'remote',
    'add',
    'origin',
    'gitea-nas:mofua-suki/other-project.git',
  );
  const decoy = path.join(box.root, 'decoy');
  fs.mkdirSync(decoy);
  git(decoy, 'init', '-q');
  git(decoy, 'remote', 'add', 'origin', INTAKE);
  fs.mkdirSync(path.join(decoy, 'scripts'));
  fs.writeFileSync(
    path.join(decoy, 'scripts/verify.sh'),
    `printf 'decoy\\n' >> '${box.state}/calls'\n`,
  );
  const globalConfig = path.join(box.root, 'gitconfig');
  fs.writeFileSync(globalConfig, `[remote "origin"]\n\turl = ${INTAKE}\n`);
  const refused =
    '必須検証（段1）: 拒否。必須検証の対応表に無いリポジトリのため、検証済みにできない。\n';
  const attempts = [
    {},
    { GIT_DIR: path.join(decoy, '.git') },
    { GIT_DIR: path.join(decoy, '.git'), GIT_WORK_TREE: decoy },
    {
      GIT_CONFIG_COUNT: '1',
      GIT_CONFIG_KEY_0: 'remote.origin.url',
      GIT_CONFIG_VALUE_0: INTAKE,
    },
    { GIT_CONFIG_PARAMETERS: `'remote.origin.url=${INTAKE}'` },
    { GIT_CONFIG_GLOBAL: globalConfig },
    { GIT_CONFIG_SYSTEM: globalConfig },
  ];
  for (const env of attempts) {
    const result = box.verify([], { env });
    assert.equal(result.status, 2, JSON.stringify(env));
    assert.equal(result.stdout, refused);
    assert.deepEqual(box.calls(), []);
  }
});

test('環境上書き: GO・GOFMT で成功する偽の道具を指しても、固定の道具の失敗で不合格になる', (t) => {
  const box = sandbox(t, WORKER);
  const evil = path.join(box.root, 'evil');
  box.tool('go', 'printf \'evil\\n\' >> "$S/evil-ran"', evil);
  box.tool('gofmt', 'printf \'evil\\n\' >> "$S/evil-ran"', evil);
  box.set('exit-go-test', 1);
  box.set('out-gofmt', 'a.go\n');
  const result = box.verify([], {
    env: { GO: path.join(evil, 'go'), GOFMT: path.join(evil, 'gofmt') },
  });
  assert.equal(result.status, 1);
  assert.ok(!fs.existsSync(path.join(box.state, 'evil-ran')));
  assert.deepEqual(box.calls(), [
    'go test ./...',
    'go vet ./...',
    'gofmt -l .',
  ]);
});

test('例外なし: 変更と無関係な失敗も、修正前からある失敗も不合格になる', (t) => {
  const unrelated = sandbox(t, INTAKE);
  unrelated.set(
    'out-verify',
    'stage=go-test\n--- FAIL: TestUnrelatedLegacyArea (0.01s)\nFAIL\n',
  );
  unrelated.set('exit-verify', 1);
  let result = unrelated.verify();
  assert.equal(result.status, 1);
  assert.deepEqual(shownNames(result.stdout), ['- TestUnrelatedLegacyArea']);
  assert.match(
    result.stdout,
    /変更と無関係な失敗・修正前からの失敗も免除しない。\n$/,
  );

  // 修正前から落ちているという申告（環境値・標準入力）を付けても結果は変わらない。
  const existing = sandbox(t, WORKER);
  existing.set(
    'out-go-test',
    '--- FAIL: TestKnownBrokenBeforeChange (0.00s)\nFAIL\texample/pkg\t0.1s\n',
  );
  existing.set('exit-go-test', 1);
  result = existing.verify([], {
    input: '{"preexisting":["TestKnownBrokenBeforeChange"]}',
    env: {
      GUARD_KNOWN_FAILURES: 'TestKnownBrokenBeforeChange',
      GUARD_BASELINE_FAILED: '1',
    },
  });
  assert.equal(result.status, 1);
  assert.deepEqual(shownNames(result.stdout), [
    '- TestKnownBrokenBeforeChange',
  ]);
});

test('失敗情報: 失敗テスト名は重複を除いて最大10件で、秘密やログの本文を出さない', (t) => {
  const box = sandbox(t, INTAKE);
  const lines = [
    'stage=go-test start',
    `接続文字列 postgres://user:${SECRET}@db/issue_desk_test を使用`,
    `SECRET_TOKEN=${SECRET}`,
    'panic: 未検査のログ本文',
  ];
  for (let i = 1; i <= 13; i += 1)
    lines.push(
      `    --- FAIL: TestGo${i}/sub_case (0.00s)`,
      `        detail_${i}: got ${SECRET}`,
    );
  lines.push(
    '--- FAIL: TestGo1/sub_case (0.00s)',
    'not ok 3 - 相談の一覧を読む',
    'not ok 4 - 省略した試験 # SKIP 理由',
    '✖ 窓口の振り分け (12.5ms)',
    'FAIL\texample/pkg\t0.2s',
  );
  box.set('out-verify', lines.join('\n') + '\n');
  box.set('exit-verify', 1);
  const result = box.verify();
  assert.equal(result.status, 1);
  assert.deepEqual(
    shownNames(result.stdout),
    Array.from({ length: 10 }, (_, i) => `- TestGo${i + 1}/sub_case`),
  );
  assert.equal(result.stdout.split('\n').length, 15);
  assert.ok(
    result.stdout.startsWith(
      '失敗した検証: sh scripts/verify.sh（終了コード 1）\n失敗テスト名（最大10件）:\n',
    ),
  );
  assert.ok(result.stdout.includes('ほか 5 件は表示しない\n'));
  for (const leaked of [
    SECRET,
    'SECRET_TOKEN',
    'postgres://',
    'panic',
    'detail_',
    '未検査',
    '省略した試験',
  ])
    assert.ok(!result.stdout.includes(leaked), leaked);
});

test('失敗情報: node --test の形式（not ok・✖）から日本語のテスト名を取り出す', (t) => {
  const box = sandbox(t, INTAKE);
  box.set(
    'out-verify',
    'ok 1 - 通った試験\nnot ok 2 - 相談の一覧を読む\n  ---\n  duration_ms: 1.2\n✖ 窓口の振り分け (12.5ms)\n\n✖ failing tests:\n\n✖ 窓口の振り分け (12.5ms)\n',
  );
  box.set('exit-verify', 1);
  const result = box.verify();
  assert.equal(result.status, 1);
  assert.deepEqual(shownNames(result.stdout), [
    '- 相談の一覧を読む',
    '- 窓口の振り分け',
  ]);
});

test('失敗情報: 安全に名前を取得できないときは失敗した固定の検証コマンドと固定の理由だけを出す', (t) => {
  const fixed =
    '失敗テスト名: 取得できない（検証の出力から安全に表示できる失敗テスト名を取り出せなかった。上の検証を実行して確かめる）\n';
  const noNames = sandbox(t, WORKER);
  noNames.set(
    'out-go-vet',
    `# example/pkg\nvet: internal/x.go:3:1: value ${SECRET} unused\n`,
  );
  noNames.set('exit-go-vet', 2);
  let result = noNames.verify();
  assert.equal(result.status, 1);
  assert.equal(
    result.stdout,
    `失敗した検証: go vet ./...（終了コード 2）\n${fixed}必須検証（段1）: 不合格 repo=mofua-suki/issue-desk-worker。変更と無関係な失敗・修正前からの失敗も免除しない。\n`,
  );

  const unsafe = sandbox(t, INTAKE);
  unsafe.set(
    'out-verify',
    [
      `--- FAIL: Test/${SECRET} (0.00s)`,
      '--- FAIL: TestKey=abc (0.00s)',
      'not ok 1 - $(touch pwned) `touch pwned`',
      'not ok 2 - password: "hunter2"',
      `not ok 3 - ${'長'.repeat(201)}`,
      '✖ a\u001b[31mb',
    ].join('\n') + '\n',
  );
  unsafe.set('exit-verify', 1);
  result = unsafe.verify();
  assert.equal(result.status, 1);
  assert.equal(
    result.stdout,
    `失敗した検証: sh scripts/verify.sh（終了コード 1）\n${fixed}安全に表示できない名前 6 件は表示しない\n必須検証（段1）: 不合格 repo=mofua-suki/issue-desk-intake。変更と無関係な失敗・修正前からの失敗も免除しない。\n`,
  );
});

test('コード評価なし: origin・検証の出力・標準入力に埋めたコマンドは実行されない', (t) => {
  const payload = (dir) =>
    `$(touch ${dir}/pwned) \`touch ${dir}/pwned\`; touch ${dir}/pwned`;
  const byOrigin = sandbox(t, null);
  byOrigin.set('origin', `gitea-nas:mofua-suki/${payload(byOrigin.state)}\n`);
  assert.equal(
    byOrigin.verify([], { input: payload(byOrigin.state) }).status,
    3,
  );
  assert.ok(!fs.existsSync(path.join(byOrigin.state, 'pwned')));

  for (const origin of [INTAKE, WORKER]) {
    const box = sandbox(t, origin);
    const text = `--- FAIL: ${payload(box.state)}\nnot ok 1 - ${payload(box.state)}\n✖ ${payload(box.state)}\n${payload(box.state)}\n`;
    for (const name of ['verify', 'go-test', 'go-vet', 'gofmt']) {
      box.set(`out-${name}`, text);
      box.set(`exit-${name}`, 1);
    }
    const result = box.verify([], { input: payload(box.state) });
    assert.equal(result.status, 1);
    assert.ok(!result.stdout.includes('pwned'));
    assert.ok(!fs.existsSync(path.join(box.state, 'pwned')));
  }
});

test('資料どおりの呼び方: docs/chat-operations.md に書いた入口のコマンドをそのまま実行できる', (t) => {
  const doc = fs.readFileSync(
    path.join(repoRoot, 'docs/chat-operations.md'),
    'utf8',
  );
  const command = 'bash ~/.claude/hooks/guard-gitea.sh verify';
  assert.ok(doc.split('\n').includes(command));
  const box = sandbox(t, INTAKE);
  const hooks = path.join(box.home, '.claude/hooks');
  fs.mkdirSync(hooks, { recursive: true });
  fs.copyFileSync(guard, path.join(hooks, 'guard-gitea.sh'));
  const run = () =>
    spawnSync(bash, ['-c', command], {
      cwd: box.repo,
      env: box.env(),
      encoding: 'utf8',
    });
  let result = run();
  assert.equal(result.stderr, '');
  assert.equal(result.status, 0);
  assert.equal(result.stdout, passed());
  box.set('exit-verify', 1);
  result = run();
  assert.equal(result.status, 1);
});

test('中間結果: 合格は保存先に記録を1つ作るだけで送信許可を作らず、フックの判定を変えない', (t) => {
  const box = sandbox(t, INTAKE);
  const push = {
    hook_event_name: 'PreToolUse',
    tool_name: 'Bash',
    cwd: box.repo,
    tool_input: { command: 'git push origin HEAD' },
  };
  const watched = [box.repo, box.tmp, box.bin];
  const before = watched.map(tree);
  const hookBefore = hook(push, box.env());
  const result = box.verify();
  assert.equal(result.status, 0);
  assert.ok(!/permissionDecision|allow/.test(result.stdout));
  // 作業場所・一時置き場・道具の置き場には何も作らず、HOME の保存先に記録を1つだけ残す。
  assert.deepEqual(watched.map(tree), before);
  const leaf = path.join(
    '.local/state/guard-gitea/verify',
    box.repo.replaceAll('/', '%2F'),
  );
  assert.deepEqual(tree(box.home), [
    '.local',
    '.local/state',
    '.local/state/guard-gitea',
    '.local/state/guard-gitea/verify',
    leaf,
    path.join(leaf, 'record'),
  ]);
  const hookAfter = hook(push, box.env());
  assert.equal(hookAfter, hookBefore);
  assert.ok(!hookAfter.includes('allow'));
  assert.deepEqual(box.calls(), ['verify']);
});

test('既存フック: 正当な受付窓口の呼び出しと通常の操作は止めない', () => {
  const allowed = [
    {
      tool_name: 'Bash',
      tool_input: {
        command:
          '~/.issue-desk/desk create-issue.ts --file ~/.issue-desk/requests/a.md',
      },
    },
    {
      tool_name: 'Bash',
      tool_input: {
        command: '$HOME/.issue-desk/desk desk-get.ts GET /api/issue?desk=6108',
      },
    },
    { tool_name: 'Bash', tool_input: { command: 'ls -la' } },
    {
      tool_name: 'Bash',
      tool_input: { command: 'bash ~/.claude/hooks/guard-gitea.sh verify' },
    },
    {
      tool_name: 'Write',
      tool_input: {
        file_path: '/home/u/.issue-desk/requests/a.md',
        content: '依頼文',
      },
    },
  ];
  for (const input of allowed)
    assert.equal(hook(input), '', JSON.stringify(input));
});

// ---- 既存フックの回帰（拒否と許可が段1の前と変わらないこと） ----
// PreToolUse の JSON を標準入力で渡し、標準出力の判定を読む。何も出さなければ許可。
const ISSUE_API =
  'http://nas.example:3000/api/v1/repos/mofua-suki/issue-desk-intake/issues';
const TOKEN_FILE = '~/.config/issue-desk/gitea-token';
const ISSUE_REASON =
  /^GiteaのIssue APIは直接呼ばない。.*拒否を別の書き方で回り込まない。$/;
const TOKEN_REASON =
  /^Giteaのトークンは各プロジェクトのPR用ヘルパー（create・merge・list）からだけ使う。.*拒否を別の書き方で回り込まない。$/;
const OUTSIDE = '/home/u/work/other-project';

function pre(tool_name, tool_input, cwd = OUTSIDE) {
  return { hook_event_name: 'PreToolUse', tool_name, cwd, tool_input };
}

// 拒否の応答を読み、理由を返す。拒否でなければ失敗する。
function denyReason(input) {
  const stdout = hook(input);
  assert.notEqual(
    stdout,
    '',
    `拒否されるはずの入力が通った: ${JSON.stringify(input)}`,
  );
  const decision = JSON.parse(stdout);
  assert.deepEqual(Object.keys(decision), ['hookSpecificOutput']);
  assert.equal(decision.hookSpecificOutput.hookEventName, 'PreToolUse');
  assert.equal(decision.hookSpecificOutput.permissionDecision, 'deny');
  return decision.hookSpecificOutput.permissionDecisionReason;
}

test('既存フック回帰 (c): GiteaのIssue APIを直接呼ぶ入力は拒否する', () => {
  const denied = [
    pre('Bash', { command: `curl -s ${ISSUE_API}` }),
    pre('Bash', {
      command: `curl -s -X POST -d '{"title":"x"}' ${ISSUE_API}/12/comments`,
    }),
    pre('Bash', {
      command: 'tea api repos/mofua-suki/issue-desk-worker/issues/3',
    }),
    pre('WebFetch', { url: ISSUE_API, prompt: '一覧を読む' }),
    pre('Write', {
      file_path: `${OUTSIDE}/call.sh`,
      content: `curl ${ISSUE_API}\n`,
    }),
    pre('Edit', {
      file_path: `${OUTSIDE}/call.sh`,
      old_string: 'x',
      new_string: `curl ${ISSUE_API}`,
    }),
  ];
  for (const input of denied)
    assert.match(denyReason(input), ISSUE_REASON, JSON.stringify(input));
});

test('既存フック回帰 (d): Giteaのトークンを直接扱う入力は拒否する', () => {
  const denied = [
    pre('Bash', { command: `cat ${TOKEN_FILE}` }),
    pre('Read', { file_path: '/home/u/.config/issue-desk/gitea-token' }),
    pre('Bash', { command: 'python3 -c "import gitea_pr; print(1)"' }),
    pre('Bash', { command: 'python3 -c "from gitea_pr import create"' }),
    pre('Bash', { command: "python3 -c 'print(gitea_pr.load_token())'" }),
    pre('Bash', { command: "python3 -c 'gitea_pr.call(1)'" }),
    pre('Bash', { command: "python3 -c 'print(gitea_pr.DECRYPT)'" }),
    pre('Bash', { command: 'powershell -Command "ConvertTo-SecureString $x"' }),
    pre('Write', {
      file_path: `${OUTSIDE}/read.sh`,
      content: `cat ${TOKEN_FILE}\n`,
    }),
  ];
  for (const input of denied)
    assert.match(denyReason(input), TOKEN_REASON, JSON.stringify(input));
});

test('既存フック回帰 (a): 所有ファイルへの編集は、拒否対象の語を含んでも通す', () => {
  const content = `curl ${ISSUE_API}\ncat ${TOKEN_FILE}\nimport gitea_pr\n`;
  const owned = [
    'scripts/gitea_pr.py',
    'tests/test_gitea_pr_script.py',
    'tests/test_guard_gitea.py',
    'scripts/guard-gitea.sh',
    'test/guard-gitea.test.ts',
    'skills/pr/SKILL.md',
    'skills/issue-desk-request/SKILL.md',
    '.claude/hooks/guard-gitea.sh',
  ];
  for (const file of owned) {
    assert.equal(
      hook(pre('Write', { file_path: `${OUTSIDE}/${file}`, content })),
      '',
      file,
    );
    assert.equal(
      hook(
        pre('Edit', {
          file_path: `${OUTSIDE}/${file}`,
          old_string: 'x',
          new_string: content,
        }),
      ),
      '',
      file,
    );
  }
  // 所有ファイルでない名前は例外に当たらない。
  for (const file of [
    'scripts/guard-other.sh',
    'skills/other/SKILL.md',
    'test/gitea.test.mjs',
  ]) {
    assert.match(
      denyReason(pre('Write', { file_path: `${OUTSIDE}/${file}`, content })),
      ISSUE_REASON,
      file,
    );
  }
});

test('既存フック回帰 (b): issue-deskのフォルダーでの編集は通し、同じ場所のコマンド実行は引き続き調べる', () => {
  const content = `curl ${ISSUE_API}\ncat ${TOKEN_FILE}\n`;
  const folders = [
    '/home/u/.issue-desk',
    '/home/u/work/issue-desk',
    '/home/u/work/issue-desk-0a1b2c',
    '/home/u/work/issue-desk__job',
  ];
  for (const folder of folders) {
    for (const tool of [
      'Write',
      'Edit',
      'MultiEdit',
      'NotebookEdit',
      'apply_patch',
    ]) {
      assert.equal(
        hook(pre(tool, { file_path: `${folder}/docs/a.md`, content })),
        '',
        `${tool} ${folder}`,
      );
    }
    // 作業場所が issue-desk なら、編集先の書き方によらず通す。
    assert.equal(
      hook(pre('Write', { file_path: 'docs/a.md', content }, folder)),
      '',
      folder,
    );
    // 編集の道具でなければ例外に当たらない。
    assert.match(
      denyReason(pre('Bash', { command: `curl ${ISSUE_API}` }, folder)),
      ISSUE_REASON,
      folder,
    );
    assert.match(
      denyReason(pre('Bash', { command: `cat ${TOKEN_FILE}` }, folder)),
      TOKEN_REASON,
      folder,
    );
  }
  // issue-desk でないフォルダーでの編集は例外に当たらない（名前が issue-desk で始まるだけの別のフォルダーを含む）。
  assert.match(
    denyReason(pre('Write', { file_path: `${OUTSIDE}/docs/a.md`, content })),
    ISSUE_REASON,
  );
  for (const folder of [
    '/home/u/rc/issue-desk-intake',
    '/home/u/rc/issue-desk-worker',
    '/home/u/work/issue-desks',
  ]) {
    assert.match(
      denyReason(
        pre('Write', { file_path: `${folder}/docs/a.md`, content }, folder),
      ),
      ISSUE_REASON,
      folder,
    );
  }
  assert.match(
    denyReason(
      pre('Write', {
        file_path: '/home/u/work/issue-tracker/a.md',
        content: `cat ${TOKEN_FILE}`,
      }),
    ),
    TOKEN_REASON,
  );
});

test('既存フック回帰 窓口: deskの1コマンドだけの呼び出しは、拒否対象の語を含んでも通す', () => {
  const words = `repos/mofua-suki/issue-desk-intake/issues gitea-token`;
  const allowed = [
    `~/.issue-desk/desk create-issue.ts --title "${words}"`,
    `$HOME/.issue-desk/desk reply-issue.ts --desk 6108 --message "${words}"`,
    `\${HOME}/.issue-desk/desk desk-get.ts GET /api/issue --note ${words}`,
    `/home/user/.issue-desk/desk follow-issues.ts --desk 6108 --note ${words}`,
    `/Users/someone/.issue-desk/desk list-issues.ts --duplicates --project owner/repo --query ${words}`,
    `/root/.issue-desk/desk list-issues.ts --duplicates --project owner/repo --query ${words} --state $STATE_DIR/a.json`,
  ];
  for (const command of allowed)
    assert.equal(hook(pre('Bash', { command })), '', command);

  // 窓口の1コマンドでないもの（連結・置換・リダイレクト・改行・別の実行ファイル・別の道具）は例外に当たらない。
  const desk = '~/.issue-desk/desk desk-get.ts GET /api/issue?desk=6108';
  const denied = [
    `${desk}; curl ${ISSUE_API}`,
    `${desk} && curl ${ISSUE_API}`,
    `${desk} | curl -d @- ${ISSUE_API}`,
    `${desk} --note $(curl ${ISSUE_API})`,
    `${desk} --note \`curl ${ISSUE_API}\``,
    `${desk} > /tmp/repos/a/b/issues`,
    `${desk} < /tmp/repos/a/b/issues`,
    `${desk}\ncurl ${ISSUE_API}`,
    `/tmp/.issue-desk/desk desk-get.ts GET /api/issue --note ${words}`,
    `~/.issue-desk/desk run.sh --note ${words}`,
    // 窓口は .ts の入口だけを動かすので、改名前の .mjs の名前は例外に当たらない。
    `~/.issue-desk/desk create-issue.mjs --title "${words}"`,
    `~/.issue-desk/desk --note ${words}`,
    `echo ok; ${desk} --note ${words}`,
  ];
  for (const command of denied)
    assert.match(denyReason(pre('Bash', { command })), ISSUE_REASON, command);
  assert.match(
    denyReason(pre('Bash', { command: `${desk}; cat ${TOKEN_FILE}` })),
    TOKEN_REASON,
  );
  // 窓口の例外は Bash だけ。
  assert.match(
    denyReason(pre('WebFetch', { command: `${desk} --note ${words}` })),
    ISSUE_REASON,
  );
});

test('既存フック回帰: 検査に当たらない入力は何も出さずに通す', () => {
  const allowed = [
    pre('Bash', { command: 'git status --short' }),
    pre('Bash', { command: 'git push origin HEAD' }),
    pre('Bash', { command: 'grep -rn issues docs' }),
    pre('Bash', {
      command:
        'curl -s http://nas.example:3000/api/v1/repos/mofua-suki/issue-desk-intake/pulls',
    }),
    pre('Bash', { command: 'python3 scripts/gitea_pr.py list' }),
    pre('Bash', { command: 'echo token' }),
    pre('Read', { file_path: `${OUTSIDE}/README.md` }),
    pre('Write', {
      file_path: `${OUTSIDE}/notes.md`,
      content: 'Gitea の issues を受付経由で読む\n',
    }),
    pre('WebFetch', { url: 'https://example.com/docs/issues', prompt: '読む' }),
    {},
  ];
  for (const input of allowed)
    assert.equal(hook(input), '', JSON.stringify(input));
});

// ---- 必須検証の記録・ロックの操作基盤（段2前半） ----
// 入口 verify-store で、保存先・ロック・記録の操作を隔離した HOME の下で動かす。
const STORE = '必須検証の記録（段2前半）';
const INTAKE_REPO = 'mofua-suki/issue-desk-intake';
const storeOk = (repo = INTAKE_REPO) =>
  `${STORE}: 点検に成功 repo=${repo}。保存先・記録・ロックを安全に操作できる。この点検と保存先の記録は検証結果を有効にせず、送信の許可にもならない。\n`;
const storeFailed = (reason, repo = INTAKE_REPO) =>
  `${STORE}: 実行不能 repo=${repo}。${reason}。保存先・記録・ロックを安全に扱えないときは成功として扱わない。\n`;
const LOCK_BUSY =
  'ロックを取得できない（ほかの検証が記録を操作しているか、中断で残ったロックがある）';
const DIR_LINK = '保存先がシンボリックリンクになっている';
const DIR_MODE = '保存先の権限が所有者だけ（700）になっていない';
const DIR_ACCESS = '保存先を読み書きできない';
const FILE_LINK = '記録がシンボリックリンクになっている';
const FILE_MODE = '記録の権限が所有者だけ（600）になっていない';
const BAD_BODY = '記録の中身が決まった形でない';
const BAD_LINES = '記録を読めないか、行の数が決まった形でない';
// 管理者で実行すると権限に関係なく読み書きできるため、読取・書込の不能は権限の不備として検出される。
const IS_ROOT = process.getuid?.() === 0;
// 試験が置く記録の識別子（実行ごとの識別子と同じ形の固定値）。
const RUN_ID = '0123456789abcdef0123456789abcdef';
// 試験が置く記録の同一性の要約（要約と同じ形の固定値）。
const IDENTITY = '3'.repeat(40);
// 版3の記録。identity・results を省くと、成功は「同一性あり・結果はすべて0」、ほかは「-」にする。
const record = (
  state = 'passed',
  generation: number | string = 7,
  repo = INTAKE_REPO,
  run = RUN_ID,
  identity = state === 'passed' ? IDENTITY : '-',
  results = state === 'passed' ? 'intake-verify:0' : '-',
) =>
  `guard-gitea-verify 3\nstate=${state}\ngeneration=${generation}\nrepo=${repo}\nrun=${run}\nidentity=${identity}\nresults=${results}\n`;
// 識別子の行が無い旧い版の記録（版1）と、同一性の行が無い旧い版の記録（版2）。
const recordV1 = (
  state = 'passed',
  generation: number | string = 7,
  repo = INTAKE_REPO,
) =>
  `guard-gitea-verify 1\nstate=${state}\ngeneration=${generation}\nrepo=${repo}\n`;
const recordV2 = (
  state = 'passed',
  generation: number | string = 7,
  repo = INTAKE_REPO,
  run = RUN_ID,
) =>
  `guard-gitea-verify 2\nstate=${state}\ngeneration=${generation}\nrepo=${repo}\nrun=${run}\n`;
// 記録の識別子（run= の行の値。決まった形でなければ null）。
const runOf = (store) =>
  /^run=([0-9a-f]{32})$/m.exec(fs.readFileSync(store.record, 'utf8'))?.[1] ??
  null;
// 記録の同一性の要約と各検証の結果（そのままの値）。
const identityOf = (store) =>
  /^identity=(.*)$/m.exec(fs.readFileSync(store.record, 'utf8'))?.[1] ?? null;
const resultsOf = (store) =>
  /^results=(.*)$/m.exec(fs.readFileSync(store.record, 'utf8'))?.[1] ?? null;
// 記録を読み、実行ごとに変わる識別子と、同一性の要約・各検証の結果を record() の既定値に置き換えて返す
// （状態・世代・リポジトリを record() と比べられる形にする。同一性と結果そのものは identityOf・resultsOf で確かめる）。
const readRecord = (store) => {
  const text = fs.readFileSync(store.record, 'utf8');
  const isPassed = /^state=passed$/m.test(text);
  return text
    .replace(/^run=[0-9a-f]{32}$/m, `run=${RUN_ID}`)
    .replace(
      /^identity=(-|[0-9a-f]{40})$/m,
      `identity=${isPassed ? IDENTITY : '-'}`,
    )
    .replace(
      /^results=[a-z0-9:,x-]+$/m,
      `results=${isPassed ? 'intake-verify:0' : '-'}`,
    );
};

// 保存先のパス（作業場所の最上位のパスの % と / を置き換えた名前）。
function storePaths(box, root = box.repo) {
  const base = path.join(box.home, '.local/state/guard-gitea');
  const leaf = path.join(
    base,
    'verify',
    root.replaceAll('%', '%25').replaceAll('/', '%2F'),
  );
  return {
    base,
    verify: path.join(base, 'verify'),
    leaf,
    record: path.join(leaf, 'record'),
    probe: path.join(leaf, 'probe'),
    lock: path.join(leaf, 'lock'),
  };
}

// 正当な保存先（所有者だけの権限）を先に作る。content があれば正当な権限の記録も置く。
function prepareStore(box: Box, content?: string) {
  const store = storePaths(box);
  fs.mkdirSync(store.leaf, { recursive: true });
  for (const dir of [store.base, store.verify, store.leaf])
    fs.chmodSync(dir, 0o700);
  if (content !== undefined)
    fs.writeFileSync(store.record, content, { mode: 0o600 });
  return store;
}

function assertStoreFails(box: Box, reason: string, message?: string) {
  const result = box.store();
  assert.equal(result.stdout, storeFailed(reason), message);
  assert.equal(result.status, 3, message);
  assert.deepEqual(box.calls(), []);
  return result;
}

const mode = (file) => fs.statSync(file).mode & 0o7777;

test('記録の基盤: 保存先が無ければ所有者だけの権限で作り、点検に成功して点検用の記録とロックを残さない', (t) => {
  for (const [origin, repo] of [
    [INTAKE, INTAKE_REPO],
    [WORKER, 'mofua-suki/issue-desk-worker'],
  ]) {
    const box = sandbox(t, origin);
    const store = storePaths(box);
    for (let i = 0; i < 2; i += 1) {
      const result = box.store();
      assert.equal(result.stdout, storeOk(repo));
      assert.equal(result.status, 0);
      for (const dir of [store.base, store.verify, store.leaf])
        assert.equal(mode(dir), 0o700, dir);
      assert.deepEqual(tree(store.verify), [path.basename(store.leaf)]);
    }
    // 点検は検証を実行しない。
    assert.deepEqual(box.calls(), []);
  }
});

test('記録の基盤: 正当な所有・権限の記録があれば読めて点検に成功し、記録を書き換えない', (t) => {
  for (const content of [
    record('passed'),
    record('failed', 0),
    record('invalid', '123456789012345678'),
    record('running', 1, 'mofua-suki/issue-desk-worker'),
  ]) {
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, content);
    const result = box.store();
    assert.equal(result.stdout, storeOk(), content);
    assert.equal(result.status, 0);
    assert.equal(fs.readFileSync(store.record, 'utf8'), content);
    assert.equal(mode(store.record), 0o600);
    assert.deepEqual(tree(store.leaf), ['record']);
  }
});

test('記録の基盤: 作業場所ごとに別の保存先を使い、パスの記号はそのまま名前の一部として扱う', (t) => {
  const box = sandbox(t, INTAKE);
  const other = path.join(
    box.root,
    're po%2F 100% $(touch pwned) `touch pwned`;touch pwned',
  );
  fs.mkdirSync(other);
  assert.equal(box.store().status, 0);
  box.set('toplevel', other + '\n');
  const result = box.store([], { cwd: other });
  assert.equal(result.stdout, storeOk());
  assert.equal(result.status, 0);
  assert.deepEqual(
    tree(storePaths(box).verify),
    [
      path.basename(storePaths(box).leaf),
      path.basename(storePaths(box, other).leaf),
    ].sort(),
  );
  assert.deepEqual(
    tree(box.root).filter((file) => path.basename(file) === 'pwned'),
    [],
  );
});

// 隔離した PATH には id を置いていないので、ほかのテストの保存先は必ず HOME の下になる。基点の選択はここで id を置いて確かめる。
const realId = ['/usr/bin/id', '/bin/id'].find((file) => fs.existsSync(file));

// 実機の /run/user/<実行者の番号> が基点として使える状態か（リンクでないディレクトリ・実行者の所有・権限ちょうど700）。
function usableRunBase() {
  const uid = process.getuid?.();
  if (uid === undefined) return null;
  const dir = `/run/user/${uid}`;
  try {
    const stat = fs.lstatSync(dir);
    fs.accessSync(
      dir,
      fs.constants.R_OK | fs.constants.W_OK | fs.constants.X_OK,
    );
    return stat.isDirectory() &&
      stat.uid === uid &&
      (stat.mode & 0o7777) === 0o700 &&
      fs.realpathSync(dir) === dir
      ? dir
      : null;
  } catch {
    return null;
  }
}

test('記録の基盤 基点: id が無い・失敗する・番号を返さない・その番号の /run/user が無いときは HOME の下を保存先にする', (t) => {
  const absent = '4294967294999';
  assert.equal(fs.existsSync(`/run/user/${absent}`), false);
  for (const [label, body] of [
    ['id なし', null],
    ['失敗', 'echo 0; exit 1'],
    ['番号でない', 'echo "../../tmp"'],
    ['空', 'exit 0'],
    ['候補なし', `echo ${absent}`],
  ]) {
    const box = sandbox(t, INTAKE);
    if (body !== null) box.tool('id', `[ "$*" = "-u" ] || exit 2\n${body}`);
    const store = storePaths(box);
    const result = box.store([], {
      env: { XDG_RUNTIME_DIR: box.tmp, XDG_STATE_HOME: box.tmp },
    });
    assert.equal(result.stdout, storeOk(), label);
    assert.equal(result.status, 0, label);
    assert.deepEqual(tree(store.verify), [path.basename(store.leaf)], label);
    assert.deepEqual(tree(box.tmp), [], label);
  }
});

test('記録の基盤 基点: /run/user/<実行者の番号> が所有者だけの権限のディレクトリならそこを保存先にし、環境値の場所は使わない', (t) => {
  assert.ok(realId, 'id が必要です');
  const box = sandbox(t, INTAKE);
  box.tool('id', `exec '${realId}' "$@"`);
  const run = usableRunBase();
  const home = storePaths(box);
  const key = path.basename(home.leaf);
  // 保存先の名前は一時ディレクトリごとに違うので、同じ基点を使うほかの実行と衝突しない。
  assert.ok(key.includes(path.basename(box.root)));
  const base = run ? path.join(run, 'guard-gitea') : home.base;
  const leaf = path.join(base, 'verify', key);
  const existed = [base, path.join(base, 'verify')].map((dir) =>
    fs.existsSync(dir),
  );
  assert.equal(fs.existsSync(leaf), false);
  // 実機の基点に前からある保存先が不正なら、製品の不具合と区別できるよう理由を示して失敗する（黙って通さない）。
  if (run) {
    for (const dir of [base, path.join(base, 'verify')].filter(
      (_, index) => existed[index],
    )) {
      const stat = fs.lstatSync(dir);
      assert.ok(
        stat.isDirectory() &&
          stat.uid === process.getuid() &&
          (stat.mode & 0o7777) === 0o700,
        `この筐体に前からある ${dir} が、実行者所有・権限700のディレクトリでない。テスト対象の不具合ではなく、この保存先を直す必要がある`,
      );
    }
  }
  try {
    for (let i = 0; i < 2; i += 1) {
      const result = box.store([], {
        env: { XDG_RUNTIME_DIR: box.tmp, XDG_STATE_HOME: box.tmp },
      });
      assert.equal(result.stdout, storeOk());
      assert.equal(result.status, 0);
      for (const dir of [base, path.join(base, 'verify'), leaf])
        assert.equal(mode(dir), 0o700, dir);
      assert.deepEqual(tree(leaf), []);
    }
    assert.deepEqual(tree(box.tmp), []);
    // /run/user の基点を使ったときは、HOME の下に何も作らない。
    if (run) assert.deepEqual(tree(box.home), []);
    // 基点が変わっても、保存先の検査は同じ（権限を広げた保存先は失敗する）。
    fs.chmodSync(leaf, 0o750);
    assertStoreFails(box, DIR_MODE);
    assert.deepEqual(tree(leaf), []);
  } finally {
    // 実機の基点へ書いた分を消す。前からあったディレクトリは残し、このテストが作って空のものだけ消す。
    fs.rmSync(leaf, { recursive: true, force: true });
    if (run) {
      for (const [index, dir] of [
        [1, path.join(base, 'verify')],
        [0, base],
      ] as [number, string][]) {
        if (existed[index]) continue;
        try {
          fs.rmdirSync(dir);
        } catch (error) {
          if (!['ENOTEMPTY', 'EEXIST', 'ENOENT'].includes(error.code))
            // eslint-disable-next-line no-unsafe-finally -- 想定外の後片付け失敗は検証失敗として伝える。
            throw error;
        }
      }
    }
  }
  assert.equal(fs.existsSync(leaf), false);
});

test('記録の基盤: 引数・対応表に無いリポジトリ・起動環境の細工・originの不明では保存先に触れず、成功にしない', (t) => {
  const cases: any[][] = [
    [
      INTAKE,
      { args: ['--state', 'passed'] },
      2,
      `${STORE}: 拒否。引数は受け付けない（保存先・記録の中身・結果は指定できない）。作業場所で引数なしの verify-store を実行する。\n`,
    ],
    [INTAKE, { args: ['passed'] }, 2, null],
    [INTAKE, { args: [''] }, 2, null],
    [
      'gitea-nas:mofua-suki/other-project.git',
      {},
      2,
      `${STORE}: 拒否。必須検証の対応表に無いリポジトリのため、記録を扱わない。\n`,
    ],
    [
      null,
      {},
      3,
      `${STORE}: 実行不能。origin からリポジトリを特定できない。\n`,
    ],
    [INTAKE, { env: { BASH_ENV: '/nonexistent/rc' } }, 3, null],
    [INTAKE, { env: { 'BASH_FUNC_mkdir%%': '() { return 0; }' } }, 3, null],
    [
      INTAKE,
      { env: { 'BASH_FUNC_verify_file_safe%%': '() { return 0; }' } },
      3,
      null,
    ],
  ] as [
    string | null,
    EntryOptions & { args?: string[] },
    number,
    string | null,
  ][];
  for (const [origin, options, status, stdout] of cases) {
    const box = sandbox(t, origin);
    const result = box.store(options.args, options);
    assert.equal(result.status, status, JSON.stringify(options));
    if (stdout !== null) assert.equal(result.stdout, stdout);
    assert.ok(!result.stdout.includes('点検に成功'), result.stdout);
    assert.deepEqual(tree(box.home), []);
  }
});

test('記録の基盤 不正な保存先: 保存先やその途中がリンク・ファイルなら失敗し、リンク先に記録もロックも作らない', (t) => {
  const cases: any[][] = [
    ['base', DIR_LINK],
    ['verify', DIR_LINK],
    ['leaf', DIR_LINK],
    ['.local', '保存先の途中にリンクがある'],
    ['.local/state', '保存先の途中にリンクがある'],
  ];
  for (const [where, reason] of cases) {
    const box = sandbox(t, INTAKE);
    const store = storePaths(box);
    const elsewhere = path.join(box.root, 'elsewhere');
    fs.mkdirSync(elsewhere, { mode: 0o700 });
    const link = store[where] ?? path.join(box.home, where);
    fs.mkdirSync(path.dirname(link), { recursive: true, mode: 0o700 });
    fs.symlinkSync(elsewhere, link);
    assertStoreFails(box, reason, where);
    // 途中がリンクなら、リンク先には何も作らない。
    assert.deepEqual(tree(elsewhere), [], where);
  }
  for (const where of ['base', 'verify', 'leaf']) {
    const box = sandbox(t, INTAKE);
    const store = storePaths(box);
    fs.mkdirSync(path.dirname(store[where]), { recursive: true, mode: 0o700 });
    fs.writeFileSync(store[where], record(), { mode: 0o600 });
    assertStoreFails(box, '保存先がディレクトリでない', where);
  }
  const blocked = sandbox(t, INTAKE);
  fs.writeFileSync(path.join(blocked.home, '.local'), '');
  assertStoreFails(blocked, '保存先を作れない');
});

test('記録の基盤 権限不備: 保存先のディレクトリや記録をほかの利用者が読み書きできる権限なら失敗する', (t) => {
  for (const where of ['base', 'verify', 'leaf']) {
    for (const bits of [0o755, 0o750, 0o707, 0o1700]) {
      const box = sandbox(t, INTAKE);
      const store = prepareStore(box, record());
      fs.chmodSync(store[where], bits);
      assertStoreFails(box, DIR_MODE, `${where} ${bits.toString(8)}`);
    }
  }
  for (const bits of [0o644, 0o640, 0o660, 0o604, 0o700, 0o400]) {
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, record());
    fs.chmodSync(store.record, bits);
    assertStoreFails(box, FILE_MODE, bits.toString(8));
    assert.equal(fs.readFileSync(store.record, 'utf8'), record());
    assert.equal(mode(store.record), bits);
  }
});

test('記録の基盤 所有: 保存先や記録の所有者が実行者と違えば失敗する', (t) => {
  // ほかの利用者が所有するファイルは試験では作れないため、所有者の番号を返す ls を差し替えて再現する。
  const DIR_OWNER = '保存先の所有者が実行者と違う';
  for (const [where, reason] of [
    ['.local/state', '保存先の基点の所有者が実行者と違う'],
    ['base', DIR_OWNER],
    ['verify', DIR_OWNER],
    ['leaf', DIR_OWNER],
    ['record', '記録の所有者が実行者と違う'],
  ]) {
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, record());
    box.tool(
      'ls',
      [
        `out=$('${path.join(system, 'ls')}' "$@") || exit $?`,
        'for last in "$@"; do :; done',
        'if [ "$last" = "$(cat "$S/foreign")" ]; then',
        '  set -- $out',
        '  printf \'%s %s 4000000000 %s %s\\n\' "$1" "$2" "$4" "$5"',
        'else',
        '  printf \'%s\\n\' "$out"',
        'fi',
      ].join('\n'),
    );
    box.set('foreign', store[where] ?? path.join(box.home, where));
    assertStoreFails(box, reason, where);
    assert.equal(fs.readFileSync(store.record, 'utf8'), record());
    // 差し替えた番号を外せば同じ保存先で成功する（失敗の原因が所有者の番号だけであること）。
    box.set('foreign', '');
    assert.equal(box.store().status, 0, where);
  }
});

test('記録の基盤 リンク: 記録・点検用の記録・ロックがリンクなら失敗し、リンク先を読み書きしない', (t) => {
  for (const [name, reason] of [
    ['record', FILE_LINK],
    ['probe', FILE_LINK],
    ['lock', 'ロックがシンボリックリンクになっている'],
  ]) {
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box);
    const target = path.join(box.root, 'target');
    if (name === 'lock') fs.mkdirSync(target, { mode: 0o700 });
    else fs.writeFileSync(target, record(), { mode: 0o600 });
    fs.symlinkSync(target, store[name]);
    assertStoreFails(box, reason, name);
    assert.ok(fs.lstatSync(store[name]).isSymbolicLink());
    assert.deepEqual(tree(store.leaf), [name]);
    if (name === 'lock') assert.deepEqual(tree(target), []);
    else assert.equal(fs.readFileSync(target, 'utf8'), record());
  }
  // 壊れたリンク（指す先が無い）も「記録が無い」とは扱わない。
  const dangling = sandbox(t, INTAKE);
  fs.symlinkSync(
    path.join(dangling.root, 'missing'),
    prepareStore(dangling).record,
  );
  assertStoreFails(dangling, FILE_LINK);
  assert.ok(!fs.existsSync(path.join(dangling.root, 'missing')));

  const hard = sandbox(t, INTAKE);
  fs.linkSync(
    prepareStore(hard, record()).record,
    path.join(hard.root, 'second-name'),
  );
  assertStoreFails(hard, '記録にハードリンクがある');
});

test('記録の基盤 読取不能: 読めない記録・通常のファイルでない記録・決まった形でない記録は失敗する', (t) => {
  const unreadable = sandbox(t, INTAKE);
  fs.chmodSync(prepareStore(unreadable, record()).record, 0o000);
  assertStoreFails(unreadable, IS_ROOT ? FILE_MODE : '記録を読めない');

  const closed = sandbox(t, INTAKE);
  const closedStore = prepareStore(closed, record());
  fs.chmodSync(closedStore.leaf, 0o300);
  try {
    assertStoreFails(closed, IS_ROOT ? DIR_MODE : DIR_ACCESS);
  } finally {
    fs.chmodSync(closedStore.leaf, 0o700);
  }

  const directory = sandbox(t, INTAKE);
  fs.mkdirSync(prepareStore(directory).record, { mode: 0o700 });
  assertStoreFails(directory, '記録が通常のファイルでない');

  const malformed = [
    ['', BAD_LINES],
    ['guard-gitea-verify 1\nstate=passed\n', BAD_LINES],
    [record().trimEnd(), BAD_LINES],
    [record() + 'state=passed\n', BAD_LINES],
    [record() + '\n', BAD_LINES],
    ['{"state":"passed","generation":1,"repo":"a/b"}\n', BAD_LINES],
    // 版2は識別子の行が必須。識別子は32桁の16進数（小文字）だけ。
    ['guard-gitea-verify 2\nstate=passed\ngeneration=1\nrepo=a/b\n', BAD_LINES],
    [recordV1() + `run=${RUN_ID}\n`, BAD_LINES],
    [recordV2('passed', 1, 'a/b', RUN_ID.toUpperCase()), BAD_BODY],
    // 版3は同一性と結果の行が必須。旧い版にその行を足した物は通さない。
    [
      `guard-gitea-verify 3\nstate=passed\ngeneration=1\nrepo=a/b\nrun=${RUN_ID}\n`,
      BAD_LINES,
    ],
    [recordV2() + `identity=${IDENTITY}\nresults=intake-verify:0\n`, BAD_LINES],
    [record().replace('identity=', 'id='), BAD_BODY],
    [record().replace('results=', 'result='), BAD_BODY],
    // 同一性の要約は 40桁か64桁の16進数（小文字）か「-」だけ。成功は要約が必須。
    [record('passed', 1, 'a/b', RUN_ID, '-'), BAD_BODY],
    [record('passed', 1, 'a/b', RUN_ID, ''), BAD_BODY],
    [record('passed', 1, 'a/b', RUN_ID, IDENTITY.slice(1)), BAD_BODY],
    [
      record('passed', 1, 'a/b', RUN_ID, IDENTITY.replaceAll('3', 'A')),
      BAD_BODY,
    ],
    [record('failed', 1, 'a/b', RUN_ID, 'unknown'), BAD_BODY],
    // 各検証の結果は「名前:終了コード」のカンマ区切りか「-」だけ。成功はすべて 0 が必須。
    [record('passed', 1, 'a/b', RUN_ID, IDENTITY, '-'), BAD_BODY],
    [record('passed', 1, 'a/b', RUN_ID, IDENTITY, 'intake-verify:1'), BAD_BODY],
    [
      record(
        'passed',
        1,
        'a/b',
        RUN_ID,
        IDENTITY,
        'go-test:0,go-vet:x,gofmt:0',
      ),
      BAD_BODY,
    ],
    [record('passed', 1, 'a/b', RUN_ID, IDENTITY, 'passed'), BAD_BODY],
    [
      record('failed', 1, 'a/b', RUN_ID, IDENTITY, 'intake-verify:1;x'),
      BAD_BODY,
    ],
    [
      record('failed', 1, 'a/b', RUN_ID, IDENTITY, 'intake-verify:1234'),
      BAD_BODY,
    ],
    [
      record('failed', 1, 'a/b', RUN_ID, IDENTITY, `${'a'.repeat(99)}:1`),
      BAD_BODY,
    ],
    [record('passed', 1, 'a/b', ''), BAD_BODY],
    [record('passed', 1, 'a/b', RUN_ID.slice(1)), BAD_BODY],
    [record('passed', 1, 'a/b', RUN_ID.toUpperCase()), BAD_BODY],
    [record('passed', 1, 'a/b', `${RUN_ID}0`), BAD_BODY],
    [record().replace('run=', 'id='), BAD_BODY],
    ['guard-gitea-verify 3\nstate=passed\ngeneration=1\nrepo=a/b\n', BAD_LINES],
    ['guard-gitea-verify 4\nstate=passed\ngeneration=1\nrepo=a/b\n', BAD_BODY],
    ['state=passed\nguard-gitea-verify 1\ngeneration=1\nrepo=a/b\n', BAD_BODY],
    ['guard-gitea-verify 1\nstate=passed\nrepo=a/b\ngeneration=1\n', BAD_BODY],
    [
      'guard-gitea-verify 1\nstate = passed\ngeneration=1\nrepo=a/b\n',
      BAD_BODY,
    ],
    [
      'guard-gitea-verify 1\r\nstate=passed\r\ngeneration=1\r\nrepo=a/b\r\n',
      BAD_BODY,
    ],
    [record('verified'), BAD_BODY],
    [record('PASSED'), BAD_BODY],
    [record(''), BAD_BODY],
    [record('passed', '-1'), BAD_BODY],
    [record('passed', '01'), BAD_BODY],
    [record('passed', '1234567890123456789'), BAD_BODY],
    [record('passed', ''), BAD_BODY],
    [record('passed', 1, 'issue-desk-intake'), BAD_BODY],
    [record('passed', 1, 'a/b/c'), BAD_BODY],
    [record('passed', 1, `a/${'b'.repeat(199)}`), BAD_BODY],
    // read が落とす NUL を挟んで決まった語に見せかけた記録・末尾に NUL を足した記録。
    [
      record().replace('state=passed', 'state=pa\0ssed'),
      '記録に読み取れないバイトがある',
    ],
    [record() + '\0', '記録に読み取れないバイトがある'],
    // 正規の7行が取り得る大きさを超える記録は読まない。
    [record('passed', 1, `a/${'b'.repeat(400)}`), '記録が大きすぎる'],
    [record() + 'x'.repeat(1024 * 1024), '記録が大きすぎる'],
  ];
  for (const [content, reason] of malformed) {
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, content);
    assertStoreFails(box, reason, JSON.stringify(content));
    assert.equal(fs.readFileSync(store.record, 'utf8'), content);
    assert.deepEqual(tree(store.leaf), ['record']);
  }
});

test('記録の基盤 ロケール: UTF-8 のロケールで実行しても、全角の数字や ASCII でない文字の記録・origin は通さない', (t) => {
  const listed = spawnSync('locale', ['-a'], { encoding: 'utf8' }).stdout ?? '';
  const names = listed.split('\n').map((name) => name.trim());
  // 文字の範囲が ASCII の外へ広がるロケールでだけ意味がある（C.UTF-8 では照合が元から ASCII に限られる）。
  const found = ['en_US.utf8', 'en_US.UTF-8'].find((name) =>
    names.includes(name),
  );
  if (!found) {
    t.skip(
      'en_US の UTF-8 ロケールが無いPCでは、ロケールによる照合の広がりを再現できない',
    );
    return;
  }
  const env = { LC_ALL: found, LANG: found };
  for (const content of [
    record('passed', '１'),
    record('passed', '١٢'),
    record('passed', '1１'),
    record('passed', 1, 'mofua-suki/é'),
    record('passed', 1, 'ｓ/issue-desk-intake'),
    record('ｐassed'),
  ]) {
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, content);
    const result = box.store([], { env });
    assert.equal(result.stdout, storeFailed(BAD_BODY), content);
    assert.equal(result.status, 3, content);
    assert.equal(fs.readFileSync(store.record, 'utf8'), content);
  }
  // 同じロケールでも、正当な記録は読める。
  const valid = sandbox(t, INTAKE);
  prepareStore(valid, record());
  assert.equal(valid.store([], { env }).stdout, storeOk());
  // origin の名前も ASCII だけを受け付ける（段1の入口も同じ照合を使う）。
  const origin = sandbox(t, 'gitea-nas:mofua-suki/issue-desk-intaké.git');
  const stored = origin.store([], { env });
  assert.equal(
    stored.stdout,
    `${STORE}: 実行不能。origin からリポジトリを特定できない。\n`,
  );
  assert.equal(stored.status, 3);
  const verified = origin.verify([], { env });
  assert.equal(verified.status, 3);
  assert.ok(!verified.stdout.includes('合格 repo='), verified.stdout);
  assert.deepEqual(origin.calls(), []);
});

test('記録の基盤 書込不能: 保存先に書けない・保存先を作れない・点検用の記録を置き換えられないときは失敗する', (t) => {
  const readonly = sandbox(t, INTAKE);
  const store = prepareStore(readonly, record());
  fs.chmodSync(store.leaf, 0o500);
  try {
    assertStoreFails(readonly, IS_ROOT ? DIR_MODE : DIR_ACCESS);
    assert.deepEqual(tree(store.leaf), ['record']);
  } finally {
    fs.chmodSync(store.leaf, 0o700);
  }

  // 保存先を作れない（.local/state に書けない）。管理者は権限に関係なく作れるので、その場合は成功する。
  const parent = sandbox(t, INTAKE);
  const state = path.join(parent.home, '.local/state');
  fs.mkdirSync(state, { recursive: true });
  fs.chmodSync(state, 0o500);
  try {
    if (IS_ROOT) assert.equal(parent.store().status, 0);
    else assertStoreFails(parent, '保存先を作れない');
  } finally {
    fs.chmodSync(state, 0o700);
  }

  // 点検用の記録の位置にディレクトリがあり、置き換えられない。失敗してもロックは解放し、既存の記録は変えない。
  const occupied = sandbox(t, INTAKE);
  const taken = prepareStore(occupied, record());
  fs.mkdirSync(taken.probe, { mode: 0o700 });
  assertStoreFails(occupied, '記録が通常のファイルでない');
  assert.equal(fs.readFileSync(taken.record, 'utf8'), record());
  assert.deepEqual(tree(taken.leaf), ['probe', 'record']);

  // ロックの持ち主を書き込めない（作ったロックに書けない）ときは、自分が作ったロックを残さない。
  // 管理者は権限に関係なく書けるので、その場合は成功する。
  const sealed = sandbox(t, INTAKE);
  const sealedStore = prepareStore(sealed, record());
  sealed.tool(
    'mkdir',
    [
      'for last in "$@"; do :; done',
      `case $last in */lock) exec '${path.join(system, 'mkdir')}' -m 500 -- "$last" ;; esac`,
      `exec '${path.join(system, 'mkdir')}' "$@"`,
    ].join('\n'),
  );
  if (IS_ROOT) assert.equal(sealed.store().status, 0);
  else assertStoreFails(sealed, 'ロックの持ち主を書き込めない');
  assert.deepEqual(tree(sealedStore.leaf), ['record']);
});

test('記録の基盤 ロック: ほかが持つロックは奪わず、取れなければ失敗する', (t) => {
  const box = sandbox(t, INTAKE);
  const store = prepareStore(box, record());
  fs.mkdirSync(store.lock, { mode: 0o700 });
  // 持ち主の欄にコード相当の文字列があっても評価しない。
  const owner = `a[$(touch ${box.state}/pwned)]\n`;
  fs.writeFileSync(path.join(store.lock, 'owner'), owner, { mode: 0o600 });
  assertStoreFails(box, LOCK_BUSY);
  assert.equal(fs.readFileSync(path.join(store.lock, 'owner'), 'utf8'), owner);
  assert.deepEqual(tree(store.leaf), ['lock', 'lock/owner', 'record']);
  assert.ok(!fs.existsSync(path.join(box.state, 'pwned')));
  // ロックが外れれば同じ保存先で成功する。
  fs.rmSync(store.lock, { recursive: true });
  assert.equal(box.store().status, 0);

  // ロックの位置に通常のファイルがあるときも取れない。
  const file = sandbox(t, INTAKE);
  fs.writeFileSync(prepareStore(file).lock, '', { mode: 0o600 });
  assertStoreFails(file, 'ロックを作れない');
});

test('記録の基盤 ロック: 1本目がロックを持って記録を操作している間、2本目は必ずロックを取れずに失敗する', async (t) => {
  const box = sandbox(t, INTAKE);
  const store = prepareStore(box, record());
  // 1本目を、ロックを持ったまま記録を置き換える直前（mv）で止める。合図のファイルができるまで進まない。
  box.tool(
    'mv',
    [
      ': > "$S/holding"',
      'while [ ! -f "$S/release" ]; do sleep 0.05; done',
      `exec '${path.join(system, 'mv')}' "$@"`,
    ].join('\n'),
  );
  const child = spawn(bash, [guard, 'verify-store'], {
    cwd: box.repo,
    env: box.env(),
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const first = { stdout: '', stderr: '' };
  child.stdout.on('data', (chunk) => {
    first.stdout += chunk;
  });
  child.stderr.on('data', (chunk) => {
    first.stderr += chunk;
  });
  const closed = new Promise<Closed>((resolve, reject) => {
    child.on('error', reject);
    child.on('close', resolve);
  });
  try {
    for (
      let i = 0;
      i < 400 && !fs.existsSync(path.join(box.state, 'holding'));
      i += 1
    )
      await new Promise((resolve) => setTimeout(resolve, 25));
    assert.ok(
      fs.existsSync(path.join(box.state, 'holding')),
      '1本目が記録の置き換えまで進まない',
    );
    assert.equal(
      fs.readFileSync(path.join(store.lock, 'owner'), 'utf8'),
      `${child.pid}\n`,
    );
    // 2本目は待っても取れず、失敗として返る。1本目のロックと書きかけの記録には触れない。
    assertStoreFails(box, LOCK_BUSY);
    assert.deepEqual(tree(store.leaf), [
      'lock',
      'lock/owner',
      `probe.tmp.${child.pid}`,
      'record',
    ]);
  } finally {
    box.set('release', '');
  }
  assert.equal(await closed, 0);
  assert.equal(first.stderr, '');
  assert.equal(first.stdout, storeOk());
  assert.deepEqual(tree(store.leaf), ['record']);
  // 1本目が終われば、次の実行はロックを取れる。
  assert.equal(box.store().status, 0);
});

test('記録の基盤 ロック: 競合したロックがmkdir失敗直後に解放されても再取得できる', (t) => {
  const box = sandbox(t, INTAKE);
  const store = prepareStore(box, record());
  box.tool(
    'mkdir',
    [
      'for arg do last=$arg; done',
      'case "$last" in */lock) if [ ! -f "$S/contended" ]; then',
      `  '${path.join(system, 'mkdir')}' -m 700 -- "$last" || exit 1`,
      '  : > "$S/contended"',
      `  '${path.join(system, 'rmdir')}' -- "$last" || exit 1`,
      '  exit 1',
      'fi;; esac',
      `exec '${path.join(system, 'mkdir')}' "$@"`,
    ].join('\n'),
  );
  assert.equal(box.store().status, 0);
  assert.ok(fs.existsSync(path.join(box.state, 'contended')));
  assert.deepEqual(tree(store.leaf), ['record']);
  assert.equal(fs.readFileSync(store.record, 'utf8'), record());
});

test('記録の基盤 ロック: 同時に実行しても互いの記録を壊さず、終わればロックも点検用の記録も残らない', async (t) => {
  const box = sandbox(t, INTAKE);
  const store = prepareStore(box, record());
  const runs = await Promise.all(
    Array.from(
      { length: 8 },
      () =>
        new Promise<Closed>((resolve, reject) => {
          const child = spawn(bash, [guard, 'verify-store'], {
            cwd: box.repo,
            env: box.env(),
            stdio: ['ignore', 'pipe', 'pipe'],
          });
          const out = { stdout: '', stderr: '' };
          child.stdout.on('data', (chunk) => {
            out.stdout += chunk;
          });
          child.stderr.on('data', (chunk) => {
            out.stderr += chunk;
          });
          child.on('error', reject);
          child.on('close', (status) => resolve({ status, ...out }));
        }),
    ),
  );
  for (const run of runs) {
    assert.equal(run.stderr, '');
    // ロックを待ち切れなかった実行は失敗として返る。成功か、その失敗のどちらかだけ。
    assert.ok(
      (run.status === 0 && run.stdout === storeOk()) ||
        (run.status === 3 && run.stdout === storeFailed(LOCK_BUSY)),
      JSON.stringify(run),
    );
  }
  assert.ok(runs.some((run) => run.status === 0));
  assert.deepEqual(tree(store.leaf), ['record']);
  assert.equal(fs.readFileSync(store.record, 'utf8'), record());
});

test('記録の基盤 コード評価なし: 記録・標準入力に埋めたコード相当のデータは実行されない', (t) => {
  const pwned = (box) => path.join(box.state, 'pwned');
  const payloads = (file) => [
    `guard-gitea-verify 1\nstate=$(touch ${file})\ngeneration=1\nrepo=a/b\n`,
    `guard-gitea-verify 1\nstate=passed; touch ${file}\ngeneration=1\nrepo=a/b\n`,
    `guard-gitea-verify 1\nstate=passed\ngeneration=a[$(touch ${file})]\nrepo=a/b\n`,
    `guard-gitea-verify 1\nstate=passed\ngeneration=1\nrepo=a/\`touch ${file}\`\n`,
    `guard-gitea-verify 1\nstate=passed\ngeneration=1\nrepo=a/b\ntouch ${file}\n`,
    `touch ${file}\n`,
    `VERIFY_REC_STATE=passed\ntouch ${file}\nreturn 0\nexit 0\n`,
  ];
  for (let i = 0; i < payloads('').length; i += 1) {
    const box = sandbox(t, INTAKE);
    const text = payloads(pwned(box))[i];
    const store = prepareStore(box, text);
    const result = box.store([], { input: text });
    assert.equal(result.status, 3, text);
    assert.ok(!result.stdout.includes('点検に成功'), result.stdout);
    assert.ok(!fs.existsSync(pwned(box)), text);
    assert.equal(fs.readFileSync(store.record, 'utf8'), text);
  }
  // 記録が無い保存先で、標準入力へ成功の記録やコードを渡しても記録は作られない。
  const fresh = sandbox(t, INTAKE);
  assert.equal(
    fresh.store([], { input: record() + `touch ${pwned(fresh)}\n` }).status,
    0,
  );
  assert.deepEqual(tree(storePaths(fresh).leaf), []);
  assert.ok(!fs.existsSync(pwned(fresh)));
});

test('記録だけでは送信を許可しない: 成功の記録があっても検証は省かれず、フックの送信判定も変わらない', (t) => {
  const box = sandbox(t, INTAKE);
  const push = {
    hook_event_name: 'PreToolUse',
    tool_name: 'Bash',
    cwd: box.repo,
    tool_input: { command: 'git push origin HEAD' },
  };
  const hookBefore = hook(push, box.env());
  const store = prepareStore(box, record('passed'));
  const stored = box.store();
  assert.equal(stored.status, 0);
  assert.ok(!/permissionDecision|allow/.test(stored.stdout));

  // 成功の記録があっても、固定入口は検証を実行し、失敗すれば不合格になる。
  box.set('exit-verify', 1);
  let result = box.verify();
  assert.equal(result.status, 1);
  assert.match(result.stdout, /不合格 repo=mofua-suki\/issue-desk-intake。/);
  assert.equal(readRecord(store), record('failed', 8));
  box.set('exit-verify', 0);
  result = box.verify();
  assert.equal(result.stdout, passed(9));
  assert.deepEqual(box.calls(), ['verify', 'verify']);
  assert.equal(readRecord(store), record('passed', 9));
  assert.deepEqual(tree(store.leaf), ['record']);

  // フックは記録を読まない。送信の判定は記録の有無で変わらず、許可の応答も出さない。
  const hookAfter = hook(push, box.env());
  assert.equal(hookAfter, hookBefore);
  assert.ok(!hookAfter.includes('allow'));
  assert.equal(
    hook(
      pre('Bash', {
        command: 'bash ~/.claude/hooks/guard-gitea.sh verify-store',
      }),
    ),
    '',
  );
  assert.match(
    denyReason({ ...push, tool_input: { command: `curl ${ISSUE_API}` } }),
    ISSUE_REASON,
  );
});

test('記録の基盤 資料どおりの呼び方: docs/chat-operations.md に書いた点検のコマンドをそのまま実行できる', (t) => {
  const doc = fs.readFileSync(
    path.join(repoRoot, 'docs/chat-operations.md'),
    'utf8',
  );
  const command = 'bash ~/.claude/hooks/guard-gitea.sh verify-store';
  assert.ok(doc.split('\n').includes(command));
  const box = sandbox(t, INTAKE);
  const hooks = path.join(box.home, '.claude/hooks');
  fs.mkdirSync(hooks, { recursive: true });
  fs.copyFileSync(guard, path.join(hooks, 'guard-gitea.sh'));
  const result = spawnSync(bash, ['-c', command], {
    cwd: box.repo,
    env: box.env(),
    encoding: 'utf8',
  });
  assert.equal(result.stderr, '');
  assert.equal(result.stdout, storeOk());
  assert.equal(result.status, 0);
  assert.deepEqual(tree(storePaths(box).leaf), []);
});

// ---- 検証の開始・終了と記録の接続（段2後半） ----
// 固定入口（verify）が、開始時に旧い成功を無効にし、最新に始めた検証だけが結果を保存することを確かめる。

const beginFailed = (reason, repo = INTAKE_REPO) =>
  `必須検証（段1）: 実行不能 repo=${repo}。検証の記録を開始できない（${reason}）。旧い成功を無効にして世代を記録できるまで、検証を実行しない。\n`;
const saveFailed = (reason, repo = INTAKE_REPO) =>
  `必須検証（段1）: 実行不能 repo=${repo}。検証は成功したが結果を記録できない（${reason}）。記録できない成功は合格にしない。\n`;
const saveNote = (reason) =>
  `記録: この検証の結果を保存できない（${reason}）。\n`;
// 検証は成功したが、検証後の同一性の照合で合格にしなかったときの行。
const identityFailed = (reason, repo = INTAKE_REPO) =>
  `必須検証（段1）: 実行不能 repo=${repo}。検証は成功したが、${reason}。検証した対象と今の対象が同じと確かめられない成功は合格にしない。\n`;
const identityChanged = (names, repo = INTAKE_REPO) =>
  identityFailed(`検証の間に対象が変わった（${names}）`, repo);
const identityBeginFailed = (reason, repo = INTAKE_REPO) =>
  `必須検証（段1）: 実行不能 repo=${repo}。検証の対象の同一性を記録できない（${reason}）。何を検証したかを記録できるまで、検証を実行しない。\n`;
const staleNote = (reason) =>
  `記録: この作業場所の旧い記録を無効にできない（${reason}）。\n`;
const INTERRUPTED = `必須検証（段1）: 実行不能 repo=${INTAKE_REPO}。中断された。旧い成功は無効のままで、この検証の結果は合格にしない。\n`;
const FAILED_LINE = `必須検証（段1）: 不合格 repo=${INTAKE_REPO}。変更と無関係な失敗・修正前からの失敗も免除しない。\n`;
const NOT_LATEST = '後から始まった検証があり、この検証は最新でない';
const otherSide = (reason) =>
  `今の基点でない側の保存先にある記録を無効にできない（${reason}）`;
const MV_BROKEN = '記録を置き換えられない';

// 同期用フィクスチャ: Intakeの検証（scripts/verify.sh）を、合図のファイルが置かれるまで終わらない物に替える。
// start() は検証が始まる（開始の記録が済む）まで待って返す。finish(終了コード) でその検証を終わらせる。
// 1本ずつ順に始めるので、何本目の検証かは started-<番号> の有無で決まる。
function gate(t: TestContext, box: Box) {
  const script = [
    `S='${box.state}'`,
    'n=1',
    'while [ -f "$S/started-$n" ]; do n=$((n + 1)); done',
    ': > "$S/started-$n"',
    'printf \'verify %s\\n\' "$n" >> "$S/calls"',
    // 試験の一時ディレクトリが消えたら終わる（止まったままの検証を残さない）。
    'while [ ! -f "$S/release-$n" ]; do [ -d "$S" ] || exit 1; sleep 0.05; done',
    'exit "$(cat "$S/release-$n")"',
    '',
  ].join('\n');
  // The test may append to verify.sh while the gate is waiting. Execute an
  // already-read copy so the shell cannot consume the appended bytes as input.
  fs.writeFileSync(
    path.join(box.repo, 'scripts/verify.sh'),
    `exec sh -c '${script.replace(/'/g, "'\\''")}'\n`,
  );
  let count = 0;
  t.after(() => {
    for (let n = 1; n <= count; n += 1) {
      try {
        fs.writeFileSync(path.join(box.state, `release-${n}`), '1', {
          flag: 'wx',
        });
      } catch {
        /* 終わらせ済みか、一時ディレクトリが消えている */
      }
    }
  });
  return {
    async start(script = guard) {
      count += 1;
      const n = count;
      const child = spawn(bash, [script, 'verify'], {
        cwd: box.repo,
        env: box.env(),
        stdio: ['ignore', 'pipe', 'pipe'],
      });
      const out = { stdout: '', stderr: '' };
      child.stdout.on('data', (chunk) => {
        out.stdout += chunk;
      });
      child.stderr.on('data', (chunk) => {
        out.stderr += chunk;
      });
      const closed = new Promise<Closed>((resolve, reject) => {
        child.on('error', reject);
        child.on('close', (status, signal) =>
          resolve({ status, signal, ...out }),
        );
      });
      const started = path.join(box.state, `started-${n}`);
      for (let i = 0; i < 400 && !fs.existsSync(started); i += 1)
        await new Promise((resolve) => setTimeout(resolve, 25));
      assert.ok(
        fs.existsSync(started),
        `${n}本目の検証が始まらない: ${out.stdout}`,
      );
      return {
        child,
        async finish(code: number | string = 0) {
          box.set(`release-${n}`, code);
          const result = await closed;
          assert.equal(result.stderr, '');
          return result;
        },
      };
    },
  };
}

test('無効化: 既存の成功がある状態で検証を始めると、検証が終わる前に旧い成功が無効になり、成功すれば新しい世代で記録する', async (t) => {
  const box = sandbox(t, INTAKE);
  const store = prepareStore(box, record('passed', 7));
  const runs = gate(t, box);
  const run = await runs.start();
  // 検証の実行中: 旧い成功（世代7）は消え、実行中（世代8）だけがある。ロックは持ち続けない。
  assert.equal(readRecord(store), record('running', 8));
  assert.deepEqual(tree(store.leaf), ['record']);
  const result = await run.finish(0);
  assert.equal(result.stdout, passed(8));
  assert.equal(result.status, 0);
  assert.equal(readRecord(store), record('passed', 8));
  assert.equal(mode(store.record), 0o600);
  assert.deepEqual(tree(store.leaf), ['record']);
  assert.deepEqual(box.calls(), ['verify 1']);

  // 記録が無い作業場所では世代1から始まる。別のリポジトリの記録があっても、今のリポジトリの世代として引き継ぐ。
  const fresh = sandbox(t, INTAKE);
  assert.equal(fresh.verify().stdout, passed(1));
  assert.equal(readRecord(storePaths(fresh)), record('passed', 1));
  const moved = sandbox(t, INTAKE);
  const movedStore = prepareStore(
    moved,
    record('passed', 3, 'mofua-suki/issue-desk-worker'),
  );
  moved.set('exit-verify', 1);
  assert.equal(moved.verify().status, 1);
  assert.equal(readRecord(movedStore), record('failed', 4));
});

test('無効化: 失敗・一部成功・実行不能で終わった後も、旧い成功は記録に残らない', (t) => {
  const failing = sandbox(t, INTAKE);
  const failingStore = prepareStore(failing, record('passed', 7));
  failing.set('exit-verify', 1);
  let result = failing.verify();
  assert.equal(result.status, 1);
  assert.ok(result.stdout.endsWith(FAILED_LINE));
  assert.equal(readRecord(failingStore), record('failed', 8));

  const partial = sandbox(t, WORKER);
  const partialStore = prepareStore(
    partial,
    record('passed', 7, 'mofua-suki/issue-desk-worker'),
  );
  partial.set('exit-go-vet', 1);
  result = partial.verify();
  assert.equal(result.status, 1);
  assert.equal(
    readRecord(partialStore),
    record('failed', 8, 'mofua-suki/issue-desk-worker'),
  );
  assert.deepEqual(partial.calls(), [
    'go test ./...',
    'go vet ./...',
    'gofmt -l .',
  ]);

  const missing = sandbox(t, INTAKE);
  const missingStore = prepareStore(missing, record('passed', 7));
  fs.rmSync(path.join(missing.repo, 'scripts/verify.sh'));
  result = missing.verify();
  assert.equal(result.status, 3);
  assert.ok(!result.stdout.includes('合格 repo='));
  assert.equal(readRecord(missingStore), record('invalid', 8));
  assert.deepEqual(missing.calls(), []);
  for (const store of [failingStore, partialStore, missingStore])
    assert.deepEqual(tree(store.leaf), ['record']);
});

test('無効化: 成功した検証の結果を保存できなければ合格にせず、旧い成功も成功の記録も残らない', async (t) => {
  const cases: any[][] = [
    [
      '記録の置き換えに失敗',
      (box) => {
        box.tool('mv', 'exit 1');
      },
      MV_BROKEN,
      record('running', 8),
    ],
    // 保存先そのものを使えないときは、保存より前の「検証後の同一性の照合」で止まる（一時ファイルを保存先に置くため）。
    [
      '保存先に書けない',
      (_box, store) => {
        fs.chmodSync(store.leaf, 0o500);
        return () => fs.chmodSync(store.leaf, 0o700);
      },
      IS_ROOT ? DIR_MODE : DIR_ACCESS,
      record('running', 8),
      true,
    ],
    [
      '保存先の権限が広がった',
      (_box, store) => {
        fs.chmodSync(store.leaf, 0o750);
      },
      DIR_MODE,
      record('running', 8),
      true,
    ],
    [
      '記録の権限が広がった',
      (_box, store) => {
        fs.chmodSync(store.record, 0o644);
      },
      FILE_MODE,
      record('running', 8),
    ],
    [
      '記録がリンクに替わった',
      (box, store) => {
        fs.writeFileSync(
          path.join(box.tmp, 'elsewhere'),
          record('running', 8),
          { mode: 0o600 },
        );
        fs.rmSync(store.record);
        fs.symlinkSync(path.join(box.tmp, 'elsewhere'), store.record);
      },
      FILE_LINK,
      null,
    ],
    [
      '記録が決まった形でなくなった',
      (_box, store) => {
        fs.writeFileSync(store.record, 'state=passed\n');
      },
      BAD_LINES,
      'state=passed\n',
    ],
    [
      '記録が無くなった',
      (_box, store) => {
        fs.rmSync(store.record);
      },
      '開始時に書いた記録が無くなっている',
      null,
    ],
    [
      '保存先が無くなった',
      (_box, store) => {
        fs.rmSync(store.leaf, { recursive: true });
      },
      '開始時に使った保存先が無くなっている',
      null,
      true,
    ],
    [
      'ロックを取れない',
      (_box, store) => {
        fs.mkdirSync(store.lock, { mode: 0o700 });
      },
      LOCK_BUSY,
      record('running', 8),
    ],
  ];
  for (const [label, damage, reason, left, atIdentity] of cases) {
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, record('passed', 7));
    const run = await gate(t, box).start();
    const restore = damage(box, store);
    let result;
    try {
      result = await run.finish(0);
    } finally {
      restore?.();
    }
    assert.equal(
      result.stdout,
      atIdentity
        ? saveNote(reason) +
            identityFailed(`検証の後の同一性を確かめられない（${reason}）`)
        : saveFailed(reason),
      label,
    );
    assert.equal(result.status, 3, label);
    if (left !== null) assert.equal(readRecord(store), left, label);
    // リンク先を含め、成功の記録はどこにも無い。書きかけの一時ファイルも残さない。
    for (const file of tree(box.home).concat(
      tree(box.tmp).map((name) => path.join(box.tmp, name)),
    )) {
      const full = path.isAbsolute(file) ? file : path.join(box.home, file);
      if (fs.lstatSync(full).isFile())
        assert.ok(
          !fs.readFileSync(full, 'utf8').includes('state=passed\ngeneration'),
          `${label}: ${file}`,
        );
      assert.ok(!file.includes('.tmp.'), `${label}: ${file}`);
    }
  }

  // 失敗した検証の結果を保存できないときは、不合格のまま、保存できないことも示す。
  const box = sandbox(t, INTAKE);
  const store = prepareStore(box, record('passed', 7));
  const run = await gate(t, box).start();
  box.tool('mv', 'exit 1');
  const result = await run.finish(1);
  assert.equal(result.status, 1);
  assert.ok(
    result.stdout.endsWith(saveNote(MV_BROKEN) + FAILED_LINE),
    result.stdout,
  );
  assert.equal(readRecord(store), record('running', 8));
});

test('中断: 中断の合図を受けた検証は合格にならず、自分の記録を無効にしてロックを残さない', async (t) => {
  for (const signal of ['SIGTERM', 'SIGHUP', 'SIGINT'] as NodeJS.Signals[]) {
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, record('passed', 7));
    const runs = gate(t, box);
    const run = await runs.start();
    run.child.kill(signal);
    // 合図の後も、旧い成功は無効のまま（合図は送った時点で届いていて、実行中の検証コマンドが終わるまで処理されない）。
    assert.equal(readRecord(store), record('running', 8), signal);
    // 検証そのものは成功で終わっても、中断された検証は合格にしない。
    const result = await run.finish(0);
    assert.equal(result.stdout, INTERRUPTED, signal);
    assert.equal(result.status, 3, signal);
    assert.equal(readRecord(store), record('invalid', 8), signal);
    assert.deepEqual(tree(store.leaf), ['record'], signal);
    // 後始末が済んでいるので、次の検証は開始できる。
    const next = await runs.start();
    assert.equal(readRecord(store), record('running', 9), signal);
    assert.equal((await next.finish(0)).stdout, passed(9), signal);
  }
});

test('中断: 後始末なしで強制終了された検証の記録は実行中のまま残り、成功にはならない', async (t) => {
  const box = sandbox(t, INTAKE);
  const store = prepareStore(box, record('passed', 7));
  const runs = gate(t, box);
  const run = await runs.start();
  run.child.kill('SIGKILL');
  const result = await run.finish(0);
  assert.equal(result.signal, 'SIGKILL');
  assert.equal(result.stdout, '');
  assert.equal(readRecord(store), record('running', 8));
  assert.deepEqual(tree(store.leaf), ['record']);
  // 次の検証は世代を進めて始められる。
  const next = await runs.start();
  assert.equal(readRecord(store), record('running', 9));
  assert.equal((await next.finish(0)).stdout, passed(9));
});

test('世代: 並行して始めた検証は、終わる順が逆でも最新に始めたものだけが結果を保存でき、旧い検証の遅い成功で成功の記録は復活しない', async (t) => {
  // [先に終わる側, 旧い検証の終了コード, 最新の検証の終了コード, 最後に残る記録]
  const cases: any[][] = [
    ['old', 0, 0, 'passed'],
    ['old', 0, 1, 'failed'],
    ['new', 0, 1, 'failed'],
    ['new', 0, 0, 'passed'],
    ['new', 1, 0, 'passed'],
  ];
  for (const [first, oldCode, newCode, final] of cases) {
    const label = `${first} ${oldCode} ${newCode}`;
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, record('passed', 7));
    const runs = gate(t, box);
    const older = await runs.start();
    assert.equal(readRecord(store), record('running', 8), label);
    const newer = await runs.start();
    assert.equal(readRecord(store), record('running', 9), label);

    const finishOlder = async () => {
      const before = readRecord(store);
      const result = await older.finish(oldCode);
      if (oldCode === 0) {
        assert.equal(result.stdout, saveFailed(NOT_LATEST), label);
        assert.equal(result.status, 3, label);
      } else {
        assert.ok(
          result.stdout.endsWith(saveNote(NOT_LATEST) + FAILED_LINE),
          label,
        );
        assert.equal(result.status, 1, label);
      }
      // 旧い検証は記録を一切変えない。
      assert.equal(readRecord(store), before, label);
    };
    const finishNewer = async () => {
      const result = await newer.finish(newCode);
      assert.equal(result.status, newCode, label);
      if (newCode === 0) assert.equal(result.stdout, passed(9), label);
      assert.equal(readRecord(store), record(final, 9), label);
    };
    if (first === 'old') {
      await finishOlder();
      assert.equal(readRecord(store), record('running', 9), label);
      await finishNewer();
    } else {
      await finishNewer();
      await finishOlder();
    }
    assert.equal(readRecord(store), record(final, 9), label);
    assert.deepEqual(tree(store.leaf), ['record'], label);
    assert.deepEqual(box.calls(), ['verify 1', 'verify 2'], label);
  }

  // 最新の検証が中断で終わった後に旧い検証が成功しても、記録は無効のまま。
  const box = sandbox(t, INTAKE);
  const store = prepareStore(box, record('passed', 7));
  const runs = gate(t, box);
  const older = await runs.start();
  const newer = await runs.start();
  newer.child.kill('SIGTERM');
  assert.equal((await newer.finish(0)).stdout, INTERRUPTED);
  assert.equal(readRecord(store), record('invalid', 9));
  const late = await older.finish(0);
  assert.equal(late.stdout, saveFailed(NOT_LATEST));
  assert.equal(late.status, 3);
  assert.equal(readRecord(store), record('invalid', 9));
});

test('開始できない: 無効化・排他・保存先・所有・権限・リンク・読取・書込のどれかに失敗したら、検証を実行せず合格にしない', (t) => {
  const elsewhere = (box) => {
    const dir = path.join(box.tmp, 'elsewhere');
    fs.mkdirSync(dir, { mode: 0o700 });
    return dir;
  };
  // 所有者の番号を返す ls を差し替える（ほかの利用者が所有するファイルは試験では作れないため）。
  const foreign = (box, target) => {
    box.tool(
      'ls',
      [
        `out=$('${path.join(system, 'ls')}' "$@") || exit $?`,
        'for last in "$@"; do :; done',
        `if [ "$last" = '${target}' ]; then`,
        '  set -- $out',
        '  printf \'%s %s 4000000000 %s %s\\n\' "$1" "$2" "$4" "$5"',
        'else',
        '  printf \'%s\\n\' "$out"',
        'fi',
      ].join('\n'),
    );
  };
  // [名前, 細工（後片付けを返してよい）, 理由, 旧い記録を置くか]
  const cases: any[][] = [
    [
      '排他: ロックが残っている',
      (_box, store) => {
        fs.mkdirSync(store.lock, { mode: 0o700 });
      },
      LOCK_BUSY,
    ],
    [
      '排他: ロックの位置にファイル',
      (_box, store) => {
        fs.writeFileSync(store.lock, '', { mode: 0o600 });
      },
      'ロックを作れない',
    ],
    [
      'リンク: ロックがリンク',
      (box, store) => {
        fs.symlinkSync(elsewhere(box), store.lock);
      },
      'ロックがシンボリックリンクになっている',
    ],
    [
      '不正保存先: 保存先がファイル',
      (_box, store) => {
        fs.rmSync(store.leaf, { recursive: true });
        fs.writeFileSync(store.leaf, record('passed'), { mode: 0o600 });
      },
      '保存先がディレクトリでない',
    ],
    [
      '不正保存先: 保存先がリンク',
      (box, store) => {
        fs.rmSync(store.leaf, { recursive: true });
        fs.symlinkSync(elsewhere(box), store.leaf);
      },
      DIR_LINK,
    ],
    [
      '不正保存先: 途中がリンク',
      (box, store) => {
        fs.renameSync(store.verify, path.join(box.tmp, 'moved'));
        fs.symlinkSync(path.join(box.tmp, 'moved'), store.verify);
      },
      DIR_LINK,
    ],
    [
      '不正保存先: .local がリンク',
      (box) => {
        fs.renameSync(
          path.join(box.home, '.local'),
          path.join(box.tmp, 'moved'),
        );
        fs.symlinkSync(
          path.join(box.tmp, 'moved'),
          path.join(box.home, '.local'),
        );
      },
      '保存先の途中にリンクがある',
    ],
    [
      '権限: 保存先をほかの利用者が読める',
      (_box, store) => {
        fs.chmodSync(store.leaf, 0o750);
      },
      DIR_MODE,
    ],
    [
      '権限: 途中をほかの利用者が読める',
      (_box, store) => {
        fs.chmodSync(store.base, 0o755);
      },
      DIR_MODE,
    ],
    [
      '権限: 記録をほかの利用者が読める',
      (_box, store) => {
        fs.chmodSync(store.record, 0o644);
      },
      FILE_MODE,
    ],
    [
      '所有: 保存先の所有者が違う',
      (box, store) => {
        foreign(box, store.leaf);
      },
      '保存先の所有者が実行者と違う',
    ],
    [
      '所有: 記録の所有者が違う',
      (box, store) => {
        foreign(box, store.record);
      },
      '記録の所有者が実行者と違う',
    ],
    [
      'リンク: 記録がリンク',
      (box, store) => {
        const target = path.join(elsewhere(box), 'record');
        fs.renameSync(store.record, target);
        fs.symlinkSync(target, store.record);
      },
      FILE_LINK,
    ],
    [
      'リンク: 記録にハードリンク',
      (box, store) => {
        fs.linkSync(store.record, path.join(box.tmp, 'hard'));
      },
      '記録にハードリンクがある',
    ],
    [
      '読取不能: 記録を読めない',
      (_box, store) => {
        fs.chmodSync(store.record, 0o000);
      },
      IS_ROOT ? FILE_MODE : '記録を読めない',
    ],
    [
      '読取不能: 記録がディレクトリ',
      (_box, store) => {
        fs.rmSync(store.record);
        fs.mkdirSync(store.record, { mode: 0o700 });
      },
      '記録が通常のファイルでない',
    ],
    [
      '読取不能: 記録の値が決まった形でない',
      (_box, store) => {
        fs.writeFileSync(store.record, record('passed', '07'));
      },
      BAD_BODY,
    ],
    [
      '読取不能: 記録の行が足りない',
      (_box, store) => {
        fs.writeFileSync(store.record, 'guard-gitea-verify 1\nstate=passed\n');
      },
      BAD_LINES,
    ],
    [
      '無効化: 世代をこれ以上進められない',
      (_box, store) => {
        fs.writeFileSync(store.record, record('passed', '999999999999999999'));
      },
      '書こうとした記録の中身が決まった形でない',
    ],
    [
      '書込不能: 保存先に書けない',
      (_box, store) => {
        fs.chmodSync(store.leaf, 0o500);
        return () => fs.chmodSync(store.leaf, 0o700);
      },
      IS_ROOT ? DIR_MODE : DIR_ACCESS,
    ],
    [
      '書込不能: 記録を置き換えられない',
      (box) => {
        box.tool('mv', 'exit 1');
      },
      MV_BROKEN,
    ],
  ];
  if (!IS_ROOT) {
    cases.push([
      '書込不能: 保存先を作れない',
      (_box, store) => {
        fs.rmSync(store.base, { recursive: true });
        const state = path.dirname(store.base);
        fs.chmodSync(state, 0o500);
        return () => fs.chmodSync(state, 0o700);
      },
      '保存先を作れない',
    ]);
  }
  for (const [label, damage, reason] of cases) {
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, record('passed', 7));
    const restore = damage(box, store);
    const snapshot = [box.home, box.tmp].map(tree);
    let result;
    try {
      result = box.verify();
      assert.equal(result.stdout, beginFailed(reason), label);
      assert.equal(result.status, 3, label);
      // 検証は1つも実行しない。保存先・リンク先に何も作らず、書きかけも残さない。
      assert.deepEqual(box.calls(), [], label);
      assert.deepEqual([box.home, box.tmp].map(tree), snapshot, label);
    } finally {
      restore?.();
    }
  }
});

test('開始できない: リポジトリを特定できない・対応表に無いときは、検証を実行せず、その作業場所の旧い成功を無効にする', (t) => {
  const UNKNOWN =
    '必須検証（段1）: 実行不能。origin からリポジトリを特定できない。\n';
  const UNMAPPED =
    '必須検証（段1）: 拒否。必須検証の対応表に無いリポジトリのため、検証済みにできない。\n';
  for (const [origin, status, line] of [
    [null, 3, UNKNOWN],
    ['gitea-nas:mofua-suki/other-project.git', 2, UNMAPPED],
  ] as [string | null, number, string][]) {
    const box = sandbox(t, origin);
    const store = prepareStore(box, record('passed', 7));
    let result = box.verify();
    assert.equal(result.stdout, line);
    assert.equal(result.status, status);
    assert.equal(readRecord(store), record('invalid', 8));
    assert.deepEqual(tree(store.leaf), ['record']);
    assert.deepEqual(box.calls(), []);

    // 無効にできないときは、そのことも示す（成功にはしない）。
    fs.writeFileSync(store.record, record('passed', 9), { mode: 0o600 });
    fs.mkdirSync(store.lock, { mode: 0o700 });
    result = box.verify();
    assert.equal(result.stdout, line + staleNote(LOCK_BUSY));
    assert.equal(result.status, status);

    // 記録が無い作業場所では、保存先を作らない。
    const fresh = sandbox(t, origin);
    result = fresh.verify();
    assert.equal(result.stdout, line);
    assert.deepEqual(tree(fresh.home), []);
  }
});

// 実機の /run/user/<実行者の番号> の側の保存先（基点の切り替わりを確かめる試験だけが使う）。使えない筐体では null。
// 保存先の名前は一時ディレクトリごとに違うので、ほかの実行と衝突しない。試験が書いた分は必ず消す。
function runSide(t, box) {
  const run = usableRunBase();
  if (!run || !realId) return null;
  const base = path.join(run, 'guard-gitea');
  const verify = path.join(base, 'verify');
  const leaf = path.join(verify, path.basename(storePaths(box).leaf));
  const existed = [base, verify].map((dir) => fs.existsSync(dir));
  for (const dir of [base, verify].filter((_, index) => existed[index])) {
    const stat = fs.lstatSync(dir);
    assert.ok(
      stat.isDirectory() &&
        stat.uid === process.getuid() &&
        (stat.mode & 0o7777) === 0o700,
      `この筐体に前からある ${dir} が、実行者所有・権限700のディレクトリでない。テスト対象の不具合ではなく、この保存先を直す必要がある`,
    );
  }
  assert.equal(fs.existsSync(leaf), false);
  t.after(() => {
    fs.rmSync(leaf, { recursive: true, force: true });
    for (const [index, dir] of [
      [1, verify],
      [0, base],
    ] as [number, string][]) {
      if (existed[index]) continue;
      try {
        fs.rmdirSync(dir);
      } catch (error) {
        if (!['ENOTEMPTY', 'EEXIST', 'ENOENT'].includes(error.code))
          throw error;
      }
    }
  });
  return {
    leaf,
    record: path.join(leaf, 'record'),
    lock: path.join(leaf, 'lock'),
    // 正当な権限の保存先と記録を /run/user の側に置く。
    put(content) {
      for (const dir of [base, verify, leaf]) {
        if (!fs.existsSync(dir)) fs.mkdirSync(dir, { mode: 0o700 });
        fs.chmodSync(dir, 0o700);
      }
      fs.writeFileSync(path.join(leaf, 'record'), content, { mode: 0o600 });
    },
    // この実行から /run/user の側を基点にする（id を置く）。外すと HOME の側が基点になる。
    useRun() {
      box.tool('id', `exec '${realId}' "$@"`);
    },
    useHome() {
      fs.rmSync(path.join(box.bin, 'id'), { force: true });
    },
  };
}
const NO_RUN_BASE =
  'この筐体では /run/user/<実行者の番号> を基点にできないため、基点の切り替わりを再現できない';

test('基点の切り替わり: 今の基点でない側に残った旧い成功は、開始時に無効になり、生き返らない', (t) => {
  // /run/user の側が基点のとき、HOME の側に残った成功を無効にする。
  const toRun = sandbox(t, INTAKE);
  const runA = runSide(t, toRun);
  if (!runA) return t.skip(NO_RUN_BASE);
  const homeA = prepareStore(toRun, record('passed', 7));
  runA.useRun();
  let result = toRun.verify();
  assert.equal(result.stdout, passed(1));
  assert.equal(result.status, 0);
  assert.equal(readRecord(homeA), record('invalid', 8));
  assert.equal(readRecord(runA), record('passed', 1));
  // 基点が HOME の側へ切り替わると、/run/user の側の成功を無効にし、HOME の側は世代を進める。
  runA.useHome();
  toRun.set('exit-verify', 1);
  result = toRun.verify();
  assert.equal(result.status, 1);
  assert.equal(readRecord(runA), record('invalid', 2));
  assert.equal(readRecord(homeA), record('failed', 9));
  // 失敗の後に基点が戻っても、どちらの側にも成功は無い。
  runA.useRun();
  fs.rmSync(path.join(toRun.repo, 'scripts/verify.sh'));
  result = toRun.verify();
  assert.equal(result.status, 3);
  assert.equal(readRecord(runA), record('invalid', 3));
  assert.equal(readRecord(homeA), record('invalid', 10));

  // HOME の側が基点のとき（id が無い）、/run/user の側に残った成功を無効にする。
  const toHome = sandbox(t, INTAKE);
  const runB = runSide(t, toHome);
  runB.put(record('passed', 7));
  result = toHome.verify();
  assert.equal(result.stdout, passed(1));
  assert.equal(readRecord(runB), record('invalid', 8));
  assert.equal(readRecord(storePaths(toHome)), record('passed', 1));

  // リポジトリを特定できないときは、両方の側の成功を無効にする。
  const unknown = sandbox(t, null);
  const runC = runSide(t, unknown);
  const homeC = prepareStore(unknown, record('passed', 7));
  runC.put(record('passed', 4));
  for (const use of ['useRun', 'useHome']) {
    runC[use]();
    assert.equal(unknown.verify().status, 3);
  }
  // 既に無効の記録は書き換えない（2回目は世代を進めない）。
  assert.equal(readRecord(runC), record('invalid', 5));
  assert.equal(readRecord(homeC), record('invalid', 8));
});

test('基点の切り替わり: 今の基点でない側の記録を無効にできなければ、検証を実行せず合格にしない', (t) => {
  const first = sandbox(t, INTAKE);
  if (!runSide(t, first)) return t.skip(NO_RUN_BASE);
  // [今の基点, 細工, 理由]
  const cases: any[][] = [
    [
      'run',
      (_box, _side, home) => {
        fs.writeFileSync(home.record, record('passed', '07'));
      },
      BAD_BODY,
    ],
    [
      'run',
      (_box, _side, home) => {
        fs.chmodSync(home.leaf, 0o750);
      },
      DIR_MODE,
    ],
    [
      'run',
      (_box, _side, home) => {
        fs.mkdirSync(home.lock, { mode: 0o700 });
      },
      LOCK_BUSY,
    ],
    [
      'home',
      (_box, side) => {
        fs.writeFileSync(side.record, 'state=passed\n');
      },
      BAD_LINES,
    ],
    [
      'home',
      (_box, side) => {
        fs.chmodSync(side.record, 0o644);
      },
      FILE_MODE,
    ],
    [
      'home',
      (_box, side) => {
        fs.mkdirSync(side.lock, { mode: 0o700 });
      },
      LOCK_BUSY,
    ],
  ];
  for (const [current, damage, reason] of cases) {
    const label = `${current} ${reason}`;
    const box = sandbox(t, INTAKE);
    const side = runSide(t, box);
    const home =
      current === 'run'
        ? prepareStore(box, record('passed', 7))
        : storePaths(box);
    if (current === 'run') side.useRun();
    else side.put(record('passed', 7));
    damage(box, side, home);
    const result = box.verify();
    assert.equal(result.stdout, beginFailed(otherSide(reason)), label);
    assert.equal(result.status, 3, label);
    assert.deepEqual(box.calls(), [], label);
    // 今の基点の側に保存先も記録も作らない。
    assert.equal(
      fs.existsSync(current === 'run' ? side.leaf : home.leaf),
      false,
      label,
    );
  }
});

test('基点の切り替わり: 検証の間に別の基点の側へ書かれた成功は、保存の前に無効にし、両方の側に成功が残ることはない', async (t) => {
  const box = sandbox(t, INTAKE);
  const side = runSide(t, box);
  if (!side) return t.skip(NO_RUN_BASE);
  // /run/user の側を基点に検証を始め、その間に HOME の側へ成功の記録が置かれる。
  side.useRun();
  const runs = gate(t, box);
  const run = await runs.start();
  assert.equal(readRecord(side), record('running', 1));
  const home = prepareStore(box, record('passed', 5));
  const result = await run.finish(0);
  assert.equal(result.stdout, passed(1));
  assert.equal(readRecord(home), record('invalid', 6));
  assert.equal(readRecord(side), record('passed', 1));

  // 別々の基点を選んだ2本が並行すると、互いに相手を無効にするので、どちらも合格にならない。
  const both = sandbox(t, INTAKE);
  const bothSide = runSide(t, both);
  const bothRuns = gate(t, both);
  bothSide.useRun();
  const viaRun = await bothRuns.start();
  bothSide.useHome();
  const viaHome = await bothRuns.start();
  const bothHome = storePaths(both);
  assert.equal(readRecord(bothSide), record('invalid', 2));
  assert.equal(readRecord(bothHome), record('running', 1));
  for (const late of [viaRun, viaHome]) {
    const done = await late.finish(0);
    assert.equal(done.stdout, saveFailed(NOT_LATEST));
    assert.equal(done.status, 3);
  }
  for (const store of [bothSide, bothHome])
    assert.ok(!readRecord(store).includes('state=passed'));
});

test('識別子: 記録が消えて世代が同じ番号に戻っても、先に始めた検証は後から始めた検証の記録へ結果を保存できない', async (t) => {
  for (const newCode of [1, 0]) {
    const box = sandbox(t, INTAKE);
    const store = storePaths(box);
    const runs = gate(t, box);
    const older = await runs.start();
    assert.equal(readRecord(store), record('running', 1));
    const olderId = runOf(store);
    // 記録が消える（/run/user の基点はログアウトで消える。手で消した場合も同じ）。
    fs.rmSync(store.record);
    const newer = await runs.start();
    // 世代は同じ1に戻るが、識別子は実行ごとに違う。
    assert.equal(readRecord(store), record('running', 1));
    const newerId = runOf(store);
    assert.ok(olderId && newerId && olderId !== newerId);
    const late = await older.finish(0);
    assert.equal(late.stdout, saveFailed(NOT_LATEST));
    assert.equal(late.status, 3);
    assert.equal(
      fs.readFileSync(store.record, 'utf8'),
      record('running', 1, INTAKE_REPO, newerId),
    );
    const last = await newer.finish(newCode);
    assert.equal(last.status, newCode);
    assert.match(identityOf(store), /^[0-9a-f]{40}$/);
    assert.equal(
      fs.readFileSync(store.record, 'utf8'),
      record(
        newCode === 0 ? 'passed' : 'failed',
        1,
        INTAKE_REPO,
        newerId,
        identityOf(store),
        `intake-verify:${newCode}`,
      ),
    );
  }
});

test('識別子: 実行ごとに違う値を自分で作り、環境値で指定された値は使わない', (t) => {
  const box = sandbox(t, INTAKE);
  const store = storePaths(box);
  const seen = new Set();
  for (let i = 0; i < 3; i += 1) {
    const result = box.verify([], {
      env: { SRANDOM: '1', RANDOM: '1', VERIFY_RUN_ID: RUN_ID },
    });
    assert.equal(result.stdout, passed(i + 1));
    const id = runOf(store);
    assert.match(id, /^[0-9a-f]{32}$/);
    assert.notEqual(id, RUN_ID);
    assert.notEqual(id, '00000001'.repeat(4));
    seen.add(id);
  }
  assert.equal(seen.size, 3);
  // 点検用の記録も同じ形で書かれ、読み戻せる（点検は成功し、既存の記録を変えない）。
  const before = fs.readFileSync(store.record, 'utf8');
  assert.equal(box.store().stdout, storeOk());
  assert.equal(fs.readFileSync(store.record, 'utf8'), before);
});

test('旧い版の記録: 識別子の行が無い版1・同一性の行が無い版2の成功は、開始時に上書きされ、合格として残らない', async (t) => {
  for (const old of [recordV1, recordV2]) {
    // 点検は旧い版の記録を安全に読め、書き換えない。
    const kept = sandbox(t, INTAKE);
    const keptStore = prepareStore(kept, old('passed', 7));
    assert.equal(kept.store().stdout, storeOk());
    assert.equal(fs.readFileSync(keptStore.record, 'utf8'), old('passed', 7));

    // 検証を始めると、世代を引き継いで今の版の実行中に書き換える。
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, old('passed', 7));
    const run = await gate(t, box).start();
    assert.equal(readRecord(store), record('running', 8));
    assert.notEqual(runOf(store), null);
    assert.notEqual(runOf(store), RUN_ID);
    assert.equal((await run.finish(1)).status, 1);
    assert.equal(readRecord(store), record('failed', 8));

    // リポジトリを特定できないときも、旧い版の成功を今の版の無効に書き換える。
    const unknown = sandbox(t, null);
    const unknownStore = prepareStore(unknown, old('passed', 7));
    assert.equal(unknown.verify().status, 3);
    assert.equal(readRecord(unknownStore), record('invalid', 8));
  }
});

// 入口（verify・verify-store）を待たずに起動する。closed は終了時の状態と出力を返す。
function launch(box: Box, entry: string) {
  const child = spawn(bash, [guard, entry], {
    cwd: box.repo,
    env: box.env(),
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const out = { stdout: '', stderr: '' };
  child.stdout.on('data', (chunk) => {
    out.stdout += chunk;
  });
  child.stderr.on('data', (chunk) => {
    out.stderr += chunk;
  });
  const closed = new Promise<Closed>((resolve, reject) => {
    child.on('error', reject);
    child.on('close', (status, signal) => resolve({ status, signal, ...out }));
  });
  return { child, closed };
}

// state/ に合図のファイルができるまで待つ。
async function waitState(box, name, what) {
  const file = path.join(box.state, name);
  for (let i = 0; i < 400 && !fs.existsSync(file); i += 1)
    await new Promise((resolve) => setTimeout(resolve, 25));
  assert.ok(fs.existsSync(file), what);
}

// 道具（name）を、合図（<name>-release）が置かれるまで終わらない物に替える。止まったら <name>-holding を置く。
// when は止まる条件（sh の式。空なら毎回）。after が真なら本物を実行した後で止まる。
function holdTool(box, name, { when = ':', after = false, real = true } = {}) {
  const run = real ? `'${path.join(system, name)}' "$@"; rc=$?` : 'rc=1';
  const hold = `if ${when}; then : > "$S/${name}-holding"; while [ ! -f "$S/${name}-release" ]; do [ -d "$S" ] || exit 1; '${path.join(system, 'sleep')}' 0.05; done; fi`;
  box.tool(
    name,
    [
      `n=$(( $(cat "$S/${name}-count" 2>/dev/null || echo 0) + 1 ))`,
      `echo "$n" > "$S/${name}-count"`,
      ...(after ? [run, hold] : [hold, run]),
      'exit $rc',
    ].join('\n'),
  );
}

test('中断: 成功を保存した直後に中断の合図が届いても、この実行の記録は無効になり、合格にならない', async (t) => {
  const box = sandbox(t, INTAKE);
  const store = prepareStore(box, record('passed', 7));
  // 記録の置き換え（mv）の2回目＝成功の保存が済んだところで止める（1回目は開始の書き換え）。
  holdTool(box, 'mv', { when: '[ "$n" = 2 ]', after: true });
  const run = await gate(t, box).start();
  box.set('release-1', 0);
  await waitState(box, 'mv-holding', '成功の保存まで進まない');
  // この時点で記録は成功になっている。ここへ中断が届く。
  assert.equal(readRecord(store), record('passed', 8));
  run.child.kill('SIGTERM');
  box.set('mv-release', '');
  const result = await run.finish(0);
  assert.equal(result.stdout, INTERRUPTED);
  assert.equal(result.status, 3);
  assert.equal(readRecord(store), record('invalid', 8));
  assert.deepEqual(tree(store.leaf), ['record']);
});

test('中断: 開始の書き換えの前に中断されたときは旧い記録の無効化を試み、できなかったときは無効と表示しない', async (t) => {
  const head = `必須検証（段1）: 実行不能 repo=${INTAKE_REPO}。中断された。`;
  for (const blocked of [false, true]) {
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, record('passed', 7));
    // 基点を選ぶ id の呼び出し（中断の受付より後、開始の書き換えより前）で止める。
    holdTool(box, 'id', { real: false });
    const run = launch(box, 'verify');
    await waitState(box, 'id-holding', '基点の選択まで進まない');
    assert.equal(fs.readFileSync(store.record, 'utf8'), record('passed', 7));
    if (blocked) fs.mkdirSync(store.lock, { mode: 0o700 });
    run.child.kill('SIGTERM');
    box.set('id-release', '');
    const result = await run.closed;
    assert.equal(result.stderr, '');
    assert.equal(result.status, 3);
    assert.deepEqual(box.calls(), []);
    if (blocked) {
      assert.equal(
        result.stdout,
        `${head}この作業場所の旧い記録を無効にできない（${LOCK_BUSY}）。この検証の結果は合格にしない。\n`,
      );
      assert.equal(fs.readFileSync(store.record, 'utf8'), record('passed', 7));
    } else {
      assert.equal(
        result.stdout,
        `${head}検証を始める前だったため、この作業場所の旧い記録を無効にした。この検証の結果は合格にしない。\n`,
      );
      assert.equal(readRecord(store), record('invalid', 8));
      assert.deepEqual(tree(store.leaf), ['record']);
    }
  }
});

test('中断: ロックを持っている間に中断されても、自分のロックを残さない（点検と、検証を始められない場合の無効化）', async (t) => {
  // 点検（verify-store）: 点検用の記録を置き換えるところで止めて中断する。
  const probing = sandbox(t, INTAKE);
  const probingStore = prepareStore(probing, record('passed', 7));
  holdTool(probing, 'mv');
  let run = launch(probing, 'verify-store');
  await waitState(probing, 'mv-holding', '点検用の記録の置き換えまで進まない');
  assert.equal(
    fs.readFileSync(path.join(probingStore.lock, 'owner'), 'utf8'),
    `${run.child.pid}\n`,
  );
  run.child.kill('SIGTERM');
  probing.set('mv-release', '');
  let result = await run.closed;
  assert.equal(result.stderr, '');
  assert.equal(
    result.stdout,
    `${STORE}: 実行不能 repo=${INTAKE_REPO}。中断された。中断された点検は成功として扱わない。\n`,
  );
  assert.equal(result.status, 3);
  assert.deepEqual(tree(probingStore.leaf), ['record']);
  assert.equal(
    fs.readFileSync(probingStore.record, 'utf8'),
    record('passed', 7),
  );

  // 検証（verify）: リポジトリを特定できず旧い記録を無効にしている最中に中断する。
  const unknown = sandbox(t, null);
  const unknownStore = prepareStore(unknown, record('passed', 7));
  holdTool(unknown, 'mv');
  run = launch(unknown, 'verify');
  await waitState(unknown, 'mv-holding', '旧い記録の無効化まで進まない');
  assert.equal(
    fs.readFileSync(path.join(unknownStore.lock, 'owner'), 'utf8'),
    `${run.child.pid}\n`,
  );
  run.child.kill('SIGTERM');
  unknown.set('mv-release', '');
  result = await run.closed;
  assert.equal(result.stderr, '');
  assert.equal(
    result.stdout,
    '必須検証（段1）: 実行不能。origin からリポジトリを特定できない。\n必須検証（段1）: 実行不能。中断された。検証を始める前だったため、この作業場所の旧い記録を無効にした。この検証の結果は合格にしない。\n',
  );
  assert.equal(result.status, 3);
  assert.equal(readRecord(unknownStore), record('invalid', 8));
  assert.deepEqual(tree(unknownStore.leaf), ['record']);

  // ほかの持ち主のロックは、中断されても消さない。
  const held = sandbox(t, INTAKE);
  const heldStore = prepareStore(held, record('passed', 7));
  fs.mkdirSync(heldStore.lock, { mode: 0o700 });
  fs.writeFileSync(path.join(heldStore.lock, 'owner'), '1\n', { mode: 0o600 });
  holdTool(held, 'sleep');
  run = launch(held, 'verify');
  await waitState(held, 'sleep-holding', 'ロックの待ちまで進まない');
  run.child.kill('SIGTERM');
  held.set('sleep-release', '');
  result = await run.closed;
  assert.equal(result.status, 3);
  assert.deepEqual(tree(heldStore.leaf), ['lock', 'lock/owner', 'record']);
  assert.equal(
    fs.readFileSync(path.join(heldStore.lock, 'owner'), 'utf8'),
    '1\n',
  );
  assert.ok(
    !result.stdout.includes('無効にした') &&
      !result.stdout.includes('無効のまま'),
    result.stdout,
  );
});

// ---- 同一性の記録と検証後の照合（段3/6） ----

const hashText = (text) =>
  spawnSync(realGit, ['hash-object', '--stdin'], {
    input: text,
    encoding: 'utf8',
    cwd: os.tmpdir(),
  }).stdout.trim();
const hashFile = (file) =>
  spawnSync(realGit, ['hash-object', '--no-filters', '--', file], {
    encoding: 'utf8',
    cwd: os.tmpdir(),
  }).stdout.trim();

// Gitのフィクスチャの既定の応答から、ガードが作るはずの同一性の内訳を組み立てる。
function expectedIdentity(
  box: Box,
  {
    repo = INTAKE_REPO,
    origin = INTAKE,
    definition = undefined,
    script = guard,
    ...over
  }: Record<string, any> = {},
) {
  const values = {
    repo,
    worktree: box.repo,
    gitdir: path.join(box.repo, '.git'),
    commondir: path.join(box.repo, '.git'),
    head: HEAD_ID,
    branch: 'refs/heads/main',
    fetch: origin,
    push: origin,
    definition:
      definition ??
      `intake-verify=sh scripts/verify.sh;scripts/verify.sh=${hashFile(path.join(box.repo, 'scripts/verify.sh'))};`,
    guard: hashFile(script),
    content: TREE_ID,
    ...over,
  };
  return (
    'guard-gitea-identity 1\n' +
    Object.entries(values)
      .map(([name, value]) => `${name}=${value}\n`)
      .join('')
  );
}

test('同一性の記録: 成功の記録は、検証した対象の同一性の要約と各検証の結果を持つ', (t) => {
  const box = sandbox(t, INTAKE);
  const store = storePaths(box);
  assert.equal(box.verify().stdout, passed(1));
  assert.equal(identityOf(store), hashText(expectedIdentity(box)));
  assert.equal(resultsOf(store), 'intake-verify:0');
  assert.match(
    fs.readFileSync(store.record, 'utf8'),
    /^guard-gitea-verify 3\nstate=passed\ngeneration=1\nrepo=mofua-suki\/issue-desk-intake\nrun=[0-9a-f]{32}\nidentity=[0-9a-f]{40}\nresults=intake-verify:0\n$/,
  );
  // 内容を読む一時のインデックスは保存先の中に置き、読んだ後に残さない。
  const calls = fs
    .readFileSync(path.join(box.state, 'index-calls'), 'utf8')
    .trim()
    .split('\n');
  assert.equal(calls.length, 4);
  for (const line of calls)
    assert.match(
      line,
      new RegExp(
        `^(read-tree|add) ${store.leaf.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}/identity\\.[0-9]+$`,
      ),
    );
  assert.deepEqual(tree(store.leaf), ['record']);

  // 同じ対象をもう一度検証すれば同じ要約になり、対象が違えば要約も違う。
  const first = identityOf(store);
  assert.equal(box.verify().stdout, passed(2));
  assert.equal(identityOf(store), first);
  box.set('id-head', '4'.repeat(40) + '\n');
  assert.equal(box.verify().stdout, passed(3));
  assert.equal(
    identityOf(store),
    hashText(expectedIdentity(box, { head: '4'.repeat(40) })),
  );
  assert.notEqual(identityOf(store), first);
  // 先頭がブランチを指していない状態・送信用のURLが複数ある状態も、そのまま同一性の一部として記録する。
  fs.rmSync(path.join(box.state, 'id-branch'));
  box.set(
    'id-push',
    `${INTAKE}\nssh://git@mirror.example/mofua-suki/issue-desk-intake.git\n`,
  );
  assert.equal(box.verify().stdout, passed(4));
  assert.equal(
    identityOf(store),
    hashText(
      expectedIdentity(box, {
        head: '4'.repeat(40),
        branch: '(detached)',
        push: `${INTAKE} ssh://git@mirror.example/mofua-suki/issue-desk-intake.git`,
      }),
    ),
  );

  // Workerは3つの検証の結果を順に持つ。scripts/verify.sh は検証定義に含めない。
  const worker = sandbox(t, WORKER);
  const workerStore = storePaths(worker);
  assert.equal(worker.verify().status, 0);
  assert.equal(resultsOf(workerStore), 'go-test:0,go-vet:0,gofmt:0');
  assert.equal(
    identityOf(workerStore),
    hashText(
      expectedIdentity(worker, {
        repo: 'mofua-suki/issue-desk-worker',
        origin: WORKER,
        definition:
          'go-test=go test ./...;go-vet=go vet ./...;gofmt=gofmt -l .（出力が空であること）;',
      }),
    ),
  );
  // 失敗・実行不能の記録にも、始めたときの同一性と各検証の結果が残る（成功にはならない）。
  worker.set('exit-go-vet', 2);
  worker.set('out-gofmt', 'main.go\n');
  assert.equal(worker.verify().status, 1);
  assert.equal(
    readRecord(workerStore),
    record('failed', 2, 'mofua-suki/issue-desk-worker'),
  );
  assert.equal(resultsOf(workerStore), 'go-test:0,go-vet:2,gofmt:1');
  assert.match(identityOf(workerStore), /^[0-9a-f]{40}$/);
  const missing = sandbox(t, INTAKE);
  fs.rmSync(path.join(missing.repo, 'scripts/verify.sh'));
  assert.equal(missing.verify().status, 3);
  assert.equal(readRecord(storePaths(missing)), record('invalid', 1));
  assert.equal(resultsOf(storePaths(missing)), 'intake-verify:x');
  assert.equal(
    identityOf(storePaths(missing)),
    hashText(
      expectedIdentity(missing, {
        definition:
          'intake-verify=sh scripts/verify.sh;scripts/verify.sh=(none);',
      }),
    ),
  );
});

test('検証後の照合: 検証の間に対象のどれかが変わったら、検証が成功しても合格にせず、成功を記録しない', async (t) => {
  const cases: any[][] = [
    [
      '先頭コミット',
      (box) => box.set('id-head', '5'.repeat(40) + '\n'),
      '先頭コミット',
    ],
    [
      'ブランチの切り替え',
      (box) => box.set('id-branch', 'refs/heads/other\n'),
      'ブランチ',
    ],
    [
      'ブランチを指さなくなった',
      (box) => fs.rmSync(path.join(box.state, 'id-branch')),
      'ブランチ',
    ],
    [
      '管理場所',
      (box) => box.set('id-gitdir', path.join(box.tmp, 'elsewhere.git') + '\n'),
      '管理場所',
    ],
    [
      '共有の管理場所',
      (box) => box.set('id-commondir', path.join(box.tmp, 'common.git') + '\n'),
      '管理場所',
    ],
    [
      '取得用のURL（同じリポジトリの別の書き方）',
      (box) =>
        box.set(
          'origin',
          'http://nas.example:3000/mofua-suki/issue-desk-intake\n',
        ),
      '送信先',
    ],
    [
      '送信用のURL',
      (box) => box.set('id-push', 'gitea-nas:someone/elsewhere.git\n'),
      '送信先',
    ],
    [
      '送信用のURLの追加',
      (box) =>
        box.set('id-push', `${INTAKE}\ngitea-nas:someone/elsewhere.git\n`),
      '送信先',
    ],
    ['内容', (box) => box.set('id-tree', '6'.repeat(40) + '\n'), '内容'],
    [
      '検証定義',
      (box) =>
        fs.appendFileSync(
          path.join(box.repo, 'scripts/verify.sh'),
          '# changed\n',
        ),
      '検証定義',
    ],
    [
      '複数',
      (box) => {
        box.set('id-head', '5'.repeat(40) + '\n');
        box.set('id-branch', 'refs/heads/other\n');
        box.set('id-tree', '6'.repeat(40) + '\n');
      },
      '先頭コミット・ブランチ・内容',
    ],
  ];
  for (const [label, change, names] of cases) {
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, record('passed', 7));
    const run = await gate(t, box).start();
    change(box);
    const result = await run.finish(0);
    assert.equal(result.stdout, identityChanged(names), label);
    assert.equal(result.status, 3, label);
    // 記録は無効で、始めたときの同一性と検証の結果を持つ。旧い成功も新しい成功も残らない。
    assert.equal(readRecord(store), record('invalid', 8), label);
    assert.equal(resultsOf(store), 'intake-verify:0', label);
    assert.match(identityOf(store), /^[0-9a-f]{40}$/, label);
    assert.deepEqual(tree(store.leaf), ['record'], label);
    assert.deepEqual(box.calls(), ['verify 1'], label);
  }

  // フック（このガード自身）の内容が検証の間に変わった場合。
  const box = sandbox(t, INTAKE);
  const store = storePaths(box);
  const copy = path.join(box.tmp, 'guard-copy.sh');
  fs.copyFileSync(guard, copy);
  const run = await gate(t, box).start(copy);
  fs.appendFileSync(copy, '# changed\n');
  const result = await run.finish(0);
  assert.equal(result.stdout, identityChanged('フック'));
  assert.equal(result.status, 3);
  assert.equal(readRecord(store), record('invalid', 1));

  // 変わっても検証が失敗していれば不合格のまま（照合は成功を合格にしないためのもの）。
  const failing = sandbox(t, INTAKE);
  const failingRun = await gate(t, failing).start();
  failing.set('id-head', '5'.repeat(40) + '\n');
  const failed = await failingRun.finish(1);
  assert.equal(failed.status, 1);
  assert.equal(readRecord(storePaths(failing)), record('failed', 1));

  // 検証の間に変わって元へ戻った場合は、前後が同じなので合格になる（途中の変化は見ていない）。
  const restored = sandbox(t, INTAKE);
  const restoredRun = await gate(t, restored).start();
  restored.set('id-head', '5'.repeat(40) + '\n');
  restored.set('id-head', HEAD_ID + '\n');
  assert.equal((await restoredRun.finish(0)).stdout, passed(1));
});

test('照合不能: 同一性を読めない・要約できないときは、検証を実行しないか、成功を合格にしない', async (t) => {
  const HEAD_UNREADABLE = '先頭コミットを読めない';
  const cases: any[][] = [
    [
      '管理場所を読めない',
      (box) => fs.rmSync(path.join(box.state, 'id-gitdir')),
      '管理場所（Gitのディレクトリ）を読めない',
    ],
    [
      '管理場所が絶対パスでない',
      (box) => box.set('id-gitdir', '.git\n'),
      '管理場所（Gitのディレクトリ）を読めない',
    ],
    [
      '共有の管理場所を読めない',
      (box) => fs.rmSync(path.join(box.state, 'id-commondir')),
      '管理場所（Gitのディレクトリ）を読めない',
    ],
    [
      '先頭コミットが無い',
      (box) => fs.rmSync(path.join(box.state, 'id-head')),
      HEAD_UNREADABLE,
    ],
    [
      '先頭コミットが決まった形でない',
      (box) => box.set('id-head', 'HEAD\n'),
      HEAD_UNREADABLE,
    ],
    [
      '先頭コミットが大文字',
      (box) => box.set('id-head', 'A'.repeat(40) + '\n'),
      HEAD_UNREADABLE,
    ],
    ['先頭コミットが空', (box) => box.set('id-head', ''), HEAD_UNREADABLE],
    [
      'ブランチの読取りに失敗',
      (box) => box.set('id-branch-broken', ''),
      'ブランチを読めない',
    ],
    [
      'ブランチが決まった形でない',
      (box) => box.set('id-branch', 'main\n'),
      'ブランチを読めない',
    ],
    [
      '送信用のURLが空',
      (box) => box.set('id-push', ''),
      '送信先（origin）を読めない',
    ],
    [
      '送信用のURLに制御文字',
      (box) =>
        box.set('id-push', 'gitea-nas:mofua-suki/issue-desk-intake.git\tx\n'),
      '送信先（origin）を読めない',
    ],
    [
      '内容を読めない',
      (box) => box.set('id-index-broken', ''),
      '作業場所の内容を読めない',
    ],
    [
      '内容の木が決まった形でない',
      (box) => box.set('id-tree', 'tree\n'),
      '作業場所の内容を読めない',
    ],
    [
      '内容の木が無い',
      (box) => fs.rmSync(path.join(box.state, 'id-tree')),
      '作業場所の内容を読めない',
    ],
    [
      '要約を作れない',
      (box) => box.set('id-hash-broken', ''),
      '検証定義（scripts/verify.sh）を読めない',
    ],
  ];
  for (const [label, damage, reason] of cases) {
    // 開始時: 検証を1つも実行せず、旧い成功は無効になる。
    const early = sandbox(t, INTAKE);
    const earlyStore = prepareStore(early, record('passed', 7));
    damage(early);
    const refused = early.verify();
    assert.equal(refused.stdout, identityBeginFailed(reason), label);
    assert.equal(refused.status, 3, label);
    assert.deepEqual(early.calls(), [], label);
    assert.equal(readRecord(earlyStore), record('invalid', 8), label);
    assert.equal(identityOf(earlyStore), '-', label);
    assert.deepEqual(tree(earlyStore.leaf), ['record'], label);

    // 検証の後: 成功していても合格にせず、記録を無効にする。
    const late = sandbox(t, INTAKE);
    const lateStore = prepareStore(late, record('passed', 7));
    const run = await gate(t, late).start();
    damage(late);
    const result = await run.finish(0);
    assert.equal(
      result.stdout,
      identityFailed(`検証の後の同一性を確かめられない（${reason}）`),
      label,
    );
    assert.equal(result.status, 3, label);
    assert.equal(readRecord(lateStore), record('invalid', 8), label);
    assert.deepEqual(tree(lateStore.leaf), ['record'], label);
  }

  // 一時のインデックスが作られても、読めたかどうかに依らず残さない。前からある同じ名前の物は自分の物ではないので使わない。
  for (const broken of [false, true]) {
    const box = sandbox(t, INTAKE);
    const store = storePaths(box);
    box.set('id-index-leave', '');
    if (broken) box.set('id-tree', 'tree\n');
    assert.equal(box.verify().status, broken ? 3 : 0);
    assert.ok(
      fs
        .readFileSync(path.join(box.state, 'index-calls'), 'utf8')
        .includes('/identity.'),
    );
    assert.deepEqual(tree(store.leaf), ['record']);
  }

  // 前から在る同じ名前の物（本体・.lock）は使わず、消しも書き換えもしない。成功していても合格にしない。
  for (const suffix of ['', '.lock']) {
    const box = sandbox(t, INTAKE);
    const store = storePaths(box);
    gate(t, box);
    const run = launch(box, 'verify');
    await waitState(box, 'started-1', '検証まで進まない');
    const callsBefore = fs.readFileSync(
      path.join(box.state, 'index-calls'),
      'utf8',
    );
    const placed = path.join(store.leaf, `identity.${run.child.pid}${suffix}`);
    fs.writeFileSync(placed, 'not mine\n', { mode: 0o600 });
    box.set('release-1', '0');
    const result = await run.closed;
    assert.equal(
      result.stdout,
      identityFailed(
        '検証の後の同一性を確かめられない（内容を読むための一時ファイルを置けない）',
      ),
      suffix,
    );
    assert.equal(result.status, 3, suffix);
    assert.equal(readRecord(store), record('invalid', 1), suffix);
    assert.equal(fs.readFileSync(placed, 'utf8'), 'not mine\n', suffix);
    assert.deepEqual(
      tree(store.leaf).sort(),
      [`identity.${run.child.pid}${suffix}`, 'record'],
      suffix,
    );
    // 前から在る物をインデックスとして Git に渡していない。
    assert.equal(
      fs.readFileSync(path.join(box.state, 'index-calls'), 'utf8'),
      callsBefore,
      suffix,
    );
  }
});

test('同一性 実際の git: 作業場所のインデックスを変えずに内容を読み、検証の間の変更・コミット・切り替えを見つける', async (t) => {
  const git = (box, ...args) => {
    const result = spawnSync(
      realGit,
      [
        '-c',
        'user.name=Test',
        '-c',
        'user.email=test@example.invalid',
        ...args,
      ],
      {
        cwd: box.repo,
        env: { HOME: box.home, PATH: process.env.PATH },
        encoding: 'utf8',
      },
    );
    assert.equal(result.status, 0, `${args.join(' ')}: ${result.stderr}`);
    return result.stdout;
  };
  // 実際の git のリポジトリ（origin は Intake）で、同期用フィクスチャの検証を使う。
  const realRepo = (commit = true) => {
    const box = sandbox(t, null);
    fs.rmSync(path.join(box.bin, 'git'));
    fs.symlinkSync(realGit, path.join(box.bin, 'git'));
    const runs = gate(t, box);
    git(box, 'init', '-q', '-b', 'main');
    git(box, 'remote', 'add', 'origin', INTAKE);
    fs.writeFileSync(path.join(box.repo, '.gitignore'), 'ignored.txt\n');
    fs.writeFileSync(path.join(box.repo, 'tracked.txt'), 'one\n');
    if (commit) {
      git(box, 'add', '-A');
      git(box, 'commit', '-q', '-m', 'first');
    }
    // 未コミットの変更（変更済み・ステージ済み・未追跡）がある状態から始める。
    fs.writeFileSync(path.join(box.repo, 'untracked.txt'), 'new\n');
    return { box, runs };
  };
  const cases: any[][] = [
    ['変更なし', () => {}, null],
    [
      '無視されているファイルの変更（同一性に含めない）',
      (box) => fs.writeFileSync(path.join(box.repo, 'ignored.txt'), 'x\n'),
      null,
    ],
    [
      '追跡ファイルの変更',
      (box) => fs.appendFileSync(path.join(box.repo, 'tracked.txt'), 'two\n'),
      '内容',
    ],
    [
      '追跡ファイルの削除',
      (box) => fs.rmSync(path.join(box.repo, 'tracked.txt')),
      '内容',
    ],
    [
      '未追跡ファイルの追加',
      (box) => fs.writeFileSync(path.join(box.repo, 'another.txt'), 'x\n'),
      '内容',
    ],
    [
      '未追跡ファイルの変更',
      (box) =>
        fs.appendFileSync(path.join(box.repo, 'untracked.txt'), 'more\n'),
      '内容',
    ],
    [
      '内容を変えないコミット',
      (box) => git(box, 'commit', '-q', '--allow-empty', '-m', 'empty'),
      '先頭コミット',
    ],
    [
      '未コミットの変更のコミット（作業場所の中身は同じ）',
      (box) => {
        git(box, 'add', '-A');
        git(box, 'commit', '-q', '-m', 'second');
      },
      '先頭コミット',
    ],
    [
      'ブランチの作成と切り替え',
      (box) => git(box, 'checkout', '-q', '-b', 'other'),
      'ブランチ',
    ],
    [
      '先頭をブランチから外す',
      (box) => git(box, 'checkout', '-q', '--detach'),
      'ブランチ',
    ],
    [
      '送信用のURLの変更',
      (box) =>
        git(
          box,
          'remote',
          'set-url',
          '--push',
          'origin',
          'gitea-nas:someone/elsewhere.git',
        ),
      '送信先',
    ],
    [
      'originのURLの書き方の変更',
      (box) =>
        git(
          box,
          'remote',
          'set-url',
          'origin',
          'ssh://git@nas.example:2222/mofua-suki/issue-desk-intake.git',
        ),
      '送信先',
    ],
    [
      '検証定義（scripts/verify.sh）の変更',
      (box) =>
        fs.appendFileSync(
          path.join(box.repo, 'scripts/verify.sh'),
          '# changed\n',
        ),
      '検証定義・内容',
    ],
  ];
  for (const [label, change, names] of cases) {
    const { box, runs } = realRepo();
    const store = storePaths(box);
    const before = git(box, 'status', '--porcelain');
    assert.ok(before.includes('?? untracked.txt'), before);
    const run = await runs.start();
    // 内容を読んでも、作業場所のインデックス（何がステージ済みか）は変わらない。
    assert.equal(git(box, 'status', '--porcelain'), before, label);
    change(box);
    const result = await run.finish(0);
    if (names === null) {
      assert.equal(result.stdout, passed(1), label);
      assert.equal(result.status, 0, label);
      assert.match(identityOf(store), /^[0-9a-f]{40}$/, label);
      assert.equal(resultsOf(store), 'intake-verify:0', label);
    } else {
      assert.equal(result.stdout, identityChanged(names), label);
      assert.equal(result.status, 3, label);
      assert.equal(readRecord(store), record('invalid', 1), label);
    }
    assert.deepEqual(tree(store.leaf), ['record'], label);
  }

  // 同じ内容なら実行をまたいで同じ要約になり、内容が違えば違う要約になる。
  const { box, runs } = realRepo();
  const store = storePaths(box);
  assert.equal((await (await runs.start()).finish(0)).status, 0);
  const first = identityOf(store);
  assert.equal((await (await runs.start()).finish(0)).status, 0);
  assert.equal(identityOf(store), first);
  fs.appendFileSync(path.join(box.repo, 'untracked.txt'), 'more\n');
  assert.equal((await (await runs.start()).finish(0)).status, 0);
  assert.notEqual(identityOf(store), first);

  // コミットが1つも無いリポジトリでは先頭コミットを読めないので、検証を実行しない。
  const unborn = realRepo(false);
  const refused = unborn.box.verify();
  assert.equal(refused.stdout, identityBeginFailed('先頭コミットを読めない'));
  assert.equal(refused.status, 3);
  assert.deepEqual(unborn.box.calls(), []);
});

test('中断: プロセスの組の全員へ中断の合図が届いても、ロックの解放が途中で止まらず、ロックを残さない', async (t) => {
  for (const entry of ['verify-store', 'verify']) {
    const box = sandbox(t, INTAKE);
    const store = prepareStore(box, record('passed', 7));
    // ロックの解放（rmdir）の最中で止める。合図はガードと道具の両方に届く（端末の Ctrl+C と同じ）。
    holdTool(box, 'rmdir');
    const child = spawn(bash, [guard, entry], {
      cwd: box.repo,
      env: box.env(),
      stdio: ['ignore', 'pipe', 'pipe'],
      detached: true,
    });
    let stdout = '';
    child.stdout.on('data', (chunk) => {
      stdout += chunk;
    });
    const closed = new Promise<Closed>((resolve, reject) => {
      child.on('error', reject);
      child.on('close', (status, signal) => resolve({ status, signal }));
    });
    await waitState(box, 'rmdir-holding', 'ロックの解放まで進まない');
    assert.ok(fs.existsSync(store.lock));
    for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP'])
      process.kill(-child.pid, signal);
    // 合図が届いても、解放の道具は止まらずに待ち続けている。
    await new Promise((resolve) => setTimeout(resolve, 100));
    assert.ok(fs.existsSync(store.lock));
    box.set('rmdir-release', '');
    const result = await closed;
    assert.equal(result.status, 3, `${entry}: ${stdout}`);
    assert.ok(stdout.includes('中断された'), stdout);
    assert.ok(!fs.existsSync(store.lock), entry);
    assert.deepEqual(tree(store.leaf), ['record'], entry);
    assert.deepEqual(box.calls(), [], entry);
    assert.notEqual(readRecord(store), record('running', 8));
    assert.ok(
      !fs.readFileSync(store.record, 'utf8').includes('state=passed') ||
        entry === 'verify-store',
      entry,
    );
  }
});

test('コード評価なし: 記録に埋めたコード相当のデータは、固定入口でも実行されず、読めない記録として検証を始めない', (t) => {
  const payloads = (file) => [
    `guard-gitea-verify 1\nstate=$(touch ${file})\ngeneration=1\nrepo=a/b\n`,
    `guard-gitea-verify 1\nstate=passed; touch ${file}\ngeneration=1\nrepo=a/b\n`,
    `guard-gitea-verify 1\nstate=passed\ngeneration=a[$(touch ${file})]\nrepo=a/b\n`,
    `guard-gitea-verify 1\nstate=passed\ngeneration=1+$(touch ${file})\nrepo=a/b\n`,
    `guard-gitea-verify 1\nstate=passed\ngeneration=1\nrepo=a/\`touch ${file}\`\n`,
    `guard-gitea-verify 1\nstate=passed\ngeneration=1\nrepo=a/b\ntouch ${file}\n`,
    `VERIFY_RUN_GEN=1\nVERIFY_REC_STATE=passed\ntouch ${file}\nexit 0\n`,
  ];
  for (let i = 0; i < payloads('').length; i += 1) {
    const box = sandbox(t, INTAKE);
    const pwned = path.join(box.state, 'pwned');
    const text = payloads(pwned)[i];
    const store = prepareStore(box, text);
    const result = box.verify([], { input: text });
    assert.equal(result.status, 3, text);
    assert.ok(
      result.stdout.startsWith(
        `必須検証（段1）: 実行不能 repo=${INTAKE_REPO}。検証の記録を開始できない（`,
      ),
      result.stdout,
    );
    assert.ok(!fs.existsSync(pwned), text);
    assert.equal(readRecord(store), text);
    assert.deepEqual(box.calls(), []);
  }
  // 標準入力や環境値で記録の中身を申告しても、記録は検証の結果だけで決まる。
  const box = sandbox(t, INTAKE);
  box.set('exit-verify', 1);
  const result = box.verify([], {
    input: record('passed', 99),
    env: {
      VERIFY_RUN_GEN: '99',
      VERIFY_REC_STATE: 'passed',
      VERIFY_STORE: box.tmp,
      VERIFY_LOCK: box.tmp,
    },
  });
  assert.equal(result.status, 1);
  assert.equal(readRecord(storePaths(box)), record('failed', 1));
  assert.deepEqual(tree(box.tmp), []);
});
