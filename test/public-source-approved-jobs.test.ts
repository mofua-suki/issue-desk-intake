import { test, type TestContext } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { createRequire } from 'node:module';
import { createHash } from 'node:crypto';

const require = createRequire(import.meta.url);
const { run } = require('../scripts/run-approved-public-source-jobs.cjs');
function fixture(t: { after: (fn: () => void) => void }) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'approved-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const file = (...parts: string[]) => path.join(root, ...parts);
  const write = (name: string, value: string) =>
    fs.writeFileSync(file(name), value, { mode: 0o600 });
  for (const dir of ['scripts', 'prepared', 'confirmations'])
    fs.mkdirSync(file(dir), { mode: 0o700 });
  const targets = ['alpha', 'beta', 'gamma'];
  const jobs = Object.fromEntries(
    targets.map((target) => [
      target,
      { entry: 'public-source', registry: file('registry') },
    ]),
  );
  write('jobs', JSON.stringify(jobs));
  write(
    'registry',
    JSON.stringify(Object.fromEntries(targets.map((target) => [target, {}]))),
  );
  write('issuer', '{}');
  write(
    'scripts/publish-public-source.sh',
    `#!/bin/sh\nprintf '{"status":"preview","target":"%s","confirmation":"${'a'.repeat(64)}"}\\n' "$3"\n`,
  );
  for (const target of targets) {
    write(
      `prepared/${target}.json`,
      JSON.stringify({ target, status: 'prepared' }),
    );
    fs.mkdirSync(file('prepared', `${target}.tree`), { mode: 0o700 });
    write(`confirmations/${target}`, 'a'.repeat(64));
  }
  const args = [
    '--registry',
    file('jobs'),
    '--prepared-dir',
    file('prepared'),
    '--confirmations-dir',
    file('confirmations'),
    '--issuer-registry',
    file('issuer'),
  ];
  return { root, file, write, args };
}

test('approved targets are isolated, failures continue, and waiting can resume', (t) => {
  const { root, file, write, args } = fixture(t);
  write(
    'scripts/issue-public-source-github-app.cjs',
    `process.stdout.write('secret-' + process.argv[3]);`,
  );
  write(
    'scripts/run-public-source-jobs.cjs',
    `
    const fs = require('node:fs'), target = process.argv[6];
    const input = fs.readFileSync(0, 'utf8');
    if (input !== 'secret-' + target || process.argv.join(' ').includes('secret-') || JSON.stringify(process.env).includes('secret-')) process.exit(1);
    fs.appendFileSync(${JSON.stringify(file('calls'))}, target + '\\n');
    if (target === 'beta' && !fs.existsSync(${JSON.stringify(file('resume'))})) { console.log('waiting'); process.exit(2); }
    console.log('merged');
  `,
  );
  const invoke = () =>
    spawnSync(
      process.execPath,
      [
        '-e',
        `const {run}=require(${JSON.stringify(require.resolve('../scripts/run-approved-public-source-jobs.cjs'))}); process.exitCode=run(${JSON.stringify(args)},${JSON.stringify(root)});`,
      ],
      { encoding: 'utf8' },
    );
  write('confirmations/alpha', 'missing');
  let result = invoke();
  assert.equal(result.status, 1);
  assert.equal(result.stdout, 'alpha: failed\nbeta: waiting\ngamma: merged\n');
  assert.equal(result.stderr, '');
  assert.equal(fs.readFileSync(file('calls'), 'utf8'), 'beta\ngamma\n');
  write('confirmations/alpha', 'a'.repeat(64));
  write('resume', '');
  result = invoke();
  assert.equal(result.status, 0);
  assert.equal(result.stdout, 'alpha: merged\nbeta: merged\ngamma: merged\n');
  assert.equal(result.stderr, '');
  assert.equal(
    fs.readFileSync(file('calls'), 'utf8'),
    'beta\ngamma\nalpha\nbeta\ngamma\n',
  );
  for (const name of fs.readdirSync(root, { recursive: true })) {
    const p = file(String(name));
    if (fs.statSync(p).isFile() && !p.startsWith(file('scripts')))
      assert.equal(fs.readFileSync(p, 'utf8').includes('secret-'), false);
  }
});

