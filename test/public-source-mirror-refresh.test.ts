import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  rmSync,
  readFileSync,
  writeFileSync,
} from 'node:fs';
import path from 'node:path';

const root = path.resolve(import.meta.dirname, '..');
function command(cmd: string, args: string[], env = process.env) {
  const result = spawnSync(cmd, args, { encoding: 'utf8', env });
  assert.equal(result.status, 0, result.stderr);
  return result.stdout.trim();
}
function fixture() {
  const dir = mkdtempSync(path.join(root, '.mirror-test-'));
  const work = path.join(dir, 'upstream');
  const repo = path.join(dir, 'mirror.git');
  const bin = path.join(dir, 'bin');
  const scripts = path.join(dir, 'install/scripts');
  const nas = path.join(dir, 'install/deploy/nas');
  const runs = path.join(dir, 'runs');
  for (const p of [scripts, nas, runs, bin])
    mkdirSync(p, { recursive: true, mode: 0o700 });
  command('git', ['init', '-q', '-b', 'main', work]);
  command('git', ['-C', work, 'config', 'user.name', 'Test']);
  command('git', ['-C', work, 'config', 'user.email', 'test@example.invalid']);
  writeFileSync(path.join(work, 'file'), 'first');
  command('git', ['-C', work, 'add', 'file']);
  command('git', ['-C', work, 'commit', '-qm', 'first']);
  command('git', ['clone', '-q', '--bare', work, repo]);
  chmodSync(repo, 0o700);
  copyFileSync(
    path.join(root, 'scripts/refresh-public-source-mirrors.cjs'),
    path.join(scripts, 'refresh-public-source-mirrors.cjs'),
  );
  copyFileSync(
    path.join(root, 'deploy/nas/run-public-source-preparation.sh'),
    path.join(nas, 'run-public-source-preparation.sh'),
  );
  const log = path.join(dir, 'prepared');
  writeFileSync(
    path.join(scripts, 'run-public-source-jobs.sh'),
    `#!/bin/sh\ntouch '${log}'\n`,
  );
  const registry = path.join(dir, 'registry.json');
  const jobs = path.join(dir, 'jobs.json');
  const refresh = path.join(dir, 'refresh.json');
  const item = { repo, remote: 'origin', ref: 'refs/heads/main' };
  writeFileSync(
    registry,
    JSON.stringify({ one: { source: { repo }, ref: item.ref } }),
    { mode: 0o600 },
  );
  writeFileSync(
    jobs,
    JSON.stringify({ one: { entry: 'public-source', registry } }),
    { mode: 0o600 },
  );
  writeFileSync(refresh, JSON.stringify({ one: item }), { mode: 0o600 });
  writeFileSync(
    path.join(bin, 'git'),
    `#!/bin/sh
case " $* " in
  *' fetch '*) [ "$GIT_TERMINAL_PROMPT" = 0 ] && [ "$GIT_ASKPASS" = /bin/false ] && [ "$GIT_CONFIG_GLOBAL" = /dev/null ] || exit 1; case " $* " in *' credential.helper= '*' http.extraHeader= '*) ;; *) exit 1;; esac;;
esac
exec /usr/bin/git "$@"
`,
    { mode: 0o755 },
  );
  const env = {
    ...process.env,
    PATH: bin + ':' + process.env.PATH,
    JOBS_REGISTRY: jobs,
    PREPARATION_ROOT: runs,
    MIRROR_REFRESH_REGISTRY: refresh,
  };
  const run = (extra: Record<string, string> = {}) =>
    spawnSync('sh', [path.join(nas, 'run-public-source-preparation.sh')], {
      encoding: 'utf8',
      env: { ...env, ...extra },
    });
  return { dir, work, repo, refresh, item, log, run, scripts, bin };
}

