import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  existsSync,
  rmSync,
  copyFileSync,
  chmodSync,
  symlinkSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const source = process.cwd();
const a = 'a'.repeat(40),
  b = 'b'.repeat(40),
  c = 'c'.repeat(40);
function fixture(t) {
  const root = mkdtempSync(path.join(tmpdir(), 'desk-pin-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const home = path.join(root, 'home'),
    dir = path.join(root, 'repo'),
    bin = path.join(root, 'bin');
  for (const p of [
    home,
    bin,
    path.join(dir, 'deploy/nas'),
    path.join(dir, 'deploy/clients/remote-control'),
    path.join(root, 'tree/scripts'),
    path.join(root, 'tree/shared'),
    path.join(root, 'tree/deploy'),
  ])
    mkdirSync(p, { recursive: true });
  for (const p of [
    'deploy/nas/update-desk-access.sh',
    'deploy/nas/update-desk-scripts.sh',
    'deploy/nas/desk-pc-routes.json',
    'deploy/clients/remote-control/desk-access.sh',
  ])
    copyFileSync(path.join(source, p), path.join(dir, p));
  writeFileSync(path.join(root, 'tree/scripts/desk-client.ts'), 'export {}');
  writeFileSync(path.join(root, 'tree/deploy/versions.json'), '{}');
  writeFileSync(
    path.join(dir, 'deploy/nas/intake.sh'),
    'printf \'{"revision":"%s"}\\n\' "$REVISION"\n',
  );
  writeFileSync(
    path.join(bin, 'git'),
    '#!/bin/sh\n[ "${FAIL_ARCHIVE:-0}" = 0 ] || exit 1\nexec tar -cf "$6" -C "$TREE" .\n',
  );
  chmodSync(path.join(bin, 'git'), 0o755);
  // update-desk-access expects the update.sh logging functions.
  const invoke = (revision, extra = {}) =>
    spawnSync(
      'sh',
      [
        '-c',
        'say() { :; }; plan() { :; }; . "$DIR/deploy/nas/update-desk-access.sh"; desk_access',
      ],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          HOME: home,
          DIR: dir,
          DRY_RUN: '0',
          REVISION: revision,
          TREE: path.join(root, 'tree'),
          PATH: `${bin}:${process.env.PATH}`,
          ...extra,
        },
      },
    );
  return { home, dir, bin, root, invoke };
}

test('NAS窓口は見張りの状態をホーム内に限定して起動する', (t) => {
  const f = fixture(t);
  assert.equal(f.invoke(a).status, 0);
  writeFileSync(path.join(f.dir, 'deploy/nas/intake.sh'), 'echo intake-blue\n');
  const log = path.join(f.root, 'runs');
  writeFileSync(
    path.join(f.bin, 'docker'),
    `#!/bin/sh\ncase "$1" in\n inspect) case "$3" in *Config.Env*) printf 'ISSUE_DESK_APP_STORE=postgres\\nISSUE_DESK_DATABASE_URL=test\\n\\n';; *) echo image;; esac;;\n run) echo run >> "$RUN_LOG";;\nesac\n`,
    { mode: 0o755 },
  );
  symlinkSync(f.root, path.join(f.home, 'outside'));
  symlinkSync(f.home, path.join(f.root, 'inside'));
  symlinkSync('loop', path.join(f.home, 'loop'));
  const run = (script, ...args) =>
    spawnSync('sh', [path.join(f.home, '.issue-desk/desk'), script, ...args], {
      cwd: f.home,
      encoding: 'utf8',
      env: {
        ...process.env,
        HOME: f.home,
        PATH: `${f.bin}:${process.env.PATH}`,
        ISSUE_DESK_NAS_DIR: f.dir,
        RUN_LOG: log,
      },
    });
  for (const state of [
    '/tmp/x.json',
    `${f.home}/../outside.json`,
    `${f.home}/outside/new/state.json`,
    `${f.home}/loop/state.json`,
  ]) {
    const result = run('follow-issues.ts', '--desk', '1', '--state', state);
    assert.equal(result.status, 1, state);
    assert.match(result.stderr, /ホームの下/, state);
    assert.equal(existsSync(log), false, state);
  }
  for (const state of [
    `${f.home}/state.json`,
    `${f.home}/new/parent/state.json`,
    `${f.root}/inside/new/state.json`,
  ]) {
    const result = run(
      'follow-issues.ts',
      '--desk',
      '1',
      '--state',
      state,
      '--interval',
      '30',
    );
    assert.equal(result.status, 0, result.stderr);
  }
  assert.equal(
    run(
      'list-issues.ts',
      '--duplicates',
      '--project',
      'owner/repo',
      '--query',
      'test',
    ).status,
    0,
  );
  assert.equal(readFileSync(log, 'utf8').trim().split('\n').length, 4);
});

