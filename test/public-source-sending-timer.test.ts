import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  utimesSync,
  writeFileSync,
} from 'node:fs';
import path from 'node:path';

const root = path.resolve(import.meta.dirname, '..');
function fixture() {
  const dir = mkdtempSync(path.join(root, '.sending-timer-'));
  const nas = path.join(dir, 'deploy/nas');
  const scripts = path.join(dir, 'scripts');
  mkdirSync(nas, { recursive: true });
  mkdirSync(scripts);
  for (const name of [
    'run-public-source-preparation.sh',
    'run-public-source-sending.sh',
  ])
    copyFileSync(path.join(root, 'deploy/nas', name), path.join(nas, name));
  const runs = path.join(dir, 'runs');
  mkdirSync(runs, { mode: 0o700 });
  const log = path.join(dir, 'calls');
  writeFileSync(
    path.join(scripts, 'run-public-source-jobs.sh'),
    `#!/bin/sh\nprintf 'prepare:%s\\n' "\${GH_TOKEN-unset}" >> '${log}'\nwhile [ "$#" -gt 0 ]; do case "$1" in --output) out=$2; shift 2;; *) shift;; esac; done\nmkdir "$out"\n[ ! -f '${dir}/fail' ]\n`,
  );
  writeFileSync(
    path.join(scripts, 'run-approved-public-source-jobs.cjs'),
    `
const fs = require('node:fs');
fs.appendFileSync(${JSON.stringify(log)}, JSON.stringify({args: process.argv.slice(2), env: process.env}) + '\\n');
const status = fs.readFileSync(${JSON.stringify(path.join(dir, 'status'))}, 'utf8').trim();
console.log('one: ' + status);
process.exitCode = status === 'waiting' ? 2 : status === 'failed' ? 1 : 0;
`,
  );
  writeFileSync(path.join(dir, 'status'), 'merged');
  const env = {
    ...process.env,
    JOBS_REGISTRY: path.join(dir, 'jobs'),
    PREPARATION_ROOT: runs,
    SEND_PREPARED_DIR: path.join(dir, 'approved'),
    SEND_CONFIRMATIONS_DIR: path.join(dir, 'confirmations'),
    SEND_ISSUER_REGISTRY: path.join(dir, 'issuer'),
    GH_TOKEN: 'synthetic-secret',
  };
  const run = (name: string, values: Record<string, string> = {}) =>
    spawnSync('sh', [path.join(nas, name)], {
      env: { ...env, ...values },
      encoding: 'utf8',
    });
  return { dir, log, env, run };
}

