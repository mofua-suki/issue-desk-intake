import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { verifyCommand } from '../scripts/ssh-development-policy.ts';
import { buildSpec, fleetHost, main } from '../scripts/ssh-verify.ts';
import {
  dependencyPolicy,
  runVerify,
  steps,
  validateSpec,
} from '../scripts/ssh-verify-runner.ts';

const root = fileURLToPath(new URL('..', import.meta.url));
const readJSON = (file) =>
  JSON.parse(fs.readFileSync(path.join(root, file), 'utf8'));
const policy = readJSON('host-ssh.json');
const spec = (extra = {}) => ({
  version: 1,
  repository: 'mofua-suki/demo',
  setupCommand: [],
  testCommand: ['true'],
  pnpm: '',
  setupTimeoutSeconds: 60,
  testTimeoutSeconds: 60,
  ...extra,
});

test('汎用の固定テスト verify は登録済みハッシュのコードだけを動かし、自動承認の対象にしない', () => {
  assert.equal(policy.operations.verify, verifyCommand());
  assert.ok(!policy.readOnlyOperations.includes('verify'));
  const tampered = spawnSync('sh', ['-c', policy.operations.verify], {
    input:
      Buffer.from('echo 不正なコードが実行された').toString('base64') +
      '\n' +
      Buffer.from('invalid').toString('base64') +
      '\n',
    encoding: 'utf8',
  });
  assert.notEqual(tampered.status, 0);
  assert.doesNotMatch(tampered.stdout, /不正なコードが実行された/);
  assert.match(tampered.stderr, /FAILED/);
});

test('固定テストのコマンドはWorkerと同じ fleet.json の担当PCの設定から作る', () => {
  const config = {
    fleet: readJSON('fleet.json'),
    assignments: readJSON('assignments.json'),
  };
  const a9 = config.fleet.hosts.a9max;
  const repo = a9.repositories.find(
    (item) => item.name === 'mofua-suki/demo-image-generator',
  );
  const got = buildSpec(config, 'a9-max', 'mofua-suki/demo-image-generator');
  assert.deepEqual(
    got.setupCommand,
    repo.setupCommand.map((part) => (part === '{python}' ? a9.python : part)),
  );
  assert.deepEqual(got.testCommand, repo.testCommand);
  assert.equal(got.pnpm, a9.pnpm);
  // 担当名とPC名が同じホスト（x1-pro1）も対応付ける。
  assert.deepEqual(
    buildSpec(config, 'x1-pro1', 'mofua-suki/issue-desk-worker').testCommand,
    ['sh', 'scripts/verify.sh'],
  );
  assert.throws(
    () => buildSpec(config, 'gt12-pro', 'mofua-suki/demo-image-generator'),
    /担当していません。担当PC: .*a9-max/,
  );
  // host-ssh.json に登録した全PCが fleet.json の担当名へ対応付く（名前を変えたら止める）。
  for (const host of Object.keys(policy.hosts)) {
    const name = fleetHost(config.assignments, config.fleet, host);
    assert.ok(name, `${host} を fleet.json の担当名へ対応付けられません`);
    // Macのテスト専用PCはネイティブ検証だけを担当し、この操作では流さない。
    if (config.assignments.hosts[name]?.role === 'test-only') {
      for (const repo of config.fleet.hosts[name].repositories)
        assert.throws(
          () => buildSpec(config, host, repo.name),
          /Macのネイティブ検証/,
        );
      continue;
    }
    const usable = config.fleet.hosts[name].repositories.some((repo) => {
      try {
        buildSpec(config, host, repo.name);
        return true;
      } catch {
        return false;
      }
    });
    assert.ok(usable, `${host} で流せる固定テストがありません`);
  }
});

