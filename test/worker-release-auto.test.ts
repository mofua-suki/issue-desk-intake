import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { execFileSync, spawnSync } from 'node:child_process';

// deploy/nas/update-worker-release.sh: Workerのtrunkの先頭を発行し、PCごとの望む版として登録する。
const script = path.resolve('deploy/nas/update-worker-release.sh');
const git = (cwd, ...args) =>
  execFileSync(
    'git',
    ['-c', 'user.name=t', '-c', 'user.email=t@example.com', ...args],
    { cwd, encoding: 'utf8' },
  ).trim();

function fixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'worker-release-'));
  const remote = path.join(root, 'worker');
  const dir = path.join(root, 'nas-dir');
  const bin = path.join(root, 'bin');
  fs.mkdirSync(remote);
  fs.mkdirSync(path.join(dir, 'deploy/nas'), { recursive: true });
  fs.mkdirSync(bin);
  git(remote, 'init', '-q', '-b', 'trunk');
  fs.writeFileSync(path.join(remote, 'Dockerfile.tools'), 'FROM scratch\n');
  fs.mkdirSync(path.join(remote, 'internal'));
  fs.writeFileSync(path.join(remote, 'internal/a.go'), 'package a\n');
  git(remote, 'add', '-A');
  git(remote, 'commit', '-q', '-m', 'base');
  const base = git(remote, 'rev-parse', 'HEAD');
  const commit = (file, content) => {
    fs.mkdirSync(path.dirname(path.join(remote, file)), { recursive: true });
    fs.writeFileSync(path.join(remote, file), content);
    git(remote, 'add', '-A');
    git(remote, 'commit', '-q', '-m', file);
    return git(remote, 'rev-parse', 'HEAD');
  };
  fs.writeFileSync(
    path.join(dir, 'deploy/nas/worker-release.json'),
    JSON.stringify({
      remote: 'unused',
      branch: 'trunk',
      flavor: '-python',
      pushRegistry: '127.0.0.1:5000',
      hosts: { nas: '127.0.0.1:5000', gt12: 'tail.example:5443' },
    }),
  );
  const log = path.join(root, 'calls.log');
  const registers = path.join(root, 'registers.jsonl');
  fs.writeFileSync(
    path.join(dir, 'deploy/nas/intake.sh'),
    `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
const i = args.indexOf('client-release');
const action = args[i + 1];
const prev = ${JSON.stringify(path.join(root, 'prev'))};
if (action === 'previous') { process.stdout.write(fs.existsSync(prev) ? fs.readFileSync(prev, 'utf8') : ''); }
else if (action === 'register') {
  const body = fs.readFileSync(0, 'utf8');
  if (process.env.REGISTER_FAIL) { console.error('register failed'); process.exit(1); }
  fs.appendFileSync(${JSON.stringify(registers)}, body + '\\n');
  fs.writeFileSync(prev, JSON.parse(body).revision);
  if (process.env.REGISTER_WARN) console.log('警告: ' + process.env.REGISTER_WARN);
  console.log('望む版を登録しました。');
} else process.exit(9);
`,
  );
  // sh intake.sh を node スクリプトとして実行させる
  fs.writeFileSync(
    path.join(bin, 'sh'),
    `#!/bin/sh
case "$1" in */intake.sh) s=$1; shift; exec ${process.execPath} "$s" "$@";; esac
exec /bin/sh "$@"
`,
  );
  fs.writeFileSync(
    path.join(bin, 'docker'),
    `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
fs.appendFileSync(${JSON.stringify(log)}, 'docker ' + args.join(' ') + '\\n');
if (args[0] === 'image') { const image = args[2]; process.exit(process.env.NO_TOOLS || (image !== ${JSON.stringify('issue-desk-tools:')} + ${JSON.stringify(base.slice(0, 12))} && image !== process.env.EXISTING_TOOLS && !fs.existsSync(${JSON.stringify(path.join(root, 'built-tools'))} + '-' + image.split(':').pop())) ? 1 : 0); }
if (args[0] === 'build') { fs.readFileSync(0); if (process.env.BUILD_FAIL || (args.includes('Dockerfile.tools') && process.env.TOOLS_BUILD_FAIL)) process.exit(1); if (args.includes('Dockerfile.tools')) fs.writeFileSync(${JSON.stringify(path.join(root, 'built-tools'))} + '-' + args[args.indexOf('-t') + 1].split(':').pop(), '1'); process.exit(0); }
if (args[0] === 'push') { fs.writeFileSync(${JSON.stringify(path.join(root, 'published'))}, '1'); process.exit(0); }
process.exit(9);
`,
  );
  fs.writeFileSync(
    path.join(bin, 'curl'),
    `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
const published = fs.existsSync(${JSON.stringify(path.join(root, 'published'))});
if (args.includes('-w')) { process.stdout.write(published ? '200' : '404'); process.exit(0); }
if (args.includes('-D')) { process.stdout.write('HTTP/1.1 200 OK\\r\\nDocker-Content-Digest: sha256:' + 'a'.repeat(64) + '\\r\\n\\r\\n'); process.exit(0); }
process.exit(0);
`,
  );
  for (const name of ['sh', 'docker', 'curl'])
    fs.chmodSync(path.join(bin, name), 0o755);
  const run = (extra = {}) => {
    const r = spawnSync(
      '/bin/sh',
      [
        '-c',
        `. "${script}"; say() { echo "$*"; }; plan() { echo "$*"; }; worker_release`,
      ],
      {
        env: {
          ...process.env,
          PATH: bin + ':' + process.env.PATH,
          DIR: dir,
          DRY_RUN: '0',
          TOOLS_IMAGE: 'issue-desk-tools:' + base.slice(0, 12),
          ISSUE_DESK_WORKER_REMOTE: remote,
          ...extra,
        },
        encoding: 'utf8',
      },
    );
    return r;
  };
  const calls = () => (fs.existsSync(log) ? fs.readFileSync(log, 'utf8') : '');
  const body = () =>
    fs.existsSync(registers)
      ? fs
          .readFileSync(registers, 'utf8')
          .split('\n')
          .filter(Boolean)
          .map((l) => JSON.parse(l))
      : [];
  return {
    root,
    dir,
    remote,
    base,
    commit,
    run,
    calls,
    body,
    state: path.join(dir, 'runtime/worker-release'),
  };
}
const cleanup = (f) => fs.rmSync(f.root, { recursive: true, force: true });
const ageRegistered = (f, seconds = 3700) => {
  const time = new Date(Date.now() - seconds * 1000);
  fs.utimesSync(path.join(f.state, 'registered'), time, time);
};