test('準備後に承認済み保存先で一巡し、待ちと失敗は同じ候補で再実行する', () => {
  const f = fixture();
  try {
    // The service does not supply credentials to preparation.
    const prep = f.run('run-public-source-preparation.sh', { GH_TOKEN: '' });
    assert.equal(prep.status, 0, prep.stderr);
    for (const [status, code] of [
      ['merged', 0],
      ['waiting', 2],
      ['failed', 1],
      ['published', 0],
    ] as const) {
      writeFileSync(path.join(f.dir, 'status'), status);
      const result = f.run('run-public-source-sending.sh');
      assert.equal(result.status, code, result.stderr);
      assert.match(result.stdout, new RegExp(`one: ${status}`));
      assert.doesNotMatch(result.stdout + result.stderr, /synthetic-secret/);
    }
    const calls = readFileSync(f.log, 'utf8').trim().split('\n');
    assert.equal(calls[0], 'prepare:');
    assert.equal(calls.length, 5);
    for (const line of calls.slice(1)) {
      const call = JSON.parse(line);
      assert.deepEqual(call.args, [
        '--registry',
        f.env.JOBS_REGISTRY,
        '--prepared-dir',
        f.env.SEND_PREPARED_DIR,
        '--confirmations-dir',
        f.env.SEND_CONFIRMATIONS_DIR,
        '--issuer-registry',
        f.env.SEND_ISSUER_REGISTRY,
      ]);
      assert.equal(call.env.GH_TOKEN, undefined);
      assert.doesNotMatch(line, /synthetic-secret/);
    }
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('成功した準備だけを最新候補として送信する', () => {
  const f = fixture();
  try {
    const config = {
      SEND_PREPARED_DIR: '',
      SEND_PREPARATION_ROOT: f.env.PREPARATION_ROOT,
    };
    const record = path.join(f.env.PREPARATION_ROOT, '.latest-prepared');
    for (let i = 0; i < 2; i++) {
      assert.equal(f.run('run-public-source-preparation.sh').status, 0);
      assert.equal(f.run('run-public-source-sending.sh', config).status, 0);
    }
    const sent = readFileSync(f.log, 'utf8')
      .trim()
      .split('\n')
      .filter((x) => x.startsWith('{'))
      .map((x) => JSON.parse(x).args[3]);
    assert.equal(sent.length, 2);
    assert.notEqual(sent[0], sent[1]);
    writeFileSync(path.join(f.dir, 'fail'), '');
    assert.notEqual(f.run('run-public-source-preparation.sh').status, 0);
    assert.notEqual(f.run('run-public-source-sending.sh', config).status, 0);
    rmSync(path.join(f.dir, 'fail'));
    assert.equal(f.run('run-public-source-preparation.sh').status, 0);
    utimesSync(record, 1, 1);
    assert.notEqual(f.run('run-public-source-sending.sh', config).status, 0);
    writeFileSync(record, '../outside\n');
    assert.notEqual(f.run('run-public-source-sending.sh', config).status, 0);
    rmSync(record);
    symlinkSync(sent[0], record);
    assert.notEqual(f.run('run-public-source-sending.sh', config).status, 0);
    rmSync(record);
    mkdirSync(record);
    assert.notEqual(f.run('run-public-source-sending.sh', config).status, 0);
    rmSync(record, { recursive: true });
    const linkName = 'run-123-456';
    symlinkSync(sent[0], path.join(f.env.PREPARATION_ROOT, linkName));
    writeFileSync(record, linkName + '\n', { mode: 0o600 });
    assert.notEqual(f.run('run-public-source-sending.sh', config).status, 0);
    const locked = spawnSync(
      'flock',
      [
        '-n',
        f.env.PREPARATION_ROOT,
        'sh',
        path.join(f.dir, 'deploy/nas/run-public-source-sending.sh'),
      ],
      { env: { ...f.env, ...config }, encoding: 'utf8' },
    );
    assert.notEqual(locked.status, 0);
    assert.equal(
      readFileSync(f.log, 'utf8')
        .trim()
        .split('\n')
        .filter((x) => x.startsWith('{')).length,
      2,
    );
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('未設定なら送信せず、不完全または相対パスの設定は送信前に失敗する', () => {
  const f = fixture();
  try {
    const empty = {
      SEND_PREPARED_DIR: '',
      SEND_CONFIRMATIONS_DIR: '',
      SEND_ISSUER_REGISTRY: '',
    };
    assert.equal(
      f.run('run-public-source-preparation.sh', { GH_TOKEN: '' }).status,
      0,
    );
    assert.equal(f.run('run-public-source-sending.sh', empty).status, 0);
    for (const values of [
      { ...empty, SEND_PREPARED_DIR: '/approved' },
      { SEND_CONFIRMATIONS_DIR: 'relative' },
      { JOBS_REGISTRY: '' },
    ]) {
      const result = f.run('run-public-source-sending.sh', values);
      assert.notEqual(result.status, 0);
      assert.doesNotMatch(
        result.stdout + result.stderr,
        /synthetic-secret|relative/,
      );
    }
    assert.equal(existsSync(f.log), true);
    assert.equal(readFileSync(f.log, 'utf8').trim().split('\n').length, 1);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('実際の一巡入口は登録対象を検査し、承認済み候補だけを送る', () => {
  const f = fixture();
  try {
    const scripts = path.join(f.dir, 'scripts');
    copyFileSync(
      path.join(root, 'scripts/run-approved-public-source-jobs.cjs'),
      path.join(scripts, 'run-approved-public-source-jobs.cjs'),
    );
    copyFileSync(
      path.join(root, 'scripts/public-source-release-policy.cjs'),
      path.join(scripts, 'public-source-release-policy.cjs'),
    );
    writeFileSync(
      path.join(scripts, 'publish-public-source.sh'),
      `#!/bin/sh\nprintf '{"status":"preview","target":"%s","confirmation":"${'a'.repeat(64)}"}\\n' "$3"\n`,
    );
    mkdirSync(f.env.SEND_PREPARED_DIR, { mode: 0o700 });
    mkdirSync(f.env.SEND_CONFIRMATIONS_DIR, { mode: 0o700 });
    const registry = path.join(f.dir, 'registry');
    const jobs = Object.fromEntries(
      ['one', 'two'].map((id) => [id, { entry: 'public-source', registry }]),
    );
    writeFileSync(f.env.JOBS_REGISTRY, JSON.stringify(jobs), { mode: 0o600 });
    writeFileSync(registry, JSON.stringify({ one: {}, two: {} }), {
      mode: 0o600,
    });
    writeFileSync(f.env.SEND_ISSUER_REGISTRY, '{}', { mode: 0o600 });
    for (const id of ['one', 'two']) {
      mkdirSync(path.join(f.env.SEND_PREPARED_DIR, id + '.tree'), {
        mode: 0o700,
      });
      writeFileSync(
        path.join(f.env.SEND_PREPARED_DIR, id + '.json'),
        JSON.stringify({ target: id, status: 'prepared' }),
        { mode: 0o600 },
      );
      writeFileSync(
        path.join(f.env.SEND_CONFIRMATIONS_DIR, id),
        'a'.repeat(64),
        { mode: 0o600 },
      );
    }
    writeFileSync(
      path.join(scripts, 'issue-public-source-github-app.cjs'),
      "process.stdout.write('issued-secret');",
    );
    writeFileSync(
      path.join(scripts, 'run-public-source-jobs.cjs'),
      `
const fs = require('node:fs');
const input = fs.readFileSync(0, 'utf8');
if (input !== 'issued-secret' || JSON.stringify(process.env).includes(input) || process.argv.join(' ').includes(input)) process.exit(1);
fs.appendFileSync(${JSON.stringify(f.log)}, process.argv[6] + '\\n');
console.log('merged');
`,
    );
    const result = f.run('run-public-source-sending.sh');
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout, 'one: merged\ntwo: merged\n');
    assert.equal(readFileSync(f.log, 'utf8'), 'one\ntwo\n');
    assert.doesNotMatch(
      result.stdout + result.stderr + readFileSync(f.log, 'utf8'),
      /issued-secret|synthetic-secret/,
    );
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('実際の一巡と送信入口は確認値の不一致を拒否し非準備候補を発行しない', () => {
  const f = fixture();
  try {
    const scripts = path.join(f.dir, 'scripts');
    for (const name of [
      'run-approved-public-source-jobs.cjs',
      'public-source-release-policy.cjs',
      'run-public-source-jobs.cjs',
      'run-public-source-jobs-send.cjs',
    ])
      copyFileSync(path.join(root, 'scripts', name), path.join(scripts, name));
    mkdirSync(f.env.SEND_PREPARED_DIR, { mode: 0o700 });
    mkdirSync(f.env.SEND_CONFIRMATIONS_DIR, { mode: 0o700 });
    const ids = ['mismatch', 'no-diff', 'skipped'];
    const registry = path.join(f.dir, 'registry');
    writeFileSync(
      f.env.JOBS_REGISTRY,
      JSON.stringify(
        Object.fromEntries(
          ids.map((id) => [id, { entry: 'public-source', registry }]),
        ),
      ),
      { mode: 0o600 },
    );
    writeFileSync(
      registry,
      JSON.stringify(Object.fromEntries(ids.map((id) => [id, {}]))),
      { mode: 0o600 },
    );
    writeFileSync(f.env.SEND_ISSUER_REGISTRY, '{}', { mode: 0o600 });
    for (const id of ids) {
      mkdirSync(path.join(f.env.SEND_PREPARED_DIR, id + '.tree'), {
        mode: 0o700,
      });
      writeFileSync(
        path.join(f.env.SEND_PREPARED_DIR, id + '.json'),
        JSON.stringify({
          target: id,
          status: id === 'mismatch' ? 'prepared' : id,
        }),
        { mode: 0o600 },
      );
      writeFileSync(
        path.join(f.env.SEND_CONFIRMATIONS_DIR, id),
        'a'.repeat(64),
        { mode: 0o600 },
      );
    }
    writeFileSync(
      path.join(scripts, 'issue-public-source-github-app.cjs'),
      `require('node:fs').appendFileSync(${JSON.stringify(f.log)}, process.argv[3] + '\\n'); process.stdout.write('issued');`,
    );
    writeFileSync(
      path.join(scripts, 'publish-public-source.sh'),
      `#!/bin/sh\ncase "$*" in *--confirm*) echo sent >> '${f.log}';; *) printf '{"status":"preview","target":"mismatch","confirmation":"%064d"}\\n' 0;; esac\n`,
    );
    const result = f.run('run-public-source-sending.sh');
    assert.equal(result.status, 1);
    assert.equal(
      result.stdout,
      'mismatch: failed\nno-diff: failed\nskipped: failed\n',
    );
    assert.equal(existsSync(f.log), false);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('定時の待ち設定は範囲内だけを一巡入口へ渡す', () => {
  const f = fixture();
  try {
    for (const value of ['1', '3600']) {
      assert.equal(
        f.run('run-public-source-sending.sh', { SEND_WAIT_SECONDS: value })
          .status,
        0,
      );
      const call = JSON.parse(
        readFileSync(f.log, 'utf8').trim().split('\n').at(-1)!,
      );
      assert.deepEqual(call.args.slice(-2), ['--wait-seconds', value]);
    }
    const before = readFileSync(f.log, 'utf8');
    for (const value of ['', '0', '3601', '-1', '1.5', '01', 'abc'])
      assert.notEqual(
        f.run('run-public-source-sending.sh', { SEND_WAIT_SECONDS: value })
          .status,
        0,
      );
    assert.equal(readFileSync(f.log, 'utf8'), before);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test('待ち対象だけ同じ候補で再確認し、変更や期限では再送しない', () => {
  for (const scenario of [
    'failed',
    'skipped',
    'candidate',
    'base',
    'confirmation',
    'preview',
    'deadline',
    'deadline-only',
  ]) {
    const f = fixture();
    try {
      const scripts = path.join(f.dir, 'scripts');
      for (const name of [
        'run-approved-public-source-jobs.cjs',
        'public-source-release-policy.cjs',
      ])
        copyFileSync(
          path.join(root, 'scripts', name),
          path.join(scripts, name),
        );
      mkdirSync(f.env.SEND_PREPARED_DIR, { mode: 0o700 });
      mkdirSync(f.env.SEND_CONFIRMATIONS_DIR, { mode: 0o700 });
      const registry = path.join(f.dir, 'registry');
      writeFileSync(
        f.env.JOBS_REGISTRY,
        JSON.stringify(
          Object.fromEntries(
            ['one', 'two'].map((id) => [
              id,
              { entry: 'public-source', registry },
            ]),
          ),
        ),
        { mode: 0o600 },
      );
      writeFileSync(registry, JSON.stringify({ one: {}, two: {} }), {
        mode: 0o600,
      });
      writeFileSync(f.env.SEND_ISSUER_REGISTRY, '{}', { mode: 0o600 });
      for (const id of ['one', 'two']) {
        mkdirSync(path.join(f.env.SEND_PREPARED_DIR, id + '.tree'), {
          mode: 0o700,
        });
        const evidence =
          id === 'two' &&
          (scenario === 'skipped' || scenario === 'deadline-only')
            ? {
                target: id,
                status: 'skipped',
                inputSha: '1'.repeat(40),
                configSha256: '2'.repeat(64),
                generatorSha256: '3'.repeat(64),
                ciSha256: '4'.repeat(64),
                treeSha256: '5'.repeat(64),
              }
            : {
                target: id,
                status: 'prepared',
                commit: 'a'.repeat(40),
                publicBase: 'b'.repeat(40),
              };
        writeFileSync(
          path.join(f.env.SEND_PREPARED_DIR, id + '.json'),
          JSON.stringify(evidence),
          { mode: 0o600 },
        );
        writeFileSync(
          path.join(f.env.SEND_CONFIRMATIONS_DIR, id),
          'a'.repeat(64),
          { mode: 0o600 },
        );
      }
      writeFileSync(
        path.join(scripts, 'publish-public-source.sh'),
        `#!/bin/sh\nhead=$(cat '${f.env.SEND_PREPARED_DIR}/one.json')\nnode -e 'const e=JSON.parse(process.argv[1]); console.log(JSON.stringify({status:"preview",target:process.argv[2],confirmation:"a".repeat(64),head:e.commit,base:e.publicBase,extra:process.argv[3]}))' "$head" "$3" "$(cat '${f.dir}/preview' 2>/dev/null || :)"\n`,
      );
      writeFileSync(
        path.join(scripts, 'issue-public-source-github-app.cjs'),
        "process.stdout.write('issued');",
      );
      writeFileSync(
        path.join(scripts, 'run-public-source-jobs.cjs'),
        `
const fs = require('node:fs');
const id = process.argv[6], log = ${JSON.stringify(f.log)}, dir = ${JSON.stringify(f.env.SEND_PREPARED_DIR)}, scenario = ${JSON.stringify(scenario)};
const evidence = JSON.parse(fs.readFileSync(dir + '/' + id + '.json'));
let calls = fs.existsSync(log) ? fs.readFileSync(log, 'utf8') : '';
fs.appendFileSync(log, id + ':' + evidence.commit + '\\n');
if (id === 'two') { console.log('failed'); process.exit(1); }
if (!calls.includes('one:')) {
  if (scenario === 'candidate' || scenario === 'base') { evidence[scenario === 'candidate' ? 'commit' : 'publicBase'] = 'c'.repeat(40); fs.writeFileSync(dir + '/one.json', JSON.stringify(evidence)); }
  if (scenario === 'confirmation') fs.writeFileSync(${JSON.stringify(path.join(f.env.SEND_CONFIRMATIONS_DIR, 'one'))}, 'b'.repeat(64));
  if (scenario === 'preview') fs.writeFileSync(${JSON.stringify(path.join(f.dir, 'preview'))}, 'changed');
  console.log('waiting'); process.exit(2);
}
console.log('merged');
`,
      );
      const result = f.run('run-public-source-sending.sh', {
        SEND_WAIT_SECONDS: scenario.startsWith('deadline') ? '1' : '2',
      });
      const calls = readFileSync(f.log, 'utf8').trim().split('\n');
      assert.equal(
        calls.filter((x) => x.startsWith('two:')).length,
        scenario === 'skipped' || scenario === 'deadline-only' ? 0 : 1,
        scenario,
      );
      assert.deepEqual(
        calls.filter((x) => x.startsWith('one:')),
        Array(scenario === 'failed' || scenario === 'skipped' ? 2 : 1).fill(
          'one:' + 'a'.repeat(40),
        ),
        scenario,
      );
      assert.match(
        result.stdout,
        scenario.startsWith('deadline')
          ? /one: waiting/
          : scenario === 'failed' || scenario === 'skipped'
            ? /one: merged/
            : /one: failed/,
      );
      assert.equal(
        result.status,
        scenario === 'skipped' ? 0 : scenario === 'deadline-only' ? 2 : 1,
      );
    } finally {
      rmSync(f.dir, { recursive: true, force: true });
    }
  }
});
