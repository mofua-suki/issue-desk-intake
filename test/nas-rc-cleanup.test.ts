import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { encoding: 'utf8', ...opts });
  assert.equal(r.status, 0, `${cmd}: ${r.stderr}`);
  return r.stdout;
}
function fixture(t, root = 'rc') {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), 'rc-cleanup-'));
  t.after(() => fs.rmSync(base, { recursive: true, force: true }));
  // procfs のアクセス制限を持つテスト環境では、他ユーザーの cwd を
  // 候補外として再現する。通常の読み取りと使用中判定は実際の /proc を通す。
  const preload = path.join(base, 'proc.cjs');
  fs.writeFileSync(
    preload,
    `const fs = require('node:fs');
const readlink = fs.readlinkSync;
const readdir = fs.readdirSync;
fs.readdirSync = function(p, ...args) {
  const entries = readdir.call(this, p, ...args);
  if (p === '/proc' && process.env.TEST_ACTIVE_DIR) entries.push('99999999');
  if (p === '/proc' && process.env.TEST_DENIED) entries.push('99999998');
  return entries;
};
fs.readlinkSync = function(p, ...args) {
  if (p === '/proc/99999999/cwd') return process.env.TEST_ACTIVE_DIR;
  if (p === '/proc/99999998/cwd') { const error = new Error('denied'); error.code = 'EACCES'; throw error; }
  try { return readlink.call(this, p, ...args); }
  catch (e) { if (/^\\/proc\\/\\d+\\/cwd$/.test(String(p)) && (e.code === 'EACCES' || e.code === 'EPERM')) return '/'; throw e; }
};
`,
  );
  const home = path.join(base, 'home');
  const rc = path.join(home, root);
  fs.mkdirSync(rc, { recursive: true });
  const origin = path.join(base, 'origin.git');
  run('git', ['init', '--bare', origin]);
  const seed = path.join(base, 'seed');
  run('git', ['clone', origin, seed]);
  run('git', ['-C', seed, 'config', 'user.email', 'test@example.invalid']);
  run('git', ['-C', seed, 'config', 'user.name', 'Test']);
  fs.writeFileSync(path.join(seed, 'file'), 'initial');
  run('git', ['-C', seed, 'add', 'file']);
  run('git', ['-C', seed, 'commit', '-m', 'initial']);
  run('git', ['-C', seed, 'push', 'origin', 'HEAD']);
  const clone = (name) => {
    const dir = path.join(rc, name);
    run('git', ['clone', origin, dir]);
    run('git', ['-C', dir, 'config', 'user.email', 'test@example.invalid']);
    run('git', ['-C', dir, 'config', 'user.name', 'Test']);
    return dir;
  };
  const old = (dir) => {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      if (e.name === '.git') continue;
      const p = path.join(dir, e.name);
      if (e.isDirectory()) old(p);
      fs.utimesSync(p, 1, 1);
    }
  };
  const cleanup = (extra = {}) => {
    const script =
      '. "$SCRIPT"; say() { printf "%s\\n" "$*"; }; rc_cleanup; printf "return=%s\\n" "$?"';
    const r = spawnSync('sh', ['-c', script], {
      encoding: 'utf8',
      env: {
        ...process.env,
        HOME: home,
        SCRIPT: path.resolve('deploy/nas/rc-cleanup.sh'),
        RC_PRIMARY: 'primary',
        RC_KEEP: 'kept',
        NODE_OPTIONS: `--require=${preload}`,
        ...extra,
      },
    });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout.trim().split('\n');
  };
  return { base, rc, origin, clone, old, cleanup };
}

test('rc-cleanup: keep names links active and new directories', (t) => {
  const f = fixture(t);
  const retained = [
    'primary',
    'kept',
    'polling-kit',
    'demo-image-culling',
    'sample-scraper',
    'active',
    'new',
  ];
  for (const n of retained) {
    const dir = f.clone(n);
    if (n !== 'new') f.old(dir);
  }
  fs.symlinkSync(path.join(f.rc, 'new'), path.join(f.rc, 'link'));
  assert.deepEqual(f.cleanup({ TEST_ACTIVE_DIR: path.join(f.rc, 'active') }), [
    'return=0',
  ]);
  for (const n of [...retained, 'link'])
    assert.ok(fs.existsSync(path.join(f.rc, n)), n);
});

