import { createHash } from 'node:crypto';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  existsSync,
  rmSync,
  statSync,
  chmodSync,
  copyFileSync,
  readdirSync,
  symlinkSync,
} from 'node:fs';
import { join, resolve } from 'node:path';
import { spawnSync, execFileSync } from 'node:child_process';

const script = resolve('scripts/build-public-source.sh');
const license = resolve('deploy/public/LICENSE');

test('公開検査はintakeだけで依存キャッシュの複製をオフラインで使う', () => {
  const root = mkdtempSync(resolve('.public-source-test-'));
  try {
    const source = join(root, 'source');
    const bin = join(root, 'bin');
    const tmp = join(root, 'tmp');
    const deps = join(root, 'deps');
    const log = join(root, 'commands.jsonl');
    for (const dir of [
      source,
      bin,
      tmp,
      join(source, 'client'),
      join(source, 'server'),
      join(deps, 'pnpm'),
      join(deps, 'cache'),
    ])
      mkdirSync(dir, { recursive: true });
    writeFileSync(join(deps, 'pnpm', 'store'), 'store');
    writeFileSync(join(deps, 'cache', 'metadata'), 'metadata');
    const executable = (name: string, body: string) =>
      writeFileSync(join(bin, name), `#!${process.execPath}\n${body}`, {
        mode: 0o755,
      });
    executable(
      'docker',
      `
const {spawnSync} = require('node:child_process');
const args = process.argv.slice(2);
if (args.includes('inspect')) { console.log('null'); process.exit(0); }
const command = args.at(-1).replaceAll('/opt/public-deps', ${JSON.stringify(deps)}).replaceAll('/source', ${JSON.stringify(source)}).replaceAll('/tmp', ${JSON.stringify(tmp)});
const result = spawnSync('/bin/sh', ['-c', command], {env: {...process.env, PATH: ${JSON.stringify(bin)} + ':' + process.env.PATH}, stdio: 'inherit'});
process.exit(result.status ?? 1);
`,
    );
    executable(
      'pnpm',
      `
const fs = require('node:fs');
fs.appendFileSync(${JSON.stringify(log)}, JSON.stringify({args: process.argv.slice(2), cache: process.env.XDG_CACHE_HOME}) + '\\n');
if (process.argv[2] === 'install' && (fs.readFileSync(${JSON.stringify(join(tmp, 'pnpm', 'store'))}, 'utf8') !== 'store' || fs.readFileSync(process.env.XDG_CACHE_HOME + '/metadata', 'utf8') !== 'metadata')) process.exit(1);
`,
    );
    executable(
      'go',
      `
const fs = require('node:fs');
if (process.argv[2] === 'build') {
  const worker = process.argv.at(-1) === './cmd/worker';
  const failure = fs.existsSync(${JSON.stringify(join(root, 'worker-failure'))}) ? fs.readFileSync(${JSON.stringify(join(root, 'worker-failure'))}, 'utf8') : '';
  const output = failure === 'identity' ? 'other-worker revision\\nissue-desk-worker revision' : 'issue-desk-worker revision';
  const versionStatus = failure === 'status' ? 1 : 0;
  fs.writeFileSync(${JSON.stringify(join(tmp, 'application'))}, worker
    ? '#!/bin/sh\\nif [ "$1" = "version" ]; then echo "' + output + '"; exit ' + versionStatus + '; fi\\nexit 1\\n'
    : '#!/bin/sh\\nif [ "$1" = "--help" ]; then echo usage; exit 0; fi\\nexit 1\\n', {mode: 0o755});
}
`,
    );
    for (const method of ['intake', 'worker']) {
      const result = spawnSync(
        process.execPath,
        [
          resolve('scripts/public-source-verifier.mts'),
          source,
          method,
          'fixture',
        ],
        {
          encoding: 'utf8',
          env: { ...process.env, PATH: bin + ':' + process.env.PATH },
        },
      );
      assert.equal(result.status, 0, result.stderr);
      const calls = readFileSync(log, 'utf8')
        .trim()
        .split('\n')
        .map((line) => JSON.parse(line));
      assert.deepEqual(calls, [
        {
          args: [
            'install',
            '--offline',
            '--frozen-lockfile',
            '--ignore-scripts',
            '--store-dir',
            join(tmp, 'pnpm'),
          ],
          cache: join(tmp, 'pnpm-cache'),
        },
        { args: ['build'], cache: join(tmp, 'pnpm-cache') },
      ]);
      rmSync(join(tmp, 'pnpm'), { recursive: true, force: true });
      rmSync(join(tmp, 'pnpm-cache'), { recursive: true, force: true });
    }
    for (const failure of ['identity', 'status']) {
      writeFileSync(join(root, 'worker-failure'), failure);
      const badWorker = spawnSync(
        process.execPath,
        [
          resolve('scripts/public-source-verifier.mts'),
          source,
          'worker',
          'fixture',
        ],
        {
          encoding: 'utf8',
          env: { ...process.env, PATH: bin + ':' + process.env.PATH },
        },
      );
      assert.notEqual(badWorker.status, 0);
    }
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('公開生成の収録と検査は秘密値を出力やログに残さない', () => {
  const root = mkdtempSync(resolve('.public-source-test-'));
  try {
    const repo = join(root, 'repo');
    mkdirSync(repo);
    const git = (...args: string[]) =>
      execFileSync('git', ['-C', repo, ...args], { stdio: 'pipe' });
    git('init');
    git('config', 'user.name', 'Test');
    git('config', 'user.email', 'test@example.invalid');
    const policy = join(root, 'policy.json');
    const internal = 'fixture-internal-value';
    writeFileSync(
      policy,
      JSON.stringify({
        intake: { paths: ['data.txt'], replacements: [], reject: [internal] },
      }),
    );
    const secrets = [
      '-----BEGIN PRIVATE KEY-----',
      'AKIA' + 'X'.repeat(16),
      'ghp_' + 'a'.repeat(36),
      'github_pat_' + 'b'.repeat(24),
      'xoxb-' + 'c'.repeat(20),
      internal,
    ];
    for (const [index, secret] of secrets.entries()) {
      writeFileSync(
        join(repo, 'data.txt'),
        Buffer.concat([Buffer.from([0xff]), Buffer.from(secret)]),
      );
      git('add', 'data.txt');
      git('commit', '-m', 'fixture');
      const output = join(root, `out-${index}`);
      const result = spawnSync(
        'sh',
        [
          script,
          '--target',
          'intake',
          '--repo',
          repo,
          '--ref',
          'HEAD',
          '--output',
          output,
          '--policy',
          policy,
        ],
        { encoding: 'utf8' },
      );
      assert.notEqual(result.status, 0);
      assert.equal(existsSync(output), false);
      assert.equal((result.stdout + result.stderr).includes(secret), false);
    }
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('公開生成の互換性: ヘルプは指定方法を示して正常終了する', () => {
  const result = spawnSync('sh', [script, '--help'], { encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  for (const option of ['--target', '--repo', '--ref', '--output', '--policy'])
    assert.ok(result.stdout.includes(option));
});

test('公開生成の互換性: 両対象で固定コミットと定義から安全に生成する', () => {
  const root = mkdtempSync(resolve('.public-source-test-'));
  try {
    const repo = join(root, 'repo');
    mkdirSync(repo);
    const git = (...args: string[]) =>
      execFileSync('git', ['-C', repo, ...args], { stdio: 'pipe' });
    git('init');
    git('config', 'user.name', 'Test');
    git('config', 'user.email', 'test@example.invalid');
    writeFileSync(join(repo, 'readme.txt'), 'private-host\n');
    writeFileSync(join(repo, 'unapproved.txt'), 'not public\n');
    writeFileSync(join(repo, '.env'), 'secret\n');
    writeFileSync(join(repo, 'LICENSE'), 'fixture license\n');
    git('add', '.');
    git('commit', '-m', 'fixture');
    const commit = git('rev-parse', 'HEAD').toString().trim();
    writeFileSync(join(repo, 'readme.txt'), 'uncommitted\n');
    const policy = join(root, 'policy.json');
    const definition = {
      paths: ['readme.txt'],
      replacements: [
        { path: 'readme.txt', from: 'private-host', to: 'example.invalid' },
      ],
      reject: ['private-host'],
    };
    writeFileSync(
      policy,
      JSON.stringify({ intake: definition, worker: definition }),
    );
    const run = (target: string, output: string, ref = commit) =>
      spawnSync(
        'sh',
        [
          script,
          '--target',
          target,
          '--repo',
          repo,
          '--ref',
          ref,
          '--output',
          output,
          '--policy',
          policy,
        ],
        { encoding: 'utf8' },
      );
    for (const target of ['intake', 'worker']) {
      const first = join(root, target),
        second = join(root, target + '-again');
      const result = run(target, first);
      assert.equal(result.status, 0, result.stderr);
      const lines = result.stdout.trim().split('\n');
      assert.deepEqual(lines.slice(0, -1), [
        ...definition.paths,
        'LICENSE',
        '.github/workflows/verify.yml',
      ]);
      assert.match(lines.at(-1)!, /公開前検査: 成功/);
      assert.equal(statSync(first).mode & 0o777, 0o755);
      const reference = join(root, target + '-reference');
      mkdirSync(reference);
      writeFileSync(join(reference, 'readme.txt'), 'example.invalid\n');
      writeFileSync(join(reference, 'LICENSE'), readFileSync(license));
      mkdirSync(join(reference, '.github/workflows'), { recursive: true });
      copyFileSync(
        resolve(`deploy/public/${target}-verify.yml`),
        join(reference, '.github/workflows/verify.yml'),
      );
      const workflow = readFileSync(
        join(first, '.github/workflows/verify.yml'),
        'utf8',
      );
      for (const expected of [
        "branches: ['**']",
        'contents: read',
        'runs-on: [self-hosted, Linux, X64, pc-ci]',
        'persist-credentials: false',
        'actions/checkout@v5',
        'actions/setup-go@v6',
        'actions/setup-node@v6',
        'pnpm/action-setup@v5',
      ])
        assert.ok(workflow.includes(expected), expected);
      assert.match(
        workflow,
        /on:\n {2}push:\n {4}branches: \['\*\*'\]\npermissions:/,
      );
      assert.doesNotMatch(workflow, /pull_request(?:_target)?:/);
      const expected =
        target === 'intake'
          ? [
              'timeout-minutes: 30',
              '26.13.1',
              'server/go.mod',
              'server/go.sum',
              'client/package.json',
              'client/pnpm-lock.yaml',
              '$RUNNER_TEMP/issue-desk-test-support.',
              'fonts-dejavu-core fonts-noto-cjk gnu-coreutils',
              'dpkg-deb -x',
              'caddy_2.10.2_linux_amd64.tar.gz',
              '5c218bc34c9197369263da7e9317a83acdbd80ef45d94dca5eff76e727c67cdd',
              'sha256sum --check',
              'tar -xzf',
              'gnutail',
              'PATH="$support/bin:$PATH"',
              'FONTCONFIG_FILE="$support/fonts.conf"',
              'ISSUE_DESK_TEST_CADDY="$support/bin/caddy"',
              'sh scripts/verify.sh',
            ]
          : [
              'timeout-minutes: 45',
              'go.mod',
              "jq -r '.node'",
              "jq -r '.pnpm'",
              'GITHUB_OUTPUT',
              '${{ runner.temp }}/issue-desk-ci-pnpm',
              'node --version',
              'gofmt -l',
              'deadcode@v0.52.0 -test ./...',
              '$(id -u)',
              '$RUNNER_TEMP/issue-desk-ci.',
              'git archive HEAD',
              "jq -er '.jdk.url' deploy/sdk-release.json",
              "jq -er '.jdk.sha256' deploy/sdk-release.json",
              'sha256sum --check',
              'tar -xzf',
              'HOME="$work/home"',
              'JAVA_HOME="$work/jdk"',
              'env -i',
              'XDG_CONFIG_HOME',
              'XDG_CACHE_HOME',
              'XDG_DATA_HOME',
              'XDG_STATE_HOME',
              'trap',
              'pnpm --version',
              'docker info',
              'go test -race -timeout 30m ./...',
              'go vet ./...',
            ];
      for (const item of expected) assert.ok(workflow.includes(item), item);
      if (target === 'intake')
        assert.equal(
          (workflow.match(/sh scripts\/verify\.sh/g) || []).length,
          1,
        );
      assert.equal(statSync(join(first, 'LICENSE')).mode & 0o777, 0o644);
      assert.equal(spawnSync('diff', ['-ru', reference, first]).status, 0);
      assert.equal(run(target, second).status, 0);
      assert.equal(
        readFileSync(join(first, 'readme.txt'), 'utf8'),
        'example.invalid\n',
      );
      assert.equal(
        readFileSync(join(first, 'readme.txt'), 'utf8'),
        readFileSync(join(second, 'readme.txt'), 'utf8'),
      );
      for (const name of ['.git', '.env', 'unapproved.txt'])
        assert.equal(existsSync(join(first, name)), false);
      assert.notEqual(run(target, first).status, 0);
      const missing = join(root, target + '-missing');
      assert.notEqual(run(target, missing, 'missing-ref').status, 0);
      assert.equal(existsSync(missing), false);
    }
    for (const target of ['intake', 'worker']) {
      const collision = join(root, target + '-collision');
      writeFileSync(
        policy,
        JSON.stringify({
          [target]: {
            ...definition,
            paths: ['.github/workflows/verify.yml'],
            replacements: [],
          },
        }),
      );
      assert.notEqual(run(target, collision).status, 0);
      assert.equal(existsSync(collision), false);
      const fixture = join(root, target + '-generator');
      mkdirSync(join(fixture, 'scripts'), { recursive: true });
      mkdirSync(join(fixture, 'deploy/public'), { recursive: true });
      copyFileSync(script, join(fixture, 'scripts/build-public-source.sh'));
      copyFileSync(license, join(fixture, 'deploy/public/LICENSE'));
      writeFileSync(policy, JSON.stringify({ [target]: definition }));
      const missingOutput = join(root, target + '-missing-template');
      const missingTemplate = spawnSync(
        'sh',
        [
          join(fixture, 'scripts/build-public-source.sh'),
          '--target',
          target,
          '--repo',
          repo,
          '--ref',
          commit,
          '--output',
          missingOutput,
          '--policy',
          policy,
        ],
        { encoding: 'utf8' },
      );
      assert.notEqual(missingTemplate.status, 0);
      assert.equal(existsSync(missingOutput), false);
    }
    const customLicense = join(root, 'custom-license');
    writeFileSync(
      policy,
      JSON.stringify({
        worker: { ...definition, paths: [...definition.paths, 'LICENSE'] },
      }),
    );
    assert.equal(run('worker', customLicense).status, 0);
    assert.equal(
      readFileSync(join(customLicense, 'LICENSE'), 'utf8'),
      'fixture license\n',
    );
    const blockedLicense = join(root, 'blocked-license');
    writeFileSync(
      policy,
      JSON.stringify({
        intake: { ...definition, reject: ['Apache License'] },
      }),
    );
    const blocked = run('intake', blockedLicense);
    assert.notEqual(blocked.status, 0);
    assert.match(blocked.stderr, /公開前検査に失敗: LICENSE/);
    assert.equal(existsSync(blockedLicense), false);
    writeFileSync(
      policy,
      JSON.stringify({ intake: definition, worker: definition }),
    );
    const bin = join(root, 'bin');
    mkdirSync(bin);
    const fakeMv = join(bin, 'mv');
    writeFileSync(fakeMv, '#!/bin/sh\nexit 1\n');
    chmodSync(fakeMv, 0o755);
    const unsupportedOutput = join(root, 'unsupported');
    const unsupported = spawnSync(
      'sh',
      [
        script,
        '--target',
        'intake',
        '--repo',
        repo,
        '--ref',
        commit,
        '--output',
        unsupportedOutput,
        '--policy',
        policy,
      ],
      {
        encoding: 'utf8',
        env: { ...process.env, PATH: bin + ':' + process.env.PATH },
      },
    );
    assert.notEqual(unsupported.status, 0);
    assert.match(unsupported.stderr, /GNU coreutils/);
    assert.equal(existsSync(unsupportedOutput), false);
    writeFileSync(
      policy,
      JSON.stringify({
        intake: { ...definition, reject: ['example.invalid'] },
      }),
    );
    const rejected = join(root, 'rejected');
    assert.notEqual(run('intake', rejected).status, 0);
    assert.equal(existsSync(rejected), false);
    writeFileSync(
      policy,
      JSON.stringify({
        intake: { ...definition, paths: ['.env'], replacements: [] },
      }),
    );
    assert.notEqual(run('intake', rejected).status, 0);
    assert.equal(existsSync(rejected), false);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('公開生成の互換性: ランナーハッシュを置換後に照合する', () => {
  const root = mkdtempSync(resolve('.public-source-test-'));
  try {
    const repo = join(root, 'repo');
    const operation = 'host-ssh.json';
    const runner = 'deploy/nas-host/install-github-actions-runner.sh';
    const name = 'install-github-actions-runner';
    mkdirSync(join(repo, 'deploy/nas-host'), { recursive: true });
    const git = (...args: string[]) =>
      execFileSync('git', ['-C', repo, ...args], { stdio: 'pipe' });
    git('init');
    git('config', 'user.name', 'Test');
    git('config', 'user.email', 'test@example.invalid');
    const hash = (data: string) =>
      createHash('sha256').update(data).digest('hex');
    const original = '#!/bin/sh\necho private-host\n';
    const changed = original.replace('private-host', 'example.invalid');
    const expected = hash(original);
    const replacementHash = hash(changed);
    writeFileSync(join(repo, runner), original);
    writeFileSync(
      join(repo, operation),
      JSON.stringify({ operations: { [name]: `check ${expected}` } }),
    );
    git('add', '.');
    git('commit', '-m', 'fixture');
    const policy = join(root, 'policy.json');
    const run = (
      label: string,
      paths: string[],
      replacements: readonly object[] = [],
      reject: string[] = [],
    ) => {
      writeFileSync(
        policy,
        JSON.stringify({ intake: { paths, replacements, reject } }),
      );
      const output = join(root, label);
      const result = spawnSync(
        'sh',
        [
          script,
          '--target',
          'intake',
          '--repo',
          repo,
          '--ref',
          'HEAD',
          '--output',
          output,
          '--policy',
          policy,
        ],
        { encoding: 'utf8' },
      );
      return { output, result };
    };
    const paths = [operation, runner];
    const scriptReplacement = {
      path: runner,
      from: 'private-host',
      to: 'example.invalid',
    };
    const originalResult = run('original', paths);
    assert.equal(originalResult.result.status, 0, originalResult.result.stderr);
    assert.equal(
      JSON.parse(readFileSync(join(originalResult.output, operation), 'utf8'))
        .operations[name],
      `check ${expected}`,
    );
    const matched = run('matched', paths, [
      scriptReplacement,
      { path: operation, from: expected, to: replacementHash },
    ]);
    assert.equal(matched.result.status, 0, matched.result.stderr);
    assert.equal(
      JSON.parse(readFileSync(join(matched.output, operation), 'utf8'))
        .operations[name],
      `check ${replacementHash}`,
    );
    assert.equal(readFileSync(join(matched.output, runner), 'utf8'), changed);
    for (const [label, replacements] of [
      ['mismatch', [scriptReplacement]],
      ['missing', [{ path: operation, from: expected, to: 'missing' }]],
      [
        'multiple',
        [
          {
            path: operation,
            from: expected,
            to: `${expected} ${replacementHash}`,
          },
        ],
      ],
    ] as const) {
      const failed = run(label, paths, replacements);
      assert.notEqual(failed.result.status, 0);
      for (const item of [name, operation, runner])
        assert.ok(failed.result.stderr.includes(item));
      assert.equal(existsSync(failed.output), false);
    }
    for (const [label, only] of [
      ['operation-only', operation],
      ['runner-only', runner],
    ]) {
      const partial = run(label, [only]);
      assert.equal(partial.result.status, 0, partial.result.stderr);
      assert.ok(existsSync(join(partial.output, only)));
      assert.deepEqual(
        readFileSync(join(partial.output, 'LICENSE')),
        readFileSync(license),
      );
      assert.deepEqual(
        readFileSync(join(partial.output, '.github/workflows/verify.yml')),
        readFileSync(resolve('deploy/public/intake-verify.yml')),
      );
    }
    const rejected = run('rejected-hash', paths, [], ['private-host']);
    assert.notEqual(rejected.result.status, 0);
    assert.match(rejected.result.stderr, /公開前検査に失敗/);
    assert.equal(existsSync(rejected.output), false);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

function registryFixture() {
  const root = mkdtempSync(resolve('.public-source-test-'));
  const repo = join(root, 'repo');
  const bin = join(root, 'bin');
  const log = join(root, 'runner.log');
  const control = join(root, 'runner-control');
  mkdirSync(repo);
  mkdirSync(bin);
  writeFileSync(log, '');
  const git = (...args: string[]) =>
    execFileSync('git', ['-C', repo, ...args], { stdio: 'pipe' });
  git('init');
  git('config', 'user.name', 'Test');
  git('config', 'user.email', 'test@example.invalid');
  writeFileSync(join(repo, 'file.txt'), 'public data\n');
  writeFileSync(join(repo, '.env'), 'not public\n');
  writeFileSync(join(repo, 'fixture-private.txt'), 'not public\n');
  git('add', '.');
  git('commit', '-m', 'fixture');
  const sha = git('rev-parse', 'HEAD').toString().trim();
  const runner = `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
const control = ${JSON.stringify(control)};
const mode = fs.existsSync(control) ? fs.readFileSync(control, 'utf8') : '';
if (args.includes('inspect')) {
  if (mode === 'inspectfail') process.exit(1);
  process.stdout.write(mode === 'volume' ? '{"/data":{}}' : 'null');
  process.exit(0);
}
fs.appendFileSync(${JSON.stringify(log)}, JSON.stringify(args) + '\\n');
if (mode === 'change') {
  const mount = args[args.indexOf('--mount') + 1];
  const source = mount.slice('type=bind,source='.length).split(',')[0];
  fs.writeFileSync(source + '/file.txt', 'modified');
}
process.stdout.write('synthetic-private-runner-output');
process.stderr.write('synthetic-private-runner-output');
if (mode && mode !== 'change') process.exit(Number(mode));
`;
  writeFileSync(join(bin, 'docker'), runner, { mode: 0o755 });
  const registry = join(root, 'registry.json');
  const base = {
    repo,
    sha,
    paths: ['file.txt'],
    replacements: [],
    reject: ['fixture-internal'],
    ci: 'intake',
    verify: 'intake',
    image: 'public-check@sha256:' + 'a'.repeat(64),
  };
  const run = (target: string, output: string, entries: object) => {
    writeFileSync(registry, JSON.stringify(entries));
    return spawnSync(
      'sh',
      [script, '--target', target, '--registry', registry, '--output', output],
      {
        encoding: 'utf8',
        env: { ...process.env, PATH: bin + ':' + process.env.PATH },
      },
    );
  };
  return { root, repo, git, sha, log, control, base, run };
}

test('公開生成の収録と検査は登録された通常ファイルと付属物だけを確定する', () => {
  const f = registryFixture();
  try {
    const output = join(f.root, 'content');
    const result = f.run('intake', output, { intake: f.base });
    assert.equal(result.status, 0, result.stderr);
    const files = (dir: string, prefix = ''): string[] =>
      readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
        const name = prefix ? prefix + '/' + entry.name : entry.name;
        return entry.isDirectory()
          ? files(join(dir, entry.name), name)
          : [name];
      });
    assert.deepEqual(files(output).sort(), [
      '.github/workflows/verify.yml',
      'LICENSE',
      'file.txt',
    ]);
    const secret = 'fixture-internal';
    writeFileSync(join(f.repo, 'file.txt'), secret);
    f.git('add', 'file.txt');
    f.git('commit', '-m', 'secret');
    const sha = f.git('rev-parse', 'HEAD').toString().trim();
    const rejected = join(f.root, 'secret-output');
    const failed = f.run('intake', rejected, { intake: { ...f.base, sha } });
    assert.notEqual(failed.status, 0);
    assert.equal(existsSync(rejected), false);
    assert.equal((failed.stdout + failed.stderr).includes(secret), false);
    for (const [index, paths] of [['.env'], ['missing.txt']].entries()) {
      const denied = join(f.root, `denied-${index}`);
      assert.notEqual(
        f.run('intake', denied, { intake: { ...f.base, paths } }).status,
        0,
      );
      assert.equal(existsSync(denied), false);
    }
    const incomplete = join(f.root, 'incomplete-generator');
    mkdirSync(join(incomplete, 'scripts'), { recursive: true });
    mkdirSync(join(incomplete, 'deploy/public'), { recursive: true });
    copyFileSync(script, join(incomplete, 'scripts/build-public-source.sh'));
    copyFileSync(license, join(incomplete, 'deploy/public/LICENSE'));
    const registry = join(f.root, 'missing-template-registry.json');
    writeFileSync(registry, JSON.stringify({ intake: f.base }));
    const missing = join(f.root, 'missing-template-output');
    const unscannable = spawnSync(
      'sh',
      [
        join(incomplete, 'scripts/build-public-source.sh'),
        '--target',
        'intake',
        '--registry',
        registry,
        '--output',
        missing,
      ],
      { encoding: 'utf8' },
    );
    assert.notEqual(unscannable.status, 0);
    assert.equal(existsSync(missing), false);
    assert.equal(readFileSync(f.log, 'utf8').trim().split('\n').length, 1);
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

test('公開検査の隔離引数と登録方式、不正な登録の拒否', () => {
  const f = registryFixture();
  try {
    const entries = {
      intake: f.base,
      worker: { ...f.base, ci: 'worker', verify: 'worker' },
      third: {
        ...f.base,
        ci: 'worker',
        verify: 'intake',
        image: '127.0.0.1:5000/public-check@sha256:' + 'b'.repeat(64),
      },
    };
    for (const target of ['intake', 'worker', 'third'] as const) {
      const output = join(f.root, target);
      const result = f.run(target, output, entries);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(
        readFileSync(join(output, 'file.txt'), 'utf8'),
        'public data\n',
      );
      assert.deepEqual(
        readFileSync(join(output, '.github/workflows/verify.yml')),
        readFileSync(resolve(`deploy/public/${entries[target].ci}-verify.yml`)),
      );
      assert.match(result.stdout, new RegExp(`input-sha: ${f.sha}`));
      assert.match(result.stdout, /config-sha256: [0-9a-f]{64}/);
      assert.match(result.stdout, /tree-sha256: [0-9a-f]{64}/);
      assert.equal(existsSync(join(output, '.env')), false);
      assert.equal(existsSync(join(output, 'fixture-private.txt')), false);
    }
    const invocations = readFileSync(f.log, 'utf8')
      .trim()
      .split('\n')
      .map((line) => JSON.parse(line));
    assert.equal(invocations.length, 3);
    for (const [index, mode] of ['intake', 'worker', 'intake'].entries()) {
      const args = invocations[index];
      assert.equal(
        args[args.indexOf('--entrypoint') + 2],
        entries[
          ['intake', 'worker', 'third'][index] as 'intake' | 'worker' | 'third'
        ].image,
      );
      assert.ok(
        args.includes('--network') &&
          args[args.indexOf('--network') + 1] === 'none',
      );
      assert.ok(args.includes('--read-only'));
      assert.equal(args[args.indexOf('--cap-drop') + 1], 'ALL');
      assert.equal(args[args.indexOf('--pull') + 1], 'never');
      assert.equal(
        args[args.indexOf('--security-opt') + 1],
        'no-new-privileges',
      );
      assert.equal(args.filter((arg: string) => arg === '--tmpfs').length, 1);
      assert.equal(
        args[args.indexOf('--tmpfs') + 1],
        '/tmp:rw,exec,nosuid,nodev,uid=1000,gid=1000',
      );
      assert.equal(args.filter((arg: string) => arg === '--mount').length, 1);
      assert.match(
        args[args.indexOf('--mount') + 1],
        /^type=bind,source=.+,destination=\/source,readonly$/,
      );
      assert.ok(
        args.includes('--user') &&
          args[args.indexOf('--user') + 1] === '1000:1000',
      );
      assert.ok(
        args
          .at(-1)
          .includes(mode === 'intake' ? './cmd/intake' : './cmd/worker'),
      );
      assert.match(
        args.at(-1),
        /go build -mod=readonly -o \/tmp\/application /,
      );
      assert.match(
        args.at(-1),
        mode === 'intake'
          ? /\/tmp\/application --help/
          : /\/tmp\/application version/,
      );
    }
    for (const [index, entries] of [
      { intake: f.base },
      { intake: { ...f.base, verify: 'unknown' } },
      { intake: { ...f.base, ci: 'unknown' } },
      { intake: { ...f.base, command: 'echo unsafe' } },
    ].entries()) {
      const output = join(f.root, `invalid-${index}`);
      const result = f.run(index === 0 ? 'missing' : 'intake', output, entries);
      assert.notEqual(result.status, 0);
      assert.equal(existsSync(output), false);
    }
    assert.equal(readFileSync(f.log, 'utf8').trim().split('\n').length, 3);
    for (const [index, image] of [
      'public-check:latest',
      '127.0.0.1:5000/public-check@sha256:' + 'a'.repeat(63),
      '127.0.0.1:5000/public-check@sha256:' + 'A'.repeat(64),
      '127.0.0.1:5001/public-check@sha256:' + 'a'.repeat(64),
      'example.com:5000/public-check@sha256:' + 'a'.repeat(64),
      'https://127.0.0.1:5000/public-check@sha256:' + 'a'.repeat(64),
      'user:password@127.0.0.1:5000/public-check@sha256:' + 'a'.repeat(64),
    ].entries()) {
      const output = join(f.root, `invalid-image-${index}`);
      assert.notEqual(
        f.run('intake', output, { intake: { ...f.base, image } }).status,
        0,
      );
      assert.equal(existsSync(output), false);
    }
    assert.equal(readFileSync(f.log, 'utf8').trim().split('\n').length, 3);
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

test('公開生成の隔離と引渡しは失敗や生成木の変更を確定しない', () => {
  const f = registryFixture();
  try {
    const success = join(f.root, 'success');
    const passed = f.run('intake', success, { intake: f.base });
    assert.equal(passed.status, 0, passed.stderr);
    assert.match(passed.stdout, new RegExp(`input-sha: ${f.sha}`));
    assert.match(passed.stdout, /config-sha256: [a-f0-9]{64}/);
    assert.match(passed.stdout, /tree-sha256: [a-f0-9]{64}/);
    assert.equal(
      readFileSync(join(success, 'file.txt'), 'utf8'),
      'public data\n',
    );
    assert.doesNotMatch(
      passed.stdout + passed.stderr,
      /synthetic-private-runner-output/,
    );
    const id = (result: string, name: string) =>
      new RegExp(`^${name}: ([a-f0-9]+)$`, 'm').exec(result)?.[1];
    const changedConfig = f.run('intake', join(f.root, 'same-tree'), {
      intake: { ...f.base, reject: ['another-internal-value'] },
    });
    assert.equal(changedConfig.status, 0, changedConfig.stderr);
    assert.equal(
      id(passed.stdout, 'tree-sha256'),
      id(changedConfig.stdout, 'tree-sha256'),
    );
    assert.notEqual(
      id(passed.stdout, 'config-sha256'),
      id(changedConfig.stdout, 'config-sha256'),
    );
    writeFileSync(join(f.repo, 'file.txt'), 'different public data\n');
    f.git('add', 'file.txt');
    f.git('commit', '-m', 'changed tree');
    const changedSha = f.git('rev-parse', 'HEAD').toString().trim();
    const changedTree = f.run('intake', join(f.root, 'changed-tree'), {
      intake: { ...f.base, sha: changedSha },
    });
    assert.equal(changedTree.status, 0, changedTree.stderr);
    assert.notEqual(
      id(passed.stdout, 'tree-sha256'),
      id(changedTree.stdout, 'tree-sha256'),
    );
    assert.equal(id(changedTree.stdout, 'input-sha'), changedSha);
    for (const mode of [
      'inspectfail',
      'volume',
      '125',
      '21',
      '22',
      '23',
      'change',
    ]) {
      writeFileSync(f.control, mode);
      const output = join(f.root, 'failure-' + mode);
      const result = f.run('intake', output, {
        intake: {
          ...f.base,
          image: '127.0.0.1:5000/public-check@sha256:' + 'b'.repeat(64),
        },
      });
      assert.notEqual(result.status, 0);
      assert.equal(existsSync(output), false);
      assert.doesNotMatch(
        result.stdout + result.stderr,
        /synthetic-private-runner-output|公開前検査: 成功/,
      );
    }
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

test('Worker runtime は個別の公開ファイルだけを収録し内容検査を通す', () => {
  const f = registryFixture();
  try {
    const dir = 'internal/runtime';
    mkdirSync(join(f.repo, dir, 'testdata'), { recursive: true });
    const go = `${dir}/lease_test.go`,
      json = `${dir}/testdata/sample.json`;
    const files: Record<string, string> = {
      [go]: 'package runtime // fixture-private\n',
      [json]: '{"fixture":true}\n',
      [`${dir}/other.go`]: 'package runtime\n',
      [`${dir}/state.json`]: '{}',
      [`${dir}/testdata/credentials.json`]: '{}',
      [`${dir}/testdata/key.pem`]: 'key',
      'runtime/state.json': '{}',
      'other/runtime/state.json': '{}',
      [`${dir}/secret.go`]: 'package runtime',
      [`${dir}/bad.go`]: '-----BEGIN PRIVATE KEY-----',
      [`${dir}/forbidden.go`]: 'fixture-internal',
    };
    for (const [name, content] of Object.entries(files)) {
      mkdirSync(join(f.repo, name, '..'), { recursive: true });
      writeFileSync(join(f.repo, name), content);
    }
    symlinkSync('other.go', join(f.repo, dir, 'link.go'));
    f.git('add', '.');
    f.git('commit', '-m', 'runtime fixtures');
    const sha = f.git('rev-parse', 'HEAD').toString().trim();
    const definition = {
      ...f.base,
      sha,
      ci: 'worker',
      verify: 'worker',
      paths: [go, json],
      replacements: [
        { path: go, from: 'fixture-private', to: 'fixture-public' },
      ],
    };
    const policy = join(f.root, 'runtime-policy.json');
    const run = (
      mode: string,
      label: string,
      def: typeof definition,
      target = 'worker',
    ) => {
      const output = join(f.root, label);
      if (mode === 'registry')
        return { output, result: f.run(target, output, { [target]: def }) };
      writeFileSync(policy, JSON.stringify({ [target]: def }));
      return {
        output,
        result: spawnSync(
          'sh',
          [
            script,
            ...['--target', '--repo', '--ref', '--output', '--policy'].flatMap(
              (key, i) => [key, [target, f.repo, sha, output, policy][i]],
            ),
          ],
          { encoding: 'utf8' },
        ),
      };
    };
    for (const mode of ['registry', 'legacy']) {
      const ok = run(mode, `${mode}-ok`, definition);
      assert.equal(ok.result.status, 0, ok.result.stderr);
      assert.equal(
        readFileSync(join(ok.output, go), 'utf8'),
        'package runtime // fixture-public\n',
      );
      assert.equal(readFileSync(join(ok.output, json), 'utf8'), files[json]);
      assert.equal(existsSync(join(ok.output, dir, 'other.go')), false);
      const denied = [
        ...'. /*.go /testdata /testdata/*.json /state.json /testdata/credentials.json /testdata/key.pem /secret.go /link.go /bad.go /forbidden.go'
          .split(' ')
          .map((suffix) => dir + (suffix === '.' ? '' : suffix)),
        'runtime/state.json',
        'other/runtime/state.json',
      ];
      for (const [index, name] of denied.entries()) {
        const result = run(mode, `${mode}-denied-${index}`, {
          ...definition,
          paths: [name],
          replacements: [],
        });
        assert.notEqual(result.result.status, 0, name);
        assert.equal(existsSync(result.output), false, name);
      }
      const intake = run(
        mode,
        `${mode}-intake`,
        { ...definition, ci: 'intake', verify: 'intake' },
        'intake',
      );
      assert.notEqual(intake.result.status, 0);
      assert.equal(existsSync(intake.output), false);
    }
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

test('別名収録の置換は同梱LICENSEとCIを変更しない', () => {
  const f = registryFixture();
  try {
    const workflow = '.github/workflows/verify.yml';
    writeFileSync(join(f.repo, 'LICENSE'), 'fixture-license\n');
    mkdirSync(join(f.repo, '.github/workflows'), { recursive: true });
    writeFileSync(join(f.repo, workflow), 'fixture-workflow\n');
    f.git('add', '.');
    f.git('commit', '-m', 'bundled alias fixtures');
    const base = {
      ...f.base,
      sha: f.git('rev-parse', 'HEAD').toString().trim(),
      paths: ['LICENSE', 'file.txt'],
      outputs: { LICENSE: 'docs/LICENSE.txt' },
      replacements: [
        { path: 'LICENSE', from: 'fixture-license', to: 'public' },
      ],
    };
    const output = join(f.root, 'bundled-alias');
    const result = f.run('intake', output, { intake: base });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(
      readFileSync(join(output, 'docs/LICENSE.txt'), 'utf8'),
      'public\n',
    );
    assert.deepEqual(
      readFileSync(join(output, 'LICENSE')),
      readFileSync(license),
    );
    const ciOutput = join(f.root, 'bundled-ci-alias');
    const ci = {
      ...base,
      paths: [workflow, 'file.txt'],
      outputs: { [workflow]: 'docs/verify.yml' },
      replacements: [
        { path: workflow, from: 'fixture-workflow', to: 'public' },
      ],
    };
    const ciResult = f.run('intake', ciOutput, { intake: ci });
    assert.equal(ciResult.status, 0, ciResult.stderr);
    assert.equal(
      readFileSync(join(ciOutput, 'docs/verify.yml'), 'utf8'),
      'public\n',
    );
    assert.deepEqual(
      readFileSync(join(ciOutput, workflow)),
      readFileSync(resolve('deploy/public/intake-verify.yml')),
    );
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

test('登録済みファイルの公開名と候補木を検証する', () => {
  const f = registryFixture();
  try {
    writeFileSync(
      join(f.repo, 'file.txt'),
      '#!/bin/sh\necho fixture-private\n',
    );
    chmodSync(join(f.repo, 'file.txt'), 0o755);
    symlinkSync('file.txt', join(f.repo, 'link.txt'));
    f.git('add', '.');
    f.git('commit', '-m', 'alias fixtures');
    const sha = f.git('rev-parse', 'HEAD').toString().trim();
    const base = {
      ...f.base,
      sha,
      outputs: { 'file.txt': 'public/run.sh' },
      replacements: [
        { path: 'file.txt', from: 'fixture-private', to: 'public' },
      ],
    };
    const output = join(f.root, 'alias');
    const result = f.run('intake', output, { intake: base });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(
      readFileSync(join(output, 'public/run.sh'), 'utf8'),
      '#!/bin/sh\necho public\n',
    );
    assert.equal(statSync(join(output, 'public/run.sh')).mode & 0o777, 0o755);
    assert.equal(existsSync(join(output, 'file.txt')), false);
    const destination = join(f.root, 'public-repo');
    mkdirSync(destination);
    const git = (...args: string[]) =>
      execFileSync('git', ['-C', destination, ...args], {
        encoding: 'utf8',
      }).trim();
    git('init', '-q');
    git('config', 'user.name', 'Test');
    git('config', 'user.email', 'test@example.invalid');
    for (const name of [
      'public/run.sh',
      'LICENSE',
      '.github/workflows/verify.yml',
    ]) {
      mkdirSync(join(destination, name, '..'), { recursive: true });
      copyFileSync(join(output, name), join(destination, name));
      chmodSync(
        join(destination, name),
        statSync(join(output, name)).mode & 0o777,
      );
    }
    git('add', '.');
    git('commit', '-qm', 'public baseline');
    const registry = join(f.root, 'publish-registry.json');
    writeFileSync(
      registry,
      JSON.stringify({
        intake: {
          source: base,
          ref: 'HEAD',
          publicRepo: destination,
          publicRef: 'HEAD',
        },
      }),
    );
    const candidate = join(f.root, 'candidate');
    const prepared = spawnSync(
      'sh',
      [
        resolve('scripts/publish-public-source.sh'),
        '--target',
        'intake',
        '--registry',
        registry,
        '--output',
        candidate,
      ],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          PATH: join(f.root, 'bin') + ':' + process.env.PATH,
        },
      },
    );
    assert.equal(prepared.status, 0, prepared.stderr);
    const record = JSON.parse(prepared.stdout);
    assert.equal(record.status, 'no-diff');
    assert.equal(record.generatedTree, record.publicTree);
    assert.equal(
      record.treeSha256,
      /^tree-sha256: ([a-f0-9]{64})$/m.exec(result.stdout)?.[1],
    );
    assert.equal(
      readFileSync(join(candidate, 'public/run.sh'), 'utf8'),
      readFileSync(join(output, 'public/run.sh'), 'utf8'),
    );
    assert.equal(
      statSync(join(candidate, 'public/run.sh')).mode & 0o777,
      0o755,
    );
    const invalid = [
      { outputs: { 'missing.txt': 'x' } },
      { outputs: { 'file.txt': '.env' } },
      { outputs: { 'file.txt': '../escape' } },
      { outputs: { 'file.txt': 'LICENSE' } },
      { outputs: { 'file.txt': '.github/workflows' } },
      {
        paths: ['file.txt', 'fixture-private.txt'],
        outputs: { 'file.txt': 'same', 'fixture-private.txt': 'same' },
      },
      {
        paths: ['file.txt', 'fixture-private.txt'],
        outputs: { 'file.txt': 'same', 'fixture-private.txt': 'same/child' },
      },
      {
        paths: ['link.txt'],
        outputs: { 'link.txt': 'safe' },
        replacements: [],
      },
      { paths: ['.env'], outputs: { '.env': 'safe' }, replacements: [] },
      { replacements: [], reject: ['fixture-private'] },
    ];
    for (const [i, change] of invalid.entries()) {
      const denied = join(f.root, `alias-denied-${i}`);
      const failed = f.run('intake', denied, {
        intake: { ...base, ...change },
      });
      assert.notEqual(failed.status, 0, String(i));
      assert.equal(existsSync(denied), false);
      assert.equal(
        (failed.stdout + failed.stderr).includes('fixture-private'),
        false,
      );
    }
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});