function ignoredRules(classify) {
  assert.equal(
    (classify.match(/\bcontinue\b/g) || []).length,
    1,
    'continue節は1つだけ',
  );
  const match = classify.match(/if\s+([^{}]+)\{\s*continue\s*\}/);
  assert.ok(match, '無視する条件を検出できる');
  const rules = { exact: [], prefixes: [], suffixes: [] };
  for (const term of match[1].split('||')) {
    const rule = term
      .trim()
      .match(
        /^(?:file == "([^"\n]+)"|strings\.HasPrefix\(file, "([^"\n]+)"\)|strings\.HasSuffix\(file, "([^"\n]+)"\))$/,
      );
    assert.ok(rule, `未対応の無視条件: ${term.trim()}`);
    if (rule[1]) rules.exact.push(rule[1]);
    else if (rule[2]) rules.prefixes.push(rule[2]);
    else rules.suffixes.push(rule[3]);
  }
  for (const values of Object.values(rules)) values.sort();
  return rules;
}

test('配備する設定にUTF-8 BOMがあっても全PCに登録する', () => {
  const f = fixture();
  try {
    const configBytes = fs.readFileSync('deploy/nas/worker-release.json');
    const config = JSON.parse(configBytes.toString('utf8'));
    fs.writeFileSync(
      path.join(f.dir, 'deploy/nas/worker-release.json'),
      Buffer.concat([Buffer.from([0xef, 0xbb, 0xbf]), configBytes]),
    );
    const sha = f.commit('internal/b.go', 'package b\n');
    const r = f.run();
    assert.equal(r.status, 0, r.stderr);
    assert.match(r.stdout, /\[update:worker-release-success\]/);
    const bodies = f.body();
    assert.equal(bodies.length, Object.keys(config.hosts).length);
    assert.deepEqual(
      Object.fromEntries(bodies.map((b) => [b.host, b.registry])),
      config.hosts,
    );
    for (const body of bodies) {
      assert.equal(body.revision, sha);
      assert.equal(body.flavor, config.flavor);
    }
    assert.equal(
      fs.readFileSync(path.join(f.state, 'registered'), 'utf8').trim(),
      sha,
    );
  } finally {
    cleanup(f);
  }
});

test('BOM付き設定のremoteを環境変数なしで読み、配備設定の全PCに登録する', () => {
  const f = fixture();
  try {
    const config = JSON.parse(
      fs.readFileSync('deploy/nas/worker-release.json', 'utf8'),
    );
    config.remote = f.remote;
    fs.writeFileSync(
      path.join(f.dir, 'deploy/nas/worker-release.json'),
      '\ufeff' + JSON.stringify(config),
    );
    const sha = f.commit('internal/b.go', 'package b\n');
    const r = f.run({ ISSUE_DESK_WORKER_REMOTE: '' });
    assert.equal(r.status, 0, r.stderr);
    assert.match(r.stdout, /\[update:worker-release-success\]/);
    assert.deepEqual(
      Object.fromEntries(f.body().map((b) => [b.host, b.registry])),
      config.hosts,
    );
    assert.ok(f.body().every((b) => b.revision === sha));
    assert.equal(
      fs.readFileSync(path.join(f.state, 'registered'), 'utf8').trim(),
      sha,
    );
  } finally {
    cleanup(f);
  }
});

