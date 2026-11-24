import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

const root = path.resolve(import.meta.dirname, '..');
// prettier-ignore
function fixture() {
  const dir = mkdtempSync(path.join(tmpdir(), 'preparation-timer-'));
  const scripts = path.join(dir, 'install/scripts');
  const nas = path.join(dir, 'install/deploy/nas');
  const bin = path.join(dir, 'bin');
  const runs = path.join(dir, 'runs');
  for (const p of [scripts, nas, bin, runs])
    mkdirSync(p, { recursive: true, mode: 0o700 });
  for (const name of ['run-public-source-jobs.sh', 'run-public-source-jobs.cjs', 'scan-public-source-candidates.cjs'])
    copyFileSync(path.join(root, 'scripts', name), path.join(scripts, name));
  const wrapper = path.join(nas, 'run-public-source-preparation.sh');
  copyFileSync(path.join(root, 'deploy/nas/run-public-source-preparation.sh'), wrapper);
  const log = path.join(dir, 'calls');
  const release = path.join(dir, 'release');
  writeFileSync(
    path.join(scripts, 'publish-public-source.sh'),
    `#!/bin/sh
printf '%s\n' "$*" >> '${log}'
[ -z "\${GH_TOKEN:-}" ] || exit 9
while [ -f '${dir}/block' ] && [ ! -f '${release}' ]; do sleep 0.05; done
while [ "$#" -gt 0 ]; do
 case "$1" in --target) target=$2; shift 2;; --output) out=$2; shift 2;; *) shift;; esac
done
status=prepared
[ ! -f '${dir}/status' ] || status=$(cat '${dir}/status')
if [ "$status" != skipped ]; then
  mkdir "$out"
  if [ -f '${dir}/new-value' ]; then printf 'new-synthetic-value\n' > "$out/item.txt"; else printf 'synthetic-value\n' > "$out/item.txt"; fi
fi
node - "$target" "$status" "$out" <<'NODE'
const fs = require('fs'), crypto = require('crypto');
const [target,status,out] = process.argv.slice(2);
const h = crypto.createHash('sha256');
if (status !== 'skipped') {
 const b=fs.readFileSync(out+'/item.txt'), st=fs.statSync(out+'/item.txt');
 h.update(JSON.stringify(['item.txt',st.mode & 511,'f',b.length])); h.update('\\0'); h.update(b);
}
const treeSha256=status === 'skipped' ? 'a'.repeat(64) : h.digest('hex');
const e={target,status,treeSha256,inputSha:'a'.repeat(40),configSha256:'b'.repeat(64),generatorSha256:'c'.repeat(64),ciSha256:'d'.repeat(64)};
if (status !== 'skipped') Object.assign(e,{publicTree:'e'.repeat(40),generatedTree:status === 'no-diff' ? 'e'.repeat(40) : 'f'.repeat(40)});
if (status === 'prepared') Object.assign(e,{commit:'1'.repeat(40),publicBase:'2'.repeat(40)});
console.log(JSON.stringify(e));
NODE
`,
    { mode: 0o755 },
  );
  writeFileSync(path.join(bin, 'git'), '#!/bin/sh\nprintf "%040d\\n" 1\n', {
    mode: 0o755,
  });
  writeFileSync(path.join(bin, 'docker'), '#!/bin/sh\nexit 0\n', { mode: 0o755 });
  writeFileSync(
    path.join(bin, 'gh'),
    `#!/bin/sh\ntouch '${dir}/sent'\nexit 1\n`,
    { mode: 0o755 },
  );
  const registry = path.join(dir, 'private-registry.json');
  const source = { repo: dir, paths: ['README.md'], replacements: [], reject: [],
    ci: 'intake', verify: 'intake', image: 'example.invalid/check@sha256:' + '0'.repeat(64) };
  const registration = { source, ref: 'HEAD', publicRepo: dir, publicRef: 'HEAD' };
  writeFileSync(registry, JSON.stringify({ one: registration, two: registration }));
  const jobs = path.join(dir, 'private-jobs.json');
  const job = { entry: 'public-source', registry };
  writeFileSync(jobs, JSON.stringify({ one: job, two: job }));
  const previous = path.join(dir, 'previous');
  mkdirSync(previous, { mode: 0o700 });
  for (const id of ['one', 'two']) {
    mkdirSync(path.join(previous, id + '.tree'), { mode: 0o700 });
    writeFileSync(path.join(previous, id + '.merged.json'), 'merged-secret', {
      mode: 0o600,
    });
  }
  const env = { ...process.env, PATH: bin + ':' + process.env.PATH,
    JOBS_REGISTRY: jobs, PREPARATION_ROOT: runs, PREVIOUS_DIR: previous,
    GH_TOKEN: 'synthetic-secret' };
  const run = (extra: Record<string, string> = {}) =>
    spawnSync('sh', [wrapper], { env: { ...env, ...extra }, encoding: 'utf8' });
  return { dir, jobs, runs, previous, wrapper, log, release, env, run };
}

