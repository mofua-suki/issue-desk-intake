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
const setup = (
  root,
  { version, reported = version, tamper = false, failure = '' },
) => {
  const repo = path.join(root, 'repo');
  fs.mkdirSync(path.join(repo, 'deploy'), { recursive: true });
  fs.writeFileSync(
    path.join(repo, 'deploy', 'versions.json'),
    `{\n  "go": "1.0.0",\n  "node": "${version}",\n  "pnpm": "11.26.0"\n}\n`,
  );
  const name = `node-v${version}-linux-x64`;
  const build = path.join(root, 'build');
  fs.mkdirSync(path.join(build, name, 'bin'), { recursive: true });
  script(
    path.join(build, name, 'bin', 'node'),
    `${failure === 'final' ? 'case "$0" in ./*|*/.download.*/*) ;; *) exit 1;; esac\n' : ''}echo v${reported}`,
  );
  script(
    path.join(build, name, 'bin', 'npm'),
    `
if [ "$1" = install ]; then
  [ "${failure}" != fetch ] || exit 1
  shift; shift; prefix=$1
  for pair in corepack:0.36.0 pnpm:11.26.0 pnpx:11.26.0; do
    tool=\${pair%:*}; value=\${pair#*:}
    printf '#!/bin/sh\necho %s\n' "$value" > "$prefix/bin/$tool"
    [ "${failure}" != "$tool" ] || printf '#!/bin/sh\\nexit 1\\n' > "$prefix/bin/$tool"
    if [ "$tool" = pnpx ]; then
      printf '#!/bin/sh\n[ "$1" = --help ] || exit 1\nprintf "Version 11.26.0\\nUsage: pnpm dlx <command> [args...]\\n"\n' > "$prefix/bin/$tool"
      case "${failure}" in
        pnpx) printf '#!/bin/sh\nexit 1\n' > "$prefix/bin/$tool" ;;
        pnpx-version) sed -i 's/Version 11.26.0/Version 11.26.1/' "$prefix/bin/$tool" ;;
        pnpx-usage) sed -i 's/Usage: pnpm dlx/Usage: other/' "$prefix/bin/$tool" ;;
      esac
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

const tools = ['node', 'npm', 'npx', 'corepack', 'pnpm', 'pnpx'];
const oldInstallation = (env) => {
  const lib = path.join(env.home, '.local', 'lib', 'nodejs');
  const bin = path.join(env.home, '.local', 'bin');
  const old = path.join(lib, 'old');
  fs.mkdirSync(path.join(old, 'bin'), { recursive: true });
  fs.mkdirSync(bin, { recursive: true });
  fs.symlinkSync(old, path.join(lib, 'current'));
  for (const tool of tools) {
    script(path.join(old, 'bin', tool), `echo old-${tool}`);
    fs.symlinkSync(
      path.join(lib, 'current', 'bin', tool),
      path.join(bin, tool),
    );
  }
  return { lib, bin, old };
};

test('候補を検証してから切り替え、全入口と再実行を維持する', () => {
  withDir((root) => {
    const env = setup(root, { version: '26.13.1' });
    const { lib, bin } = oldInstallation(env);
    for (let i = 0; i < 2; i++) {
      const result = run(env);
      assert.equal(result.status, 0, result.stdout + result.stderr);
      assert.equal(
        fs.readlinkSync(path.join(lib, 'current')),
        path.join(lib, env.name),
      );
      for (const [tool, expected] of Object.entries({
        node: 'v26.13.1',
        npm: 'npm',
        npx: 'npx',
        corepack: '0.36.0',
        pnpm: '11.26.0',
      })) {
        const command = spawnSync(path.join(bin, tool), ['--version'], {
          encoding: 'utf8',
        });
        assert.equal(command.status, 0, command.stderr);
        assert.equal(command.stdout.trim(), expected);
      }
      const pnpxVersion = spawnSync(path.join(bin, 'pnpx'), ['--version'], {
        encoding: 'utf8',
      });
      assert.notEqual(pnpxVersion.status, 0);
      const pnpxHelp = spawnSync(path.join(bin, 'pnpx'), ['--help'], {
        encoding: 'utf8',
      });
      assert.equal(pnpxHelp.status, 0);
      assert.match(
        pnpxHelp.stdout,
        /^Version 11\.26\.0\nUsage: pnpm dlx <command> \[args\.\.\.\]$/m,
      );
    }
  });
});

test('取得または候補の起動に失敗すると旧入口を維持する', () => {
  for (const failure of [
    'fetch',
    'corepack',
    'pnpm',
    'pnpx',
    'pnpx-version',
    'pnpx-usage',
  ]) {
    withDir((root) => {
      const env = setup(root, { version: '26.13.1', failure });
      const { lib, bin, old } = oldInstallation(env);
      for (let i = 0; i < 2; i++) {
        const result = run(env);
        assert.notEqual(result.status, 0, failure);
        assert.equal(fs.readlinkSync(path.join(lib, 'current')), old);
        assert.equal(fs.existsSync(path.join(lib, env.name)), false);
        for (const tool of tools) {
          const command = spawnSync(path.join(bin, tool), ['--version'], {
            encoding: 'utf8',
          });
          assert.equal(command.status, 0);
          assert.equal(command.stdout.trim(), `old-${tool}`);
        }
      }
    });
  }
});

test('Corepack と pnpm のない既存候補を取り直して全入口を復旧する', () => {
  for (const currentIsCandidate of [false, true]) {
    withDir((root) => {
      const env = setup(root, { version: '26.13.1' });
      const { lib, bin } = oldInstallation(env);
      const candidate = path.join(lib, env.name, 'bin');
      fs.mkdirSync(candidate, { recursive: true });
      script(path.join(candidate, 'node'), 'echo v26.13.1');
      if (currentIsCandidate) {
        fs.unlinkSync(path.join(lib, 'current'));
        fs.symlinkSync(path.join(lib, env.name), path.join(lib, 'current'));
      }
      for (let i = 0; i < 2; i++) {
        const result = run(env);
        assert.equal(result.status, 0, result.stdout + result.stderr);
        assert.deepEqual(
          fs.readdirSync(lib).filter((name) => name.startsWith('.replaced.')),
          [],
        );
        assert.equal(
          fs.readlinkSync(path.join(lib, 'current')),
          path.join(lib, env.name),
        );
        for (const [tool, expected] of Object.entries({
          node: 'v26.13.1',
          npm: 'npm',
          npx: 'npx',
          corepack: '0.36.0',
          pnpm: '11.26.0',
        })) {
          const command = spawnSync(path.join(bin, tool), ['--version'], {
            encoding: 'utf8',
          });
          assert.equal(command.status, 0, command.stderr);
          assert.equal(command.stdout.trim(), expected);
        }
      }
    });
  }
});

test('配置後の最終検証に失敗すると退避した候補を保持する', () => {
  withDir((root) => {
    const env = setup(root, { version: '26.13.1', failure: 'final' });
    const { lib, old } = oldInstallation(env);
    const candidate = path.join(lib, env.name, 'bin');
    fs.mkdirSync(candidate, { recursive: true });
    script(path.join(candidate, 'node'), 'echo original');
    const result = run(env);
    assert.notEqual(result.status, 0);
    assert.equal(fs.readlinkSync(path.join(lib, 'current')), old);
    const backups = fs
      .readdirSync(lib)
      .filter((name) => name.startsWith('.replaced.'));
    assert.equal(backups.length, 1);
    const saved = path.join(lib, backups[0], env.name, 'bin', 'node');
    assert.equal(
      spawnSync(saved, ['--version'], { encoding: 'utf8' }).stdout.trim(),
      'original',
    );
  });
});

test('既存候補の取り直しに失敗すると元の候補と入口を保持する', () => {
  withDir((root) => {
    const env = setup(root, { version: '26.13.1', failure: 'fetch' });
    const { lib, bin, old } = oldInstallation(env);
    const candidate = path.join(lib, env.name, 'bin');
    fs.mkdirSync(candidate, { recursive: true });
    script(path.join(candidate, 'node'), 'echo v26.13.1');
    const result = run(env);
    assert.notEqual(result.status, 0);
    assert.equal(fs.readlinkSync(path.join(lib, 'current')), old);
    assert.equal(
      spawnSync(path.join(candidate, 'node'), ['--version'], {
        encoding: 'utf8',
      }).stdout.trim(),
      'v26.13.1',
    );
    assert.equal(
      spawnSync(path.join(bin, 'node'), ['--version'], {
        encoding: 'utf8',
      }).stdout.trim(),
      'old-node',
    );
  });
});
