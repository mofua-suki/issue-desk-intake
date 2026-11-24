import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import crypto from 'node:crypto';
import { spawnSync } from 'node:child_process';

const source = path.resolve('deploy/nas/update-host-node.sh');

const withDir = (run) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'desk-host-node-'));
  try {
    return run(dir);
  } finally {
    try {
      fs.rmSync(dir, {
        recursive: true,
        force: true,
        maxRetries: 5,
        retryDelay: 100,
      });
    } catch {
      // 一時ディレクトリは作業領域ごと捨てられるので、残っても害がない。
    }
  }
};

const script = (file, body) => {
  fs.writeFileSync(file, `#!/bin/sh\n${body}\n`);
  fs.chmodSync(file, 0o755);
};

// 公式の配布物と同じ形の tar.xz と SHASUMS256.txt を作り、curl を差し替えてそこから写す。
const setup = (root, { version, reported = version, tamper = false }) => {
  const repo = path.join(root, 'repo');
  fs.mkdirSync(path.join(repo, 'deploy'), { recursive: true });
  fs.writeFileSync(
    path.join(repo, 'deploy', 'versions.json'),
    `{\n  "go": "1.0.0",\n  "node": "${version}",\n  "pnpm": "10.0.0"\n}\n`,
  );
  const name = `node-v${version}-linux-x64`;
  const build = path.join(root, 'build');
  fs.mkdirSync(path.join(build, name, 'bin'), { recursive: true });
  script(path.join(build, name, 'bin', 'node'), `echo v${reported}`);
  script(
    path.join(build, name, 'bin', 'npm'),
    `
if [ "$1" = install ]; then
  shift; shift; prefix=$1
  for pair in corepack:0.36.0 pnpm:10.0.0 pnpx:10.0.0; do
    tool=\${pair%:*}; value=\${pair#*:}
    printf '#!/bin/sh\necho %s\n' "$value" > "$prefix/bin/$tool"
    if [ "$tool" = pnpx ]; then
      printf '#!/bin/sh\n[ "$1" = --help ] || exit 1\nprintf "Version 10.0.0\\nUsage: pnpm dlx <command> [args...]\\n"\n' > "$prefix/bin/$tool"
    fi
    chmod +x "$prefix/bin/$tool"
  done
else echo npm; fi`,
  );
  script(path.join(build, name, 'bin', 'npx'), 'echo npx');
  const dist = path.join(root, 'dist');
  fs.mkdirSync(dist);
  const tar = spawnSync(
    'tar',
    ['-cJf', path.join(dist, `${name}.tar.xz`), '-C', build, name],
    { encoding: 'utf8' },
  );
  assert.equal(tar.status, 0, tar.stderr);
  const sum = crypto
    .createHash('sha256')
    .update(fs.readFileSync(path.join(dist, `${name}.tar.xz`)))
    .digest('hex');
  fs.writeFileSync(
    path.join(dist, 'SHASUMS256.txt'),
    `${tamper ? '0'.repeat(64) : sum}  ${name}.tar.xz\n`,
  );
  const fake = path.join(root, 'fake-bin');
  fs.mkdirSync(fake);
  // 最後の引数のURLの末尾のファイルを、今のディレクトリへ写す。呼ばれたURLは記録する。
  script(
    path.join(fake, 'curl'),
    `for a; do url=$a; done\necho "$url" >> "${root}/curl.log"\ncp "${dist}/\${url##*/}" .`,
  );
  script(path.join(fake, 'uname'), 'echo x86_64');
  const home = path.join(root, 'home');
  fs.mkdirSync(home);
  return { repo, home, fake, name };
};

const run = ({ repo, home, fake }, extra = {}) =>
  spawnSync(
    'sh',
    ['-c', '. "$1"; say() { echo "$@"; }; host_node', 'sh', source],
    {
      env: {
        PATH: `${fake}:${process.env.PATH}`,
        HOME: home,
        DIR: repo,
        ...extra,
      },
      encoding: 'utf8',
    },
  );

test('NAS更新は本体のNodeを deploy/versions.json の版にし、失敗しても更新を止めない', () => {
  const update = fs.readFileSync('deploy/nas/update.sh', 'utf8');
  assert.match(
    update,
    /^\s*\. "\$DIR\/deploy\/nas\/update-host-node\.sh"\s*$/m,
  );
  assert.match(update, /host_node \|\|/);
});

test('照合した配布物を展開して current と ~/.local/bin を切り替え、前の版を残す', () => {
  withDir((root) => {
    const env = setup(root, { version: '24.22.3' });
    const lib = path.join(env.home, '.local', 'lib', 'nodejs');
    const bin = path.join(env.home, '.local', 'bin');
    // 手で入れた前の版と、それを指すリンク。
    fs.mkdirSync(path.join(lib, 'node-v24.21.0-linux-x64', 'bin'), {
      recursive: true,
    });
    fs.symlinkSync(
      path.join(lib, 'node-v24.21.0-linux-x64'),
      path.join(lib, 'current'),
    );
    fs.mkdirSync(bin, { recursive: true });
    fs.symlinkSync(
      path.join(lib, 'current', 'bin', 'node'),
      path.join(bin, 'node'),
    );
    fs.symlinkSync('/somewhere/old/pnpm.js', path.join(bin, 'pnpm'));

    const first = run(env);
    assert.equal(first.status, 0, first.stdout + first.stderr);
    assert.match(first.stdout, /v24\.22\.3 にしました/);
    assert.equal(
      fs.readlinkSync(path.join(lib, 'current')),
      path.join(lib, env.name),
    );
    assert.ok(
      fs.existsSync(path.join(lib, 'node-v24.21.0-linux-x64')),
      '前の版を消してはいけません',
    );
    for (const tool of ['node', 'npm', 'npx', 'corepack', 'pnpm', 'pnpx']) {
      assert.equal(
        fs.readlinkSync(path.join(bin, tool)),
        path.join(lib, 'current', 'bin', tool),
      );
    }
    assert.equal(
      spawnSync(path.join(bin, 'node'), [], { encoding: 'utf8' }).stdout.trim(),
      'v24.22.3',
    );
    assert.deepEqual(
      fs.readFileSync(path.join(root, 'curl.log'), 'utf8').trim().split('\n'),
      [
        `https://nodejs.org/dist/v24.22.3/${env.name}.tar.xz`,
        'https://nodejs.org/dist/v24.22.3/SHASUMS256.txt',
      ],
    );
    assert.deepEqual(
      fs.readdirSync(lib).filter((f) => f.startsWith('.')),
      [],
      '作業用のディレクトリを残してはいけません',
    );

    // 2回目は取得も切り替えもしない。
    const second = run(env);
    assert.equal(second.status, 0, second.stderr);
    assert.equal(second.stdout, '');
    assert.equal(
      fs.readFileSync(path.join(root, 'curl.log'), 'utf8').trim().split('\n')
        .length,
      2,
    );
  });
});