test('容器内の実行はWorkerと同じ順に流し、最初に失敗した段の終了コードで止まる', () => {
  const tree = fs.mkdtempSync(path.join(os.tmpdir(), 'ssh-verify-runner-'));
  try {
    fs.writeFileSync(path.join(tree, 'package.json'), '{}');
    const s = spec({
      setupCommand: ['setup'],
      pnpm: '/opt/pnpm',
      testCommand: ['test-cmd'],
      testTimeoutSeconds: 120,
    });
    assert.deepEqual(
      steps(s, tree).map((step) => [step.name, step.argv[0], step.timeout]),
      [
        ['setupCommand', 'setup', 60],
        ['pnpmInstall', '/opt/pnpm', 60],
        ['testCommand', 'test-cmd', 120],
      ],
    );
    const ran = [];
    const run = (codes) => (file, _args, options) => {
      ran.push(file);
      assert.equal(options.env.HOME, '/tmp');
      assert.equal(options.env.CI, 'true');
      return { status: codes.shift() };
    };
    assert.equal(
      runVerify(s, tree, run([0, 0, 0]), () => {}),
      0,
    );
    assert.deepEqual(ran.splice(0), ['setup', '/opt/pnpm', 'test-cmd']);
    assert.equal(
      runVerify(s, tree, run([0, 5, 0]), () => {}),
      5,
    );
    assert.deepEqual(ran.splice(0), ['setup', '/opt/pnpm']);
    assert.equal(
      runVerify(
        s,
        tree,
        () => ({
          status: null,
          error: Object.assign(Error('t'), { code: 'ETIMEDOUT' }),
        }),
        () => {},
      ),
      124,
    );
    assert.equal(
      runVerify(
        s,
        tree,
        () => ({
          status: null,
          error: Object.assign(Error('n'), { code: 'ENOENT' }),
        }),
        () => {},
      ),
      127,
    );
    fs.writeFileSync(
      path.join(tree, '.npmrc'),
      'minimum-release-age-exclude=evil\n',
    );
    assert.throws(() => dependencyPolicy(tree), /14日制限/);
    assert.throws(() => runVerify(s, tree, run([0]), () => {}), /14日制限/);
    for (const bad of [
      { testCommand: [] },
      { repository: 'x' },
      { setupCommand: [''] },
      { pnpm: 'pnpm' },
      { testTimeoutSeconds: 0 },
      { version: 2 },
    ])
      assert.throws(() => validateSpec(spec(bad)), /指定が不正/);
  } finally {
    fs.rmSync(tree, { recursive: true, force: true });
  }
});

// 偽のdockerは稼働Workerの一覧・イメージの確認に答え、docker run は容器の代わりに実行部をその場で動かす。
const fakeDocker = `#!${process.execPath}
const fs = require('node:fs'), path = require('node:path'), { spawnSync } = require('node:child_process');
const [cmd, ...args] = process.argv.slice(2);
const log = process.env.FAKE_LOG;
if (cmd === 'ps') { process.stdout.write('container1\\n'); process.exit(0); }
if (cmd === 'inspect') { console.log('sha256:workerimage'); process.exit(0); }
if (cmd === 'rm') { fs.appendFileSync(path.join(log, 'rm'), args.join(' ') + '\\n'); process.exit(0); }
if (cmd !== 'run') process.exit(42);
fs.writeFileSync(path.join(log, 'args'), args.join(' '));
const mounts = {}, env = { PATH: process.env.PATH };
for (let i = 0; i < args.length; i++) {
  if (args[i] === '-v') { const [src, dst] = args[++i].split(':'); mounts[dst] = src; }
  if (args[i] === '-e') { const [key, ...rest] = args[++i].split('='); env[key] = rest.join('='); }
}
fs.writeFileSync(path.join(log, 'work'), mounts['/work']);
const script = args.at(-1).replace('/runner', mounts['/runner']);
const r = spawnSync(process.execPath, [script], { cwd: mounts['/work'], env, stdio: 'inherit' });
process.exit(r.status ?? 1);
`;
// 偽のshは host-ssh.sh の呼び出しだけを受け、承認・発券の代わりに固定操作 verify を --input の標準入力で直接流す。
const fakeShell = `#!${process.execPath}
const fs = require('node:fs'), { spawnSync } = require('node:child_process');
const args = process.argv.slice(2);
if (!(args[0] ?? '').endsWith('deploy/nas-host/host-ssh.sh')) {
  const r = spawnSync('/bin/sh', args, { stdio: 'inherit' });
  process.exit(r.status ?? 1);
}
const flag = name => args[args.indexOf(name) + 1];
fs.writeFileSync(process.env.FAKE_LOG + '/request', JSON.stringify({ host: flag('--host'), operation: flag('--operation'), evidence: flag('--evidence'), input: flag('--input') }));
const policy = JSON.parse(fs.readFileSync(process.env.FAKE_POLICY, 'utf8'));
const r = spawnSync('/bin/sh', ['-c', policy.operations[flag('--operation')]], { input: fs.readFileSync(flag('--input')), encoding: 'utf8' });
fs.writeFileSync(process.env.FAKE_LOG + '/out', r.stdout);
process.exit(r.status ?? 1);
`;