test('rc-cleanup: unsafe changes local branches and fetch failure', (t) => {
  const f = fixture(t);
  const dirty = f.clone('dirty');
  const local = f.clone('local');
  const failed = f.clone('failed');
  const untracked = f.clone('untracked');
  const stashed = f.clone('stashed');
  const ignored = f.clone('ignored');
  fs.writeFileSync(path.join(untracked, 'new source'), 'work');
  fs.writeFileSync(path.join(ignored, '.git', 'info', 'exclude'), 'private\n');
  fs.writeFileSync(path.join(ignored, 'private'), 'data');
  fs.writeFileSync(path.join(stashed, 'file'), 'saved work');
  run('git', ['-C', stashed, 'stash', 'push']);
  fs.writeFileSync(path.join(dirty, 'file'), 'changed');
  run('git', ['-C', local, 'checkout', '-b', 'local-only']);
  fs.writeFileSync(path.join(local, 'file'), 'commit');
  run('git', ['-C', local, 'commit', '-am', 'local']);
  run('git', ['-C', local, 'checkout', '-']);
  run('git', [
    '-C',
    failed,
    'remote',
    'set-url',
    'origin',
    path.join(f.base, 'missing'),
  ]);
  for (const p of [dirty, local, failed, untracked, stashed, ignored]) f.old(p);
  const lines = f.cleanup();
  assert.equal(lines.length, 7);
  for (const p of [dirty, local, failed, untracked, stashed, ignored]) {
    assert.ok(fs.existsSync(p));
    assert.equal(lines.filter((l) => l.includes(p)).length, 1);
  }
  assert.match(
    lines.find((l) => l.includes(dirty)),
    /uncommitted changes/,
  );
  assert.match(
    lines.find((l) => l.includes(local)),
    /local commits/,
  );
  assert.match(
    lines.find((l) => l.includes(failed)),
    /inspection failed/,
  );
  assert.match(
    lines.find((l) => l.includes(untracked)),
    /untracked files/,
  );
  assert.match(
    lines.find((l) => l.includes(stashed)),
    /stash/,
  );
  assert.match(
    lines.find((l) => l.includes(ignored)),
    /untracked files/,
  );
  assert.equal(
    fs.readFileSync(path.join(untracked, 'new source'), 'utf8'),
    'work',
  );
  assert.match(
    run('git', ['-C', stashed, 'stash', 'show', '-p']),
    /saved work/,
  );
  assert.equal(lines.at(-1), 'return=0');
});

test('rc-cleanup: delete clean clone with untracked temporary files', (t) => {
  const f = fixture(t);
  const dir = f.clone('clean');
  fs.mkdirSync(path.join(dir, '.tmp'));
  fs.writeFileSync(path.join(dir, '.tmp', 'x'), 'x');
  f.old(dir);
  const lines = f.cleanup();
  assert.equal(fs.existsSync(dir), false);
  assert.equal(lines.length, 2);
  assert.match(lines[0], /削除しました/);
});

test('rc-cleanup: worktree group and blocked child', (t) => {
  const f = fixture(t);
  const parent = f.clone('parent');
  const child = path.join(f.rc, 'child');
  run('git', ['-C', parent, 'worktree', 'add', '--detach', child]);
  f.old(parent);
  f.old(child);
  fs.writeFileSync(path.join(child, 'file'), 'changed');
  f.old(child);
  f.cleanup();
  assert.ok(fs.existsSync(parent));
  assert.ok(fs.existsSync(child));
  run('git', ['-C', child, 'checkout', '--', 'file']);
  f.old(child);
  const lines = f.cleanup();
  assert.equal(fs.existsSync(parent), false);
  assert.equal(fs.existsSync(child), false);
  assert.equal(lines.length, 3);
});

test('rc-cleanup: worktree child removed from retained parent', (t) => {
  const f = fixture(t);
  const parent = f.clone('primary');
  const child = path.join(f.rc, 'child');
  run('git', ['-C', parent, 'worktree', 'add', '--detach', child]);
  f.old(child);
  f.cleanup();
  assert.ok(fs.existsSync(parent));
  assert.equal(fs.existsSync(child), false);
  assert.doesNotMatch(
    run('git', ['-C', parent, 'worktree', 'list', '--porcelain']),
    /worktree .*\/child/,
  );
});

