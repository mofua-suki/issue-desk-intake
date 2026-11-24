import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  rmSync,
  copyFileSync,
  symlinkSync,
  existsSync,
  chmodSync,
  statSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
const root = path.resolve(import.meta.dirname, '..');
const cli = path.join(root, 'scripts/run-public-source-jobs.sh');
const image = 'example.invalid/check@sha256:' + '0'.repeat(64);
function fixture() {
  const dir = mkdtempSync(path.join(tmpdir(), 'public-jobs-'));
  const bin = path.join(dir, 'bin');
  mkdirSync(bin);
  const log = path.join(dir, 'calls');
  writeFileSync(
    path.join(bin, 'docker'),
    `#!/bin/sh\nprintf 'docker %s\n' "$*" >> '${log}'\nexit 0\n`,
    { mode: 0o755 },
  );
  const repo = path.join(dir, 'repo');
  mkdirSync(repo);
  const git = (...args: string[]) => {
    const r = spawnSync('git', ['-C', repo, ...args], { encoding: 'utf8' });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout.trim();
  };
  git('init', '-q');
  git(
    '-c',
    'user.name=Test',
    '-c',
    'user.email=test@example.invalid',
    'commit',
    '--allow-empty',
    '-qm',
    'initial',
  );
  const ref = git('rev-parse', 'HEAD');
  const source = {
    repo,
    paths: ['README.md'],
    replacements: [],
    reject: [],
    ci: 'intake',
    verify: 'intake',
    image,
  };
  const registry = path.join(dir, 'registry.json');
  writeFileSync(
    registry,
    JSON.stringify(
      Object.fromEntries(
        ['intake', 'worker', 'third'].map((id) => [
          id,
          { source, ref, publicRepo: repo, publicRef: ref },
        ]),
      ),
    ),
  );
  const jobs = path.join(dir, 'jobs.json');
  const invoke = (data: object, nodeOptions = '') => {
    writeFileSync(jobs, JSON.stringify(data));
    return spawnSync('sh', [cli, '--dry-run', '--registry', jobs], {
      encoding: 'utf8',
      env: {
        ...process.env,
        PATH: bin + ':' + process.env.PATH,
        TEST_CALLS: log,
        NODE_OPTIONS: nodeOptions,
      },
    });
  };
  return { dir, repo, ref, registry, invoke, log };
}