test('呼出元は作業ツリーと固定テストの指定を送り、接続先は結果と終了コードを返して片付ける', () => {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), 'ssh-verify-e2e-'));
  const source = path.join(base, 'source'),
    bin = path.join(base, 'bin'),
    log = path.join(base, 'log');
  for (const dir of [source, bin, log]) fs.mkdirSync(dir);
  const oldEnv = {
    PATH: process.env.PATH,
    FAKE_LOG: process.env.FAKE_LOG,
    FAKE_POLICY: process.env.FAKE_POLICY,
  };
  const git = (...args) =>
    assert.equal(
      spawnSync('git', args, { cwd: source, encoding: 'utf8' }).status,
      0,
    );
  try {
    fs.writeFileSync(path.join(bin, 'docker'), fakeDocker, { mode: 0o755 });
    fs.writeFileSync(path.join(bin, 'sh'), fakeShell, { mode: 0o755 });
    git('init', '-q');
    git('config', 'user.name', 't');
    git('config', 'user.email', 't@example.invalid');
    fs.writeFileSync(
      path.join(source, 'check.mjs'),
      `import fs from 'node:fs';
const want = fs.existsSync('kept.txt') && fs.existsSync('new.txt') && !fs.existsSync('gone.txt') && !fs.existsSync('.tmp');
console.log('token=漏れてはいけない'); console.log('LAST LINE');
process.exit(want ? Number(fs.readFileSync('code.txt', 'utf8')) : 9);
`,
    );
    for (const file of ['kept.txt', 'gone.txt'])
      fs.writeFileSync(path.join(source, file), file);
    fs.writeFileSync(path.join(source, 'code.txt'), '0');
    git('add', '.');
    git('commit', '-qm', 'base');
    git('remote', 'add', 'origin', 'gitea-nas:mofua-suki/demo.git');
    fs.rmSync(path.join(source, 'gone.txt'));
    fs.writeFileSync(path.join(source, 'new.txt'), '未コミット');
    const config = {
      fleet: {
        hosts: {
          demo: {
            node: process.execPath,
            pnpm: '',
            testTimeoutSeconds: 60,
            repositories: [
              { name: 'mofua-suki/demo', testCommand: ['{node}', 'check.mjs'] },
            ],
          },
        },
      },
      assignments: { hosts: { demo: { machineName: 'gt12-pro' } } },
    };
    Object.assign(process.env, {
      PATH: bin + ':' + oldEnv.PATH,
      FAKE_LOG: log,
      FAKE_POLICY: path.join(root, 'host-ssh.json'),
    });
    for (const code of ['0', '3']) {
      fs.writeFileSync(path.join(source, 'code.txt'), code);
      assert.equal(
        main(
          ['--host', 'gt12-pro', '--repo', 'mofua-suki/demo', '--cwd', source],
          { config },
        ),
        Number(code),
      );
      const out = fs.readFileSync(path.join(log, 'out'), 'utf8');
      assert.match(out, new RegExp(`verify=${code}\\n?$`));
      assert.match(out, /LAST LINE/);
      assert.doesNotMatch(out, /漏れてはいけない/);
      const request = JSON.parse(
        fs.readFileSync(path.join(log, 'request'), 'utf8'),
      );
      assert.equal(request.operation, 'verify');
      assert.match(
        request.evidence,
        /^repo=mofua-suki\/demo head=[0-9a-f]{40} dirty=true sha256=[0-9a-f]{64} setupCommand=\[\] testCommand=\[".*","check.mjs"\]$/,
      );
      for (const want of [
        '--pull never',
        '--init',
        '--cap-drop ALL',
        '--security-opt no-new-privileges:true',
        '--network host',
        '--group-add 1001',
        ':/runner:ro',
        '--entrypoint node sha256:workerimage /runner/ssh-verify-runner.ts',
      ])
        assert.ok(
          fs.readFileSync(path.join(log, 'args'), 'utf8').includes(want),
          want,
        );
      assert.ok(
        !fs.existsSync(fs.readFileSync(path.join(log, 'work'), 'utf8')),
        '展開先が消えていません',
      );
      assert.match(
        fs.readFileSync(path.join(log, 'rm'), 'utf8'),
        /-f host-ssh-verify-/,
      );
      assert.ok(!fs.existsSync(request.input), '送信本文が消えていません');
    }
    assert.deepEqual(fs.readdirSync(path.join(source, '.tmp')), []);
  } finally {
    for (const [key, value] of Object.entries(oldEnv))
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    fs.rmSync(base, { recursive: true, force: true });
  }
});