test('rc-cleanup: cross-root worktree group remains', (t) => {
  const f = fixture(t, 'Workspace');
  const parent = f.clone('parent');
  const child = path.join(f.base, 'home', 'rc', 'child');
  fs.mkdirSync(path.dirname(child), { recursive: true });
  run('git', ['-C', parent, 'worktree', 'add', '--detach', child]);
  f.old(parent);
  f.old(child);
  f.cleanup();
  assert.ok(fs.existsSync(parent));
  assert.ok(fs.existsSync(child));
  assert.match(
    run('git', ['-C', parent, 'worktree', 'list', '--porcelain']),
    /worktree .*\/rc\/child/,
  );
});

test('rc-cleanup: non-git remains', (t) => {
  const f = fixture(t);
  const dir = path.join(f.rc, 'plain');
  fs.mkdirSync(dir);
  f.old(dir);
  assert.deepEqual(f.cleanup(), ['return=0']);
  assert.ok(fs.existsSync(dir));
});

test('rc-cleanup: dry-run retains clone and prints plan', (t) => {
  const f = fixture(t);
  const dir = f.clone('clean');
  f.old(dir);
  const lines = f.cleanup({ DRY_RUN: '1' });
  assert.ok(fs.existsSync(dir));
  assert.equal(lines.length, 2);
  assert.match(lines[0], /削除予定/);
});

test('rc-cleanup: failure retains candidate and continues', (t) => {
  const f = fixture(t);
  const bad = f.clone('bad');
  const good = f.clone('good');
  f.old(bad);
  f.old(good);
  const bin = path.join(f.base, 'bin');
  fs.mkdirSync(bin);
  fs.writeFileSync(
    path.join(bin, 'git'),
    `#!/bin/sh\ncase "$2:$3" in */bad:status) exit 1;; esac\nexec /usr/bin/git "$@"\n`,
    { mode: 0o755 },
  );
  const lines = f.cleanup({ PATH: `${bin}:${process.env.PATH}` });
  assert.ok(fs.existsSync(bad));
  assert.equal(fs.existsSync(good), false);
  assert.equal(lines.filter((l) => l.includes(bad)).length, 1);
  assert.equal(lines.at(-1), 'return=0');
});

test('rc-cleanup: failure inaccessible cwd retains candidate', (t) => {
  const f = fixture(t);
  const dir = f.clone('candidate');
  f.old(dir);
  const lines = f.cleanup({ TEST_DENIED: '1' });
  assert.ok(fs.existsSync(dir));
  assert.equal(lines.length, 2);
  assert.match(lines[0], /inspection failed/);
  assert.equal(lines[1], 'return=0');
});

test('rc-cleanup: failure worktree removal retains child and parent', (t) => {
  const f = fixture(t);
  const parent = f.clone('parent');
  const child = path.join(f.rc, 'child');
  run('git', ['-C', parent, 'worktree', 'add', '--detach', child]);
  f.old(parent);
  f.old(child);
  const bin = path.join(f.base, 'bin');
  fs.mkdirSync(bin);
  fs.writeFileSync(
    path.join(bin, 'git'),
    `#!/bin/sh\nif [ "$3" = worktree ] && [ "$4" = remove ]; then exit 1; fi\nexec /usr/bin/git "$@"\n`,
    { mode: 0o755 },
  );
  const lines = f.cleanup({ PATH: `${bin}:${process.env.PATH}` });
  assert.ok(fs.existsSync(parent));
  assert.ok(fs.existsSync(child));
  assert.equal(lines.filter((l) => l.includes(child)).length, 1);
  assert.match(lines[0], /削除できず保持/);
  assert.equal(lines.at(-1), 'return=0');
});

test('rc-cleanup: worktree locked child keeps the entire group', (t) => {
  const f = fixture(t);
  const parent = f.clone('parent');
  const first = path.join(f.rc, 'first');
  const locked = path.join(f.rc, 'locked');
  run('git', ['-C', parent, 'worktree', 'add', '--detach', first]);
  run('git', ['-C', parent, 'worktree', 'add', '--detach', locked]);
  run('git', ['-C', parent, 'worktree', 'lock', locked]);
  for (const p of [parent, first, locked]) f.old(p);
  assert.deepEqual(f.cleanup(), ['return=0']);
  for (const p of [parent, first, locked]) assert.ok(fs.existsSync(p));
});

