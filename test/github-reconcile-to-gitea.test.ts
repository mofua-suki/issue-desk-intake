import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import {
  mkdtempSync,
  mkdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs';
import { join, resolve } from 'node:path';

const script = resolve('scripts/github-reconcile-to-gitea.ts');
function fixture() {
  const dir = mkdtempSync(join(process.cwd(), '.reconcile github test-'));
  const source = join(dir, 'source'),
    dest = join(dir, 'dest'),
    bin = join(dir, 'bin'),
    state = join(dir, 'pr.json'),
    log = join(dir, 'writes'),
    hook = join(dir, 'hook');
  mkdirSync(source);
  mkdirSync(dest);
  mkdirSync(bin);
  writeFileSync(log, '');
  const git = (cwd: string, ...args: string[]) =>
    execFileSync('git', args, {
      cwd,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    }).trim();
  for (const repo of [source, dest]) {
    git(repo, 'init', '-q', '-b', 'main');
    git(repo, 'config', 'user.name', 'Test');
    git(repo, 'config', 'user.email', 'test@invalid');
  }
  const commit = (repo: string, file: string, data: string) => {
    writeFileSync(join(repo, file), data);
    git(repo, 'add', file);
    git(repo, 'commit', '-qm', 'change');
    return git(repo, 'rev-parse', 'HEAD');
  };
  const base = commit(source, 'line', 'before\n');
  git(dest, 'fetch', '-q', source, 'main');
  git(dest, 'reset', '--hard', '-q', 'FETCH_HEAD');
  const published = commit(source, 'line', 'published\n');
  const head = commit(source, 'line', 'later\n');
  writeFileSync(join(dest, 'line'), 'published\n');
  git(dest, 'add', 'line');
  const tree = git(dest, 'write-tree');
  const proof = createHash('sha256')
    .update(
      JSON.stringify({
        repository: 'owner/repo',
        baseBranch: 'main',
        baseHead: base,
        source,
        sourceBranch: 'main',
        sha: published,
        tree,
        paths: ['line'],
      }),
    )
    .digest('hex');
  const branch = `intake-publish/${proof}`;
  git(dest, 'commit', '-qm', 'published');
  const prHead = git(dest, 'rev-parse', 'HEAD');
  git(dest, 'update-ref', 'refs/pull/1/head', prHead);
  git(source, 'config', 'receive.denyCurrentBranch', 'updateInstead');
  git(dest, 'reset', '--hard', '-q', base);
  writeFileSync(join(dest, 'line'), 'published\n');
  git(dest, 'add', 'line');
  git(dest, 'commit', '-qm', 'squash merge');
  const merged = git(dest, 'rev-parse', 'HEAD');
  assert.notEqual(merged, prHead);
  const githubHead = commit(dest, 'other', 'other\n');
  const pr = {
    number: 1,
    state: 'closed',
    merged_at: '2026-01-01',
    merge_commit_sha: merged,
    base: { ref: 'main', sha: base, repo: { full_name: 'owner/repo' } },
    head: { ref: branch, sha: prHead, repo: { full_name: 'owner/repo' } },
    body: `Source SHA: ${published}\nCandidate tree: ${tree}`,
  };
  writeFileSync(state, JSON.stringify(pr));
  writeFileSync(
    join(bin, 'gh'),
    `#!${process.execPath}\nconst fs=require('node:fs'); const a=process.argv.slice(2); if(a[3]==='repos/owner/repo') console.log(JSON.stringify({full_name:'owner/repo',default_branch:'main',clone_url:'https://github.com/owner/repo.git'})); else if(a[3]==='repos/owner/repo/pulls/1') console.log(fs.readFileSync(${JSON.stringify(state)},'utf8')); else {fs.appendFileSync(${JSON.stringify(log)},'gh-write\\n'); process.exit(1)}\n`,
    { mode: 0o755 },
  );
  writeFileSync(
    join(bin, 'git'),
    `#!/bin/bash\nargs=("$@")\nfor i in "\${!args[@]}"; do if [ "\${args[$i]}" = 'https://github.com/owner/repo.git' ]; then args[$i]=${JSON.stringify(dest)}; fi; done\ncase " $* " in *' push '*) printf '%s\\0' "$@" >> ${JSON.stringify(log)}; printf '\\0' >> ${JSON.stringify(log)}; if [ -f ${JSON.stringify(hook)} ]; then /bin/bash ${JSON.stringify(hook)} "\${args[@]}" || exit 1; fi;; esac\nexec /usr/bin/git "\${args[@]}"\n`,
    { mode: 0o755 },
  );
  const call = (overrides: Record<string, string> = {}, apply = false) => {
    const opts = {
      '--source': source,
      '--branch': 'main',
      '--source-sha': published,
      '--destination-sha': head,
      '--repository': 'owner/repo',
      '--base-branch': 'main',
      '--base-sha': githubHead,
      '--pr': '1',
      '--candidate-tree': tree,
      ...overrides,
    };
    return spawnSync(
      process.execPath,
      [script, ...(apply ? ['--apply'] : []), ...Object.entries(opts).flat()],
      {
        encoding: 'utf8',
        env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
      },
    );
  };
  return {
    dir,
    source,
    dest,
    git,
    commit,
    base,
    head,
    published,
    githubHead,
    tree,
    pr,
    state,
    log,
    hook,
    call,
  };
}

test('PRと参照を照合し不一致を拒否する', () => {
  const f = fixture();
  try {
    const ok = f.call();
    assert.equal(ok.status, 0, ok.stderr);
    for (const change of [
      { merged_at: null },
      { body: 'invalid' },
      { merge_commit_sha: f.head },
      { head: { ...f.pr.head, sha: f.base } },
      { base: { ...f.pr.base, sha: f.head } },
    ]) {
      writeFileSync(f.state, JSON.stringify({ ...f.pr, ...change }));
      const r = f.call();
      assert.notEqual(r.status, 0);
      assert.ok(r.stderr);
    }
    writeFileSync(f.state, JSON.stringify(f.pr));
    for (const opts of [
      { '--destination-sha': f.base },
      { '--base-sha': f.base },
      { '--candidate-tree': '0'.repeat(40) },
      { '--source-sha': f.base },
    ]) {
      const r = f.call(opts);
      assert.notEqual(r.status, 0);
      assert.ok(r.stderr);
    }
    assert.equal(readFileSync(f.log, 'utf8'), '');
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('公開済みSHAを基点に後続変更と公開側の変更を保持し競合と取得不能を拒否する', () => {
  const f = fixture();
  try {
    const r = f.call();
    assert.equal(r.status, 0, r.stderr);
    const tree = r.stdout.match(/再取り込み候補木: ([a-f0-9]{40})/)?.[1];
    assert.ok(tree);
    writeFileSync(join(f.dest, 'line'), 'later\n');
    f.git(f.dest, 'add', 'line');
    assert.equal(tree, f.git(f.dest, 'write-tree'));
    const conflictHead = f.commit(f.dest, 'line', 'github conflict\n');
    const conflict = f.call({ '--base-sha': conflictHead });
    assert.notEqual(conflict.status, 0);
    assert.match(conflict.stderr, /競合/);
    const missing = f.call({ '--source': join(f.dir, 'missing') });
    assert.notEqual(missing.status, 0);
    assert.match(missing.stderr, /読み取れません/);
    assert.equal(readFileSync(f.log, 'utf8'), '');
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('再取り込みは両履歴を保持し再実行で変更しない', () => {
  const f = fixture();
  try {
    const r = f.call({}, true);
    assert.equal(r.status, 0, r.stderr);
    const result = f.git(f.source, 'rev-parse', 'main');
    f.git(f.source, 'merge-base', '--is-ancestor', f.head, result);
    f.git(f.source, 'merge-base', '--is-ancestor', f.githubHead, result);
    assert.equal(readFileSync(join(f.source, 'line'), 'utf8'), 'later\n');
    assert.equal(readFileSync(join(f.source, 'other'), 'utf8'), 'other\n');
    const again = f.call({ '--destination-sha': result }, true);
    assert.equal(again.status, 0, again.stderr);
    assert.equal(f.git(f.source, 'rev-parse', 'main'), result);
    const preview = spawnSync(
      process.execPath,
      [
        resolve('scripts/github-publish-from-gitea.ts'),
        '--source',
        f.source,
        '--branch',
        'main',
        '--source-sha',
        result,
        '--repository',
        'owner/repo',
        '--base-branch',
        'main',
        '--base-sha',
        f.githubHead,
      ],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          PATH: `${join(f.dir, 'bin')}:${process.env.PATH}`,
        },
      },
    );
    assert.equal(preview.status, 0, preview.stderr);
    assert.match(preview.stdout, /line/);
    assert.doesNotMatch(preview.stdout, /other/);
    assert.equal(
      readFileSync(f.log, 'utf8'),
      [
        'push',
        '--no-follow-tags',
        '--recurse-submodules=no',
        '--',
        f.source,
        `${result}:refs/heads/main`,
        '',
        '',
      ].join('\0'),
    );
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('並行更新と送信失敗後に参照を照合し再開する', () => {
  const f = fixture();
  try {
    f.git(f.source, 'branch', 'other', f.head);
    f.git(f.source, 'tag', 'keep', f.published);
    const concurrent = f.commit(f.source, 'parallel', 'parallel\n');
    f.git(f.source, 'reset', '--hard', '-q', f.head);
    writeFileSync(
      f.hook,
      `/usr/bin/git -C "${f.source}" reset --hard -q ${concurrent}\n`,
    );
    let r = f.call({}, true);
    assert.notEqual(r.status, 0);
    assert.match(r.stderr, /pushに失敗/);
    assert.equal(f.git(f.source, 'rev-parse', 'main'), concurrent);
    assert.equal(f.git(f.source, 'rev-parse', 'other'), f.head);
    assert.equal(f.git(f.source, 'rev-parse', 'keep'), f.published);
    assert.equal(f.git(f.dest, 'rev-parse', 'main'), f.githubHead);
    const writes = readFileSync(f.log, 'utf8').split('\0');
    assert.match(writes[5] ?? '', /^[a-f0-9]{40}:refs\/heads\/main$/);
    assert.deepEqual(writes, [
      'push',
      '--no-follow-tags',
      '--recurse-submodules=no',
      '--',
      f.source,
      writes[5],
      '',
      '',
    ]);
    f.git(f.source, 'reset', '--hard', '-q', f.head);
    writeFileSync(f.log, '');
    writeFileSync(f.hook, 'exit 1\n');
    r = f.call({}, true);
    assert.notEqual(r.status, 0);
    assert.match(r.stderr, /pushに失敗/);
    assert.equal(f.git(f.source, 'rev-parse', 'main'), f.head);
    assert.equal(f.git(f.dest, 'rev-parse', 'main'), f.githubHead);
    writeFileSync(f.hook, '/usr/bin/git "$@" || exit 1\nexit 1\n');
    r = f.call({}, true);
    assert.notEqual(r.status, 0);
    assert.match(r.stderr, /pushに失敗/);
    const applied = f.git(f.source, 'rev-parse', 'main');
    f.git(f.source, 'merge-base', '--is-ancestor', f.head, applied);
    assert.equal(f.git(f.dest, 'rev-parse', 'main'), f.githubHead);
    rmSync(f.hook);
    r = f.call({ '--destination-sha': applied }, true);
    assert.equal(r.status, 0, r.stderr);
    assert.equal(f.git(f.source, 'rev-parse', 'main'), applied);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('初回取込から公開とマージ後の再取り込みまで値を引き継ぐ', () => {
  const dir = mkdtempSync(join(process.cwd(), '.reconcile-flow-'));
  try {
    const github = join(dir, 'github'),
      gitea = join(dir, 'gitea'),
      work = join(dir, 'work'),
      bin = join(dir, 'bin'),
      state = join(dir, 'pr.json');
    mkdirSync(github);
    mkdirSync(gitea);
    mkdirSync(bin);
    const git = (cwd: string, ...args: string[]) =>
      execFileSync('git', args, {
        cwd,
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'pipe'],
      }).trim();
    git(github, 'init', '-q', '-b', 'main');
    git(github, 'config', 'user.name', 'Test');
    git(github, 'config', 'user.email', 'test@invalid');
    writeFileSync(join(github, 'line'), 'before\n');
    git(github, 'add', '.');
    git(github, 'commit', '-qm', 'base');
    const base = git(github, 'rev-parse', 'HEAD');
    git(gitea, 'init', '-q', '--bare');
    writeFileSync(
      join(bin, 'git'),
      `#!/bin/bash\na=("$@")\nfor i in "\${!a[@]}"; do if [ "\${a[$i]}" = 'https://github.com/owner/repo.git' ]; then a[$i]=${JSON.stringify(github)}; fi; done\nexec /usr/bin/git "\${a[@]}"\n`,
      { mode: 0o755 },
    );
    writeFileSync(
      join(bin, 'gh'),
      `#!${process.execPath}
const fs=require('node:fs'), cp=require('node:child_process');
const a=process.argv.slice(2), state=${JSON.stringify(state)}, repo=${JSON.stringify(github)};
const read=()=>fs.existsSync(state)?JSON.parse(fs.readFileSync(state,'utf8')):null;
if(a[0]==='api' && a[3]==='repos/owner/repo') console.log(JSON.stringify({full_name:'owner/repo',default_branch:'main',clone_url:'https://github.com/owner/repo.git'}));
else if(a[0]==='api' && a[3].startsWith('repos/owner/repo/pulls?')) console.log(JSON.stringify(read()?[read()]:[]));
else if(a[0]==='api' && a[3]==='repos/owner/repo/pulls/1') console.log(JSON.stringify(read()));
else if(a[0]==='pr' && a[1]==='create') { const head=a[a.indexOf('--head')+1], base=a[a.indexOf('--base')+1]; const rev=r=>cp.execFileSync('/usr/bin/git',['rev-parse',r],{cwd:repo,encoding:'utf8'}).trim(); fs.writeFileSync(state,JSON.stringify({number:1,state:'open',html_url:'https://github.com/owner/repo/pull/1',head:{ref:head,sha:rev('refs/heads/'+head),repo:{full_name:'owner/repo'}},base:{ref:base,sha:rev('refs/heads/'+base),repo:{full_name:'owner/repo'}},body:a[a.indexOf('--body')+1]})); }
else process.exit(1);
`,
      { mode: 0o755 },
    );
    const call = (file: string, args: string[]) =>
      spawnSync(process.execPath, [resolve(`scripts/${file}`), ...args], {
        encoding: 'utf8',
        env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
      });
    let r = call('github-import-to-gitea.ts', [
      '--source',
      github,
      '--destination',
      gitea,
      '--branch',
      'main',
      '--expected-destination',
      'empty',
    ]);
    assert.equal(r.status, 0, r.stderr);
    assert.equal(git(gitea, 'rev-parse', 'refs/heads/main'), base);
    git(dir, 'clone', '-q', '-b', 'main', gitea, work);
    git(work, 'config', 'user.name', 'Test');
    git(work, 'config', 'user.email', 'test@invalid');
    writeFileSync(join(work, 'line'), 'published\n');
    git(work, 'add', '.');
    git(work, 'commit', '-qm', 'first');
    writeFileSync(join(work, 'second'), 'second\n');
    git(work, 'add', '.');
    git(work, 'commit', '-qm', 'second');
    const published = git(work, 'rev-parse', 'HEAD');
    git(work, 'push', '-q', 'origin', 'main');
    const pubArgs = [
      '--source',
      gitea,
      '--branch',
      'main',
      '--source-sha',
      published,
      '--repository',
      'owner/repo',
      '--base-branch',
      'main',
      '--base-sha',
      base,
    ];
    r = call('github-publish-from-gitea.ts', pubArgs);
    assert.equal(r.status, 0, r.stderr);
    const proof = r.stdout.match(/確認値: ([a-f0-9]{64})/)?.[1],
      tree = r.stdout.match(/候補ツリー指紋: ([a-f0-9]{40})/)?.[1];
    assert.ok(proof && tree);
    r = call('github-publish-from-gitea.ts', [...pubArgs, '--confirm', proof]);
    assert.equal(r.status, 0, r.stderr);
    const pr = JSON.parse(readFileSync(state, 'utf8'));
    git(github, 'update-ref', 'refs/pull/1/head', pr.head.sha);
    git(github, 'merge', '--squash', pr.head.sha);
    git(github, 'commit', '-qm', 'squash');
    pr.merge_commit_sha = git(github, 'rev-parse', 'HEAD');
    pr.state = 'closed';
    pr.merged_at = '2026-01-01';
    writeFileSync(state, JSON.stringify(pr));
    git(github, 'branch', '-D', pr.head.ref);
    writeFileSync(join(github, 'public'), 'public\n');
    git(github, 'add', '.');
    git(github, 'commit', '-qm', 'public');
    const baseHead = git(github, 'rev-parse', 'HEAD');
    writeFileSync(join(work, 'line'), 'later\n');
    git(work, 'add', '.');
    git(work, 'commit', '-qm', 'later');
    const later = git(work, 'rev-parse', 'HEAD');
    git(work, 'push', '-q', 'origin', 'main');
    const recArgs = [
      '--source',
      gitea,
      '--branch',
      'main',
      '--source-sha',
      published,
      '--destination-sha',
      later,
      '--repository',
      'owner/repo',
      '--base-branch',
      'main',
      '--base-sha',
      baseHead,
      '--pr',
      '1',
      '--candidate-tree',
      tree,
    ];
    r = call('github-reconcile-to-gitea.ts', ['--apply', ...recArgs]);
    assert.equal(r.status, 0, r.stderr);
    const result = git(gitea, 'rev-parse', 'refs/heads/main');
    git(gitea, 'merge-base', '--is-ancestor', later, result);
    git(gitea, 'merge-base', '--is-ancestor', baseHead, result);
    const next = [
      '--source',
      gitea,
      '--branch',
      'main',
      '--source-sha',
      result,
      '--repository',
      'owner/repo',
      '--base-branch',
      'main',
      '--base-sha',
      baseHead,
    ];
    r = call('github-publish-from-gitea.ts', next);
    assert.equal(r.status, 0, r.stderr);
    assert.match(r.stdout, /変更パス: \["line"\]/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