test('接続先で接続が切れたら、テストの終了を待たずに容器と作業場所を片付ける', async () => {
  const shells = ['/bin/sh', '/usr/bin/dash', '/bin/dash'].filter((shell) =>
    fs.existsSync(shell),
  );
  for (const shell of shells) {
    const base = fs.mkdtempSync(path.join(os.tmpdir(), 'ssh-verify-cut-'));
    const bin = path.join(base, 'bin'),
      log = path.join(base, 'log'),
      temp = path.join(base, 'tmp');
    for (const dir of [bin, log, temp]) fs.mkdirSync(dir);
    try {
      fs.writeFileSync(
        path.join(bin, 'docker'),
        `#!/bin/sh
case "$1" in
ps) echo container1 ;;
inspect) echo sha256:workerimage ;;
rm) echo "$*" >> "${log}/rm"; kill "$(cat "${log}/pid" 2>/dev/null)" 2>/dev/null; exit 0 ;;
run) echo $$ > "${log}/pid"; exec sleep 60 ;;
esac
`,
        { mode: 0o755 },
      );
      const tar = spawnSync('tar', ['-czf', '-', '-C', base, 'log'], {});
      const { spawn } = await import('node:child_process');
      const child = spawn(
        shell,
        [path.join(root, 'scripts/ssh-verify-host.sh')],
        {
          env: { ...process.env, PATH: bin + ':/usr/bin:/bin', TMPDIR: temp },
          stdio: ['pipe', 'pipe', 'inherit'],
        },
      );
      child.stdin.end(
        Buffer.concat([
          Buffer.from(
            Buffer.from(JSON.stringify(spec())).toString('base64') + '\n',
          ),
          tar.stdout,
        ]),
      );
      const started = Date.now();
      await new Promise<any>((resolve) => child.stdout.once('data', resolve));
      child.stdout.destroy();
      const code = await new Promise<any>((resolve) =>
        child.once('exit', resolve),
      );
      assert.notEqual(code, 0, shell);
      assert.ok(
        Date.now() - started < 30000,
        `${shell}: 切断の後もテストの終了を待っています`,
      );
      assert.match(
        fs.readFileSync(path.join(log, 'rm'), 'utf8'),
        /-f host-ssh-verify-/,
      );
      assert.deepEqual(
        fs.readdirSync(temp),
        [],
        `${shell}: 作業場所・出力が残っています`,
      );
    } finally {
      fs.rmSync(base, { recursive: true, force: true });
    }
  }
});

test('接続先は稼働Workerを一意に確かめられないとき・指定や入力が不正なときは何も流さない', () => {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), 'ssh-verify-refuse-'));
  const bin = path.join(base, 'bin'),
    temp = path.join(base, 'tmp');
  for (const dir of [bin, temp]) fs.mkdirSync(dir);
  try {
    fs.writeFileSync(
      path.join(bin, 'docker'),
      `#!/bin/sh
case "$1" in
ps) printf '%s' "$FAKE_PS" ;;
inspect) echo sha256:workerimage ;;
run) touch "${base}/ran" ;;
esac
`,
      { mode: 0o755 },
    );
    const good = Buffer.from(JSON.stringify(spec())).toString('base64') + '\n';
    for (const [ps, input, want] of [
      ['', good, /一意に確認できません/],
      ['a\nb\n', good, /一意に確認できません/],
      ['container1\n', '不正な指定\n', /指定が不正です/],
      ['container1\n', good + 'tar.gzではない', /展開できません/],
    ] as [string, string, RegExp][]) {
      const r = spawnSync(
        'sh',
        [path.join(root, 'scripts/ssh-verify-host.sh')],
        {
          env: {
            ...process.env,
            PATH: bin + ':/usr/bin:/bin',
            TMPDIR: temp,
            FAKE_PS: ps,
          },
          input,
          encoding: 'utf8',
        },
      );
      assert.notEqual(r.status, 0);
      assert.match(r.stdout, want);
      assert.ok(
        !fs.existsSync(path.join(base, 'ran')),
        'docker run が呼ばれています',
      );
      assert.deepEqual(fs.readdirSync(temp), []);
    }
  } finally {
    fs.rmSync(base, { recursive: true, force: true });
  }
});