test('定時準備は HOME の Node を優先し、Node がなければ失敗する', () => {
  const f = fixture();
  try {
    const home = path.join(f.dir, 'home');
    const localBin = path.join(home, '.local/bin');
    const systemBin = path.join(f.dir, 'system-bin');
    mkdirSync(localBin, { recursive: true });
    mkdirSync(systemBin);
    for (const name of [
      'stat',
      'id',
      'flock',
      'dirname',
      'date',
      'sh',
      'realpath',
      'rm',
      'mktemp',
      'mv',
    ]) {
      symlinkSync(`/usr/bin/${name}`, path.join(systemBin, name));
    }
    const node = path.join(localBin, 'node');
    const calls = path.join(f.dir, 'node-calls');
    writeFileSync(node, `#!/bin/sh\nprintf '%s\\n' "$@" > '${calls}'\n`, {
      mode: 0o755,
    });
    const wrapper = path.join(
      root,
      'deploy/nas/run-public-source-preparation.sh',
    );
    const env = { ...f.env, HOME: home, PATH: systemBin };
    const run = () =>
      spawnSync('/bin/sh', [wrapper], { env, encoding: 'utf8' });
    const result = run();
    assert.equal(result.status, 0, result.stderr);
    const args = readFileSync(calls, 'utf8').trim().split('\n');
    assert.equal(
      args[0],
      path.join(root, 'scripts/run-public-source-jobs.cjs'),
    );
    assert.deepEqual(args.slice(1, 4), ['--prepare', '--registry', f.jobs]);
    rmSync(node);
    const missing = run();
    assert.notEqual(missing.status, 0);
    assert.match(missing.stderr, /node.*not found/);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('複数対象を固定準備入口に渡し安全な前回候補と非公開出力を保つ', () => {
  const f = fixture();
  try {
    const result = f.run();
    assert.equal(result.status, 0, result.stderr + result.stdout);
    assert.match(result.stdout, /one: prepared/);
    assert.match(result.stdout, /two: prepared/);
    const calls = readFileSync(f.log, 'utf8').trim().split('\n');
    assert.equal(calls.length, 2);
    for (const [i, id] of ['one', 'two'].entries()) {
      assert.ok(
        calls[i].includes(
          `--target ${id} --registry ${f.dir}/private-registry.json`,
        ),
      );
      assert.match(
        calls[i],
        new RegExp(`--output ${f.runs}/run-[^ ]+/${id}\\.tree`),
      );
      assert.ok(
        calls[i].includes(
          `--previous ${f.previous}/${id}.merged.json --previous-tree ${f.previous}/${id}.tree`,
        ),
      );
      assert.equal(
        readFileSync(path.join(f.previous, id + '.merged.json'), 'utf8'),
        'merged-secret',
      );
    }
    assert.equal(existsSync(path.join(f.dir, 'sent')), false);
    assert.doesNotMatch(
      result.stdout + result.stderr,
      /private-jobs|private-registry|synthetic-secret|merged-secret/,
    );
    chmodSync(path.join(f.previous, 'one.merged.json'), 0o644);
    const next = f.run();
    assert.equal(next.status, 0, next.stderr + next.stdout);
    const later = readFileSync(f.log, 'utf8').trim().split('\n').slice(2);
    assert.ok(!later[0].includes('--previous'));
    assert.ok(later[1].includes('--previous'));
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('設定や前提の不足は失敗し入力を表示しない', () => {
  const f = fixture();
  try {
    for (const extra of [
      { JOBS_REGISTRY: '' },
      { PREPARATION_ROOT: 'relative' },
      { PREVIOUS_DIR: 'relative' },
      { JOBS_REGISTRY: path.join(f.dir, 'missing-secret') },
    ]) {
      const result = f.run(extra);
      assert.notEqual(result.status, 0);
      assert.ok(result.stderr || result.stdout);
      assert.doesNotMatch(
        result.stdout + result.stderr,
        /missing-secret|synthetic-secret|private-jobs/,
      );
    }
    chmodSync(f.runs, 0o755);
    assert.notEqual(f.run().status, 0);
    chmodSync(f.runs, 0o700);
    writeFileSync(
      f.jobs,
      JSON.stringify({
        one: { entry: 'untrusted-secret', registry: '/missing' },
      }),
    );
    const invalid = f.run();
    assert.notEqual(invalid.status, 0);
    assert.match(invalid.stdout, /前提不足/);
    assert.doesNotMatch(invalid.stdout + invalid.stderr, /untrusted-secret/);
    assert.equal(existsSync(f.log), false);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('同じ保存先での重複起動を拒否する', async () => {
  const f = fixture();
  let first;
  try {
    writeFileSync(path.join(f.dir, 'block'), '');
    first = spawn('sh', [f.wrapper], { env: f.env, stdio: 'ignore' });
    const ended = new Promise<number | null>((resolve) =>
      first!.once('close', resolve),
    );
    while (!existsSync(f.log) && first.exitCode === null) await delay(20);
    assert.ok(existsSync(f.log), 'first preparation did not start');
    const second = f.run();
    assert.notEqual(second.status, 0);
    assert.match(second.stderr, /実行中/);
    writeFileSync(f.release, '');
    assert.equal(await ended, 0);
    assert.equal(readFileSync(f.log, 'utf8').trim().split('\n').length, 2);
  } finally {
    writeFileSync(f.release, '');
    if (first && first.exitCode === null)
      await new Promise<void>((resolve) =>
        first!.once('close', () => resolve()),
      );
    rmSync(f.dir, { recursive: true, force: true });
  }
});

function configureScan(f: ReturnType<typeof fixture>) {
  const config = path.join(f.dir, 'scan.json');
  const value = createHash('sha256').update('synthetic-value').digest('hex');
  const exception = (target: string) => ({
    target,
    path: 'item.txt',
    rule: 'github-pat',
    sha256: value,
  });
  writeFileSync(
    config,
    JSON.stringify({ exceptions: [exception('one'), exception('two')] }),
    { mode: 0o600 },
  );
  const report = path.join(f.dir, 'report');
  const calls = path.join(f.dir, 'docker-calls');
  const finding = {
    File: '/source/item.txt',
    RuleID: 'github-pat',
    StartLine: 1,
    EndLine: 1,
    Secret: 'REDACTED',
  };
  writeFileSync(report, JSON.stringify([finding]));
  writeFileSync(
    path.join(f.dir, 'bin/docker'),
    `#!/bin/sh
case "$*" in *'image inspect'*) exit 0;; esac
printf '%s\\n' "$@" > '${calls}'
[ -z "\${GH_TOKEN:-}" ] || exit 9
[ -z "\${SSH_AUTH_SOCK:-}" ] || exit 9
[ ! -f '${f.dir}/alter-tree' ] || printf 'altered' >> "$(find '${f.runs}' -name item.txt | head -1)"
[ ! -f '${f.dir}/alter-record' ] || printf ' ' >> "$(find '${f.runs}' -name one.json | head -1)"
[ ! -f '${f.dir}/scan-fail' ] || exit 7
[ ! -f '${f.dir}/missing-report' ] || exit 0
cat '${report}'
[ ! -f '${f.dir}/empty-report' ] || exit 0
exit 14
`,
    { mode: 0o755 },
  );
  return { config, report, calls, finding, exception };
}

test('機密検査は一致する登録を許し全対象の最新記録を作る', () => {
  const f = fixture();
  try {
    const s = configureScan(f);
    const result = f.run({
      PUBLIC_SOURCE_SCAN_CONFIG: s.config,
      SSH_AUTH_SOCK: 'synthetic-key',
    });
    assert.equal(result.status, 0, result.stdout + result.stderr);
    assert.ok(existsSync(path.join(f.runs, '.latest-prepared')));
    const args = readFileSync(s.calls, 'utf8').split('\n');
    for (const arg of [
      '--read-only',
      '--network',
      'none',
      '--cap-drop',
      'ALL',
      '--pull',
      'never',
      '--ignore-gitleaks-allow',
      '--gitleaks-ignore-path',
      '/empty-ignore',
      'ghcr.io/gitleaks/gitleaks@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f',
    ])
      assert.ok(args.includes(arg), arg);
    assert.equal(args.filter((a) => a.includes('readonly')).length, 3);
    assert.doesNotMatch(
      result.stdout + result.stderr,
      /synthetic-value|synthetic-secret|synthetic-key/,
    );
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('機密検査は未知の検出や破損と改変を拒み最新記録を残さない', () => {
  for (const scenario of [
    'new-value',
    'unknown-rule',
    'bad-path',
    'scan-fail',
    'missing-report',
    'bad-report',
    'alter-tree',
    'alter-record',
    'bad-range',
  ]) {
    const f = fixture();
    try {
      const s = configureScan(f);
      const finding = { ...s.finding };
      if (scenario === 'new-value')
        writeFileSync(path.join(f.dir, 'new-value'), '');
      if (scenario === 'unknown-rule') finding.RuleID = 'unknown-rule';
      if (scenario === 'bad-path') finding.File = '/source/../item.txt';
      if (scenario === 'bad-range') finding.EndLine = 2;
      if (scenario === 'bad-report')
        writeFileSync(s.report, '{synthetic-value');
      else writeFileSync(s.report, JSON.stringify([finding]));
      if (
        ['scan-fail', 'missing-report', 'alter-tree', 'alter-record'].includes(
          scenario,
        )
      )
        writeFileSync(path.join(f.dir, scenario), '');
      writeFileSync(path.join(f.runs, '.latest-prepared'), 'old');
      const result = f.run({ PUBLIC_SOURCE_SCAN_CONFIG: s.config });
      assert.notEqual(result.status, 0, scenario);
      assert.equal(
        existsSync(path.join(f.runs, '.latest-prepared')),
        false,
        scenario,
      );
      assert.doesNotMatch(
        result.stdout + result.stderr,
        /synthetic-value|REDACTED/,
        scenario,
      );
    } finally {
      rmSync(f.dir, { recursive: true, force: true });
    }
  }
});

test('設定なしとno-diff・skippedは準備と送信見送りを保つ', () => {
  for (const status of ['no-diff', 'skipped']) {
    const f = fixture();
    try {
      const s = configureScan(f);
      writeFileSync(path.join(f.dir, 'status'), status);
      if (status === 'no-diff') {
        writeFileSync(s.report, '[]');
        writeFileSync(path.join(f.dir, 'empty-report'), '');
      }
      const result = f.run({ PUBLIC_SOURCE_SCAN_CONFIG: s.config });
      assert.equal(result.status, 0, result.stdout + result.stderr);
      assert.match(result.stdout, new RegExp(status));
      assert.ok(existsSync(path.join(f.runs, '.latest-prepared')));
      assert.equal(existsSync(path.join(f.dir, 'sent')), false);
      if (status === 'skipped') assert.equal(existsSync(s.calls), false);
    } finally {
      rmSync(f.dir, { recursive: true, force: true });
    }
  }
});