test('unsafe inputs and changed previews never send', (t) => {
  const { root, file, write, args } = fixture(t);
  for (const name of [
    'run-public-source-jobs.cjs',
    'run-public-source-jobs-send.cjs',
  ])
    fs.copyFileSync(
      path.join(process.cwd(), 'scripts', name),
      file('scripts', name),
    );
  write(
    'scripts/issue-public-source-github-app.cjs',
    `require('node:fs').appendFileSync(${JSON.stringify(file('issued'))}, process.argv[3] + '\\n'); process.stdout.write('secret');`,
  );
  write(
    'scripts/publish-public-source.sh',
    `#!/bin/sh\ncase " $* " in *' --confirm '*) touch '${root}/sent';; esac\nprintf '%s\\n' '{"status":"preview","target":"alpha","confirmation":"${'b'.repeat(64)}"}'\n`,
  );
  write('prepared/alpha.json', '{"target":"other","status":"prepared"}');
  fs.chmodSync(file('prepared', 'beta.json'), 0o644);
  fs.unlinkSync(file('confirmations', 'gamma'));
  fs.symlinkSync(file('issuer'), file('confirmations', 'gamma'));
  assert.equal(run(args, root), 1);
  assert.equal(fs.existsSync(file('issued')), false);
  write('prepared/alpha.json', '{"target":"alpha","status":"prepared"}');
  assert.equal(run(args, root), 1);
  assert.equal(fs.existsSync(file('issued')), false);
  assert.equal(fs.existsSync(file('sent')), false);
});

test('valid no-diff and skipped targets bypass issuance while candidates proceed', (t) => {
  const { root, file, write, args } = fixture(t);
  const emptyTree =
    'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855';
  const evidence = (target: string, status: string) => ({
    target,
    status,
    inputSha: 'a'.repeat(40),
    configSha256: 'b'.repeat(64),
    generatorSha256: 'c'.repeat(64),
    ciSha256: 'd'.repeat(64),
    treeSha256: emptyTree,
    ...(status === 'no-diff'
      ? { publicTree: 'e'.repeat(40), generatedTree: 'e'.repeat(40) }
      : {}),
  });
  write(
    'scripts/issue-public-source-github-app.cjs',
    `require('node:fs').appendFileSync(${JSON.stringify(file('issued'))}, process.argv[3] + '\\n'); process.stdout.write('token');`,
  );
  write(
    'scripts/run-public-source-jobs.cjs',
    `require('node:fs').appendFileSync(${JSON.stringify(file('sent'))}, process.argv[6] + '\\n'); console.log('merged');`,
  );
  const invoke = () =>
    spawnSync(
      process.execPath,
      [
        '-e',
        `const {run}=require(${JSON.stringify(require.resolve('../scripts/run-approved-public-source-jobs.cjs'))}); process.exitCode=run(${JSON.stringify(args)},${JSON.stringify(root)});`,
      ],
      { encoding: 'utf8' },
    );
  write('prepared/alpha.json', JSON.stringify(evidence('alpha', 'no-diff')));
  write('prepared/beta.json', JSON.stringify(evidence('beta', 'skipped')));
  fs.rmSync(file('prepared', 'beta.tree'), { recursive: true });
  fs.unlinkSync(file('confirmations', 'alpha'));
  fs.unlinkSync(file('confirmations', 'beta'));
  write('prepared/alpha.merged.json', 'original');
  write('prepared/beta.merged.json', 'original');
  write(
    'jobs',
    JSON.stringify({
      alpha: { entry: 'public-source', registry: file('registry') },
      beta: { entry: 'public-source', registry: file('registry') },
    }),
  );
  let result = invoke();
  assert.equal(result.status, 0);
  assert.equal(result.stdout, 'alpha: no-diff\nbeta: skipped\n');
  assert.equal(fs.existsSync(file('issued')), false);
  assert.equal(fs.existsSync(file('sent')), false);
  assert.equal(
    fs.readFileSync(file('prepared', 'alpha.merged.json'), 'utf8'),
    'original',
  );
  assert.equal(
    fs.readFileSync(file('prepared', 'beta.merged.json'), 'utf8'),
    'original',
  );
  write(
    'jobs',
    JSON.stringify(
      Object.fromEntries(
        ['alpha', 'beta', 'gamma'].map((target) => [
          target,
          { entry: 'public-source', registry: file('registry') },
        ]),
      ),
    ),
  );
  result = invoke();
  assert.equal(result.status, 0);
  assert.equal(result.stdout, 'alpha: no-diff\nbeta: skipped\ngamma: merged\n');
  assert.equal(fs.readFileSync(file('issued'), 'utf8'), 'gamma\n');
  assert.equal(fs.readFileSync(file('sent'), 'utf8'), 'gamma\n');
  for (const bad of [
    { ...evidence('alpha', 'no-diff'), generatedTree: 'f'.repeat(40) },
    { ...evidence('other', 'no-diff') },
    { ...evidence('alpha', 'no-diff'), treeSha256: 'f'.repeat(64) },
    { ...evidence('alpha', 'no-diff'), commit: 'a'.repeat(40) },
    { ...evidence('alpha', 'skipped'), ciSha256: undefined },
  ]) {
    write('prepared/alpha.json', JSON.stringify(bad));
    assert.equal(run(args, root), 1);
  }
  write('prepared/alpha.json', JSON.stringify(evidence('alpha', 'no-diff')));
  write('prepared/alpha.tree/changed', 'corrupt');
  assert.equal(run(args, root), 1);
  fs.unlinkSync(file('prepared', 'alpha.tree', 'changed'));
  write('prepared/alpha.json', '{');
  assert.equal(run(args, root), 1);
  fs.unlinkSync(file('prepared', 'alpha.json'));
  assert.equal(run(args, root), 1);
});