test('段ごとの上限の合計が容器全体の上限を超える指定は、入口と接続先の両方で流す前に断る', async () => {
  const { checkBudget, stepBudgetSeconds, containerSeconds } = await import(
    '../scripts/ssh-verify-runner.ts'
  );
  const hostScript = fs.readFileSync(
    path.join(root, 'scripts/ssh-verify-host.sh'),
    'utf8',
  );
  assert.match(
    hostScript,
    new RegExp(`timeout -k 10 ${containerSeconds} docker run`),
  );
  assert.match(
    fs.readFileSync(path.join(root, 'deploy/nas-host/host-ssh.sh'), 'utf8'),
    /\[ "\$operation" != verify \] \|\| ssh_timeout=(\d+)/,
  );
  const sshTimeout = Number(
    fs
      .readFileSync(path.join(root, 'deploy/nas-host/host-ssh.sh'), 'utf8')
      .match(/\[ "\$operation" != verify \] \|\| ssh_timeout=(\d+)/)[1],
  );
  assert.ok(
    sshTimeout > containerSeconds + 10,
    'host-ssh.sh は容器全体の打ち切りより後に切る',
  );
  const tree = fs.mkdtempSync(path.join(os.tmpdir(), 'ssh-verify-budget-'));
  try {
    const over = spec({
      setupCommand: ['setup'],
      setupTimeoutSeconds: 2000,
      testTimeoutSeconds: 3500,
    });
    assert.throws(() => checkBudget(over, tree), /容器全体の上限/);
    let ran = 0;
    assert.throws(
      () =>
        runVerify(
          over,
          tree,
          () => {
            ran++;
            return { status: 0 };
          },
          () => {},
        ),
      /容器全体の上限/,
    );
    assert.equal(ran, 0);
    // package.json があれば pnpm の段も数える。
    const fit = spec({
      setupCommand: ['setup'],
      pnpm: '/opt/pnpm',
      setupTimeoutSeconds: 1700,
      testTimeoutSeconds: stepBudgetSeconds - 3400,
    });
    assert.equal(checkBudget(fit, tree), stepBudgetSeconds - 1700);
    fs.writeFileSync(path.join(tree, 'package.json'), '{}');
    assert.equal(checkBudget(fit, tree), stepBudgetSeconds);
    assert.throws(
      () =>
        checkBudget(
          { ...fit, testTimeoutSeconds: fit.testTimeoutSeconds + 1 },
          tree,
        ),
      /容器全体の上限/,
    );
    // 実際の fleet.json の指定はすべて収まる。
    const config = {
      fleet: readJSON('fleet.json'),
      assignments: readJSON('assignments.json'),
    };
    const empty = fs.mkdtempSync(path.join(tree, 'empty-'));
    for (const host of Object.keys(policy.hosts))
      for (const repo of Object.values<any>(config.fleet.hosts).flatMap(
        (value) => value.repositories ?? [],
      )) {
        let s;
        try {
          s = buildSpec(config, host, repo.name);
        } catch {
          continue;
        }
        fs.writeFileSync(path.join(empty, 'package.json'), '{}');
        checkBudget(s, empty);
      }
  } finally {
    fs.rmSync(tree, { recursive: true, force: true });
  }
});

