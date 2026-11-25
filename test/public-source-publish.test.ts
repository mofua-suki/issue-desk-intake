import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  rmSync,
  existsSync,
  chmodSync,
  symlinkSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { execFileSync, spawnSync, spawn } from 'node:child_process';

test('登録・再評価・候補準備', () => {
  const root = mkdtempSync(join(tmpdir(), 'publish-test-'));
  try {
    const source = join(root, 'source'),
      destination = join(root, 'destination'),
      bin = join(root, 'bin'),
      log = join(root, 'log');
    for (const dir of [source, destination, bin]) mkdirSync(dir);
    const git = (dir: string, ...args: string[]) =>
      execFileSync('git', ['-C', dir, ...args], { encoding: 'utf8' }).trim();
    for (const dir of [source, destination]) {
      git(dir, 'init', '-q');
      git(dir, 'config', 'user.name', 'Test');
      git(dir, 'config', 'user.email', 'test@example.invalid');
      writeFileSync(
        join(dir, 'file.txt'),
        dir === source ? 'source\n' : 'destination\n',
      );
      git(dir, 'add', '.');
      git(dir, 'commit', '-qm', 'fixture');
    }
    const sha = git(source, 'rev-parse', 'HEAD');
    const gitPath = execFileSync('which', ['git'], { encoding: 'utf8' }).trim();
    const gitLog = join(root, 'git.log');
    const mutation = join(root, 'mutation');
    writeFileSync(
      join(bin, 'git'),
      `#!${process.execPath}\nconst fs=require('node:fs'), cp=require('node:child_process'), args=process.argv.slice(2), git=${JSON.stringify(gitPath)}; fs.appendFileSync(${JSON.stringify(gitLog)}, JSON.stringify(args)+'\\n'); const r=cp.spawnSync(git,args,{stdio:'inherit',env:process.env}); if(r.status===0 && args[1]===${JSON.stringify(destination)} && args[2]==='add' && fs.existsSync(${JSON.stringify(mutation)})){const mode=fs.readFileSync(${JSON.stringify(mutation)},'utf8'); const extra=mode==='mode'?['--chmod=+x','file.txt']:mode==='missing'?['--force-remove','file.txt']:['--add','--cacheinfo','100644,'+cp.execFileSync(git,['-C',args[1],'hash-object','-w','file.txt'],{encoding:'utf8'}).trim()+',extra.txt']; cp.execFileSync(git,['-C',args[1],'update-index',...extra],{env:process.env});} process.exit(r.status ?? 1);\n`,
      { mode: 0o755 },
    );
    writeFileSync(log, '');
    const ghLog = join(root, 'gh.log');
    writeFileSync(
      join(bin, 'gh'),
      `#!${process.execPath}\nrequire('node:fs').appendFileSync(${JSON.stringify(ghLog)}, 'called\\n'); process.exit(1);\n`,
      { mode: 0o755 },
    );
    writeFileSync(
      join(bin, 'docker'),
      `#!${process.execPath}\nconst fs=require('node:fs'); if(process.argv.includes('inspect')) process.stdout.write('null'); else fs.appendFileSync(${JSON.stringify(log)},'run\\n');\n`,
      { mode: 0o755 },
    );
    const definition = {
      repo: source,
      paths: ['file.txt'],
      replacements: [],
      reject: [],
      ci: 'intake',
      verify: 'intake',
      image: 'example@sha256:' + 'a'.repeat(64),
    };
    const entry = {
      source: definition,
      ref: 'HEAD',
      publicRepo: destination,
      publicRef: 'HEAD',
    };
    const registry = join(root, 'registry.json');
    writeFileSync(
      registry,
      JSON.stringify({ one: entry, two: entry, third: entry }),
    );
    const script = resolve('scripts/publish-public-source.sh');
    const run = (name: string, extra: string[] = []) => {
      const output = join(root, name);
      const result = spawnSync(
        'sh',
        [
          script,
          '--target',
          'third',
          '--registry',
          registry,
          '--output',
          output,
          ...extra,
        ],
        {
          encoding: 'utf8',
          env: { ...process.env, PATH: bin + ':' + process.env.PATH },
        },
      );
      assert.equal(result.status, 0, result.stderr);
      return { output, data: JSON.parse(result.stdout) };
    };
    const refsBefore = git(destination, 'show-ref');
    const first = run('first');
    const commitInfo = git(destination, 'cat-file', '-p', first.data.commit);
    assert.equal(first.data.publicBase, git(destination, 'rev-parse', 'HEAD'));
    assert.equal(
      git(destination, 'rev-parse', first.data.commit + '^{tree}'),
      first.data.generatedTree,
    );
    assert.deepEqual(
      git(
        destination,
        'rev-list',
        '--parents',
        '-n',
        '1',
        first.data.commit,
      ).split(' '),
      [first.data.commit, first.data.publicBase],
    );
    assert.equal(commitInfo.includes(sha), false);
    assert.match(
      commitInfo,
      /author Public Source <public-source@example\.invalid>/,
    );
    assert.match(
      commitInfo,
      /committer Public Source <public-source@example\.invalid>/,
    );
    assert.equal(git(destination, 'show-ref'), refsBefore);
    const calls = readFileSync(gitLog, 'utf8')
      .trim()
      .split('\n')
      .map((line) => JSON.parse(line));
    assert.equal(
      calls.filter(
        (args) =>
          args[1] === source &&
          args.includes('rev-parse') &&
          args.includes('HEAD^{commit}'),
      ).length,
      1,
    );
    assert.equal(calls.filter((args) => args.includes('push')).length, 0);
    assert.equal(first.data.status, 'prepared');
    assert.equal(first.data.inputSha, sha);
    assert.equal(
      readFileSync(join(first.output, 'file.txt'), 'utf8'),
      'source\n',
    );
    const previous = join(root, 'previous.json');
    writeFileSync(
      previous,
      JSON.stringify({ ...first.data, status: 'merged' }),
    );
    const previousContents = readFileSync(previous);
    const reject = (name: string) => {
      const result = spawnSync(
        'sh',
        [
          script,
          '--target',
          'third',
          '--registry',
          registry,
          '--output',
          join(root, name),
        ],
        {
          encoding: 'utf8',
          env: { ...process.env, PATH: bin + ':' + process.env.PATH },
        },
      );
      assert.notEqual(result.status, 0, name);
      assert.equal(result.stdout.trim(), '', name);
      assert.equal(git(destination, 'show-ref'), refsBefore);
      assert.deepEqual(readFileSync(previous), previousContents);
    };
    git(destination, 'config', 'filter.change.clean', 'sed s/source/changed/');
    writeFileSync(
      join(destination, '.git', 'info', 'attributes'),
      'file.txt filter=change\n',
    );
    reject('filtered');
    writeFileSync(join(destination, '.git', 'info', 'attributes'), '');
    for (const kind of ['mode', 'missing', 'extra']) {
      writeFileSync(mutation, kind);
      reject('invalid-' + kind);
    }
    rmSync(mutation);
    const skipped = run('skipped', [
      '--previous',
      previous,
      '--previous-tree',
      first.output,
    ]);
    assert.equal(skipped.data.status, 'skipped');
    assert.equal('commit' in skipped.data, false);
    assert.equal('publicBase' in skipped.data, false);
    assert.equal(readFileSync(log, 'utf8').trim().split('\n').length, 5);
    writeFileSync(
      previous,
      JSON.stringify({
        ...first.data,
        status: 'merged',
        inputSha: (sha[0] === '0' ? '1' : '0') + sha.slice(1),
      }),
    );
    const changedInput = run('changed-input', [
      '--previous',
      previous,
      '--previous-tree',
      first.output,
    ]);
    assert.equal(changedInput.data.status, 'prepared');
    assert.equal(changedInput.data.inputSha, sha);
    assert.equal(
      readFileSync(join(changedInput.output, 'file.txt'), 'utf8'),
      'source\n',
    );
    assert.equal(readFileSync(log, 'utf8').trim().split('\n').length, 6);
    for (const field of [
      'configSha256',
      'generatorSha256',
      'ciSha256',
      'treeSha256',
    ]) {
      writeFileSync(
        previous,
        JSON.stringify({
          ...first.data,
          status: 'merged',
          [field]: '0'.repeat(64),
        }),
      );
      assert.equal(
        run('changed-' + field, [
          '--previous',
          previous,
          '--previous-tree',
          first.output,
        ]).data.status,
        'prepared',
      );
    }
    git(destination, 'read-tree', '--empty');
    execFileSync('git', ['-C', destination, 'add', '-f', '--all'], {
      env: { ...process.env, GIT_WORK_TREE: first.output },
    });
    git(destination, 'commit', '-qm', 'public');
    const same = run('same').data;
    assert.equal(same.status, 'no-diff');
    assert.equal('commit' in same, false);
    assert.equal('publicBase' in same, false);
    assert.equal(readFileSync(gitLog, 'utf8').includes('"push"'), false);
    const allCalls = readFileSync(gitLog, 'utf8')
      .trim()
      .split('\n')
      .map((line) => JSON.parse(line));
    assert.equal(
      allCalls.some((args) =>
        ['push', 'fetch', 'pull', 'update-ref'].some((command) =>
          args.includes(command),
        ),
      ),
      false,
    );
    assert.equal(existsSync(ghLog), false);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

function sendFixture(merge = false, author?: { name: string; email: string }) {
  const root = mkdtempSync(join(tmpdir(), 'public-send-test-'));
  const source = join(root, 'source'),
    destination = join(root, 'destination'),
    bin = join(root, 'bin'),
    stateDir = join(root, 'state'),
    remote = join(root, 'remote.json');
  for (const dir of [source, destination, bin, stateDir]) mkdirSync(dir);
  const git = (dir: string, ...args: string[]) =>
    execFileSync('git', ['-C', dir, ...args], { encoding: 'utf8' }).trim();
  for (const dir of [source, destination]) {
    git(dir, 'init', '-q');
    git(dir, 'config', 'user.name', 'Fixture');
    git(dir, 'config', 'user.email', 'fixture@example.invalid');
    writeFileSync(
      join(dir, 'file.txt'),
      dir === source ? 'public contents\n' : 'old contents\n',
    );
    git(dir, 'add', '.');
    git(dir, 'commit', '-qm', 'fixture');
  }
  const base = git(destination, 'rev-parse', 'HEAD');
  const realGit = execFileSync('which', ['git'], { encoding: 'utf8' }).trim();
  writeFileSync(
    join(bin, 'git'),
    `#!${process.execPath}
const fs=require('node:fs'), cp=require('node:child_process'), a=process.argv.slice(2), p=${JSON.stringify(remote)}, real=${JSON.stringify(realGit)};
if (a.includes('ls-remote') || a.includes('push')) {
 const s=JSON.parse(fs.readFileSync(p)); const push=a.includes('push'); s.log.push(push?'push':'read');
 if(push) { const spec=a[a.length-1].split(':'), head=spec[0], ref=spec[1]; s.pushArgs=a;
   if(a[a.length-2]!=='https://github.com/example/public.git' || !a.includes('--force-with-lease='+ref+':') || !ref.startsWith('refs/heads/public-source/') || a.includes('--force')) { fs.writeFileSync(p,JSON.stringify(s)); process.exit(1); }
   if(s.mode==='branch-conflict') { s.branch='f'.repeat(40); }
   else if(s.mode==='branch-same') { s.branch=head; fs.writeFileSync(p,JSON.stringify(s)); process.exit(1); }
   else if(!s.branch) { s.branch=head; if(s.mode==='base-after-push') s.base='e'.repeat(40); }
   fs.writeFileSync(p,JSON.stringify(s)); process.exit(s.branch===head?0:1);
 }
 const ref=a[a.length-1], value=ref==='refs/heads/main'?s.base:s.branch; fs.writeFileSync(p,JSON.stringify(s));
 if(value) process.stdout.write(value+'\\t'+ref+'\\n'); process.exit(0);
}
const r=cp.spawnSync(real,a,{stdio:'inherit',env:process.env}); process.exit(r.status??1);
`,
    { mode: 0o755 },
  );
  writeFileSync(
    join(bin, 'gh'),
    `#!${process.execPath}
const fs=require('node:fs'), p=${JSON.stringify(remote)}, a=process.argv.slice(2), s=JSON.parse(fs.readFileSync(p));
if(process.env.GH_TOKEN!=='synthetic-token') process.exit(2);
if(s.mode==='parallel' && a.includes('repos/example/public') && !fs.existsSync(p+'.entered')) { fs.writeFileSync(p+'.entered',''); while(!fs.existsSync(p+'.release')) Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,20); }
const endpoint=a.find(x=>x.startsWith('repos/example/public/'))||'';
const done=(value)=>{fs.writeFileSync(p,JSON.stringify(s));process.stdout.write(JSON.stringify(value));};
if(endpoint.endsWith('/merge') && a.includes('PUT')) { s.log.push('merge'); s.mergeArgs=a; s.pr.state='closed'; s.pr.merged=true; s.pr.merged_at='now'; s.pr.merge_commit_sha='c'.repeat(40); s.base=s.pr.merge_commit_sha; done({merged:true}); if(s.mode==='merge-lost') process.exit(1); }
else if(endpoint==='repos/example/public/pulls/7') { s.log.push('detail'); if(s.mode==='changed-head') s.pr.head.sha='d'.repeat(40); if(s.mode==='changed-base') s.pr.base.sha='d'.repeat(40); done(s.pr); }
else if(endpoint.includes('/rules/branches/')) { s.log.push('rules'); if(s.mode==='rules-error') process.exit(1); done((s.mode.startsWith('rules-') || s.mode.startsWith('mixed-')) && s.mode!=='rules-empty' ? [{type:'required_status_checks',parameters:{strict_required_status_checks_policy:s.mode!=='rules-nonstrict',required_status_checks:s.mode==='rules-no-checks'?[]:[{context:'verify',integration_id:['rules-wrong-app','mixed-conflict'].includes(s.mode)?1:15368}]}}] : []); }
else if(endpoint.includes('/protection/required_status_checks')) { s.log.push('protection'); fs.writeFileSync(p,JSON.stringify(s)); if(s.mode.startsWith('rules-')) { const msg=s.mode==='rules-not-found'?'Not Found':'Branch not protected'; process.stdout.write('HTTP/2.0 404 Not Found\\r\\ncontent-type: application/json\\r\\n\\r\\n'+JSON.stringify({message:msg})); process.exit(1); } if(s.mode==='protection-error') { process.stdout.write('HTTP/2.0 403 Forbidden\\r\\n\\r\\n{}'); process.exit(1); } process.stdout.write('HTTP/2.0 200 OK\\r\\ncontent-type: application/json\\r\\n\\r\\n'+JSON.stringify({strict:s.mode!=='nonstrict',contexts:s.mode==='waiting-failure'?['verify','lint']:['verify'],checks:s.mode==='waiting-failure'?[{context:'verify',app_id:15368},{context:'lint',app_id:15368}]:[{context:'verify',app_id:15368}]})); }
else if(endpoint.includes('/check-runs?')) { s.log.push('checks'); const waiting=['queued','in_progress','waiting','pending','requested','waiting-wrong-app','waiting-wrong-sha','waiting-failure'].includes(s.mode); const runs=s.mode==='missing'?[]:[{name:'verify',app:{id:['wrong-app','waiting-wrong-app'].includes(s.mode)?1:15368},head_sha:s.mode==='waiting-wrong-sha'?'d'.repeat(40):s.branch,status:waiting?(s.mode.startsWith('waiting-')?'queued':s.mode):'completed',conclusion:waiting?null:s.mode==='failed'?'failure':'success'}]; if(s.mode==='waiting-failure') runs.push({name:'lint',app:{id:15368},head_sha:s.branch,status:'completed',conclusion:'failure'}); done({total_count:runs.length,check_runs:runs}); }
else if(endpoint.includes('/status?')) { s.log.push('statuses'); done({sha:s.branch,total_count:0,statuses:[]}); }
else if(endpoint.includes('/git/commits/')) { s.log.push('commit'); const sha=endpoint.split('/').pop(); done(sha==='c'.repeat(40)?{sha,tree:{sha:s.mode==='wrong-tree'?'f'.repeat(40):s.tree},parents:[{sha:s.originalBase},{sha:s.branch}]}:{sha,tree:{sha:s.tree},parents:[{sha:s.originalBase}]}); }
else if(a.includes('repos/example/public') && !a.includes('POST')) { s.log.push('repository'); fs.writeFileSync(p,JSON.stringify(s)); process.stdout.write(JSON.stringify({full_name:s.mode==='wrong-repo'?'other/public':'example/public',default_branch:s.mode==='default-after-pr'&&s.pr?'other':'main'})); }
else if(a.includes('POST')) { s.log.push('create'); s.post=a; const fields=Object.fromEntries(a.filter(x=>x.includes('=')).map(x=>x.split('='))); if(s.mode!=='lost-empty') s.pr={number:7,state:'open',head:{ref:fields.head,sha:s.mode==='pr-mismatch'?'f'.repeat(40):s.branch,repo:{full_name:'example/public'}},base:{ref:fields.base,sha:s.base,repo:{full_name:'example/public'}}}; if(s.mode==='base-after-pr') s.base='d'.repeat(40); fs.writeFileSync(p,JSON.stringify(s)); if(s.mode==='lost-response'||s.mode==='lost-empty') process.exit(1); process.stdout.write('{}'); }
else { s.log.push('list'); fs.writeFileSync(p,JSON.stringify(s)); if(s.mode==='query-failure') process.exit(1); process.stdout.write(JSON.stringify(s.pr?[s.pr]:[])); }
`,
    { mode: 0o755 },
  );
  const leaked = join(root, 'generator-leak');
  writeFileSync(
    join(bin, 'docker'),
    `#!${process.execPath}\nif(process.env.GH_TOKEN||process.env.PUBLIC_SEND_TOKEN) require('node:fs').writeFileSync(${JSON.stringify(leaked)},'leaked'); if(process.argv.includes('inspect')) process.stdout.write('null');\n`,
    { mode: 0o755 },
  );
  const registry = join(root, 'registry.json');
  const entry = {
    source: {
      repo: source,
      paths: ['file.txt'],
      replacements: [],
      reject: [],
      ci: 'intake',
      verify: 'intake',
      image: 'example@sha256:' + 'a'.repeat(64),
    },
    ref: 'HEAD',
    publicRepo: destination,
    publicRef: 'HEAD',
    ...(author ? { author } : {}),
    send: {
      repository: 'example/public',
      credentialRef: 'issuer:test',
      base: 'refs/heads/main',
      stateDir,
      ...(merge ? { mergeMethod: 'merge' } : {}),
    },
  };
  writeFileSync(registry, JSON.stringify({ example: entry }));
  const env = {
    ...process.env,
    PATH: bin + ':' + process.env.PATH,
    GH_TOKEN: 'synthetic-token',
    GIT_AUTHOR_NAME: 'Injected',
    GIT_AUTHOR_EMAIL: 'injected@example.invalid',
    GIT_COMMITTER_NAME: 'Injected',
    GIT_COMMITTER_EMAIL: 'injected@example.invalid',
  };
  const script = resolve('scripts/publish-public-source.sh'),
    tree = join(root, 'tree'),
    candidatePath = join(root, 'candidate.json');
  const prepared = spawnSync(
    'sh',
    [script, '--target', 'example', '--registry', registry, '--output', tree],
    { encoding: 'utf8', env },
  );
  assert.equal(prepared.status, 0, prepared.stderr);
  const candidate = JSON.parse(prepared.stdout);
  writeFileSync(candidatePath, prepared.stdout);
  const initial = {
    base,
    originalBase: base,
    tree: candidate.generatedTree,
    branch: '',
    pr: null,
    mode: '',
    log: [] as string[],
  };
  writeFileSync(remote, JSON.stringify(initial));
  const args = [
    script,
    '--send',
    '--target',
    'example',
    '--registry',
    registry,
    '--candidate',
    candidatePath,
    '--tree',
    tree,
  ];
  const preview = spawnSync('sh', args, { encoding: 'utf8', env });
  assert.equal(preview.status, 0, preview.stderr);
  const proof = JSON.parse(preview.stdout).confirmation;
  const credential = {
    token: 'synthetic-token',
    metadata: {
      repository: 'example/public',
      credentialRef: 'issuer:test',
      permissions: {
        contents: 'write',
        pull_requests: 'write',
        workflows: 'write',
      },
    },
  };
  const send = (input: unknown = credential) =>
    spawnSync('sh', [...args, '--confirm', proof], {
      encoding: 'utf8',
      env,
      input: JSON.stringify(input),
    });
  const read = () => JSON.parse(readFileSync(remote, 'utf8'));
  const configure = (patch: object) =>
    writeFileSync(remote, JSON.stringify({ ...read(), ...patch }));
  return {
    root,
    tree,
    candidate,
    candidatePath,
    base,
    credential,
    send,
    read,
    configure,
    git,
    destination,
    registry,
    entry,
    leaked,
    stateDir,
    remote,
    startSend: () => {
      const child = spawn('sh', [...args, '--confirm', proof], {
        env,
        stdio: ['pipe', 'pipe', 'pipe'],
      });
      child.stdin.end(JSON.stringify(credential));
      const done = new Promise<number | null>((resolve, reject) => {
        child.on('error', reject);
        child.on('close', resolve);
      });
      return { child, done };
    },
    prepare: (output: string, extra: string[] = []) =>
      spawnSync(
        'sh',
        [
          script,
          '--target',
          'example',
          '--registry',
          registry,
          '--output',
          output,
          ...extra,
        ],
        { encoding: 'utf8', env },
      ),
  };
}

test('登録著者の生成とコミット改ざんの拒否', () => {
  const author = {
    name: 'Example Author',
    email: '12345+fictional@users.noreply.github.com',
  };
  const f = sendFixture(false, author);
  try {
    const raw = execFileSync(
      'git',
      ['-C', f.destination, 'cat-file', 'commit', f.candidate.commit],
      { encoding: 'utf8' },
    );
    assert.match(
      raw,
      /author Example Author <12345\+fictional@users\.noreply\.github\.com>/,
    );
    assert.match(
      raw,
      /committer Public Source <public-source@example\.invalid>/,
    );
    const variants = [
      raw.replace('Example Author', 'Other Author'),
      raw.replace(
        '12345+fictional@users.noreply.github.com',
        'other@users.noreply.github.com',
      ),
      raw.replace('\nauthor ', `\nparent ${f.base}\nauthor `),
      raw.replace('\n\nPrepare', '\nextra injected\n\nPrepare'),
      raw + 'extra\n',
      raw + '\n',
      raw.replace(
        'tree ' + f.candidate.generatedTree,
        'tree ' + f.git(f.destination, 'rev-parse', f.base + '^{tree}'),
      ),
    ];
    for (const contents of variants) {
      const commit = execFileSync(
        'git',
        ['-C', f.destination, 'hash-object', '-t', 'commit', '-w', '--stdin'],
        { input: contents, encoding: 'utf8' },
      ).trim();
      writeFileSync(
        f.candidatePath,
        JSON.stringify({ ...f.candidate, commit }),
      );
      assert.notEqual(f.send().status, 0);
      assert.deepEqual(f.read().log, []);
    }
    for (const change of [
      { inputSha: '0'.repeat(40) },
      { publicBase: f.candidate.commit },
    ]) {
      writeFileSync(
        f.candidatePath,
        JSON.stringify({ ...f.candidate, ...change }),
      );
      assert.notEqual(f.send().status, 0);
      assert.deepEqual(f.read().log, []);
    }
    writeFileSync(f.candidatePath, JSON.stringify(f.candidate));
    writeFileSync(
      f.registry,
      JSON.stringify({
        example: { ...f.entry, author: { ...author, name: 'Changed Author' } },
      }),
    );
    assert.notEqual(f.send().status, 0);
    assert.deepEqual(f.read().log, []);
    writeFileSync(f.registry, JSON.stringify({ example: f.entry }));
    assert.equal(f.send().status, 0);
    for (const invalid of [
      { name: '', email: author.email },
      { name: '  ', email: author.email },
      { name: 'Bad\ncommitter evil', email: author.email },
      { name: 'Bad <name>', email: author.email },
      { name: 'Bad\u0000name', email: author.email },
      { name: author.name, email: '' },
      { name: author.name, email: 'person@example.com' },
      { name: author.name, email: 'bad@users.noreply.github.com\nextra' },
      { name: author.name, email: 'bad..name@users.noreply.github.com' },
    ]) {
      writeFileSync(
        f.registry,
        JSON.stringify({ example: { ...f.entry, author: invalid } }),
      );
      assert.notEqual(
        f.prepare(
          join(
            f.root,
            'invalid-' + invalid.name.length + '-' + invalid.email.length,
          ),
        ).status,
        0,
      );
    }
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

test('送信候補の再照合', () => {
  const f = sendFixture();
  try {
    const denied = structuredClone(f.credential);
    denied.metadata.permissions.contents = 'read';
    const rejected = f.send(denied);
    assert.notEqual(rejected.status, 0);
    assert.equal(
      (rejected.stdout + rejected.stderr).includes('synthetic-token'),
      false,
    );
    assert.deepEqual(f.read().log, []);
    denied.metadata.permissions.contents = 'write';
    denied.metadata.repository = 'other/public';
    assert.notEqual(f.send(denied).status, 0);
    assert.deepEqual(f.read().log, []);
    denied.metadata.repository = 'example/public';
    denied.metadata.permissions.workflows = 'read';
    assert.notEqual(f.send(denied).status, 0);
    assert.deepEqual(f.read().log, []);
    assert.notEqual(f.send({ token: 'synthetic-token' }).status, 0);
    assert.deepEqual(f.read().log, []);
    writeFileSync(join(f.tree, 'internal.txt'), 'synthetic-private-value');
    const extra = f.send();
    assert.notEqual(extra.status, 0);
    assert.equal(
      (extra.stdout + extra.stderr).includes('synthetic-private-value'),
      false,
    );
    assert.deepEqual(f.read().log, []);
    rmSync(join(f.tree, 'internal.txt'));
    chmodSync(join(f.tree, 'file.txt'), 0o755);
    assert.notEqual(f.send().status, 0);
    assert.deepEqual(f.read().log, []);
    chmodSync(join(f.tree, 'file.txt'), 0o644);
    writeFileSync(join(f.tree, 'file.txt'), 'tampered\n');
    assert.notEqual(f.send().status, 0);
    assert.deepEqual(f.read().log, []);
    writeFileSync(join(f.tree, 'file.txt'), 'public contents\n');
    writeFileSync(
      f.candidatePath,
      JSON.stringify({ ...f.candidate, generatedTree: f.base }),
    );
    assert.notEqual(f.send().status, 0);
    assert.deepEqual(f.read().log, []);
    writeFileSync(
      f.candidatePath,
      JSON.stringify({ ...f.candidate, commit: f.base }),
    );
    assert.notEqual(f.send().status, 0);
    assert.deepEqual(f.read().log, []);
    writeFileSync(f.candidatePath, JSON.stringify(f.candidate));
    f.git(
      f.destination,
      'config',
      'filter.change.clean',
      'sed s/public/changed/',
    );
    writeFileSync(
      join(f.destination, '.git', 'info', 'attributes'),
      'file.txt filter=change\n',
    );
    assert.notEqual(f.send().status, 0);
    assert.deepEqual(f.read().log, []);
    writeFileSync(join(f.destination, '.git', 'info', 'attributes'), '');
    const result = f.send();
    assert.equal(result.status, 0, result.stderr);
    const output = JSON.parse(result.stdout),
      state = f.read();
    assert.equal(output.head, f.candidate.commit);
    assert.equal(output.base, f.base);
    assert.equal(output.tree, f.candidate.generatedTree);
    assert.equal(output.pr, 7);
    assert.equal(state.log.filter((x: string) => x === 'create').length, 1);
    assert.equal(result.stdout.includes('synthetic-token'), false);
    assert.equal(result.stdout.includes('public contents'), false);
    assert.equal(JSON.stringify(state.post).includes('synthetic-token'), false);
    assert.equal(
      JSON.stringify(state.post).includes(f.candidate.inputSha),
      false,
    );
    assert.equal(JSON.stringify(state.post).includes(f.root), false);
    assert.equal(existsSync(f.leaked), false);
    const again = f.send();
    assert.equal(again.status, 0, again.stderr);
    assert.equal(f.read().log.filter((x: string) => x === 'create').length, 1);
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

test('競合・再開', async () => {
  for (const mode of [
    'base-before',
    'wrong-repo',
    'base-after-push',
    'branch-conflict',
    'branch-same',
    'base-after-pr',
    'default-after-pr',
    'pr-mismatch',
    'lost-response',
    'lost-empty',
    'query-failure',
  ]) {
    const f = sendFixture();
    try {
      f.configure({
        mode,
        ...(mode === 'base-before' ? { base: 'a'.repeat(40) } : {}),
      });
      const result = f.send(),
        state = f.read();
      if (mode === 'lost-response' || mode === 'branch-same') {
        assert.equal(result.status, 0, result.stderr);
        assert.equal(JSON.parse(result.stdout).pr, 7);
        assert.equal(f.send().status, 0);
        assert.equal(
          f.read().log.filter((x: string) => x === 'create').length,
          1,
        );
      } else {
        assert.notEqual(result.status, 0, mode);
        assert.equal(result.stdout, '');
        assert.equal(
          state.log.filter((x: string) => x === 'create').length,
          [
            'base-after-pr',
            'default-after-pr',
            'pr-mismatch',
            'lost-empty',
          ].includes(mode)
            ? 1
            : 0,
        );
        if (mode === 'base-before' || mode === 'wrong-repo')
          assert.equal(state.log.includes('push'), false);
        if (mode === 'base-after-push')
          assert.equal(state.branch, f.candidate.commit);
        if (mode === 'branch-conflict')
          assert.equal(state.branch, 'f'.repeat(40));
        if (mode === 'lost-empty') {
          assert.notEqual(f.send().status, 0);
          assert.equal(
            f.read().log.filter((x: string) => x === 'create').length,
            1,
          );
        }
      }
      assert.equal(f.git(f.destination, 'rev-parse', 'HEAD'), f.base);
    } finally {
      rmSync(f.root, { recursive: true, force: true });
    }
  }

  const concurrent = sendFixture();
  try {
    concurrent.configure({ mode: 'parallel' });
    const first = concurrent.startSend();
    let ended = false;
    void first.done.then(
      () => {
        ended = true;
      },
      () => {
        ended = true;
      },
    );
    try {
      while (!existsSync(concurrent.remote + '.entered')) {
        if (ended) throw Error('先行送信が排他取得前に終了');
        await new Promise((resolve) => setTimeout(resolve, 20));
      }
      const second = concurrent.send();
      assert.notEqual(second.status, 0);
    } finally {
      writeFileSync(concurrent.remote + '.release', '');
    }
    assert.equal(await first.done, 0);
    assert.equal(
      concurrent.read().log.filter((x: string) => x === 'create').length,
      1,
    );
  } finally {
    rmSync(concurrent.root, { recursive: true, force: true });
  }
});

test('マージ証拠による準備の見送り', () => {
  const f = sendFixture(true);
  try {
    const recordPath = join(f.stateDir, 'example.merged.json');
    const merge = f.git(
      f.destination,
      'commit-tree',
      f.candidate.generatedTree,
      '-p',
      f.base,
      '-p',
      f.candidate.commit,
      '-m',
      'merge',
    );
    f.git(f.destination, 'update-ref', 'HEAD', merge);
    const record = {
      ...f.candidate,
      status: 'merged',
      repository: 'example/public',
      mergeCommit: merge,
    };
    const save = (value: object) =>
      writeFileSync(recordPath, JSON.stringify(value), { mode: 0o600 });
    const prepare = (name: string) => f.prepare(join(f.root, name));
    save(record);
    let result = prepare('merged');
    assert.equal(result.status, 0, result.stderr);
    assert.equal(JSON.parse(result.stdout).status, 'skipped');
    const changedTree = f.git(f.destination, 'rev-parse', f.base + '^{tree}');
    const next = f.git(
      f.destination,
      'commit-tree',
      changedTree,
      '-p',
      merge,
      '-m',
      'later public change',
    );
    f.git(f.destination, 'update-ref', 'HEAD', next);
    result = prepare('advanced');
    assert.equal(result.status, 0, result.stderr);
    assert.equal(JSON.parse(result.stdout).status, 'skipped');
    result = f.prepare(join(f.root, 'scheduled-base'), ['--public-base', next]);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(JSON.parse(result.stdout).status, 'skipped');
    for (const [field, value] of Object.entries({
      target: 'other',
      repository: 'other/public',
      inputSha: '0'.repeat(40),
      configSha256: '0'.repeat(64),
      generatorSha256: '0'.repeat(64),
      ciSha256: '0'.repeat(64),
      generatedTree: f.base,
      publicBase: f.candidate.commit,
      commit: f.base,
      mergeCommit: f.base,
      status: 'no-diff',
    })) {
      save({ ...record, [field]: value });
      const attempt = prepare('mismatch-' + field);
      assert.equal(attempt.status, 0, attempt.stderr);
      assert.notEqual(JSON.parse(attempt.stdout).status, 'skipped', field);
    }
    const unrelated = f.git(
      f.destination,
      'commit-tree',
      f.candidate.generatedTree,
      '-p',
      f.base,
      '-p',
      f.candidate.commit,
      '-m',
      'unrelated',
    );
    save({ ...record, mergeCommit: unrelated });
    assert.notEqual(
      JSON.parse(prepare('unreachable').stdout).status,
      'skipped',
    );
    save(record);
    chmodSync(recordPath, 0o644);
    assert.notEqual(prepare('bad-mode').status, 0);
    rmSync(recordPath);
    symlinkSync(join(f.root, 'candidate.json'), recordPath);
    assert.notEqual(prepare('link').status, 0);
    rmSync(recordPath);
    result = prepare('missing');
    assert.equal(result.status, 0, result.stderr);
    assert.notEqual(JSON.parse(result.stdout).status, 'skipped');
    save(record);
    writeFileSync(join(f.entry.source.repo, 'file.txt'), 'updated input\n');
    f.git(f.entry.source.repo, 'add', 'file.txt');
    f.git(f.entry.source.repo, 'commit', '-qm', 'updated input');
    result = prepare('new-input');
    assert.equal(result.status, 0, result.stderr);
    assert.equal(JSON.parse(result.stdout).status, 'prepared');
    const badEntry = {
      ...f.entry,
      send: { ...f.entry.send, stateDir: f.stateDir + '/..' },
    };
    writeFileSync(f.registry, JSON.stringify({ example: badEntry }));
    assert.notEqual(prepare('bad-path').status, 0);
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

test('必須CIと結果記録', () => {
  for (const mode of [
    '',
    'failed',
    'queued',
    'in_progress',
    'waiting',
    'pending',
    'requested',
    'waiting-wrong-app',
    'waiting-wrong-sha',
    'waiting-failure',
    'missing',
    'wrong-app',
    'nonstrict',
    'changed-head',
    'changed-base',
    'wrong-tree',
    'merge-lost',
    'rules-only',
    'rules-empty',
    'rules-nonstrict',
    'rules-no-checks',
    'rules-wrong-app',
    'rules-not-found',
    'rules-error',
    'protection-error',
    'mixed-match',
    'mixed-conflict',
  ]) {
    const f = sendFixture(true);
    try {
      f.configure({ mode });
      const previous = join(f.stateDir, 'example.merged.json');
      writeFileSync(previous, '{"status":"old"}\n', { mode: 0o600 });
      const result = f.send();
      const state = f.read();
      if (mode === '' || mode === 'rules-only' || mode === 'mixed-match') {
        assert.equal(result.status, 0, result.stderr);
        assert.equal(JSON.parse(result.stdout).status, 'merged');
        assert.equal(
          JSON.parse(readFileSync(previous, 'utf8')).inputSha,
          f.candidate.inputSha,
        );
        assert.equal(state.log.filter((x: string) => x === 'merge').length, 1);
        assert.ok(state.mergeArgs.includes(`sha=${f.candidate.commit}`));
        assert.ok(state.mergeArgs.includes('merge_method=merge'));
      } else {
        assert.notEqual(result.status, 0, mode);
        if (
          ['queued', 'in_progress', 'waiting', 'pending', 'requested'].includes(
            mode,
          )
        ) {
          const output = JSON.parse(result.stdout);
          assert.equal(output.status, 'waiting');
          assert.equal(output.target, 'example');
          assert.equal(output.repository, 'example/public');
          assert.equal(output.head, f.candidate.commit);
          assert.equal(output.base, f.candidate.publicBase);
          assert.equal(output.pr, 7);
        } else assert.equal(result.stdout, '', mode);
        assert.equal(readFileSync(previous, 'utf8'), '{"status":"old"}\n');
        assert.equal(
          state.log.filter((x: string) => x === 'merge').length,
          ['wrong-tree', 'merge-lost'].includes(mode) ? 1 : 0,
        );
        if (mode === 'merge-lost') {
          f.configure({ mode: '' });
          f.git(f.destination, 'reset', '--hard', f.candidate.commit);
          const resumed = f.send();
          assert.equal(resumed.status, 0, resumed.stderr);
          assert.equal(
            JSON.parse(readFileSync(previous, 'utf8')).inputSha,
            f.candidate.inputSha,
          );
          assert.equal(
            f.read().log.filter((x: string) => x === 'merge').length,
            1,
          );
        }
      }
    } finally {
      rmSync(f.root, { recursive: true, force: true });
    }
  }
  const f = sendFixture(true);
  try {
    const previous = join(f.stateDir, 'example.merged.json');
    writeFileSync(previous, '{"status":"old"}\n', { mode: 0o600 });
    f.git(f.destination, 'read-tree', '--empty');
    execFileSync('git', ['-C', f.destination, 'add', '-f', '--all'], {
      env: { ...process.env, GIT_WORK_TREE: f.tree },
    });
    f.git(f.destination, 'commit', '-qm', 'public');
    const result = f.prepare(join(f.root, 'same-tree'));
    assert.equal(result.status, 0, result.stderr);
    assert.equal(JSON.parse(result.stdout).status, 'no-diff');
    assert.equal(
      JSON.parse(readFileSync(join(f.stateDir, 'example.no-diff.json'), 'utf8'))
        .status,
      'no-diff',
    );
    assert.equal(readFileSync(previous, 'utf8'), '{"status":"old"}\n');
    assert.deepEqual(f.read().log, []);
  } finally {
    rmSync(f.root, { recursive: true, force: true });
  }
});

function failureReasonFixtures(
  check: (
    result: ReturnType<typeof spawnSync>,
    reason: string,
    secrets: string[],
  ) => void,
) {
  const root = mkdtempSync(join(tmpdir(), 'reason-fixture-'));
  try {
    const source = join(root, 'private-source'),
      destination = join(root, 'private-public'),
      bin = join(root, 'bin');
    for (const dir of [source, destination, bin]) mkdirSync(dir);
    const git = (dir: string, ...args: string[]) =>
      execFileSync('git', ['-C', dir, ...args], { encoding: 'utf8' }).trim();
    for (const dir of [source, destination]) {
      git(dir, 'init', '-q');
      git(dir, 'config', 'user.name', 'Fixture');
      git(dir, 'config', 'user.email', 'fixture@example.invalid');
      writeFileSync(
        join(dir, 'file.txt'),
        'content unique-reject-0 unique-from-0\n',
      );
      if (dir === source) {
        mkdirSync(join(dir, 'deploy/nas-host'), { recursive: true });
        writeFileSync(
          join(dir, 'deploy/nas-host/install-github-actions-runner.sh'),
          'runner\n',
        );
        writeFileSync(
          join(dir, 'host-ssh.json'),
          JSON.stringify({
            operations: { 'install-github-actions-runner': 'a'.repeat(64) },
            extra: 'unique-from-3',
          }),
        );
        symlinkSync('file.txt', join(dir, 'link.txt'));
      }
      git(dir, 'add', '.');
      git(dir, 'commit', '-qm', 'fixture');
    }
    writeFileSync(join(bin, 'docker'), '#!/bin/sh\nexit 0\n', { mode: 0o755 });
    const cases = [
      {
        paths: ['file.txt'],
        outputs: { 'file.txt': 'published/rejected.txt' },
        reject: ['unique-reject-0'],
        replacements: [
          { path: 'file.txt', from: 'unique-from-0', to: 'unique-to-0' },
        ],
        reason: '公開前検査に失敗: published/rejected.txt',
      },
      {
        paths: ['file.txt'],
        outputs: { 'file.txt': 'published/replaced.txt' },
        reject: ['unique-reject-1'],
        replacements: [
          { path: 'file.txt', from: 'unique-from-1', to: 'unique-to-1' },
        ],
        reason: '置換元がありません: published/replaced.txt',
      },
      {
        paths: ['link.txt'],
        outputs: { 'link.txt': 'published/link.txt' },
        reject: ['unique-reject-2'],
        replacements: [
          { path: 'link.txt', from: 'unique-from-2', to: 'unique-to-2' },
        ],
        reason: '指定パスが通常ファイルではありません: published/link.txt',
      },
      {
        paths: [
          'host-ssh.json',
          'deploy/nas-host/install-github-actions-runner.sh',
        ],
        outputs: {
          'deploy/nas-host/install-github-actions-runner.sh':
            'published/runner.sh',
        },
        reject: ['unique-reject-3'],
        replacements: [
          { path: 'host-ssh.json', from: 'unique-from-3', to: 'unique-to-3' },
        ],
        reason: 'runnerの固定SHA-256が一致しません: published/runner.sh',
      },
    ];
    for (const [index, item] of cases.entries()) {
      const registry = join(root, 'registry-' + index + '.json'),
        jobs = join(root, 'jobs-' + index + '.json');
      const { reason, ...definition } = item;
      writeFileSync(
        registry,
        JSON.stringify({
          intake: {
            source: {
              ...definition,
              repo: source,
              ci: 'intake',
              verify: 'intake',
              image: 'example@sha256:' + 'a'.repeat(64),
            },
            ref: 'HEAD',
            publicRepo: destination,
            publicRef: 'HEAD',
          },
        }),
      );
      writeFileSync(
        jobs,
        JSON.stringify({ intake: { entry: 'public-source', registry } }),
      );
      const result = spawnSync(
        'sh',
        [
          resolve('scripts/run-public-source-jobs.sh'),
          '--prepare',
          '--registry',
          jobs,
          '--output',
          join(root, 'output-' + index),
        ],
        {
          encoding: 'utf8',
          env: { ...process.env, PATH: bin + ':' + process.env.PATH },
        },
      );
      check(result, reason, [
        `unique-reject-${index}`,
        `unique-from-${index}`,
        `unique-to-${index}`,
        root,
        registry,
        jobs,
        source,
        destination,
        '/tmp/public-prepare-',
      ]);
    }
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

test('登録方式の失敗理由: 検査名と相対パス', () => {
  failureReasonFixtures((result, reason) => {
    assert.equal(result.status, 1);
    assert.equal(result.stdout, `intake: 失敗（${reason}）\n`);
  });
});

test('登録方式の失敗理由: 機密を出さない', () => {
  failureReasonFixtures((result, _reason, secrets) => {
    assert.equal(result.status, 1);
    const output = String(result.stdout) + String(result.stderr);
    for (const secret of secrets) assert.equal(output.includes(secret), false);
  });
});
