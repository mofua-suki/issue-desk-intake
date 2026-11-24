import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const root = path.resolve(import.meta.dirname, '..');
const script = path.join(root, 'deploy/clients/mac-ssh-accept.sh');
const policy = JSON.parse(
  fs.readFileSync(path.join(root, 'host-ssh.json'), 'utf8'),
);
const host = policy.hosts['macbook-pro'];

function fakeMac(
  t,
  sshdConfig = 'Include /etc/ssh/sshd_config.d/*\nPasswordAuthentication no\n',
) {
  fs.mkdirSync(path.join(root, '.tmp'), { recursive: true });
  const dir = fs.mkdtempSync(path.join(root, '.tmp', 'mac-ssh-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const bin = path.join(dir, 'bin'),
    base = path.join(dir, 'root');
  fs.mkdirSync(bin);
  fs.mkdirSync(path.join(base, 'etc/ssh'), { recursive: true });
  fs.writeFileSync(path.join(base, 'etc/ssh/sshd_config'), sshdConfig);
  fs.writeFileSync(path.join(bin, 'uname'), '#!/bin/sh\necho Darwin\n', {
    mode: 0o755,
  });
  fs.writeFileSync(path.join(bin, 'id'), '#!/bin/sh\necho 0\n', {
    mode: 0o755,
  });
  const ssh = path.join(base, 'etc/ssh');
  const run = (user = host.user) =>
    spawnSync('sh', [script], {
      encoding: 'utf8',
      env: {
        PATH: `${bin}:/usr/bin:/bin`,
        SUDO_USER: user,
        ISSUE_DESK_SSH_ACCEPT_ROOT: base,
      },
    });
  const read = (file) => fs.readFileSync(path.join(ssh, file), 'utf8');
  return { ssh, run, read };
}

test('MacBook Proの登録はMacのiOSテストを書き込みの操作とし、SSHの利用者のホームを受付のMac登録と揃える', () => {
  assert.equal(host.principal, 'issue-desk-repair-macbook-pro');
  assert.ok(policy.operations['verify-sample-expense-client-ios']);
  assert.ok(
    !policy.readOnlyOperations.includes('verify-sample-expense-client-ios'),
    '送ったコードを実行するので自動承認しない',
  );
  // 隔離（Workerのrunnerと同じsandbox-execの規則）の中でiOSテストを試す操作も、送ったコードを実行するので自動承認しない。
  assert.ok(policy.operations['probe-worker-apple-ios']);
  assert.ok(!policy.readOnlyOperations.includes('probe-worker-apple-ios'));
  // runnerの入れ替えも、送ったソースをビルドして常駐させるので自動承認しない。
  assert.ok(policy.operations['deploy-worker-apple-runner']);
  assert.ok(!policy.readOnlyOperations.includes('deploy-worker-apple-runner'));
  const fleet = JSON.parse(
    fs.readFileSync(path.join(root, 'fleet.json'), 'utf8'),
  );
  assert.ok(
    fleet.hosts['macbook-pro-test'].runtimeDir.startsWith(
      `/Users/${host.user}/`,
    ),
  );
});

test('登録した利用者で流すと、NASの署名役のCAとhost-ssh.jsonのprincipalだけを受け入れ、何度流しても同じ内容にする', (t) => {
  const mac = fakeMac(t);
  for (let i = 0; i < 2; i++) {
    const result = mac.run();
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.match(result.stdout, /完了: macbook-pro/);
    assert.doesNotMatch(result.stdout, /注意/);
  }
  const ca = mac.read('issue-desk-repair-ca.pub');
  assert.match(ca, /^ssh-ed25519 [A-Za-z0-9+/]+=* \S+\n$/);
  const signer = '/etc/issue-desk/ssh-repair-ca.pub';
  if (fs.existsSync(signer))
    assert.equal(ca, fs.readFileSync(signer, 'utf8').trim() + '\n');
  assert.equal(
    mac.read(`issue-desk-principals/${host.user}`),
    host.principal + '\n',
  );
  assert.deepEqual(
    mac
      .read('sshd_config.d/050-issue-desk.conf')
      .split('\n')
      .filter((line) => line && !line.startsWith('#')),
    [
      'TrustedUserCAKeys /etc/ssh/issue-desk-repair-ca.pub',
      'AuthorizedPrincipalsFile /etc/ssh/issue-desk-principals/%u',
    ],
  );
  assert.equal(
    mac.read('sshd_config'),
    'Include /etc/ssh/sshd_config.d/*\nPasswordAuthentication no\n',
  );
  assert.deepEqual(
    fs
      .readdirSync(mac.ssh, { recursive: true, encoding: 'utf8' })
      .filter((name) => name.endsWith('.new')),
    [],
  );
});

test('登録と違う利用者で流したときは、接続できないことと伝える値を示す', (t) => {
  const result = fakeMac(t).run('someone');
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /このMacは someone です/);
  assert.match(result.stdout, /Macの利用者名は someone/);
});

test('既存のSSH設定（本体・sshd_config.d）に同じ指示があれば、上書きせずに行を示して止まる', (t) => {
  const config =
    'Include /etc/ssh/sshd_config.d/*\nTrustedUserCAKeys /etc/ssh/other.pub\n';
  const mac = fakeMac(t, config);
  let result = mac.run();
  assert.equal(result.status, 1);
  assert.match(result.stderr, /TrustedUserCAKeys \/etc\/ssh\/other\.pub/);
  assert.match(result.stderr, /失敗: 前提確認/);
  assert.equal(mac.read('sshd_config'), config);
  assert.equal(
    fs.existsSync(path.join(mac.ssh, 'sshd_config.d/050-issue-desk.conf')),
    false,
  );

  const other = fakeMac(t);
  fs.mkdirSync(path.join(other.ssh, 'sshd_config.d'));
  fs.writeFileSync(
    path.join(other.ssh, 'sshd_config.d/custom'),
    'AuthorizedPrincipalsFile /etc/ssh/mine\n',
  );
  result = other.run();
  assert.equal(result.status, 1);
  assert.match(result.stderr, /AuthorizedPrincipalsFile \/etc\/ssh\/mine/);
  assert.equal(
    fs.existsSync(path.join(other.ssh, 'sshd_config.d/050-issue-desk.conf')),
    false,
  );
});

// NASが標準入力で送る作業ツリーを、Macに入れたFlutter（$HOME/development/flutter/bin）とiOSシミュレーターで検証する操作を流す。
// xcrun・xcodebuild は呼ばれた内容を記録する偽物にし、iOSの実行環境の有無と起動済みのシミュレーターの有無を選べる。
const udid = 'AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE';
function macVerify(
  t,
  { verifyExit = 0, runtime = true, booted = false, watch = false } = {},
) {
  fs.mkdirSync(path.join(root, '.tmp'), { recursive: true });
  const dir = fs.mkdtempSync(path.join(root, '.tmp', 'mac-ssh-op-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const home = path.join(dir, 'home'),
    tree = path.join(dir, 'tree'),
    tmp = path.join(dir, 'tmp'),
    bin = path.join(dir, 'bin');
  const sdk = path.join(home, 'development/flutter/bin'),
    log = path.join(dir, 'calls.log'),
    state = path.join(dir, 'state');
  for (const sub of [sdk, bin, state, path.join(tree, 'scripts'), tmp])
    fs.mkdirSync(sub, { recursive: true });
  if (runtime) fs.writeFileSync(path.join(state, 'runtime'), '');
  if (booted) fs.writeFileSync(path.join(state, 'booted'), '');
  if (watch) fs.writeFileSync(path.join(state, 'watch'), '');
  fs.writeFileSync(
    path.join(bin, 'xcodebuild'),
    `#!/bin/sh\necho "xcodebuild $*" >> "${log}"; touch "${state}/runtime"\n`,
    { mode: 0o755 },
  );
  fs.writeFileSync(
    path.join(bin, 'xcrun'),
    `#!/bin/sh
case "$*" in
  'simctl list runtimes available') [ -e "${state}/runtime" ] && echo 'iOS 26.0 (26.0 - 23A0) - com.apple.CoreSimulator.SimRuntime.iOS-26-0' ;;
  'simctl list devices booted') [ -e "${state}/watch" ] && echo '    Apple Watch Series 11 (99999999-8888-7777-6666-555555555555) (Booted)'; [ -e "${state}/booted" ] && echo '    iPhone 17 (${udid}) (Booted)' ;;
  'simctl list devices available') echo '-- iOS 26.0 --'; echo '    iPad Air (11111111-2222-3333-4444-555555555555) (Shutdown)'; echo '    iPhone 17 (${udid}) (Shutdown)' ;;
  *) echo "xcrun $*" >> "${log}" ;;
esac
exit 0
`,
    { mode: 0o755 },
  );
  fs.writeFileSync(
    path.join(tree, 'scripts/verify-apple.sh'),
    `echo "verify-apple $1"; echo 警告 >&2; exit ${verifyExit}\n`,
  );
  const input = path.join(dir, 'tree.tar.gz');
  assert.equal(spawnSync('tar', ['-czf', input, '-C', tree, '.']).status, 0);
  const run = (stdin: string | Buffer = fs.readFileSync(input)) =>
    spawnSync(
      '/bin/sh',
      ['-c', policy.operations['verify-sample-expense-client-ios']],
      {
        input: stdin,
        encoding: 'utf8',
        env: { PATH: `${bin}:/usr/bin:/bin`, HOME: home, TMPDIR: tmp },
      },
    );
  const calls = () =>
    fs.existsSync(log) ? fs.readFileSync(log, 'utf8').trim().split('\n') : [];
  return { run, tmp, calls };
}

test('MacのiOSテストは止まっているiPhoneのシミュレーターを起動して、verify-apple.sh ios（Xcodeでのビルドとシミュレーター上のテスト）を流し、終わったら止める', (t) => {
  const mac = macVerify(t);
  const result = mac.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(
    result.stdout
      .trim()
      .split('\n')
      .map((line) => line.replace(/ in .*$/, '')),
    ['verify-apple ios', '警告', 'verify=0'],
  );
  assert.deepEqual(mac.calls(), [
    `xcrun simctl boot ${udid}`,
    `xcrun simctl bootstatus ${udid} -b`,
    `xcrun simctl shutdown ${udid}`,
  ]);
  assert.deepEqual(fs.readdirSync(mac.tmp), []);
});

test('MacのiOSテストは起動済みのシミュレーターをそのまま使い、止めない', (t) => {
  const mac = macVerify(t, { booted: true });
  const result = mac.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.deepEqual(mac.calls(), []);
});

test('MacのiOSテストはiPhone・iPad以外のシミュレーターだけが起動していても、iPhoneを起動して使う', (t) => {
  const mac = macVerify(t, { watch: true });
  const result = mac.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(mac.calls()[0], `xcrun simctl boot ${udid}`);
});

test('MacのiOSテストはiOSの実行環境が無ければ先に入れる', (t) => {
  const mac = macVerify(t, { runtime: false });
  const result = mac.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(mac.calls()[0], 'xcodebuild -downloadPlatform iOS');
});

test('MacのiOSテストは失敗した終了コードをそのまま返し、起動したシミュレーターは止める', (t) => {
  const mac = macVerify(t, { verifyExit: 7 });
  const result = mac.run();
  assert.equal(result.status, 7);
  assert.match(result.stdout, /verify=7\n$/);
  assert.equal(mac.calls().at(-1), `xcrun simctl shutdown ${udid}`);
  assert.deepEqual(fs.readdirSync(mac.tmp), []);
});

test('MacのiOSテストは展開できない入力を理由付きで断る', (t) => {
  const mac = macVerify(t);
  const result = mac.run('not a tarball');
  assert.notEqual(result.status, 0);
  assert.match(result.stdout, /標準入力のtar.gzを展開できません/);
  assert.deepEqual(mac.calls(), []);
  assert.deepEqual(fs.readdirSync(mac.tmp), []);
});

// runnerの入れ替えは、送られたtar.gzの runner-setup.sh を worker/ のソースで流し、一時フォルダーを残さない。
function macDeploy(t, { setupExit = 0, withSetup = true } = {}) {
  fs.mkdirSync(path.join(root, '.tmp'), { recursive: true });
  const dir = fs.mkdtempSync(path.join(root, '.tmp', 'mac-deploy-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const tree = path.join(dir, 'tree'),
    tmp = path.join(dir, 'tmp');
  fs.mkdirSync(path.join(tree, 'worker'), { recursive: true });
  fs.mkdirSync(tmp);
  fs.writeFileSync(
    path.join(tree, 'worker/go.mod'),
    'module issue-desk-worker\n',
  );
  if (withSetup)
    fs.writeFileSync(
      path.join(tree, 'runner-setup.sh'),
      `echo "setup $*"; [ -f "$2/go.mod" ] && echo source-ok; exit ${setupExit}\n`,
    );
  const input = path.join(dir, 'tree.tar.gz');
  assert.equal(spawnSync('tar', ['-czf', input, '-C', tree, '.']).status, 0);
  const run = () =>
    spawnSync(
      '/bin/sh',
      ['-c', policy.operations['deploy-worker-apple-runner']],
      {
        input: fs.readFileSync(input),
        encoding: 'utf8',
        env: { PATH: '/usr/bin:/bin', TMPDIR: tmp },
      },
    );
  return { run, tmp };
}

test('Macのrunnerの入れ替えは送られたソースで runner-setup.sh --source を流し、終了コードを返して一時フォルダーを消す', (t) => {
  const mac = macDeploy(t);
  const result = mac.run();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  const lines = result.stdout.trim().split('\n');
  assert.match(
    lines[0],
    /^setup --source \/.*\/host-ssh-worker-deploy\.[^/]+\/worker$/,
  );
  assert.deepEqual(lines.slice(1), ['source-ok', 'deploy=0']);
  assert.deepEqual(fs.readdirSync(mac.tmp), []);
  const failed = macDeploy(t, { setupExit: 3 }).run();
  assert.equal(failed.status, 3);
  assert.match(failed.stdout, /deploy=3$/m);
});

test('Macのrunnerの入れ替えは runner-setup.sh の無いtar.gzを断る', (t) => {
  const result = macDeploy(t, { withSetup: false }).run();
  assert.equal(result.status, 1);
  assert.match(result.stdout, /worker\/ と runner-setup\.sh がありません/);
});