test('hostsが空なら登録済みにせず失敗を記録する', () => {
  const f = fixture();
  try {
    const configPath = path.join(f.dir, 'deploy/nas/worker-release.json');
    const config = JSON.parse(fs.readFileSync(configPath, 'utf8'));
    config.hosts = {};
    fs.writeFileSync(configPath, '\ufeff' + JSON.stringify(config));
    f.commit('internal/b.go', 'package b\n');
    const r = f.run();
    assert.match(
      r.stdout,
      /\[update:worker-release-failed\] 登録するPCがありません/,
    );
    assert.equal(f.body().length, 0);
    assert.ok(!fs.existsSync(path.join(f.state, 'registered')));
    assert.ok(fs.existsSync(path.join(f.state, 'failed')));
  } finally {
    cleanup(f);
  }
});

test('hostsが配列ならキー一覧として扱わず失敗する', () => {
  const f = fixture();
  try {
    const configPath = path.join(f.dir, 'deploy/nas/worker-release.json');
    const config = JSON.parse(fs.readFileSync(configPath, 'utf8'));
    config.hosts = ['nas', 'gt12'];
    fs.writeFileSync(configPath, JSON.stringify(config));
    f.commit('internal/b.go', 'package b\n');
    const r = f.run();
    assert.equal(r.status, 0);
    assert.match(
      r.stdout,
      /\[update:worker-release-failed\] 設定の hosts を読めません/,
    );
    assert.equal(f.body().length, 0);
    assert.ok(!fs.existsSync(path.join(f.state, 'registered')));
    assert.ok(fs.existsSync(path.join(f.state, 'failed')));
  } finally {
    cleanup(f);
  }
});

test('ドットを含むhostsのキーは他のPCの登録前に拒否する', () => {
  const f = fixture();
  try {
    const configPath = path.join(f.dir, 'deploy/nas/worker-release.json');
    const config = JSON.parse(fs.readFileSync(configPath, 'utf8'));
    config.hosts['pc.example'] = 'tail.example:5443';
    fs.writeFileSync(configPath, JSON.stringify(config));
    f.commit('internal/b.go', 'package b\n');
    const r = f.run();
    assert.equal(r.status, 0);
    assert.match(
      r.stdout,
      /\[update:worker-release-failed\] 設定の hosts のキーにドットは使えません: pc\.example/,
    );
    assert.equal(f.body().length, 0);
    assert.ok(!fs.existsSync(path.join(f.state, 'registered')));
    assert.ok(fs.existsSync(path.join(f.state, 'failed')));
  } finally {
    cleanup(f);
  }
});

test('trunkの先頭を発行し、PCごとのレジストリ名で登録する', () => {
  const f = fixture();
  try {
    const sha = f.commit('internal/b.go', 'package b\n');
    const r = f.run();
    assert.equal(r.status, 0, r.stderr);
    assert.match(r.stdout, /\[update:worker-release-success\].*2台/);
    assert.match(
      f.calls(),
      /docker build --target client-python .*ISSUE_DESK_REVISION=/,
    );
    assert.match(
      f.calls(),
      /docker push 127\.0\.0\.1:5000\/issue-desk-client-python:[0-9a-f]{12}-client-python/,
    );
    const bodies = f.body();
    assert.equal(bodies.length, 2);
    const byHost = Object.fromEntries(bodies.map((b) => [b.host, b]));
    assert.equal(
      byHost.nas.registry,
      '127.0.0.1:5000',
      'NASは自分のレジストリ名',
    );
    assert.equal(
      byHost.gt12.registry,
      'tail.example:5443',
      '他のPCはtailnetの名前',
    );
    for (const b of bodies) {
      assert.equal(b.revision, sha);
      assert.equal(b.flavor, '-python');
      assert.equal(b.tag, sha.slice(0, 12) + '-client-python');
      assert.match(b.digest, /^sha256:a{64}$/);
    }
    assert.equal(
      fs.readFileSync(path.join(f.state, 'registered'), 'utf8').trim(),
      sha,
    );
  } finally {
    cleanup(f);
  }
});