test('登録選択: 固定入口と追加対象を選び任意の実行先を拒否する', () => {
  const f = fixture();
  try {
    const job = { entry: 'public-source', registry: f.registry };
    const good = f.invoke({ intake: job, worker: job, third: job });
    assert.equal(good.status, 0, good.stderr);
    assert.match(good.stdout, /intake: 前提確認済み/);
    assert.match(good.stdout, /worker: 前提確認済み/);
    assert.match(good.stdout, /third: 前提確認済み/);
    for (const bad of [
      { ...job, entry: 'unknown' },
      { ...job, command: 'touch /tmp/forbidden' },
      { ...job, entry: './candidate/script.sh' },
    ]) {
      const result = f.invoke({ intake: bad });
      assert.notEqual(result.status, 0);
      assert.match(result.stdout, /前提不足/);
    }
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('固定イメージ参照を検査しローカル照会だけを行う', () => {
  const f = fixture();
  try {
    const registration = JSON.parse(readFileSync(f.registry, 'utf8'));
    const job = { entry: 'public-source', registry: f.registry };
    const digest = 'a'.repeat(64);
    const accepted = [image, `127.0.0.1:5000/check@sha256:${digest}`];
    const rejected = [
      `127.0.0.2:5000/check@sha256:${digest}`,
      `127.0.0.1:5001/check@sha256:${digest}`,
      'example.invalid/check:latest',
      `example.invalid/check@sha512:${digest}`,
      `example.invalid/check@sha256:${'A'.repeat(64)}`,
      `https://example.invalid/check@sha256:${digest}`,
      `user:password@example.invalid/check@sha256:${digest}`,
    ];
    for (const command of ['gh', 'curl', 'wget'])
      writeFileSync(
        path.join(f.dir, 'bin', command),
        `#!/bin/sh\necho ${command} >> '${f.log}'\nexit 1\n`,
        { mode: 0o755 },
      );
    const preload = path.join(f.dir, 'observe.cjs');
    writeFileSync(
      preload,
      `const fs = require('node:fs');
const read = fs.readFileSync;
fs.readFileSync = function(file, ...args) {
  if (typeof file === 'string' && /credential/i.test(file)) {
    fs.appendFileSync(${JSON.stringify(f.log)}, 'credential read\\n');
    throw Error('credential read');
  }
  return read.call(this, file, ...args);
};\n`,
    );
    writeFileSync(f.log, '');
    for (const ref of [...accepted, ...rejected]) {
      registration.intake.source.image = ref;
      writeFileSync(f.registry, JSON.stringify(registration));
      const result = f.invoke({ intake: job }, `--require=${preload}`);
      assert.equal(result.status === 0, accepted.includes(ref), ref);
      assert.match(
        result.stdout,
        accepted.includes(ref) ? /前提確認済み/ : /前提不足/,
      );
    }
    assert.deepEqual(
      readFileSync(f.log, 'utf8').trim().split('\n'),
      accepted.map((ref) => `docker image inspect ${ref}`),
    );
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('前提不足の分離: 他対象の成功と秘密の非出力を保つ', () => {
  const f = fixture();
  try {
    const secretPath = 'private-registry-token-example';
    const secretRepo = 'private-source-token-example';
    const repo = path.join(f.dir, secretRepo);
    symlinkSync(f.repo, repo);
    const registration = JSON.parse(readFileSync(f.registry, 'utf8'));
    registration.worker.source.repo = repo;
    registration.worker.ref = 'missing-base';
    registration.third.source.image = 'invalid image';
    const registry = path.join(f.dir, `${secretPath}.json`);
    writeFileSync(registry, JSON.stringify(registration));
    const job = { entry: 'public-source', registry };
    const result = f.invoke({
      intake: job,
      worker: job,
      third: job,
      missing: job,
    });
    assert.notEqual(result.status, 0);
    assert.match(result.stdout, /intake: 前提確認済み/);
    for (const target of ['worker', 'third', 'missing'])
      assert.match(result.stdout, new RegExp(`${target}: 前提不足`));
    for (const output of [result.stdout, result.stderr]) {
      assert.ok(!output.includes(secretPath));
      assert.ok(!output.includes(secretRepo));
    }
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('既存入口契約: 試運転は固定された準備・公開入口を選び送信しない', () => {
  const f = fixture();
  try {
    const job = { entry: 'public-source', registry: f.registry };
    // Run the real scheduler in a disposable installation with the fixed entry
    // replaced by a recording stub. Any preparation or send invocation is visible.
    const scripts = path.join(f.dir, 'installation', 'scripts');
    mkdirSync(scripts, { recursive: true });
    for (const name of [
      'run-public-source-jobs.sh',
      'run-public-source-jobs.cjs',
    ])
      copyFileSync(path.join(root, 'scripts', name), path.join(scripts, name));
    const calls = path.join(f.dir, 'entry-calls');
    writeFileSync(calls, '');
    writeFileSync(
      path.join(scripts, 'publish-public-source.sh'),
      `#!/bin/sh\necho invoked >> '${calls}'\nexit 1\n`,
      { mode: 0o755 },
    );
    const jobs = path.join(f.dir, 'contract-jobs.json');
    writeFileSync(jobs, JSON.stringify({ intake: job }));
    const result = spawnSync(
      'sh',
      [
        path.join(scripts, 'run-public-source-jobs.sh'),
        '--dry-run',
        '--registry',
        jobs,
      ],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          PATH: path.join(f.dir, 'bin') + ':' + process.env.PATH,
        },
      },
    );
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /intake: 前提確認済み/);
    assert.equal(readFileSync(calls, 'utf8'), '');
    assert.equal(
      readFileSync(f.log, 'utf8').trim(),
      `docker image inspect ${image}`,
    );
    // A registration without a reviewed CI method cannot pass the existing preparation path.
    const invalid = path.join(f.dir, 'invalid.json');
    writeFileSync(
      invalid,
      JSON.stringify({
        intake: {
          source: {
            repo: f.repo,
            paths: ['README.md'],
            replacements: [],
            reject: [],
            ci: 'arbitrary',
            verify: 'intake',
            image,
          },
          ref: f.ref,
          publicRepo: f.repo,
          publicRef: f.ref,
        },
      }),
    );
    const prepared = spawnSync(
      'sh',
      [
        path.join(root, 'scripts/publish-public-source.sh'),
        '--target',
        'intake',
        '--registry',
        invalid,
        '--output',
        path.join(f.dir, 'candidate'),
      ],
      { encoding: 'utf8' },
    );
    assert.notEqual(prepared.status, 0);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('両対象の準備は前提不足と失敗を分離し安全な前回記録だけを使う', () => {
  const f = fixture();
  try {
    const git = (...args: string[]) => {
      const r = spawnSync('git', ['-C', f.repo, ...args], { encoding: 'utf8' });
      assert.equal(r.status, 0, r.stderr);
      return r.stdout.trim();
    };
    writeFileSync(path.join(f.repo, 'README.md'), 'public fixture content\n');
    git('add', '.');
    git(
      '-c',
      'user.name=Test',
      '-c',
      'user.email=test@example.invalid',
      'commit',
      '-qm',
      'source',
    );
    const ref = git('rev-parse', 'HEAD');
    const registration = JSON.parse(readFileSync(f.registry, 'utf8'));
    for (const id of ['intake', 'worker']) {
      registration[id].ref = ref;
      registration[id].source.ci = id;
      registration[id].source.verify = id;
      registration[id].send = { credentialRef: 'secret-registration-value' };
    }
    writeFileSync(f.registry, JSON.stringify(registration));
    const calls = path.join(f.dir, 'send-calls');
    writeFileSync(
      path.join(f.dir, 'bin', 'gh'),
      `#!/bin/sh\necho called >> '${calls}'\nexit 1\n`,
      { mode: 0o755 },
    );
    const leak = path.join(f.dir, 'credential-read');
    writeFileSync(
      path.join(f.dir, 'bin', 'docker'),
      `#!${process.execPath}\nconst fs=require('node:fs'); if(process.env.GH_TOKEN) fs.writeFileSync(${JSON.stringify(leak)},'leak'); if(process.argv.includes('inspect')) process.stdout.write('null');\n`,
      { mode: 0o755 },
    );
    const jobs = path.join(f.dir, 'prepare-jobs.json');
    const job = { entry: 'public-source', registry: f.registry };
    writeFileSync(jobs, JSON.stringify({ intake: job, worker: job }));
    const run = (name: string, previous?: string) => {
      const out = path.join(f.dir, name);
      const r = spawnSync(
        'sh',
        [
          cli,
          '--prepare',
          '--registry',
          jobs,
          '--output',
          out,
          ...(previous ? ['--previous-dir', previous] : []),
        ],
        {
          encoding: 'utf8',
          env: {
            ...process.env,
            PATH: path.join(f.dir, 'bin') + ':' + process.env.PATH,
            GH_TOKEN: 'synthetic-secret-token',
          },
        },
      );
      for (const secret of [
        'synthetic-secret-token',
        'secret-registration-value',
        'public fixture content',
      ])
        assert.equal((r.stdout + r.stderr).includes(secret), false);
      return { ...r, out };
    };
    // Both targets prepare against the fixed public base.
    const first = run('first');
    assert.equal(first.status, 0, first.stderr + first.stdout);
    assert.equal(statSync(first.out).mode & 0o777, 0o700);
    for (const id of ['intake', 'worker']) {
      assert.equal(
        statSync(path.join(first.out, id + '.tree')).mode & 0o777,
        0o700,
      );
      assert.equal(
        statSync(path.join(first.out, id + '.json')).mode & 0o777,
        0o600,
      );
      assert.match(first.stdout, new RegExp(id + ': prepared'));
      assert.equal(
        readFileSync(path.join(first.out, id + '.tree', 'README.md'), 'utf8'),
        'public fixture content\n',
      );
      const evidence = JSON.parse(
        readFileSync(path.join(first.out, id + '.json'), 'utf8'),
      );
      assert.equal(evidence.status, 'prepared');
      writeFileSync(
        path.join(first.out, id + '.merged.json'),
        JSON.stringify({ ...evidence, status: 'merged' }),
        { mode: 0o600 },
      );
    }
    const prior = readFileSync(path.join(first.out, 'intake.merged.json'));
    const skipped = run('skipped', first.out);
    assert.equal(skipped.status, 0, skipped.stdout);
    assert.match(skipped.stdout, /intake: skipped/);
    assert.match(skipped.stdout, /worker: skipped/);
    assert.deepEqual(
      readFileSync(path.join(first.out, 'intake.merged.json')),
      prior,
    );
    chmodSync(path.join(first.out, 'intake.merged.json'), 0o644);
    const unsafeRecord = run('unsafe-record', first.out);
    assert.equal(unsafeRecord.status, 0, unsafeRecord.stdout);
    assert.match(unsafeRecord.stdout, /intake: (prepared|no-diff)/);
    assert.match(unsafeRecord.stdout, /worker: skipped/);
    chmodSync(path.join(first.out, 'intake.merged.json'), 0o600);
    rmSync(path.join(first.out, 'intake.tree'), { recursive: true });
    symlinkSync(
      path.join(first.out, 'worker.tree'),
      path.join(first.out, 'intake.tree'),
    );
    const unsafeTree = run('unsafe-tree', first.out);
    assert.equal(unsafeTree.status, 0, unsafeTree.stdout);
    assert.match(unsafeTree.stdout, /intake: (prepared|no-diff)/);
    assert.match(unsafeTree.stdout, /worker: skipped/);

    writeFileSync(
      jobs,
      JSON.stringify({ intake: job, worker: { ...job, entry: 'unknown' } }),
    );
    const missing = run('missing-prerequisite');
    assert.notEqual(missing.status, 0);
    assert.match(missing.stdout, /intake: prepared/);
    assert.match(missing.stdout, /worker: 前提不足/);
    assert.equal(existsSync(path.join(missing.out, 'worker.json')), false);
    assert.equal(existsSync(path.join(missing.out, 'worker.tree')), false);
    writeFileSync(jobs, JSON.stringify({ intake: job, worker: job }));
    registration.intake.publicRef = JSON.parse(
      readFileSync(path.join(first.out, 'intake.json'), 'utf8'),
    ).commit;
    registration.worker.publicRef = JSON.parse(
      readFileSync(path.join(first.out, 'worker.json'), 'utf8'),
    ).commit;
    writeFileSync(f.registry, JSON.stringify(registration));
    const same = run('same');
    assert.equal(same.status, 0, same.stdout);
    assert.match(same.stdout, /intake: no-diff/);
    assert.match(same.stdout, /worker: no-diff/);
    registration.intake.publicRef = ref;
    registration.worker.publicRef = ref;
    writeFileSync(path.join(f.repo, 'README.md'), 'new public content\n');
    git('add', '.');
    git(
      '-c',
      'user.name=Test',
      '-c',
      'user.email=test@example.invalid',
      'commit',
      '-qm',
      'new source',
    );
    const newer = git('rev-parse', 'HEAD');
    registration.intake.ref = newer;
    registration.worker.ref = newer;
    writeFileSync(f.registry, JSON.stringify(registration));
    const prepared = run('prepared');
    assert.equal(prepared.status, 0, prepared.stdout);
    assert.match(prepared.stdout, /intake: prepared/);
    assert.match(prepared.stdout, /worker: prepared/);
    assert.equal(
      JSON.parse(readFileSync(path.join(prepared.out, 'intake.json'), 'utf8'))
        .publicBase,
      ref,
    );
    // A generator failure for one target must not prevent the other from finishing.
    registration.intake.source.paths = ['missing-file'];
    writeFileSync(f.registry, JSON.stringify(registration));
    const failed = run('failed');
    assert.notEqual(failed.status, 0);
    assert.match(failed.stdout, /intake: 失敗/);
    assert.match(failed.stdout, /worker: prepared/);
    assert.equal(existsSync(calls), false);
    assert.equal(existsSync(leak), false);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('準備先の親と保存権限を検査し設定失敗時は保存しない', () => {
  const f = fixture();
  const work = mkdtempSync(path.join(root, '.public-jobs-test-'));
  try {
    const scripts = path.join(work, 'scripts');
    mkdirSync(scripts);
    for (const name of [
      'run-public-source-jobs.sh',
      'run-public-source-jobs.cjs',
    ])
      copyFileSync(path.join(root, 'scripts', name), path.join(scripts, name));
    writeFileSync(
      path.join(scripts, 'publish-public-source.sh'),
      `#!/bin/sh
while [ "$1" != "--output" ]; do shift; done
mkdir "$2"
chmod 777 "$2"
if [ -f "${work}/collision" ]; then printf existing > "${work}/out/intake.json"; fi
printf '{"target":"intake","status":"prepared"}'
`,
      { mode: 0o755 },
    );
    const jobs = path.join(work, 'jobs.json');
    writeFileSync(
      jobs,
      JSON.stringify({
        intake: { entry: 'public-source', registry: f.registry },
      }),
    );
    const preload = path.join(work, 'fault.cjs');
    writeFileSync(
      preload,
      `const fs = require('node:fs');
const originalLstat = fs.lstatSync;
fs.lstatSync = function(p, ...args) { const s = originalLstat.call(this, p, ...args); if (process.env.FAULT === 'owner' && p === ${JSON.stringify(work)}) s.uid = process.getuid() + 1; return s; };
const chmod = fs.chmodSync;
fs.chmodSync = function(p, mode) { if ((process.env.FAULT === 'output' && p === ${JSON.stringify(path.join(work, 'out'))}) || (process.env.FAULT === 'tree' && String(p).endsWith('.tree'))) throw Error('synthetic chmod failure'); return chmod.call(this, p, mode); };
const fchmod = fs.fchmodSync;
fs.fchmodSync = function(fd, mode) { if (process.env.FAULT === 'record') throw Error('synthetic fchmod failure'); return fchmod.call(this, fd, mode); };
const mkdir = fs.mkdirSync;
fs.mkdirSync = function(p, options) { const result = mkdir.call(this, p, options); if (process.env.FAULT === 'inherited' && p === ${JSON.stringify(path.join(work, 'out'))}) chmod.call(this, p, 0o777); return result; };
const open = fs.openSync;
fs.openSync = function(p, ...args) { const fd = open.call(this, p, ...args); if (process.env.FAULT === 'inherited' && String(p).endsWith('intake.json')) fchmod.call(this, fd, 0o666); return fd; };
const write = fs.writeFileSync;
fs.writeFileSync = function(p, ...args) { if (process.env.FAULT === 'inherited' && typeof p === 'number' && (fs.fstatSync(p).mode & 0o777) !== 0o600) throw Error('record not private before write'); return write.call(this, p, ...args); };
`,
    );
    const out = path.join(work, 'out');
    const run = (fault = '', destination = out) =>
      spawnSync(
        'sh',
        [
          path.join(scripts, 'run-public-source-jobs.sh'),
          '--prepare',
          '--registry',
          jobs,
          '--output',
          destination,
        ],
        {
          encoding: 'utf8',
          env: {
            ...process.env,
            PATH: path.join(f.dir, 'bin') + ':' + process.env.PATH,
            NODE_OPTIONS: '--require=' + preload,
            FAULT: fault,
          },
        },
      );
    chmodSync(work, 0o755);
    assert.notEqual(run().status, 0);
    assert.equal(statSync(work).mode & 0o777, 0o755);
    assert.equal(existsSync(out), false);
    chmodSync(work, 0o700);
    assert.notEqual(run('owner').status, 0);
    const link = path.join(work, 'link');
    symlinkSync(work, link);
    assert.notEqual(run('', path.join(link, 'out')).status, 0);
    mkdirSync(out, { mode: 0o750 });
    writeFileSync(path.join(out, 'keep'), 'unchanged');
    assert.notEqual(run().status, 0);
    assert.equal(statSync(out).mode & 0o777, 0o750);
    assert.equal(readFileSync(path.join(out, 'keep'), 'utf8'), 'unchanged');
    rmSync(out, { recursive: true });
    const inherited = run('inherited');
    assert.equal(inherited.status, 0, inherited.stdout + inherited.stderr);
    assert.equal(statSync(out).mode & 0o777, 0o700);
    assert.equal(statSync(path.join(out, 'intake.tree')).mode & 0o777, 0o700);
    assert.equal(statSync(path.join(out, 'intake.json')).mode & 0o777, 0o600);
    rmSync(out, { recursive: true });
    for (const fault of ['output', 'tree', 'record']) {
      assert.notEqual(run(fault).status, 0, fault);
      assert.equal(existsSync(path.join(out, 'intake.json')), false, fault);
      assert.equal(existsSync(path.join(out, 'intake.tree')), false, fault);
      rmSync(out, { recursive: true, force: true });
    }
    writeFileSync(path.join(work, 'collision'), '');
    assert.notEqual(run().status, 0);
    assert.equal(
      readFileSync(path.join(out, 'intake.json'), 'utf8'),
      'existing',
    );
    assert.equal(existsSync(path.join(out, 'intake.tree')), false);
  } finally {
    rmSync(work, { recursive: true, force: true });
    rmSync(f.dir, { recursive: true, force: true });
  }
});