test('originは末尾の .git の有無と大文字小文字を問わず照合し、Macのネイティブ検証は断る', async () => {
  const { originMatches } = await import('../scripts/ssh-verify.ts');
  for (const remote of [
    'gitea-nas:mofua-suki/demo.git',
    'gitea-nas:mofua-suki/demo',
    'http://192.168.0.10:3000/Mofua-suki/Demo.git',
    'ssh://git@host/mofua-suki/demo/',
    'gitea-nas:MOFUA-SUKI/demo.GIT\n',
  ])
    assert.ok(originMatches(remote, 'mofua-suki/demo'), remote);
  for (const remote of [
    'gitea-nas:mofua-suki/demo-other.git',
    'gitea-nas:other/demo.git',
    'gitea-nas:mofua-suki/demo.git.bak',
    'demo',
  ])
    assert.ok(!originMatches(remote, 'mofua-suki/demo'), remote);
  const config = {
    fleet: readJSON('fleet.json'),
    assignments: readJSON('assignments.json'),
  };
  const mac = {
    fleet: {
      hosts: {
        m: {
          repositories: [
            {
              name: 'mofua-suki/demo',
              testCommand: ['true'],
              nativeTestHost: 'm',
            },
          ],
        },
      },
    },
    assignments: { hosts: { m: { machineName: 'gt12-pro' } } },
  };
  assert.throws(
    () => buildSpec(mac, 'gt12-pro', 'mofua-suki/demo'),
    /ネイティブ検証/,
  );
  assert.throws(
    () => buildSpec(config, 'a9-max', 'mofua-suki/no-such'),
    /担当PC: なし/,
  );
});

test('入口はoriginが違う作業ツリーと上限超えの指定を、申請する前に断る', () => {
  const base = fs.mkdtempSync(
    path.join(os.tmpdir(), 'ssh-verify-refuse-caller-'),
  );
  const source = path.join(base, 'source'),
    bin = path.join(base, 'bin');
  fs.mkdirSync(source);
  fs.mkdirSync(bin);
  const oldPath = process.env.PATH;
  const git = (...args) =>
    assert.equal(
      spawnSync('git', args, { cwd: source, encoding: 'utf8' }).status,
      0,
    );
  try {
    fs.writeFileSync(
      path.join(bin, 'sh'),
      `#!/bin/sh\ntouch "${base}/called"\nexit 0\n`,
      { mode: 0o755 },
    );
    git('init', '-q');
    git('remote', 'add', 'origin', 'gitea-nas:mofua-suki/other.git');
    const config = (extra = {}) => ({
      fleet: {
        hosts: {
          demo: {
            node: process.execPath,
            pnpm: '',
            testTimeoutSeconds: 3000,
            repositories: [
              { name: 'mofua-suki/demo', testCommand: ['true'], ...extra },
            ],
          },
        },
      },
      assignments: { hosts: { demo: { machineName: 'gt12-pro' } } },
    });
    process.env.PATH = bin + ':' + oldPath;
    assert.throws(
      () =>
        main(
          ['--host', 'gt12-pro', '--repo', 'mofua-suki/demo', '--cwd', source],
          { config: config() },
        ),
      /originと対象リポジトリが一致しません/,
    );
    git('remote', 'set-url', 'origin', 'gitea-nas:Mofua-suki/Demo');
    assert.throws(
      () =>
        main(
          ['--host', 'gt12-pro', '--repo', 'mofua-suki/demo', '--cwd', source],
          {
            config: config({
              setupCommand: ['true'],
              testTimeoutSeconds: 3000,
            }),
          },
        ),
      /容器全体の上限/,
    );
    assert.ok(
      !fs.existsSync(path.join(base, 'called')),
      'host-ssh.sh を呼んでいます',
    );
  } finally {
    process.env.PATH = oldPath;
    fs.rmSync(base, { recursive: true, force: true });
  }
});