test('registered release scope gates issuance before sending', (t) => {
  const { root, file, write, args } = fixture(t);
  const repo = file('repo');
  fs.mkdirSync(repo);
  const git = (...argv: string[]) => {
    const r = spawnSync('git', ['-C', repo, ...argv], { encoding: 'utf8' });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout.trim();
  };
  git('init', '-q');
  git('config', 'user.name', 'Fixture');
  git('config', 'user.email', 'fixture@example.invalid');
  fs.writeFileSync(path.join(repo, 'README.md'), 'old\n');
  fs.writeFileSync(path.join(repo, 'other.md'), 'old\n');
  fs.mkdirSync(path.join(repo, '.github', 'workflows'), { recursive: true });
  fs.writeFileSync(
    path.join(repo, '.github', 'workflows', 'verify.yml'),
    'old\n',
  );
  fs.writeFileSync(path.join(repo, '.github', 'CODEOWNERS'), 'old\n');
  git('add', '.');
  git('commit', '-qm', 'base');
  const base = git('rev-parse', 'HEAD');
  git('branch', '-M', 'main');
  const entry = {
    source: {
      repo,
      paths: ['README.md', 'other.md'],
      ci: 'intake',
      verify: 'intake',
    },
    ref: 'refs/heads/main',
    publicRepo: repo,
    publicRef: 'refs/heads/main',
    send: { repository: 'example/project', base: 'refs/heads/main' },
  };
  const policy = {
    target: 'alpha',
    repository: entry.send.repository,
    base: entry.send.base,
    publicRepo: repo,
    publicRef: entry.publicRef,
    sourceRepo: repo,
    sourceRef: entry.ref,
    preparedRoot: root,
    configSha256: createHash('sha256')
      .update(
        JSON.stringify({
          target: 'alpha',
          ...entry,
          source: { ...entry.source, sha: undefined },
        }),
      )
      .digest('hex'),
    generatorSha256: 'b'.repeat(64),
    ciSha256: 'c'.repeat(64),
    ci: 'intake',
    verify: 'intake',
    paths: ['README.md'],
  };
  write('registry', JSON.stringify({ alpha: entry }));
  write('policy', JSON.stringify(policy));
  write(
    'jobs',
    JSON.stringify({
      alpha: {
        entry: 'public-source',
        registry: file('registry'),
        policy: file('policy'),
      },
    }),
  );
  write(
    'scripts/issue-public-source-github-app.cjs',
    `require('node:fs').appendFileSync(${JSON.stringify(file('events'))}, 'issue\\n'); process.stdout.write('token');`,
  );
  for (const name of [
    'run-public-source-jobs.cjs',
    'run-public-source-jobs-send.cjs',
  ])
    fs.copyFileSync(path.resolve('scripts', name), file('scripts', name));
  write(
    'scripts/publish-public-source.sh',
    `#!/bin/sh\ncase " $* " in *' --confirm '*) cat >/dev/null; printf 'send\\n' >> '${file('events')}'; printf '{"status":"merged"}\\n'; exit 0;; esac\nnode - "$@" <<'NODE'\nconst fs = require('fs');\nconst args = process.argv.slice(2);\nfs.appendFileSync(${JSON.stringify(file('events'))}, 'preview\\n');\nif (fs.existsSync(${JSON.stringify(file('reject'))})) process.exit(1);\nconst e = JSON.parse(fs.readFileSync(args[args.indexOf('--candidate')+1]));\nif (fs.existsSync(${JSON.stringify(file('swap'))})) fs.writeFileSync(args[args.indexOf('--candidate')+1], JSON.stringify({...e, commit:'0'.repeat(40)}));\nconst changed = fs.existsSync(${JSON.stringify(file('late'))}) && fs.readFileSync(${JSON.stringify(file('events'))}, 'utf8').includes('issue');\nconsole.log(JSON.stringify({status:'preview', target:'alpha', repository:'example/project', head:e.commit, base:e.publicBase, tree:e.generatedTree, confirmation:(changed ? 'e' : 'd').repeat(64)}));\nNODE\n`,
  );
  const candidate = () => {
    const tree = git('write-tree');
    const r = spawnSync(
      'git',
      [
        '-C',
        repo,
        '-c',
        'user.name=Fixture',
        '-c',
        'user.email=fixture@example.invalid',
        'commit-tree',
        tree,
        '-p',
        base,
      ],
      { input: 'candidate\n', encoding: 'utf8' },
    );
    assert.equal(r.status, 0, r.stderr);
    const e = {
      target: 'alpha',
      status: 'prepared',
      inputSha: base,
      publicBase: base,
      commit: r.stdout.trim(),
      generatedTree: tree,
      configSha256: policy.configSha256,
      generatorSha256: policy.generatorSha256,
      ciSha256: policy.ciSha256,
    };
    write('prepared/alpha.json', JSON.stringify(e));
  };
  const check = (expected: string) => {
    fs.rmSync(file('events'), { force: true });
    const status = run(args, root);
    const events = fs.existsSync(file('events'))
      ? fs.readFileSync(file('events'), 'utf8')
      : '';
    assert.equal(events, expected);
    assert.equal(status, expected.endsWith('send\n') ? 0 : 1);
  };
  fs.writeFileSync(path.join(repo, 'README.md'), 'approved\n');
  git('add', '.');
  candidate();
  check('preview\nissue\npreview\nsend\n');
  git('config', 'remote.fixture.promisor', 'true');
  check('');
  git('config', '--unset', 'remote.fixture.promisor');
  write('late', '');
  check('preview\nissue\npreview\n');
  fs.unlinkSync(file('late'));
  write('reject', '');
  check('preview\n');
  fs.unlinkSync(file('reject'));
  write('swap', '');
  check('preview\n');
  fs.unlinkSync(file('swap'));
  candidate();
  fs.chmodSync(file('policy'), 0o644);
  check('');
  fs.chmodSync(file('policy'), 0o600);
  write('policy', JSON.stringify({ ...policy, configSha256: '0'.repeat(64) }));
  check('');
  fs.unlinkSync(file('policy'));
  check('');
  write('policy', JSON.stringify(policy));
  fs.writeFileSync(path.join(repo, 'other.md'), 'unregistered\n');
  git('add', '.');
  candidate();
  check('');
  git('reset', '-q', '--hard', base);
  fs.rmSync(path.join(repo, 'README.md'));
  git('add', '-A');
  candidate();
  check('');
  git('reset', '-q', '--hard', base);
  fs.chmodSync(path.join(repo, 'README.md'), 0o755);
  git('add', '.');
  candidate();
  check('');
  git('reset', '-q', '--hard', base);
  fs.unlinkSync(path.join(repo, 'README.md'));
  fs.symlinkSync('other.md', path.join(repo, 'README.md'));
  git('add', '.');
  candidate();
  check('');
  git('reset', '-q', '--hard', base);
  fs.writeFileSync(
    path.join(repo, '.github', 'workflows', 'verify.yml'),
    'changed\n',
  );
  git('add', '.');
  candidate();
  check('');
  git('reset', '-q', '--hard', base);
  fs.writeFileSync(path.join(repo, 'new.md'), 'new\n');
  git('add', '.');
  candidate();
  check('');
  git('reset', '-q', '--hard', base);
  fs.writeFileSync(path.join(repo, 'README.md'), Buffer.from([0, 1]));
  git('add', '.');
  candidate();
  check('');
  git('reset', '-q', '--hard', base);
  fs.writeFileSync(path.join(repo, '.github', 'CODEOWNERS'), 'changed\n');
  git('add', '.');
  candidate();
  check('');
  git('reset', '-q', '--hard', base);
  fs.writeFileSync(path.join(repo, 'README.md'), 'approved\n');
  git('add', '.');
  candidate();
  git(
    'update-ref',
    'refs/heads/main',
    JSON.parse(fs.readFileSync(file('prepared', 'alpha.json'), 'utf8')).commit,
  );
  check('');
});