test('登録済みの版と同じなら何もしない', () => {
  const f = fixture();
  try {
    f.commit('internal/b.go', 'package b\n');
    assert.equal(f.run().status, 0);
    const before = f.calls();
    const again = f.run();
    assert.equal(again.status, 0);
    assert.equal(again.stdout.trim(), '');
    assert.equal(f.calls(), before, '2回目はdockerを呼ばない');
    assert.equal(f.body().length, 2);
  } finally {
    cleanup(f);
  }
});

test('60分未満の変更を待ち、経過後に前回を基点としてまとめて登録する', () => {
  const f = fixture();
  try {
    const first = f.commit('internal/b.go', 'package b\n');
    assert.match(f.run().stdout, /\[update:worker-release-success\]/);
    const before = f.calls();
    f.commit('internal/c.go', 'package c\n');
    const wait = f.run({ NO_TOOLS: '1' });
    assert.match(
      wait.stdout,
      /\[update:worker-release-waiting\].*60分未満.*次に登録できる時刻:/,
    );
    assert.equal(f.calls(), before);
    assert.equal(f.body().length, 2);
    assert.equal(
      fs.readFileSync(path.join(f.state, 'registered'), 'utf8').trim(),
      first,
    );
    const last = f.commit('internal/d.go', 'package d\n');
    assert.match(f.run().stdout, /\[update:worker-release-waiting\]/);
    ageRegistered(f);
    assert.match(f.run().stdout, /\[update:worker-release-success\]/);
    for (const body of f.body().slice(-2)) {
      assert.equal(body.revision, last);
      assert.equal(body.baseRevision, first);
      assert.deepEqual(body.changedFiles, ['internal/c.go', 'internal/d.go']);
    }
  } finally {
    cleanup(f);
  }
});

test('無視対象はalignにせず、intakeの実装変更は待たずに登録する', () => {
  const f = fixture();
  try {
    assert.match(f.run().stdout, /\[update:worker-release-success\]/);
    f.commit('internal/intake/a_test.go', 'package intake\n');
    f.commit('internal/intake/README.md', 'readme\n');
    f.commit('docs/README.md', 'docs\n');
    assert.match(f.run().stdout, /\[update:worker-release-waiting\]/);
    const sha = f.commit('internal/intake/a.go', 'package intake\n');
    assert.match(f.run().stdout, /\[update:worker-release-success\]/);
    assert.ok(
      f
        .body()
        .slice(-2)
        .every((b) => b.revision === sha),
    );
  } finally {
    cleanup(f);
  }
});

test('待機のalign一覧は受付のclassifyReleaseと一致する', () => {
  const go = fs.readFileSync('server/internal/hostd/register.go', 'utf8');
  const classify = go.match(
    /func classifyRelease\(files \[\]string\) string \{([\s\S]*?)\n\}/,
  )[1];
  const align = classify
    .match(/\n\s*if ([\s\S]*?)\{\s*return "align"/)[1]
    .split('continue')[1];
  const goExact = [...align.matchAll(/file == "([^"]+)"/g)]
    .map((m) => m[1])
    .sort();
  const goPrefixes = [
    ...align.matchAll(/strings\.HasPrefix\(file, "([^"]+)"\)/g),
  ]
    .map((m) => m[1])
    .sort();
  const scriptText = fs.readFileSync(script, 'utf8');
  const values = (name) =>
    [
      ...scriptText
        .match(new RegExp(`^    ${name} = \\(([^\\n]*)\\)`, 'm'))[1]
        .matchAll(/b"([^"]+)"/g),
    ]
      .map((m) => m[1])
      .sort();
  assert.deepEqual(values('align_files'), goExact);
  assert.deepEqual(values('align_prefixes'), goPrefixes);
  assert.ok(goExact.length && goPrefixes.length);
  const ignored = ignoredRules(classify);
  assert.deepEqual(values('ignore_exact'), ignored.exact);
  assert.deepEqual(values('ignore_prefixes'), ignored.prefixes);
  assert.deepEqual(values('ignore_suffixes'), ignored.suffixes);
});

test('待機時刻は実行環境のタイムゾーンに関係なく日本標準時で出す', () => {
  const f = fixture();
  try {
    assert.match(f.run().stdout, /\[update:worker-release-success\]/);
    f.commit('internal/c.go', 'package c\n');
    const registered = path.join(f.state, 'registered');
    const timestamp = new Date(Math.floor(Date.now() / 1000) * 1000 - 100000);
    fs.utimesSync(registered, timestamp, timestamp);
    const expected =
      new Date(timestamp.getTime() + 3600000 + 9 * 3600000)
        .toISOString()
        .replace('T', ' ')
        .slice(0, 19) + ' +0900';
    for (const TZ of ['UTC', 'America/New_York']) {
      const result = f.run({ TZ });
      assert.equal(result.status, 0, result.stderr);
      assert.ok(
        result.stdout.includes('次に登録できる時刻: ' + expected),
        result.stdout,
      );
    }
  } finally {
    cleanup(f);
  }
});