test('rc-cleanup: workspace keep', (t) => {
  const f = fixture(t, 'Workspace');
  const names = [
    'issue-desk-intake',
    'issue-desk-worker',
    'sample-scraper',
    'demo-image-culling',
    'polling-kit',
    'game-laboratory',
  ];
  for (const name of [...names, 'primary', 'kept', 'active', 'new'])
    f.clone(name);
  for (const name of [...names, 'primary', 'kept', 'active'])
    f.old(path.join(f.rc, name));
  fs.symlinkSync(path.join(f.rc, 'new'), path.join(f.rc, 'link'));
  f.cleanup({ TEST_ACTIVE_DIR: path.join(f.rc, 'active') });
  for (const name of [...names, 'active', 'new', 'link'])
    assert.ok(fs.existsSync(path.join(f.rc, name)), name);
  for (const name of ['primary', 'kept'])
    assert.equal(fs.existsSync(path.join(f.rc, name)), false);
});

test('rc-cleanup: workspace always keeps issue-desk-intake with empty configuration', (t) => {
  const f = fixture(t, 'Workspace');
  const protectedDir = f.clone('issue-desk-intake');
  const other = f.clone('other');
  f.old(protectedDir);
  f.old(other);
  f.cleanup({ RC_CLEANUP_WORKSPACE_KEEP: '' });
  assert.ok(fs.existsSync(protectedDir));
  assert.equal(fs.existsSync(other), false);
});

test('rc-cleanup: workspace delete', (t) => {
  const f = fixture(t, 'Workspace');
  const clean = f.clone('clean');
  const dirty = f.clone('dirty');
  fs.writeFileSync(path.join(dirty, 'file'), 'changed');
  f.old(clean);
  f.old(dirty);
  const lines = f.cleanup();
  assert.equal(fs.existsSync(clean), false);
  assert.ok(fs.existsSync(dirty));
  assert.equal(
    lines.filter((l) => l.includes(clean) && l.includes('削除しました')).length,
    1,
  );
  assert.equal(
    lines.filter((l) => l.includes(dirty) && l.includes('uncommitted changes'))
      .length,
    1,
  );
  assert.equal(lines.length, 3);
});

test('rc-cleanup: workspace nested', (t) => {
  const f = fixture(t, 'Workspace');
  for (const name of ['clean/a', 'clean/b', 'dirty/a', 'dirty/b', 'mixed/a'])
    f.clone(name);
  fs.writeFileSync(path.join(f.rc, 'dirty/b/file'), 'changed');
  fs.mkdirSync(path.join(f.rc, 'mixed/plain'));
  fs.mkdirSync(path.join(f.rc, 'empty'));
  fs.writeFileSync(path.join(f.rc, 'clean', 'log'), 'log');
  for (const name of ['clean', 'dirty', 'mixed', 'empty']) {
    const dir = path.join(f.rc, name);
    f.old(dir);
    fs.utimesSync(dir, 1, 1);
  }
  const lines = f.cleanup();
  assert.equal(fs.existsSync(path.join(f.rc, 'clean')), false);
  for (const name of ['dirty', 'mixed', 'empty'])
    assert.ok(fs.existsSync(path.join(f.rc, name)));
  assert.equal(
    lines.filter(
      (l) => l.includes(path.join(f.rc, 'clean')) && l.includes('削除しました'),
    ).length,
    1,
  );
  assert.equal(
    lines.filter((l) =>
      l.includes(path.join(f.rc, 'dirty') + ': 保持 (uncommitted changes)'),
    ).length,
    1,
  );
  assert.equal(
    lines.filter((l) =>
      l.includes(path.join(f.rc, 'mixed') + ': 保持 (not git)'),
    ).length,
    1,
  );
});

test('rc-cleanup: workspace dry-run', (t) => {
  const f = fixture(t, 'Workspace');
  const clean = f.clone('clean');
  f.clone('nested/child');
  for (const name of ['clean', 'nested']) {
    const dir = path.join(f.rc, name);
    f.old(dir);
    fs.utimesSync(dir, 1, 1);
  }
  const lines = f.cleanup({ DRY_RUN: '1' });
  assert.ok(fs.existsSync(clean));
  assert.ok(fs.existsSync(path.join(f.rc, 'nested/child')));
  assert.equal(lines.length, 3);
  assert.ok(lines.slice(0, -1).every((l) => l.includes('削除予定')));
});