function regenerationFixture(t: TestContext, addedPaths: string[] = []) {
  const { root, file, write, args } = fixture(t);
  const source = file('source'),
    publicRepo = file('public'),
    bin = file('bin'),
    state = file('state');
  for (const dir of [source, publicRepo, bin, state])
    fs.mkdirSync(dir, { mode: 0o700 });
  const git = (repo: string, ...argv: string[]) => {
    const result = spawnSync('git', ['-C', repo, ...argv], {
      encoding: 'utf8',
    });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim();
  };
  for (const repo of [source, publicRepo]) {
    git(repo, 'init', '-q');
    git(repo, 'config', 'user.name', 'Fixture');
    git(repo, 'config', 'user.email', 'fixture@example.invalid');
    fs.writeFileSync(path.join(repo, 'README.md'), 'old\n');
    if (repo === publicRepo) {
      fs.mkdirSync(path.join(repo, '.github', 'workflows'), {
        recursive: true,
      });
      fs.copyFileSync(
        path.resolve('deploy/public/intake-verify.yml'),
        path.join(repo, '.github', 'workflows', 'verify.yml'),
      );
      fs.copyFileSync(
        path.resolve('deploy/public/LICENSE'),
        path.join(repo, 'LICENSE'),
      );
    }
    git(repo, 'add', '.');
    git(repo, 'commit', '-qm', 'base');
    git(repo, 'branch', '-M', 'main');
  }
  fs.writeFileSync(path.join(source, 'README.md'), 'approved\n');
  for (const name of addedPaths)
    fs.writeFileSync(path.join(source, name), 'added\n');
  git(source, 'add', '.');
  git(source, 'commit', '-qm', 'change');
  write(
    'bin/docker',
    '#!/bin/sh\ncase " $* " in *" inspect "*) printf null;; esac\n',
  );
  fs.chmodSync(file('bin', 'docker'), 0o700);
  const entry = {
    source: {
      repo: source,
      paths: ['README.md', ...addedPaths],
      replacements: [],
      reject: ['FORBIDDEN_FIXTURE'],
      ci: 'intake',
      verify: 'intake',
      image: 'example@sha256:' + 'a'.repeat(64),
    },
    ref: 'refs/heads/main',
    publicRepo,
    publicRef: 'refs/heads/main',
    send: {
      repository: 'example/project',
      base: 'refs/heads/main',
      credentialRef: 'fixture',
      stateDir: state,
    },
  };
  write('registry', JSON.stringify({ alpha: entry }));
  const env = { ...process.env, PATH: bin + ':' + process.env.PATH };
  const saveEvidence = (evidence: object) =>
    write('prepared/alpha.json', JSON.stringify(evidence));
  const prepare = () => {
    fs.rmSync(file('prepared', 'alpha.tree'), { recursive: true, force: true });
    const prepared = spawnSync(
      'sh',
      [
        file('scripts', 'publish-public-source.sh'),
        '--target',
        'alpha',
        '--registry',
        file('registry'),
        '--output',
        file('prepared', 'alpha.tree'),
      ],
      { env, encoding: 'utf8' },
    );
    assert.equal(prepared.status, 0, prepared.stderr);
    fs.chmodSync(file('prepared', 'alpha.tree'), 0o700);
    const evidence = JSON.parse(prepared.stdout);
    assert.equal(evidence.status, 'prepared');
    saveEvidence(evidence);
    return evidence;
  };
  const policyFor = (evidence: Record<string, string>) => ({
    target: 'alpha',
    repository: entry.send.repository,
    base: entry.send.base,
    publicRepo,
    publicRef: entry.publicRef,
    sourceRepo: source,
    sourceRef: entry.ref,
    preparedRoot: root,
    configSha256: evidence.configSha256,
    generatorSha256: evidence.generatorSha256,
    ciSha256: evidence.ciSha256,
    ci: 'intake',
    verify: 'intake',
    paths: ['README.md'],
  });
  const savePolicy = (policy: object) =>
    write('policy', JSON.stringify(policy));
  write(
    'jobs',
    JSON.stringify({
      alpha: {
        entry: 'public-source',
        registry: file('registry'),
        policy: file('policy'),
      },
    }),
  );
  for (const name of [
    'publish-public-source.sh',
    'build-public-source.sh',
    'public-source-send.cjs',
    'public-source-credentials.cjs',
    'public-source-merge.cjs',
    'public-source-ci.cjs',
    'public-source-verifier.mts',
  ])
    fs.copyFileSync(path.resolve('scripts', name), file('scripts', name));
  fs.mkdirSync(file('deploy', 'public'), { recursive: true });
  for (const name of ['LICENSE', 'intake-verify.yml'])
    fs.copyFileSync(
      path.resolve('deploy/public', name),
      file('deploy', 'public', name),
    );
  write(
    'scripts/issue-public-source-github-app.cjs',
    `require('node:fs').appendFileSync(${JSON.stringify(file('events'))}, 'issue\\n'); process.stdout.write('token');`,
  );
  write(
    'scripts/run-public-source-jobs.cjs',
    `require('node:fs').appendFileSync(${JSON.stringify(file('events'))}, 'send\\n'); console.log('merged');`,
  );
  const invoke = () =>
    spawnSync(
      process.execPath,
      [
        '-e',
        `const {run}=require(${JSON.stringify(require.resolve('../scripts/run-approved-public-source-jobs.cjs'))}); process.exitCode=run(${JSON.stringify(args)},${JSON.stringify(root)});`,
      ],
      { env, encoding: 'utf8' },
    );
  return {
    file,
    write,
    git,
    source,
    publicRepo,
    env,
    invoke,
    prepare,
    saveEvidence,
    policyFor,
    savePolicy,
  };
}