test('待機判定のgit diff失敗時は待たずに登録する', () => {
  const f = fixture();
  try {
    assert.match(f.run().stdout, /\[update:worker-release-success\]/);
    const sha = f.commit('internal/c.go', 'package c\n');
    const realGit = execFileSync('which', ['git'], { encoding: 'utf8' }).trim();
    const marker = path.join(f.root, 'diff-failed');
    const wrapper = path.join(f.root, 'bin/git');
    fs.writeFileSync(
      wrapper,
      `#!/bin/sh
# 判定はサブシェルで行い、元の引数をgitに渡す。
(
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -C|-c|--git-dir|--work-tree|--namespace|--config-env)
        [ "$#" -ge 2 ] || exit 2
        shift 2 ;;
      -C?*|-c?*) shift ;;
      --git-dir=*|--work-tree=*|--namespace=*|--config-env=*) shift ;;
      --bare|--no-replace-objects|--literal-pathspecs|--glob-pathspecs|--noglob-pathspecs|--icase-pathspecs|--no-optional-locks|--no-lazy-fetch|--no-advice|--paginate|--no-pager|-p|-P) shift ;;
      -*) echo "未対応のgit大域オプション: $1" >&2; exit 2 ;;
      *) break ;;
    esac
  done
  [ "$1" = diff ] && [ ! -e "$DIFF_FAIL_MARKER" ]
)
case "$?" in
  0) : > "$DIFF_FAIL_MARKER"; exit 1 ;;
  2) exit 2 ;;
esac
# Git自体が連結形式を受け付けない環境でも、先頭の大域オプションを分離する。
remaining=$#
global=1
value=0
while [ "$remaining" -gt 0 ]; do
  if [ "$value" -eq 1 ]; then
    set -- "$@" "$1"; value=0
  elif [ "$global" -eq 0 ]; then
    set -- "$@" "$1"
  else
    case "$1" in
      -C|-c|--git-dir|--work-tree|--namespace|--config-env) set -- "$@" "$1"; value=1 ;;
      -C?*) set -- "$@" -C "\${1#-C}" ;;
      -c?*) set -- "$@" -c "\${1#-c}" ;;
      -*) set -- "$@" "$1" ;;
      *) set -- "$@" "$1"; global=0 ;;
    esac
  fi
  shift
  remaining=$((remaining - 1))
done
exec "$REAL_GIT" "$@"
`,
    );
    fs.chmodSync(wrapper, 0o755);
    git(f.remote, 'branch', 'diff');
    const env = { ...process.env, REAL_GIT: realGit, DIFF_FAIL_MARKER: marker };
    const other = spawnSync(
      wrapper,
      ['--git-dir', path.join(f.remote, '.git'), 'rev-parse', 'diff'],
      { env, encoding: 'utf8' },
    );
    assert.equal(other.status, 0, other.stderr);
    assert.ok(!fs.existsSync(marker), '引数のdiffでは失敗しない');
    const diff = spawnSync(
      wrapper,
      [`--git-dir=${path.join(f.remote, '.git')}`, 'diff'],
      { env, encoding: 'utf8' },
    );
    assert.equal(diff.status, 1);
    assert.ok(fs.existsSync(marker), 'サブコマンドのdiffを失敗させる');
    fs.rmSync(marker);
    for (const options of [
      [
        '-C',
        f.remote,
        '-c',
        'test.value=diff',
        '--git-dir',
        path.join(f.remote, '.git'),
      ],
      [
        '--no-pager',
        `--git-dir=${path.join(f.remote, '.git')}`,
        '-c',
        'test.value=diff',
        '-C',
        f.remote,
      ],
      [`-C${f.remote}`, '-ctest.value=diff'],
      ['-ctest.value=diff', `-C${f.remote}`, '--no-pager'],
    ]) {
      const nonDiff = spawnSync(wrapper, [...options, 'rev-parse', 'diff'], {
        env,
        encoding: 'utf8',
      });
      assert.equal(nonDiff.status, 0, nonDiff.stderr);
      assert.ok(
        !fs.existsSync(marker),
        'オプション値や引数のdiffでは失敗しない',
      );
      const intercepted = spawnSync(wrapper, [...options, 'diff'], {
        env,
        encoding: 'utf8',
      });
      assert.equal(intercepted.status, 1);
      assert.ok(fs.existsSync(marker), '大域オプションの後のdiffを失敗させる');
      fs.rmSync(marker);
    }
    const unknown = spawnSync(wrapper, ['--unknown-option', 'diff'], {
      env,
      encoding: 'utf8',
    });
    assert.equal(unknown.status, 2);
    assert.match(unknown.stderr, /未対応のgit大域オプション/);
    assert.ok(!fs.existsSync(marker));
    const result = f.run({ REAL_GIT: realGit, DIFF_FAIL_MARKER: marker });
    assert.equal(result.status, 0, result.stderr);
    assert.ok(fs.existsSync(marker));
    assert.doesNotMatch(result.stdout, /\[update:worker-release-waiting\]/);
    assert.match(result.stdout, /\[update:worker-release-success\]/);
    assert.ok(
      f
        .body()
        .slice(-2)
        .every((body) => body.revision === sha),
    );
    assert.equal(
      fs.readFileSync(path.join(f.state, 'registered'), 'utf8').trim(),
      sha,
    );
  } finally {
    cleanup(f);
  }
});

