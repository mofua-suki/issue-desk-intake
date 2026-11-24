import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const root = path.resolve(import.meta.dirname, '..');
const scripts = {
  'macbook-pro-test': 'deploy/clients/macbook-pro-setup.sh',
  'macbook-air-test': 'deploy/clients/macbook-air-setup.sh',
};
const read = (file) => fs.readFileSync(path.join(root, file), 'utf8');

test('各Macのセットアップは登録済みのテスト専用IDとホームを使い、ID以外は同じ内容にする', () => {
  const fleet = JSON.parse(read('fleet.json'));
  const assignments = JSON.parse(read('assignments.json'));
  const normalized = Object.entries(scripts).map(([host, file]) => {
    const source = read(file);
    assert.match(source, new RegExp(`^HOST_ID=${host}$`, 'm'));
    const home = source.match(/^REGISTERED_HOME=(.+)$/m)[1];
    assert.equal(
      fleet.hosts[host].runtimeDir,
      `${home}/.issue-desk/apple/runtime`,
    );
    assert.deepEqual(fleet.hosts[host].repositories[0].nativeToolRoots, [
      `${home}/development/flutter`,
    ]);
    assert.equal(assignments.hosts[host].role, 'test-only');
    return source
      .replaceAll(host, 'ID')
      .replace(/^# MacBook \w+/m, '# MacBook')
      .replaceAll(path.basename(file), 'SETUP')
      .replace(/^REGISTERED_HOME=.+$/m, 'REGISTERED_HOME=HOME');
  });
  assert.equal(normalized[0], normalized[1]);
});

// Macのコマンドを偽物に差し替え、Linux上で準備の全段を流す。
function fakeMac(
  t,
  {
    selected = '/Applications/Xcode.app/Contents/Developer',
    flutter = '3.47.9',
    flutterFails = false,
  } = {},
) {
  fs.mkdirSync(path.join(root, '.tmp'), { recursive: true });
  const dir = fs.mkdtempSync(path.join(root, '.tmp', 'mac-setup-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const bin = path.join(dir, 'bin'),
    home = path.join(dir, 'home'),
    go = path.join(dir, 'go');
  fs.mkdirSync(path.join(go, 'go/bin'), { recursive: true });
  fs.mkdirSync(bin);
  fs.mkdirSync(home);
  fs.writeFileSync(
    path.join(go, 'go/bin/go'),
    '#!/bin/sh\n[ "$1" = env ] && echo go1.27.0 || echo "go version go1.27.0 darwin/arm64"\n',
    { mode: 0o755 },
  );
  const tarball = path.join(dir, 'go.tar.gz');
  assert.equal(spawnSync('tar', ['-czf', tarball, '-C', go, 'go']).status, 0);
  const shim = (name, body) =>
    fs.writeFileSync(path.join(bin, name), `#!/bin/sh\n${body}\n`, {
      mode: 0o755,
    });
  shim('uname', '[ "$1" = -m ] && echo arm64 || echo Darwin');
  shim('xcode-select', `echo ${selected}`);
  shim('xcodebuild', 'echo "Xcode 26.0"');
  shim('xcrun', 'echo /sdk');
  shim('shasum', 'sha256sum "$3"');
  shim(
    'curl',
    `out=''; url=''; while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift;; http*) url=$1;; esac; shift; done
echo "$url" >> "${dir}/curl.log"
case "$url" in *.sha256) sha256sum "${tarball}" | cut -d ' ' -f 1;; *) cp "${tarball}" "$out";; esac`,
  );
  shim(
    'git',
    `case "$1" in
--version) echo "git version 2.50";;
clone) for last; do :; done; mkdir -p "$last/.git" "$last/bin" "$last/scripts"
  printf '#!/bin/sh\\n${flutterFails ? 'echo Dart SDKを取得できません >&2; exit 1\\n' : ''}echo "{\\\\"frameworkVersion\\\\": \\\\"${flutter}\\\\"}"\\n' > "$last/bin/flutter"; chmod +x "$last/bin/flutter"
  printf 'echo "検証を実行: $1"\\n' > "$last/scripts/verify-apple.sh";;
-C) [ "$3" = remote ] && echo http://192.168.0.10:3000/mofua-suki/sample-expense-client.git; :;;
*) :;;
esac`,
  );
  return {
    dir,
    home,
    run: (file, ...args) =>
      spawnSync('sh', [path.join(root, file), ...args], {
        encoding: 'utf8',
        env: { PATH: `${bin}:/usr/bin:/bin`, HOME: home },
      }),
  };
}

test('偽のMacでは準備を最後まで行ってテストは流さず、もう一度流しても既存の設定を壊さない', (t) => {
  const mac = fakeMac(t);
  fs.writeFileSync(path.join(mac.home, '.zprofile'), 'export EDITOR=vim\n');
  for (let i = 0; i < 2; i++) {
    const result = mac.run(scripts['macbook-air-test']);
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.doesNotMatch(result.stdout, /検証を実行/);
    assert.match(result.stdout, /完了: macbook-air-test/);
  }
  assert.equal(
    fs.statSync(path.join(mac.home, '.issue-desk/apple/private')).mode & 0o777,
    0o700,
  );
  const profile = fs.readFileSync(path.join(mac.home, '.zprofile'), 'utf8');
  assert.match(profile, /^export EDITOR=vim\n/);
  assert.equal(profile.match(/export PATH=/g).length, 1);
  // Goは1回だけ取得し、チェックサムと照合する。
  assert.deepEqual(
    fs
      .readFileSync(path.join(mac.dir, 'curl.log'), 'utf8')
      .trim()
      .split('\n')
      .map((url) => path.basename(url)),
    ['go1.27.0.darwin-arm64.tar.gz', 'go1.27.0.darwin-arm64.tar.gz.sha256'],
  );
  assert.ok(fs.existsSync(path.join(mac.home, '.issue-desk/tools/go/bin/go')));
});

test('Xcode本体が選ばれていなければ、何も入れる前に理由を示して止まる', (t) => {
  const mac = fakeMac(t, { selected: '/Library/Developer/CommandLineTools' });
  const result = mac.run(scripts['macbook-pro-test']);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /コマンドラインツールだけが選ばれています/);
  assert.match(result.stderr, /失敗: 前提確認/);
  assert.equal(fs.existsSync(path.join(mac.home, '.issue-desk')), false);
});

test('既にある別の版のFlutterとGitでないcloneの場所は上書きせずに止まる', (t) => {
  const mac = fakeMac(t, { flutter: '3.40.0' });
  let result = mac.run(scripts['macbook-pro-test']);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Flutter 3\.47\.9 ではありません/);
  assert.ok(
    fs.existsSync(path.join(mac.home, 'development/flutter/bin/flutter')),
  );

  const other = fakeMac(t);
  fs.mkdirSync(path.join(other.home, 'Workspace/sample-expense-client'), {
    recursive: true,
  });
  fs.writeFileSync(
    path.join(other.home, 'Workspace/sample-expense-client/memo.txt'),
    'keep',
  );
  result = other.run(scripts['macbook-pro-test']);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Gitのcloneではありません/);
  assert.equal(
    fs.readFileSync(
      path.join(other.home, 'Workspace/sample-expense-client/memo.txt'),
      'utf8',
    ),
    'keep',
  );
});

test('Flutterが起動できないときは版の違いと案内せず、原因の表示を残して止まる', (t) => {
  const mac = fakeMac(t, { flutterFails: true });
  const result = mac.run(scripts['macbook-pro-test']);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Dart SDKを取得できません/);
  assert.match(result.stderr, /Flutterを起動できません/);
  assert.doesNotMatch(result.stderr, /ではありません/);
});