test('定期準備は登録された参照を更新してから準備する', () => {
  const f = fixture();
  try {
    command('git', ['-C', f.work, 'branch', 'other']);
    writeFileSync(path.join(f.work, 'file'), 'second');
    command('git', ['-C', f.work, 'commit', '-qam', 'second']);
    const expected = command('git', ['-C', f.work, 'rev-parse', 'HEAD']);
    const result = f.run();
    assert.equal(result.status, 0, result.stderr);
    assert.equal(
      command('git', ['-C', f.repo, 'rev-parse', 'refs/heads/main']),
      expected,
    );
    assert.notEqual(
      spawnSync('git', [
        '-C',
        f.repo,
        'show-ref',
        '--verify',
        'refs/heads/other',
      ]).status,
      0,
    );
    assert.ok(existsSync(f.log));
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('不正な登録と取得失敗では準備を止め秘密を表示しない', () => {
  const f = fixture();
  try {
    for (const item of [
      { ...f.item, ref: '--secret' },
      { ...f.item, remote: 'missing-secret' },
    ]) {
      writeFileSync(f.refresh, JSON.stringify({ one: item }));
      const result = f.run();
      assert.notEqual(result.status, 0);
      assert.doesNotMatch(
        result.stdout + result.stderr,
        /secret|refresh\.json/,
      );
      assert.equal(existsSync(f.log), false);
    }
    writeFileSync(f.refresh, JSON.stringify({ one: f.item }));
    command('git', [
      '-C',
      f.repo,
      'remote',
      'set-url',
      'origin',
      path.join(f.dir, 'missing-secret'),
    ]);
    const failed = f.run();
    assert.notEqual(failed.status, 0);
    assert.doesNotMatch(failed.stdout + failed.stderr, /secret/);
    assert.equal(existsSync(f.log), false);
    assert.equal(f.run({ MIRROR_REFRESH_REGISTRY: '' }).status, 0);
    assert.ok(existsSync(f.log));
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('コミットでない上流参照は準備を開始しない', () => {
  const f = fixture();
  try {
    const blob = command('git', ['-C', f.work, 'hash-object', '-w', 'file']);
    writeFileSync(path.join(f.work, '.git/refs/heads/main'), blob + '\n');
    const result = f.run();
    assert.notEqual(result.status, 0);
    assert.equal(existsSync(f.log), false);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('公開基点を更新して準備へ渡し、公開側の異常では停止する', () => {
  const f = fixture();
  try {
    const publicWork = path.join(f.dir, 'public-upstream');
    const publicRepo = path.join(f.dir, 'public.git');
    command('git', ['clone', '-q', f.work, publicWork]);
    command('git', ['-C', publicWork, 'config', 'user.name', 'Test']);
    command('git', [
      '-C',
      publicWork,
      'config',
      'user.email',
      'test@example.invalid',
    ]);
    command('git', ['clone', '-q', '--bare', publicWork, publicRepo]);
    chmodSync(publicRepo, 0o700);
    const pub = { repo: publicRepo, remote: 'origin', ref: 'refs/heads/main' };
    const registry = path.join(f.dir, 'registry.json');
    writeFileSync(
      registry,
      JSON.stringify({
        one: {
          source: {
            repo: f.repo,
            paths: ['file'],
            replacements: [],
            reject: [],
            ci: 'intake',
            verify: 'intake',
            image: 'test@sha256:' + 'a'.repeat(64),
          },
          ref: f.item.ref,
          publicRepo,
          publicRef: pub.ref,
        },
      }),
    );
    copyFileSync(
      path.join(root, 'scripts/run-public-source-jobs.cjs'),
      path.join(f.scripts, 'run-public-source-jobs.cjs'),
    );
    copyFileSync(
      path.join(root, 'scripts/run-public-source-jobs.sh'),
      path.join(f.scripts, 'run-public-source-jobs.sh'),
    );
    writeFileSync(
      path.join(f.scripts, 'publish-public-source.sh'),
      `#!/bin/sh
while [ "$1" != "--public-base" ]; do shift; done
printf '%s' "$2" > '${f.log}'
printf '{"target":"one","status":"skipped"}\n'
`,
    );
    writeFileSync(path.join(f.bin, 'docker'), '#!/bin/sh\nexit 0\n', {
      mode: 0o755,
    });
    const privateConfig = path.join(f.dir, 'private-home');
    mkdirSync(privateConfig, { mode: 0o700 });
    writeFileSync(
      path.join(privateConfig, '.gitconfig'),
      '[credential]\n helper = sentinel\n',
    );
    // The wrapper checks each fetch's environment and config without using credentials.
    writeFileSync(
      path.join(f.bin, 'git'),
      `#!/bin/sh
case " $* " in
  *' fetch '*)
    case " $* " in
      *' ${publicRepo} '*) [ "$GIT_CONFIG_GLOBAL" = /dev/null ] && [ -z "$SSH_AUTH_SOCK" ] && [ "$GIT_SSH_COMMAND" = /bin/false ] || exit 1 ;;
      *) [ "$GIT_CONFIG_GLOBAL" = '${privateConfig}/.gitconfig' ] || exit 1 ;;
    esac ;;
esac
exec /usr/bin/git "$@"
`,
      { mode: 0o755 },
    );
    const item = { ...f.item, httpAuth: true, public: pub };
    writeFileSync(f.refresh, JSON.stringify({ one: item }));
    writeFileSync(path.join(publicWork, 'file'), 'public second');
    command('git', ['-C', publicWork, 'commit', '-qam', 'public second']);
    const expected = command('git', ['-C', publicWork, 'rev-parse', 'HEAD']);
    const privateBefore = command('git', [
      '-C',
      f.repo,
      'rev-parse',
      f.item.ref,
    ]);
    const env = { HOME: privateConfig, SSH_AUTH_SOCK: '/fake-agent' };
    const success = f.run(env);
    assert.equal(success.status, 0, success.stderr + success.stdout);
    assert.equal(readFileSync(f.log, 'utf8'), expected);
    assert.equal(
      command('git', ['-C', f.repo, 'rev-parse', f.item.ref]),
      privateBefore,
    );
    rmSync(f.log);
    for (const bad of [
      { ...pub, ref: 'refs/heads/other' },
      { ...pub, remote: 'missing' },
    ]) {
      writeFileSync(
        f.refresh,
        JSON.stringify({ one: { ...item, public: bad } }),
      );
      assert.notEqual(f.run(env).status, 0);
      assert.equal(existsSync(f.log), false);
    }
    writeFileSync(f.refresh, JSON.stringify({ one: item }));
    command('git', [
      '-C',
      publicRepo,
      'remote',
      'set-url',
      'origin',
      path.join(f.dir, 'missing'),
    ]);
    assert.notEqual(f.run(env).status, 0);
    assert.equal(existsSync(f.log), false);
    command('git', [
      '-C',
      publicRepo,
      'remote',
      'set-url',
      'origin',
      publicWork,
    ]);
    const blob = command('git', [
      '-C',
      publicWork,
      'hash-object',
      '-w',
      'file',
    ]);
    writeFileSync(path.join(publicWork, '.git/refs/heads/main'), blob + '\n');
    assert.notEqual(f.run(env).status, 0);
    assert.equal(existsSync(f.log), false);
    writeFileSync(
      f.refresh,
      JSON.stringify({ one: { ...f.item, httpAuth: true } }),
    );
    assert.equal(f.run(env).status, 0);
    assert.ok(existsSync(f.log));
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});