test('未来の登録時刻やミラーにない登録版は待たずに登録を試みる', () => {
  for (const invalid of ['future', 'missing']) {
    const f = fixture();
    try {
      assert.match(f.run().stdout, /\[update:worker-release-success\]/);
      f.commit('internal/c.go', 'package c\n');
      const registered = path.join(f.state, 'registered');
      if (invalid === 'future') {
        const future = new Date(Date.now() + 3600000);
        fs.utimesSync(registered, future, future);
      } else fs.writeFileSync(registered, 'f'.repeat(40) + '\n');
      const result = f.run();
      assert.doesNotMatch(result.stdout, /\[update:worker-release-waiting\]/);
      assert.match(result.stdout, /\[update:worker-release-success\]/);
    } finally {
      cleanup(f);
    }
  }
});

test('新しいコミットは、前回の版を基点に変更ファイルを付けて登録する', () => {
  const f = fixture();
  try {
    const first = f.commit('internal/b.go', 'package b\n');
    assert.equal(f.run().status, 0);
    ageRegistered(f);
    fs.rmSync(path.join(f.root, 'published'));
    const second = f.commit('internal/c.go', 'package c\n');
    const r = f.run();
    assert.equal(r.status, 0, r.stderr);
    const latest = f.body().slice(-2);
    for (const b of latest) {
      assert.equal(b.revision, second);
      assert.equal(b.baseRevision, first);
      assert.deepEqual(b.changedFiles, ['internal/c.go']);
    }
  } finally {
    cleanup(f);
  }
});

test('基点は受付の共通の行ではなく、前回この段が登録した版にする', () => {
  const f = fixture();
  try {
    const first = f.commit('internal/b.go', 'package b\n');
    assert.equal(f.run().status, 0);
    ageRegistered(f);
    // 受付の共通（*）の行の版は別の版を指しているが、基点には使わない
    fs.writeFileSync(path.join(f.root, 'prev'), f.base);
    fs.rmSync(path.join(f.root, 'published'));
    f.commit('internal/c.go', 'package c\n');
    assert.equal(f.run().status, 0);
    for (const b of f.body().slice(-2)) assert.equal(b.baseRevision, first);
  } finally {
    cleanup(f);
  }
});

test('登録で出た警告を更新のログへ出す', () => {
  const f = fixture();
  try {
    f.commit('internal/b.go', 'package b\n');
    const r = f.run({ REGISTER_WARN: 'PC専用の行が優先されます' });
    assert.match(
      r.stdout,
      /\[update:worker-release-warning\] PC専用の行が優先されます/,
    );
  } finally {
    cleanup(f);
  }
});

test('ビルド失敗は登録せず、同じ版を1時間は再試行しない。更新は止めない', () => {
  const f = fixture();
  try {
    f.commit('internal/b.go', 'package b\n');
    const r = f.run({ BUILD_FAIL: '1' });
    assert.equal(r.status, 0, '常に0を返す');
    assert.match(
      r.stdout,
      /\[update:worker-release-failed\] ビルドに失敗しました/,
    );
    assert.equal(f.body().length, 0);
    assert.ok(!fs.existsSync(path.join(f.state, 'registered')));
    const builds = (f.calls().match(/docker build/g) || []).length;
    const again = f.run({ BUILD_FAIL: '1' });
    assert.equal(again.stdout.trim(), '', '1時間以内は黙って飛ばす');
    assert.equal((f.calls().match(/docker build/g) || []).length, builds);
    // 1時間たてば再試行する
    const [sha] = fs
      .readFileSync(path.join(f.state, 'failed'), 'utf8')
      .split(' ');
    fs.writeFileSync(
      path.join(f.state, 'failed'),
      `${sha} ${Math.floor(Date.now() / 1000) - 3700}\n`,
    );
    f.run({ BUILD_FAIL: '1' });
    assert.equal((f.calls().match(/docker build/g) || []).length, builds + 1);
  } finally {
    cleanup(f);
  }
});