test('照合に合わない配布物では今のNodeを保持する', () => {
  withDir((root) => {
    const env = setup(root, { version: '24.22.3', tamper: true });
    const lib = path.join(env.home, '.local', 'lib', 'nodejs');
    fs.mkdirSync(path.join(lib, 'node-v24.21.0-linux-x64'), {
      recursive: true,
    });
    fs.symlinkSync(
      path.join(lib, 'node-v24.21.0-linux-x64'),
      path.join(lib, 'current'),
    );
    const result = run(env);
    assert.notEqual(result.status, 0);
    assert.match(result.stdout, /host-node-failed/);
    assert.equal(
      fs.readlinkSync(path.join(lib, 'current')),
      path.join(lib, 'node-v24.21.0-linux-x64'),
    );
    assert.ok(!fs.existsSync(path.join(lib, env.name)));
    assert.deepEqual(
      fs.readdirSync(lib).filter((f) => f.startsWith('.')),
      [],
    );
  });
});

test('展開したnodeの版が違えば切り替えない', () => {
  withDir((root) => {
    const env = setup(root, { version: '24.22.3', reported: '24.22.2' });
    const result = run(env);
    assert.notEqual(result.status, 0);
    assert.match(result.stdout, /host-node-failed/);
    assert.ok(
      !fs.existsSync(path.join(env.home, '.local', 'lib', 'nodejs', 'current')),
    );
  });
});

test('リンクでない ~/.local/bin のファイルは置き換えない', () => {
  withDir((root) => {
    const env = setup(root, { version: '24.22.3' });
    const bin = path.join(env.home, '.local', 'bin');
    fs.mkdirSync(bin, { recursive: true });
    script(path.join(bin, 'npm'), 'echo mine');
    const result = run(env);
    assert.notEqual(result.status, 0);
    assert.match(result.stdout, /npm がリンクではない/);
    assert.equal(
      fs.readFileSync(path.join(bin, 'npm'), 'utf8'),
      '#!/bin/sh\necho mine\n',
    );
  });
});

test('配布元が応答しないときは時間切れで失敗し、今のNodeを保持する', () => {
  withDir((root) => {
    const env = setup(root, { version: '24.22.3' });
    script(path.join(env.fake, 'curl'), 'sleep 30');
    const lib = path.join(env.home, '.local', 'lib', 'nodejs');
    fs.mkdirSync(path.join(lib, 'node-v24.21.0-linux-x64'), {
      recursive: true,
    });
    fs.symlinkSync(
      path.join(lib, 'node-v24.21.0-linux-x64'),
      path.join(lib, 'current'),
    );
    const started = Date.now();
    const result = run(env, { ISSUE_DESK_NODE_FETCH_TIMEOUT: '1' });
    assert.ok(Date.now() - started < 15000, '取得の時間上限が効いていません');
    assert.notEqual(result.status, 0);
    assert.match(result.stdout, /host-node-failed/);
    assert.equal(
      fs.readlinkSync(path.join(lib, 'current')),
      path.join(lib, 'node-v24.21.0-linux-x64'),
    );
    assert.deepEqual(
      fs.readdirSync(lib).filter((f) => f.startsWith('.')),
      [],
    );
  });
});

test('前の実行で残った一時リンクがあっても新しい版へ切り替える', () => {
  withDir((root) => {
    const env = setup(root, { version: '24.22.3' });
    const lib = path.join(env.home, '.local', 'lib', 'nodejs');
    const bin = path.join(env.home, '.local', 'bin');
    fs.mkdirSync(path.join(lib, 'node-v24.21.0-linux-x64', 'bin'), {
      recursive: true,
    });
    fs.symlinkSync(
      path.join(lib, 'node-v24.21.0-linux-x64'),
      path.join(lib, 'current'),
    );
    fs.symlinkSync(
      path.join(lib, 'node-v24.21.0-linux-x64'),
      path.join(lib, '.current.tmp'),
    );
    fs.mkdirSync(path.join(bin), { recursive: true });
    fs.symlinkSync(
      path.join(lib, 'node-v24.21.0-linux-x64', 'bin'),
      path.join(bin, '.node.tmp'),
    );
    const result = run(env);
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.equal(
      fs.readlinkSync(path.join(lib, 'current')),
      path.join(lib, env.name),
    );
    assert.equal(
      fs.readlinkSync(path.join(bin, 'node')),
      path.join(lib, 'current', 'bin', 'node'),
    );
    assert.ok(!fs.existsSync(path.join(lib, '.current.tmp')));
  });
});
