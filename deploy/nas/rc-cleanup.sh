# update.sh から読み込む。失敗は更新を止めず、判定できないものは保持する。
rc_cleanup() {
  rc_output=$(RC_CLEANUP_PRIMARY=${RC_PRIMARY:-} RC_CLEANUP_KEEP=${RC_KEEP:-} node - "${HOME:-}" "${DRY_RUN:-0}" "${RC_CLEANUP_DAYS:-3}" <<'NODE'
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const [home, dry, daysText] = process.argv.slice(2);
const days = Number(daysText);
const roots = [
  { root: path.join(home, 'rc'), keep: new Set([process.env.RC_CLEANUP_PRIMARY, ...(process.env.RC_CLEANUP_KEEP || '').split(/\s+/), 'polling-kit', 'demo-image-culling', 'sample-scraper']) },
  { root: path.join(home, 'Workspace'), keep: new Set(['issue-desk-intake', 'image-review-client', ...(process.env.RC_CLEANUP_WORKSPACE_KEEP ?? 'issue-desk-intake issue-desk-worker sample-scraper demo-image-culling polling-kit game-laboratory').split(/\s+/)]) },
];
let keep;
const say = (s) => process.stdout.write(`[update:rc-cleanup] ${s}\n`);
const git = (dir, ...args) => {
  const r = spawnSync('git', ['-C', dir, ...args], { encoding: 'utf8', timeout: 30000, maxBuffer: 16 * 1024 * 1024, env: { ...process.env, GIT_TERMINAL_PROMPT: '0' } });
  if (r.status !== 0 || r.error) throw new Error('git ' + args[0]);
  return r.stdout;
};
const inside = (base, p) => p === base || p.startsWith(base + path.sep);
function active(dir) {
  let unavailable = false;
  for (const pid of fs.readdirSync('/proc')) {
    if (!/^\d+$/.test(pid)) continue;
    try {
      if (inside(dir, fs.readlinkSync(`/proc/${pid}/cwd`))) return true;
    } catch (e) {
      if (e.code !== 'ENOENT' && e.code !== 'ESRCH') unavailable = true;
    }
  }
  if (unavailable) throw new Error('cwd unavailable');
  return false;
}
function recent(dir, cutoff) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === '.git') continue;
    const p = path.join(dir, entry.name);
    const st = fs.lstatSync(p);
    if (st.mtimeMs >= cutoff) return true;
    if (st.isDirectory() && recent(p, cutoff)) return true;
  }
  return false;
}
function check(dir) {
  const name = path.basename(dir);
  if (keep.has(name) || fs.lstatSync(dir).isSymbolicLink()) return 'protected';
  if (active(dir)) return 'in use';
  if (recent(dir, Date.now() - days * 86400000)) return 'recent';
  let top;
  try { top = git(dir, 'rev-parse', '--show-toplevel').trim(); } catch { return 'not git'; }
  if (top !== dir) return 'not git root';
  if (git(dir, 'status', '--porcelain', '--untracked-files=no').trim()) return 'uncommitted changes';
  if (git(dir, 'ls-files', '--others', '-z').split('\0').some(p => p && !p.startsWith('.tmp/'))) return 'untracked files';
  if (git(dir, 'for-each-ref', '--format=%(refname)', 'refs/stash').split('\n').includes('refs/stash')) return 'stash';
  git(dir, 'fetch', ...(dry === '1' ? ['--dry-run'] : []), '--all');
  if (git(dir, 'rev-list', 'HEAD', '--branches', '--not', '--remotes').trim()) return 'local commits';
  return '';
}
function nestedCheck(dir) {
  if (keep.has(path.basename(dir)) || fs.lstatSync(dir).isSymbolicLink()) return 'protected';
  if (active(dir)) return 'in use';
  if (fs.lstatSync(dir).mtimeMs >= Date.now() - days * 86400000) return 'recent';
  const children = fs.readdirSync(dir, { withFileTypes: true }).filter(e => e.isDirectory() || e.isSymbolicLink());
  if (!children.length) return 'not git';
  for (const entry of children) {
    const child = path.join(dir, entry.name);
    if (entry.isSymbolicLink() || !fs.existsSync(path.join(child, '.git')) || !fs.lstatSync(path.join(child, '.git')).isDirectory()) return 'not git';
    const reason = check(child);
    if (reason) return reason;
    const records = git(child, 'worktree', 'list', '--porcelain', '-z').split('\0\0').filter(Boolean);
    if (records.length !== 1 || !records[0].startsWith(`worktree ${child}\0`) || /(?:^|\0)(?:locked|prunable)(?: |\0)/.test(records[0])) return 'worktree group';
  }
  return '';
}
if (path.isAbsolute(home) && Number.isFinite(days) && days >= 0) for (const config of roots) {
 const { root } = config;
 keep = config.keep;
 try {
  if (!fs.existsSync(root) || fs.lstatSync(root).isSymbolicLink()) continue;
  const dirs = fs.readdirSync(root, { withFileTypes: true }).filter(e => e.isDirectory() && !e.isSymbolicLink()).map(e => path.join(root, e.name));
  const done = new Set();
  for (const dir of dirs) {
    if (done.has(dir)) continue;
    try {
      // worktree list の先頭は主作業ツリー。子から始まってもグループ全体を検査する。
      let list;
      try { git(dir, 'rev-parse', '--show-toplevel'); } catch {
        if (root === roots[1].root) {
          try {
            const reason = nestedCheck(dir);
            if (reason) { say(`${dir}: 保持 (${reason})`); continue; }
            if (dry === '1') { say(`${dir}: 削除予定`); continue; }
            const again = nestedCheck(dir);
            if (again) { say(`${dir}: 保持 (${again})`); continue; }
            fs.rmSync(dir, { recursive: true });
            say(`${dir}: 削除しました`);
          } catch { say(`${dir}: 判定できず保持`); }
        }
        continue;
      }
      const records = git(dir, 'worktree', 'list', '--porcelain', '-z').split('\0\0');
      const blockedTrees = new Set();
      list = records.filter(Boolean).map(record => {
        const fields = record.split('\0');
        if (!fields[0].startsWith('worktree ')) throw new Error('invalid worktree list');
        const worktree = fields[0].slice(9);
        if (fields.some(field => field === 'locked' || field.startsWith('locked ') || field === 'prunable' || field.startsWith('prunable '))) blockedTrees.add(worktree);
        return worktree;
      });
      if (!list.length) throw new Error('worktree list');
      if (!list.includes(dir)) throw new Error('worktree mismatch');
      const parent = list[0];
      if (list.some(p => !inside(root, p))) continue;
      const parentCandidate = dirs.includes(parent) && !keep.has(path.basename(parent));
      const group = parentCandidate ? list : [dir];
      if (group.some(p => done.has(p))) continue;
      for (const p of group) done.add(p);
      let blocked = false;
      for (const p of group) {
        if (!dirs.includes(p) || blockedTrees.has(p)) { blocked = true; continue; }
        let reason;
        try { reason = check(p); } catch { reason = 'inspection failed'; }
        if (reason) {
          blocked = true;
          if (!['protected', 'in use', 'recent', 'not git', 'not git root'].includes(reason)) say(`${p}: 保持 (${reason})`);
        }
      }
      if (blocked) continue;
      for (const p of [...group.slice(1), group[0]]) {
        if (dry === '1') { say(`${p}: 削除予定`); continue; }
        try {
          // 削除直前に使用中や変更を再確認する。
          const reason = check(p);
          if (reason) { say(`${p}: 保持 (${reason})`); break; }
          if (p !== parent) git(parent, 'worktree', 'remove', '--force', p);
          else fs.rmSync(p, { recursive: true });
          say(`${p}: 削除しました`);
        } catch { say(`${p}: 削除できず保持`); break; }
      }
    } catch { say(`${dir}: 判定できず保持`); }
  }
 } catch { say(`${root}: 判定できず保持`); }
}
NODE
  ) || { say '[update:rc-cleanup] 判定できず保持'; return 0; }
  if [ -n "$rc_output" ]; then
    while IFS= read -r rc_line || [ -n "$rc_line" ]; do say "$rc_line"; done <<EOF
$rc_output
EOF
  fi
  return 0
}
