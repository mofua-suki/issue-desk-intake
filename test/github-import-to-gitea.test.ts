import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync, mkdirSync } from 'node:fs';
import { join, resolve } from 'node:path';

const script = resolve('scripts/github-import-to-gitea.ts');
function fixture() {
  const dir = mkdtempSync(join(process.cwd(), '.import-test-'));
  const source = join(dir, 'source');
  const dest = join(dir, 'dest');
  const git = (cwd: string, ...args: string[]) =>
    execFileSync('git', args, {
      cwd,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    }).trim();
  for (const path of [source, dest]) {
    mkdirSync(path);
    git(path, 'init', '-q', '-b', 'work');
    git(path, 'config', 'user.email', 'test@example.invalid');
    git(path, 'config', 'user.name', 'Test');
  }
  const commit = (repo: string, content: string) => {
    writeFileSync(join(repo, 'file'), content);
    git(repo, 'add', 'file');
    git(repo, 'commit', '-qm', content);
    return git(repo, 'rev-parse', 'HEAD');
  };
  const first = commit(source, 'first');
  git(source, 'branch', 'main');
  const call = (expected: string, src = source) =>
    spawnSync(
      process.execPath,
      [
        script,
        '--source',
        src,
        '--destination',
        dest,
        '--branch',
        'main',
        '--expected-destination',
        expected,
      ],
      { encoding: 'utf8' },
    );
  const ref = (name: string) => git(dest, 'rev-parse', name);
  return { dir, source, dest, git, commit, first, call, ref };
}

test('初回、再実行、早送りは対象枝だけを更新する', () => {
  const f = fixture();
  try {
    const other = f.commit(f.dest, 'other');
    f.git(f.dest, 'tag', 'keep');
    let result = f.call('empty');
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, new RegExp(`結果SHA: ${f.first}`));
    assert.equal(f.ref('main'), f.first);
    result = f.call(f.first);
    assert.equal(result.status, 0, result.stderr);
    const next = f.commit(f.source, 'second');
    f.git(f.source, 'branch', '-f', 'main', next);
    result = f.call(f.first);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(f.ref('main'), next);
    assert.equal(f.ref('work'), other);
    assert.equal(f.ref('keep'), other);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('宛先だけのコミット、分岐、対象不一致と取得失敗を拒否し保全する', () => {
  const f = fixture();
  try {
    assert.equal(f.call('empty').status, 0);
    f.git(f.dest, 'checkout', '-q', 'main');
    const ahead = f.commit(f.dest, 'destination');
    f.git(f.dest, 'checkout', '-q', '--orphan', 'work2');
    const unrelated = f.commit(f.dest, 'unrelated');
    f.git(f.dest, 'tag', 'keep', unrelated);
    let result = f.call(ahead);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /宛先にのみコミット|分岐/);
    const next = f.commit(f.source, 'source');
    f.git(f.source, 'branch', '-f', 'main', next);
    result = f.call(ahead);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /分岐/);
    result = f.call(f.first);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /一致しません/);
    result = f.call(ahead, join(f.dir, 'missing'));
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /確認できません/);
    assert.equal(f.ref('main'), ahead);
    assert.equal(f.ref('work2'), unrelated);
    assert.equal(f.ref('keep'), unrelated);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('並行更新を通常のpushが拒否し、更新後の不一致も報告する', () => {
  const f = fixture();
  try {
    assert.equal(f.call('empty').status, 0);
    const next = f.commit(f.source, 'next');
    f.git(f.source, 'branch', '-f', 'main', next);
    f.git(f.dest, 'checkout', '-q', '-b', 'work', 'main');
    const concurrent = f.commit(f.dest, 'concurrent');
    const hook = join(f.dest, '.git', 'hooks', 'pre-receive');
    writeFileSync(
      hook,
      `#!/bin/sh\nunset GIT_QUARANTINE_PATH GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES
git update-ref refs/heads/main ${concurrent}\n`,
      { mode: 0o755 },
    );
    let result = f.call(f.first);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /pushが拒否/);
    assert.equal(f.ref('main'), concurrent);
    f.git(f.dest, 'update-ref', 'refs/heads/main', f.first);
    rmSync(hook);
    writeFileSync(
      join(f.dest, '.git', 'hooks', 'post-receive'),
      `#!/bin/sh\nunset GIT_QUARANTINE_PATH GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES
git update-ref refs/heads/main ${concurrent}\n`,
      { mode: 0o755 },
    );
    result = f.call(f.first);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /更新後の宛先SHAが一致しません/);
    assert.equal(f.ref('main'), concurrent);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});