test('段の時間切れ・シグナル終了でも、最後の行が同じ verify=<終了コード> なら接続成功と分かる', async () => {
  const { finishedWith } = await import(
    '../deploy/nas-host/host-ssh-output.ts'
  );
  assert.ok(finishedWith('.\nstep=testCommand exit=124\nverify=124\n', '124'));
  assert.ok(finishedWith('verify=137\r\n\n', 137));
  assert.ok(!finishedWith('.\n.\n', '124'));
  assert.ok(!finishedWith('verify=124\n.\n', '124'));
  assert.ok(!finishedWith('verify=1\n', '124'));
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ssh-verify-finished-'));
  try {
    const file = path.join(dir, 'out');
    fs.writeFileSync(file, 'x\nverify=124\n');
    const cli = (code) =>
      spawnSync(process.execPath, [
        path.join(root, 'deploy/nas-host/host-ssh-output.ts'),
        '--finished',
        file,
        code,
      ]).status;
    assert.equal(cli('124'), 0);
    assert.equal(cli('255'), 1);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('issue-desk-intake ではイメージのCaddy・Chromiumが起動できるときだけブラウザ試験に渡す', async () => {
  const { intakeBrowserTools, testEnv } = await import(
    '../scripts/ssh-verify-runner.ts'
  );
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ssh-verify-browser-'));
  try {
    const caddy = path.join(dir, 'caddy'),
      chromium = path.join(dir, 'chromium');
    for (const file of [caddy, chromium])
      fs.writeFileSync(file, '', { mode: 0o755 });
    const intake = spec({ repository: 'mofua-suki/issue-desk-intake' });
    const quiet = () => {};
    let env = testEnv();
    assert.equal(env.JAVA_TOOL_OPTIONS, '-Djava.io.tmpdir=/tmp');
    intakeBrowserTools(
      intake,
      env,
      () => ({ status: 0 }),
      quiet,
      caddy,
      chromium,
    );
    assert.equal(env.ISSUE_DESK_TEST_CADDY, caddy);
    assert.equal(env.ISSUE_DESK_TEST_CHROMIUM, chromium);
    const logs = [];
    env = testEnv();
    intakeBrowserTools(
      intake,
      env,
      () => ({ status: 1 }),
      (line) => logs.push(line),
      caddy,
      chromium,
    );
    assert.equal(env.ISSUE_DESK_TEST_CHROMIUM, undefined);
    assert.match(logs.join('\n'), /ブラウザ試験を省略/);
    env = testEnv();
    intakeBrowserTools(
      spec(),
      env,
      () => assert.fail('対象外で起動確認しています'),
      quiet,
      caddy,
      chromium,
    );
    intakeBrowserTools(
      intake,
      env,
      () => assert.fail('道具が無いのに起動確認しています'),
      quiet,
      caddy,
      path.join(dir, 'none'),
    );
    assert.equal(env.ISSUE_DESK_TEST_CADDY, undefined);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('直接PRの証明書で指定したGitコミットだけを固定テストへ渡す', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ssh-direct-pr-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const tree = path.join(dir, 'tree');
  const bin = path.join(dir, 'bin');
  const temporary = path.join(dir, 'tmp');
  for (const directory of [tree, bin, temporary]) fs.mkdirSync(directory);
  const git = (args: string[], input?: string) => {
    const result = spawnSync('git', args, {
      cwd: tree,
      input,
    });
    assert.equal(result.status, 0, result.stderr.toString());
    return result.stdout;
  };
  git(['init', '-q']);
  fs.writeFileSync(path.join(tree, 'candidate.txt'), 'expected');
  git(['add', 'candidate.txt']);
  git([
    '-c',
    'user.name=test',
    '-c',
    'user.email=test@example.test',
    'commit',
    '-qm',
    'test',
  ]);
  const head = git(['rev-parse', 'HEAD']).toString().trim();
  const treeID = git(['rev-parse', 'HEAD^{tree}']).toString().trim();
  const blob = git(['rev-parse', 'HEAD:candidate.txt']).toString().trim();
  const pack = git(
    ['pack-objects', '--stdout'],
    [head, treeID, blob, ''].join('\n'),
  );
  fs.writeFileSync(
    path.join(bin, 'docker'),
    `#!/bin/sh
case "$1" in
ps) echo running ;;
inspect) echo sha256:workerimage ;;
run)
  for arg in "$@"; do
    case "$arg" in
      *:/work) test "$(cat "\${arg%:/work}/candidate.txt")" = expected && touch "\${TEST_OUTPUT}" ;;
    esac
  done ;;
esac
`,
    { mode: 0o755 },
  );
  const input = Buffer.concat([
    Buffer.from(Buffer.from(JSON.stringify(spec())).toString('base64') + '\n'),
    pack,
  ]);
  const env = {
    ...process.env,
    PATH: bin + ':/usr/bin:/bin',
    TMPDIR: temporary,
    TEST_OUTPUT: path.join(dir, 'started'),
    ISSUE_DESK_DIRECT_PR_HEAD: 'f'.repeat(40),
  };
  const run = () =>
    spawnSync('sh', [path.join(root, 'scripts/ssh-verify-host.sh')], {
      input,
      env,
      encoding: 'utf8',
    });
  assert.notEqual(run().status, 0);
  assert.equal(fs.existsSync(env.TEST_OUTPUT), false);
  env.ISSUE_DESK_DIRECT_PR_HEAD = head;
  assert.equal(run().status, 0);
  assert.equal(fs.existsSync(env.TEST_OUTPUT), true);
});
