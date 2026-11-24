import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import {
  cpSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const script = resolve('scripts/build-public-intake-verify-image.sh');
const files = [
  'server/go.mod',
  'server/go.sum',
  'client/package.json',
  'client/pnpm-lock.yaml',
  'client/pnpm-workspace.yaml',
  'deploy/versions.json',
];
const digest = 'a'.repeat(64);

function fixture() {
  const root = mkdtempSync(join(tmpdir(), 'intake-image-test-'));
  const repo = join(root, 'repo');
  const bin = join(root, 'bin');
  const captured = join(root, 'captured');
  const args = join(root, 'args');
  mkdirSync(repo);
  mkdirSync(bin);
  for (const file of files) {
    mkdirSync(join(repo, file, '..'), { recursive: true });
    cpSync(resolve(file), join(repo, file));
  }
  const git = (...args: string[]) =>
    execFileSync('git', ['-C', repo, ...args], {
      env: {
        ...process.env,
        GIT_CONFIG_GLOBAL: '/dev/null',
        GIT_CONFIG_NOSYSTEM: '1',
      },
      stdio: 'pipe',
    })
      .toString()
      .trim();
  git('init');
  git('config', 'user.name', 'Test');
  git('config', 'user.email', 'test@example.invalid');
  writeFileSync(join(repo, 'secret.txt'), 'committed-secret-sentinel');
  git('add', '.');
  git('commit', '-m', 'fixture');
  const sha = git('rev-parse', 'HEAD');
  writeFileSync(join(repo, 'secret.txt'), 'working-tree-secret-sentinel');
  writeFileSync(join(repo, 'server/go.mod'), 'working-tree-sentinel');
  writeFileSync(
    join(bin, 'docker'),
    `#!/bin/sh\nprintf '%s\\n' "$@" > "$ARGS"\nfor last do\n  case "$last" in id=intake_ca,src=*) cp "\${last#id=intake_ca,src=}" "$CAPTURED-ca";; esac\ndone\ncp -R "$last" "$CAPTURED"\n`,
    { mode: 0o755 },
  );
  const run = (
    commit = sha,
    go = `go@sha256:${digest}`,
    node = `node@sha256:${digest}`,
    ca?: string,
  ) =>
    spawnSync('sh', [script, repo, commit, 'test-image', go, node], {
      encoding: 'utf8',
      env: {
        ...process.env,
        PATH: `${bin}:${process.env.PATH}`,
        CAPTURED: captured,
        ARGS: args,
        PUBLIC_INTAKE_CA_BUNDLE: ca,
      },
    });
  return { root, repo, bin, captured, args, git, sha, run };
}

test('指定した証明書束を一時secretで渡し、コンテキストと引数に本文を含めない', () => {
  const f = fixture();
  try {
    const ca = join(f.root, 'ca.pem');
    execFileSync(
      'openssl',
      [
        'req',
        '-x509',
        '-newkey',
        'rsa:2048',
        '-nodes',
        '-keyout',
        join(f.root, 'key'),
        '-out',
        ca,
        '-subj',
        '/CN=synthetic',
        '-days',
        '1',
      ],
      { stdio: 'ignore' },
    );
    const pem = readFileSync(ca, 'utf8');
    const result = f.run(
      f.sha,
      `go@sha256:${digest}`,
      `node@sha256:${digest}`,
      ca,
    );
    assert.equal(result.status, 0, result.stderr);
    assert.equal(readFileSync(`${f.captured}-ca`, 'utf8'), pem);
    const args = readFileSync(f.args, 'utf8');
    assert.match(args, /id=intake_ca,src=/);
    assert.doesNotMatch(
      args + result.stdout + result.stderr,
      /-----BEGIN CERTIFICATE-----/,
    );
    const secretPath = args.match(/id=intake_ca,src=([^\n]+)/)?.[1];
    assert.ok(secretPath);
    assert.throws(() => readFileSync(secretPath));
    const contents = (dir: string): string =>
      readdirSync(dir, { withFileTypes: true })
        .map((e) =>
          e.isDirectory()
            ? contents(join(dir, e.name))
            : readFileSync(join(dir, e.name), 'utf8'),
        )
        .join('');
    assert.ok(!contents(f.captured).includes(pem));
    writeFileSync(
      join(f.bin, 'docker'),
      `#!/bin/sh\nprintf '%s\n' "$@" > "$ARGS"\nexit 37\n`,
      { mode: 0o755 },
    );
    const failed = f.run(
      f.sha,
      `go@sha256:${digest}`,
      `node@sha256:${digest}`,
      ca,
    );
    assert.equal(failed.status, 37);
    const failedSecretPath = readFileSync(f.args, 'utf8').match(
      /id=intake_ca,src=([^\n]+)/,
    )?.[1];
    assert.ok(failedSecretPath);
    assert.throws(() => readFileSync(failedSecretPath));
    assert.doesNotMatch(
      failed.stdout + failed.stderr,
      /-----BEGIN CERTIFICATE-----/,
    );
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

test('不正な証明書束をビルド前に拒否する', () => {
  const f = fixture();
  try {
    const ca = join(f.root, 'ca.pem');
    for (const value of [
      '',
      'not a certificate',
      '-----BEGIN CERTIFICATE-----\ninvalid\n-----END CERTIFICATE-----\n',
    ]) {
      writeFileSync(ca, value);
      assert.notEqual(
        f.run(f.sha, `go@sha256:${digest}`, `node@sha256:${digest}`, ca).status,
        0,
      );
    }
    assert.notEqual(
      f.run(
        f.sha,
        `go@sha256:${digest}`,
        `node@sha256:${digest}`,
        join(f.root, 'missing'),
      ).status,
      0,
    );
    assert.notEqual(
      f.run(f.sha, `go@sha256:${digest}`, `node@sha256:${digest}`, '').status,
      0,
    );
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

test('公開候補の実行にCAを渡さず隔離設定を維持する', () => {
  const root = mkdtempSync(join(tmpdir(), 'intake-verifier-test-'));
  try {
    const bin = join(root, 'bin');
    const candidate = join(root, 'candidate');
    const calls = join(root, 'calls');
    mkdirSync(bin);
    mkdirSync(candidate);
    writeFileSync(join(candidate, 'sample'), 'synthetic candidate');
    writeFileSync(
      join(bin, 'docker'),
      `#!/bin/sh\nprintf '%s\\n' "$@" >> '${calls}'\ncase " $* " in *" image inspect "*) echo null;; esac\n`,
      { mode: 0o755 },
    );
    const result = spawnSync(
      process.execPath,
      [
        'scripts/public-source-verifier.mts',
        candidate,
        'intake',
        'synthetic-image',
      ],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          PATH: `${bin}:${process.env.PATH}`,
          CALLS: calls,
          PUBLIC_INTAKE_CA_BUNDLE: '/synthetic/ca',
        },
      },
    );
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /^[a-f0-9]{64}$/);
    const args = readFileSync(calls, 'utf8');
    for (const flag of [
      '--network\nnone',
      '--read-only',
      '--user\n1000:1000',
      'GOPROXY=off',
    ])
      assert.ok(args.includes(flag), flag);
    assert.ok(!args.includes('/synthetic/ca'));
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('指定コミットの依存定義だけをビルドコンテキストに渡す', () => {
  const f = fixture();
  try {
    const result = f.run();
    assert.equal(result.status, 0, result.stderr);
    assert.doesNotMatch(readFileSync(f.args, 'utf8'), /id=intake_ca/);
    const list = (dir: string, prefix = ''): string[] =>
      readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
        e.isDirectory()
          ? list(join(dir, e.name), prefix + e.name + '/')
          : [prefix + e.name],
      );
    assert.deepEqual(list(f.captured).sort(), ['Dockerfile', ...files].sort());
    for (const file of files)
      assert.equal(
        readFileSync(join(f.captured, file), 'utf8'),
        f.git('show', `${f.sha}:${file}`) + '\n',
      );
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

test('不正なコミットと不変でないベースを拒否する', () => {
  const f = fixture();
  try {
    for (const ref of ['HEAD', '0'.repeat(40), f.sha.slice(0, 12), `${f.sha}^`])
      assert.notEqual(f.run(ref).status, 0);
    assert.notEqual(f.run(f.sha, 'go:latest').status, 0);
    assert.notEqual(
      f.run(f.sha, `go@sha256:${digest}`, 'node@sha256:bad').status,
      0,
    );
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

test('必須依存の欠落と通常ファイル以外を拒否する', () => {
  const f = fixture();
  try {
    for (const file of files) {
      f.git('reset', '--hard', f.sha);
      rmSync(join(f.repo, file));
      f.git('add', '-A');
      f.git('commit', '-m', 'missing');
      assert.notEqual(f.run(f.git('rev-parse', 'HEAD')).status, 0, file);
    }
    f.git('reset', '--hard', f.sha);
    rmSync(join(f.repo, 'server/go.sum'));
    symlinkSync('go.mod', join(f.repo, 'server/go.sum'));
    f.git('add', '-A');
    f.git('commit', '-m', 'symlink');
    assert.notEqual(f.run(f.git('rev-parse', 'HEAD')).status, 0);
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});