test('登録の失敗は、登録済みの印を付けない', () => {
  const f = fixture();
  try {
    f.commit('internal/b.go', 'package b\n');
    const r = f.run({ REGISTER_FAIL: '1' });
    assert.equal(r.status, 0);
    assert.match(
      r.stdout,
      /\[update:worker-release-failed\] .*への登録に失敗しました/,
    );
    assert.ok(!fs.existsSync(path.join(f.state, 'registered')));
  } finally {
    cleanup(f);
  }
});

test('ツール定義が変わったらツールイメージを作り直して発行する', () => {
  const f = fixture();
  try {
    const envFile = path.join(f.dir, 'deploy/nas/.env');
    fs.writeFileSync(
      envFile,
      `ISSUE_DESK_TOOLS_IMAGE=issue-desk-tools:${f.base.slice(0, 12)}\n`,
    );
    const before = fs.readFileSync(envFile, 'utf8');
    const sha = f.commit('Dockerfile.tools', 'FROM scratch\nRUN true\n');
    const image = `issue-desk-tools:${sha.slice(0, 12)}`;
    const r = f.run();
    assert.equal(r.status, 0, r.stderr);
    assert.match(r.stdout, /\[update:worker-release-success\]/);
    const calls = f.calls();
    const tools = calls.indexOf(
      `docker build -f Dockerfile.tools --build-arg ISSUE_DESK_TOOLS_REVISION=${sha} -t ${image} -`,
    );
    const worker = calls.indexOf(`TOOLS_IMAGE=${image}`);
    const push = calls.indexOf('docker push ');
    assert.ok(tools >= 0 && worker > tools && push > worker, calls);
    assert.deepEqual(
      f
        .body()
        .map((b) => b.host)
        .sort(),
      ['gt12', 'nas'],
    );
    assert.equal(
      fs.readFileSync(path.join(f.state, 'tools-image'), 'utf8'),
      image + '\n',
    );
    assert.equal(fs.readFileSync(envFile, 'utf8'), before);
  } finally {
    cleanup(f);
  }
});

test('ツールイメージの構築失敗では登録を変えない', () => {
  const f = fixture();
  try {
    fs.mkdirSync(f.state, { recursive: true });
    const registered = path.join(f.state, 'registered');
    const imageFile = path.join(f.state, 'tools-image');
    fs.writeFileSync(registered, f.base + '\n');
    ageRegistered(f);
    fs.writeFileSync(
      imageFile,
      'issue-desk-tools:' + f.base.slice(0, 12) + '\n',
    );
    const sha = f.commit('Dockerfile.tools', 'changed\n');
    const r = f.run({ TOOLS_BUILD_FAIL: '1' });
    assert.equal(r.status, 0);
    assert.match(
      r.stdout,
      /\[update:worker-release-failed\].*tools-build\.log/,
    );
    assert.equal(f.body().length, 0);
    const calls = f.calls();
    assert.match(calls, /docker build -f Dockerfile\.tools /);
    assert.doesNotMatch(calls, /docker build [^\n]*--build-arg TOOLS_IMAGE=/);
    assert.doesNotMatch(calls, /docker push /);
    assert.equal(fs.readFileSync(registered, 'utf8'), f.base + '\n');
    assert.equal(
      fs.readFileSync(imageFile, 'utf8'),
      'issue-desk-tools:' + f.base.slice(0, 12) + '\n',
    );
    assert.equal(
      fs.readFileSync(path.join(f.state, 'failed'), 'utf8').split(' ')[0],
      sha,
    );
    const before = f.calls();
    f.run({ TOOLS_BUILD_FAIL: '1' });
    assert.equal(f.calls(), before);
  } finally {
    cleanup(f);
  }
});

test('同じタグのツールイメージがあれば作り直さない', () => {
  const f = fixture();
  try {
    const sha = f.commit('Dockerfile.tools', 'changed\n');
    const image = 'issue-desk-tools:' + sha.slice(0, 12);
    assert.match(
      f.run({ EXISTING_TOOLS: image }).stdout,
      /\[update:worker-release-success\]/,
    );
    assert.doesNotMatch(f.calls(), /docker build -f Dockerfile.tools/);
    assert.ok(f.calls().includes('TOOLS_IMAGE=' + image));
    assert.equal(f.body().length, 2);
  } finally {
    cleanup(f);
  }
});