test('real regeneration rejects forbidden data before app issuance', (t) => {
  const {
    file,
    git,
    source,
    invoke,
    prepare,
    saveEvidence,
    policyFor,
    savePolicy,
  } = regenerationFixture(t);
  const evidence = prepare();
  savePolicy(policyFor(evidence));
  let result = invoke();
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(fs.readFileSync(file('events'), 'utf8'), 'issue\nsend\n');
  fs.unlinkSync(file('events'));
  fs.writeFileSync(path.join(source, 'README.md'), 'FORBIDDEN_FIXTURE\n');
  git(source, 'add', '.');
  git(source, 'commit', '-qm', 'forbidden');
  saveEvidence({ ...evidence, inputSha: git(source, 'rev-parse', 'HEAD') });
  result = invoke();
  assert.equal(result.status, 1);
  assert.equal(fs.existsSync(file('events')), false);
});

test('reviewed initial candidate policy gates issuance and falls back after the base advances', (t) => {
  const {
    file,
    write,
    git,
    source,
    publicRepo,
    env,
    invoke,
    prepare,
    saveEvidence,
    policyFor,
    savePolicy,
  } = regenerationFixture(t, ['new.md']);
  const reviewed = prepare();
  const { publicBase, inputSha, generatedTree } = reviewed;
  assert.equal(
    git(
      publicRepo,
      'diff',
      '--name-status',
      '--no-renames',
      publicBase,
      reviewed.commit,
    ),
    'M\tREADME.md\nA\tnew.md',
  );
  const initialCandidate = { publicBase, inputSha, generatedTree };
  const policy = { ...policyFor(reviewed), initialCandidate };
  savePolicy(policy);
  const check = (accepted: boolean, reason: string) => {
    write('events', '');
    const result = invoke();
    assert.equal(
      result.status,
      accepted ? 0 : 1,
      reason + '\n' + result.stdout + result.stderr,
    );
    assert.equal(
      result.stdout,
      accepted ? 'alpha: merged\n' : 'alpha: failed\n',
      reason,
    );
    assert.equal(
      fs.readFileSync(file('events'), 'utf8'),
      accepted ? 'issue\nsend\n' : '',
      reason,
    );
  };
  check(true, 'reviewed tree modifies README.md and adds new.md');

  const newCommit = (message: string) => {
    const result = spawnSync(
      'git',
      [
        '-C',
        publicRepo,
        'commit-tree',
        generatedTree,
        '-p',
        publicBase,
        '-m',
        message,
      ],
      {
        env: {
          ...env,
          GIT_AUTHOR_NAME: 'Public Source',
          GIT_AUTHOR_EMAIL: 'public-source@example.invalid',
          GIT_AUTHOR_DATE: '2000-01-02T03:04:05Z',
          GIT_COMMITTER_NAME: 'Public Source',
          GIT_COMMITTER_EMAIL: 'public-source@example.invalid',
          GIT_COMMITTER_DATE: '2000-01-02T03:04:05Z',
        },
        encoding: 'utf8',
      },
    );
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim();
  };
  const reCreated = { ...reviewed, commit: newCommit('Prepare public source') };
  assert.notEqual(reCreated.commit, reviewed.commit);
  saveEvidence(reCreated);
  check(true, 'commit dates alone changed');
  saveEvidence({ ...reviewed, commit: newCommit('Different message') });
  check(false, 'same tree with different commit metadata');
  saveEvidence({ ...reviewed, commit: publicBase });
  check(false, 'claimed tree differs from the actual commit tree');
  git(
    publicRepo,
    'tag',
    '-a',
    '-m',
    'candidate alias',
    'reviewed-tag',
    reviewed.commit,
  );
  saveEvidence({
    ...reviewed,
    commit: git(publicRepo, 'rev-parse', 'refs/tags/reviewed-tag'),
  });
  check(false, 'a Git tag cannot replace the candidate commit');
  saveEvidence(reviewed);

  for (const key of ['configSha256', 'generatorSha256', 'ciSha256']) {
    savePolicy({ ...policy, [key]: 'f'.repeat(64) });
    check(false, `different ${key}`);
  }
  for (const key of ['ci', 'verify']) {
    savePolicy({ ...policy, [key]: 'worker' });
    check(false, `different ${key} mode`);
  }
  savePolicy({
    ...policy,
    initialCandidate: {
      ...initialCandidate,
      generatedTree: git(publicRepo, 'rev-parse', publicBase + '^{tree}'),
    },
  });
  check(false, 'different reviewed Git tree');
  savePolicy({
    ...policy,
    initialCandidate: { ...initialCandidate, commit: reviewed.commit },
  });
  check(false, 'unexpected field in the initial candidate policy');
  savePolicy(policy);

  git(source, 'commit', '--allow-empty', '-qm', 'different input');
  const differentInput = prepare();
  assert.notEqual(differentInput.inputSha, inputSha);
  assert.equal(differentInput.generatedTree, generatedTree);
  check(false, 'different input SHA with the same tree');
  git(source, 'reset', '--hard', inputSha);
  git(publicRepo, 'commit', '--allow-empty', '-qm', 'different public base');
  const differentBase = prepare();
  assert.notEqual(differentBase.publicBase, publicBase);
  assert.equal(differentBase.generatedTree, generatedTree);
  check(false, 'different public base with the same tree');
  git(publicRepo, 'reset', '--hard', publicBase);
  saveEvidence(reviewed);

  const docker = fs.readFileSync(file('bin', 'docker'), 'utf8');
  write('replacement.json', JSON.stringify(reCreated));
  write(
    'bin/docker',
    docker +
      `cp '${file('replacement.json')}' '${file('prepared', 'alpha.json')}'\n`,
  );
  check(false, 'candidate replaced during the real preview');
  assert.equal(
    JSON.parse(fs.readFileSync(file('prepared', 'alpha.json'), 'utf8')).commit,
    reCreated.commit,
  );
  write('bin/docker', docker);

  git(publicRepo, 'merge', '--no-ff', '--no-edit', reviewed.commit);
  const mergedBase = git(publicRepo, 'rev-parse', 'HEAD');
  fs.writeFileSync(file('source', 'README.md'), 'after merge\n');
  git(source, 'commit', '-qam', 'allowed update');
  prepare();
  check(true, 'allowed existing file changes on the advanced base');

  git(publicRepo, 'rm', 'new.md');
  git(publicRepo, 'commit', '-qm', 'base without the new file');
  prepare();
  check(false, 'unknown addition on the advanced base');
  git(publicRepo, 'reset', '--hard', mergedBase);
  fs.writeFileSync(
    file('public', '.github', 'workflows', 'verify.yml'),
    'different workflow\n',
  );
  git(publicRepo, 'commit', '-qam', 'base with a different workflow');
  prepare();
  check(false, 'workflow change on the advanced base');
});