test('PCトークン窓口は未作成の見張り状態もホーム内に限定する', (t) => {
  const f = fixture(t);
  assert.equal(f.invoke(a).status, 0);
  const direct = path.join(f.home, '.issue-desk/desk');
  const install = spawnSync(
    'sh',
    [
      '-c',
      'say() { :; }; plan() { :; }; . "$1"; linux_desk_access',
      'sh',
      path.join(source, 'deploy/clients/remote-control/desk-access.sh'),
    ],
    { env: { ...process.env, HOME: f.home, DIR: f.dir, DRY_RUN: '0' } },
  );
  assert.equal(install.status, 0);
  writeFileSync(path.join(f.root, 'tree/scripts/follow-issues.ts'), '');
  // 配置済みの正本を使う。
  writeFileSync(
    path.join(f.home, '.issue-desk/checkout', a, 'scripts/follow-issues.ts'),
    '',
  );
  const log = path.join(f.root, 'pc-runs');
  writeFileSync(
    path.join(f.bin, 'docker'),
    '#!/bin/sh\ncase "$1" in ps) echo abc123;; inspect) echo "image|volume:issue-desk-worker-state|true";; run) echo run >> "$RUN_LOG";; esac\n',
    { mode: 0o755 },
  );
  symlinkSync(f.root, path.join(f.home, 'outside'));
  symlinkSync(f.home, path.join(f.root, 'inside'));
  symlinkSync('loop', path.join(f.home, 'loop'));
  for (const desk of [direct, path.join(f.home, '.issue-desk/desk-pc')]) {
    const run = (script, ...args) =>
      spawnSync('sh', [desk, script, ...args], {
        cwd: f.home,
        encoding: 'utf8',
        env: {
          ...process.env,
          HOME: f.home,
          PATH: `${f.bin}:${process.env.PATH}`,
          RUN_LOG: log,
        },
      });
    for (const state of [
      path.join(f.root, 'missing/state.json'),
      `${f.home}/../outside.json`,
      `${f.home}/outside/new/state.json`,
      `${f.home}/loop/state.json`,
      'relative.json',
    ]) {
      const result = run('follow-issues.ts', '--desk', '1', '--state', state);
      assert.equal(result.status, 1, `${state}: ${result.stderr}`);
      assert.equal(existsSync(log), false);
    }
    for (const state of [
      `${f.home}/new/state.json`,
      `${f.root}/inside/new/state.json`,
    ]) {
      const result = run('follow-issues.ts', '--desk', '1', '--state', state);
      assert.equal(result.status, 0, result.stderr);
    }
    assert.equal(run('doctor').status, 0);
    rmSync(log);
  }
});

test('稼働版への追従と旧向け先からの移行、使用中の写しの保護', (t) => {
  const { home, invoke } = fixture(t);
  const base = path.join(home, '.issue-desk'),
    checkout = path.join(base, 'checkout');
  mkdirSync(base, { recursive: true });
  const old = path.join(home, 'old-repository');
  writeFileSync(path.join(base, 'scripts-root'), `${old}\n`);
  assert.equal(invoke(a).status, 0);
  assert.equal(
    readFileSync(path.join(base, 'scripts-root'), 'utf8'),
    `${checkout}/${a}\n${old}\n`,
  );
  mkdirSync(path.join(checkout, '.paced-use', a), { recursive: true });
  writeFileSync(path.join(checkout, '.paced-use', a, 'record'), 'in use');
  assert.equal(invoke(b).status, 0);
  assert.equal(invoke(c).status, 0);
  assert.equal(
    readFileSync(path.join(base, 'scripts-root'), 'utf8'),
    `${checkout}/${c}\n${checkout}/${b}\n`,
  );
  assert.ok(existsSync(path.join(checkout, a)));
  rmSync(path.join(checkout, '.paced-use', a), { recursive: true });
  assert.equal(invoke(c).status, 0);
  assert.ok(!existsSync(path.join(checkout, a)));
});

test('取得失敗、稼働版不明、排他中は前の向け先を保持する', (t) => {
  const { home, invoke } = fixture(t);
  assert.equal(invoke(a).status, 0);
  const base = path.join(home, '.issue-desk'),
    file = path.join(base, 'scripts-root');
  const before = readFileSync(file, 'utf8');
  assert.notEqual(invoke(b, { FAIL_ARCHIVE: '1' }).status, 0);
  assert.equal(readFileSync(file, 'utf8'), before);
  assert.equal(invoke('').status, 0);
  assert.equal(readFileSync(file, 'utf8'), before);
  mkdirSync(path.join(base, 'checkout/.paced-lock'));
  assert.equal(invoke(b).status, 0);
  assert.equal(readFileSync(file, 'utf8'), before);
});

test('desk-client が .ts の版だけを写しとして受け付け、改名前の .mjs だけの版へは向け先を移さない', (t) => {
  const { home, root, invoke } = fixture(t);
  const file = path.join(home, '.issue-desk', 'scripts-root'),
    checkout = path.join(home, '.issue-desk', 'checkout');
  assert.equal(invoke(a).status, 0);
  assert.equal(readFileSync(file, 'utf8'), `${checkout}/${a}\n`);
  rmSync(path.join(root, 'tree/scripts/desk-client.ts'));
  writeFileSync(path.join(root, 'tree/scripts/desk-client.mjs'), 'export {}');
  assert.notEqual(invoke(b).status, 0);
  assert.equal(
    readFileSync(file, 'utf8'),
    `${checkout}/${a}\n`,
    '改名前の版へは向け先を移さない',
  );
  assert.ok(!existsSync(path.join(checkout, b)));
});