test('状態ファイルが無い・壊れているときは.envの値を基点にする', () => {
  for (const state of ['missing', 'invalid', 'absent-image']) {
    const f = fixture();
    try {
      fs.mkdirSync(f.state, { recursive: true });
      if (state !== 'missing')
        fs.writeFileSync(
          path.join(f.state, 'tools-image'),
          state === 'invalid' ? 'invalid\n' : 'issue-desk-tools:ffffffffffff\n',
        );
      f.commit('internal/b.go', 'package b\n');
      assert.match(f.run().stdout, /\[update:worker-release-success\]/);
      assert.ok(
        f
          .calls()
          .includes('TOOLS_IMAGE=issue-desk-tools:' + f.base.slice(0, 12)),
      );
      ageRegistered(f);
      fs.rmSync(path.join(f.root, 'published'));
      const sha = f.commit('Dockerfile.tools', 'changed\n');
      assert.match(f.run().stdout, /\[update:worker-release-success\]/);
      ageRegistered(f);
      fs.rmSync(path.join(f.root, 'published'));
      f.commit('internal/c.go', 'package c\n');
      const before = (
        f.calls().match(/docker build -f Dockerfile.tools/g) || []
      ).length;
      assert.match(f.run().stdout, /\[update:worker-release-success\]/);
      assert.equal(
        (f.calls().match(/docker build -f Dockerfile.tools/g) || []).length,
        before,
      );
      assert.ok(
        f.calls().includes('TOOLS_IMAGE=issue-desk-tools:' + sha.slice(0, 12)),
      );
    } finally {
      cleanup(f);
    }
  }
});

test('基点がミラーにない場合は対象版のツールイメージを作る', () => {
  const f = fixture();
  try {
    const sha = f.commit('internal/b.go', 'package b\n');
    const old = 'issue-desk-tools:ffffffffffff';
    const r = f.run({ TOOLS_IMAGE: old, EXISTING_TOOLS: old });
    assert.match(r.stdout, /\[update:worker-release-success\]/);
    assert.ok(
      f
        .calls()
        .includes(
          `docker build -f Dockerfile.tools --build-arg ISSUE_DESK_TOOLS_REVISION=${sha} -t issue-desk-tools:${sha.slice(0, 12)} -`,
        ),
    );
    assert.equal(f.body().length, 2);
  } finally {
    cleanup(f);
  }
});

test('Dockerfile.toolsが使うファイルの変更を検出する', () => {
  for (const file of [
    'deploy/install-caddy.sh',
    'deploy/install-node.sh',
    'deploy/check-image-tools.mjs',
    'deploy/check-image-tools.mts',
    'deploy/image-versions.mjs',
    'deploy/tools-policy.test.mjs',
    'deploy/agy.sh',
    'deploy/install-sdks.mjs',
    'deploy/sdk-release.json',
    'internal/b.go',
  ]) {
    const f = fixture();
    try {
      f.commit(file, 'changed\n');
      assert.match(f.run().stdout, /\[update:worker-release-success\]/, file);
      assert.equal(
        (f.calls().match(/docker build -f Dockerfile.tools/g) || []).length,
        file === 'internal/b.go' ? 0 : 1,
        file,
      );
    } finally {
      cleanup(f);
    }
  }
});

test('ツールイメージが無いときはビルドせず止める', () => {
  const f = fixture();
  try {
    f.commit('internal/b.go', 'package b\n');
    const r = f.run({ NO_TOOLS: '1' });
    assert.match(r.stdout, /\[update:worker-release-failed\] ツールイメージ/);
    assert.ok(!/docker build/.test(f.calls()));
  } finally {
    cleanup(f);
  }
});

test('--dry-run は予定だけ出して何も変えない', () => {
  const f = fixture();
  try {
    f.commit('internal/b.go', 'package b\n');
    const r = f.run({ DRY_RUN: '1' });
    assert.match(r.stdout, /予定: Workerのtrunk/);
    assert.equal(f.calls(), '');
    assert.ok(!fs.existsSync(f.state));
  } finally {
    cleanup(f);
  }
});

test('設定ファイルが無ければ何もしない', () => {
  const f = fixture();
  try {
    fs.rmSync(path.join(f.dir, 'deploy/nas/worker-release.json'));
    const r = f.run();
    assert.equal(r.status, 0);
    assert.equal(r.stdout.trim(), '');
    assert.equal(f.calls(), '');
  } finally {
    cleanup(f);
  }
});

test('実際の設定は、NASだけ自分のレジストリ名で宣言している', () => {
  const conf = JSON.parse(
    fs.readFileSync('deploy/nas/worker-release.json', 'utf8'),
  );
  assert.equal(conf.hosts.nas, '127.0.0.1:5000');
  for (const [host, registry] of Object.entries(conf.hosts)) {
    if (host !== 'nas')
      assert.equal(registry, 'ugreen-nas.example.ts.net:5443', host);
  }
  assert.ok(
    conf.hosts.gt12 &&
      conf.hosts.a9max &&
      conf.hosts['x1-pro1'] &&
      conf.hosts['x1-pro2'],
  );
});
