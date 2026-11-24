import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  mkdtempSync,
  mkdirSync,
  copyFileSync,
  writeFileSync,
  readFileSync,
  rmSync,
  existsSync,
  chmodSync,
  symlinkSync,
  unlinkSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const root = path.resolve(import.meta.dirname, '..');
const proof = 'a'.repeat(64);
const secret = 'synthetic-private-token';

test('送信は一対象の標準入力を渡し待ちと失敗と成功を区別する', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'schedule-send-'));
  try {
    const scripts = path.join(dir, 'scripts');
    mkdirSync(scripts);
    for (const name of [
      'run-public-source-jobs.cjs',
      'run-public-source-jobs-send.cjs',
    ])
      copyFileSync(path.join(root, 'scripts', name), path.join(scripts, name));
    const log = path.join(dir, 'log');
    const mode = path.join(dir, 'mode');
    const child = path.join(scripts, 'child.cjs');
    writeFileSync(
      child,
      `const fs=require('fs'); const a=process.argv.slice(2), d=${JSON.stringify(dir)}, m=fs.readFileSync(d+'/mode','utf8'), s=a.includes('--confirm'); const input=s?fs.readFileSync(0,'utf8'):''; fs.appendFileSync(d+'/log',JSON.stringify({args:a,env:process.env,input})+'\\n'); if(!s&&m==='preview-fail')process.exit(1); if(!s)console.log(JSON.stringify({status:'preview',target:a[a.indexOf('--target')+1],confirmation:${JSON.stringify(proof)}})); else {console.log(JSON.stringify({status:m})); if(m==='waiting')process.exitCode=1;}`,
    );
    writeFileSync(
      path.join(scripts, 'publish-public-source.sh'),
      `#!/bin/sh\nexec node ${JSON.stringify(child)} "$@"\n`,
    );
    const registry = path.join(dir, 'registry');
    const jobs = path.join(dir, 'jobs');
    const conf = path.join(dir, 'confirmation');
    const prepared = path.join(dir, 'prepared');
    mkdirSync(prepared, { mode: 0o700 });
    for (const target of ['first', 'second']) {
      mkdirSync(path.join(prepared, target + '.tree'), { mode: 0o700 });
      writeFileSync(
        path.join(prepared, target + '.json'),
        JSON.stringify({ target, status: 'prepared' }),
        { mode: 0o600 },
      );
    }
    writeFileSync(registry, JSON.stringify({ first: {}, second: {} }), {
      mode: 0o600,
    });
    writeFileSync(
      jobs,
      JSON.stringify({
        first: { entry: 'public-source', registry },
        second: { entry: 'public-source', registry },
      }),
      { mode: 0o600 },
    );
    writeFileSync(conf, proof, { mode: 0o600 });
    const issued = JSON.stringify({
      token: secret,
      metadata: {
        repository: 'example/repo',
        credentialRef: 'first',
        permissions: {},
      },
    });
    const invoke = (
      target: string,
      state: string,
      preparedPath = prepared,
      confirmationPath = conf,
    ) => {
      writeFileSync(mode, state);
      return spawnSync(
        process.execPath,
        [
          path.join(scripts, 'run-public-source-jobs.cjs'),
          '--send',
          '--registry',
          jobs,
          '--target',
          target,
          '--prepared-dir',
          preparedPath,
          '--confirmation',
          confirmationPath,
        ],
        {
          input: issued,
          encoding: 'utf8',
          env: { ...process.env, SENTINEL: secret },
        },
      );
    };
    for (const [state, code, output] of [
      ['waiting', 2, 'waiting'],
      ['published', 0, 'published'],
      ['merged', 0, 'merged'],
      ['unknown', 1, ''],
    ] as const) {
      const r = invoke('first', state);
      assert.equal(r.status, code);
      assert.equal(r.stdout.trim(), output);
      assert.ok(!r.stdout.includes(secret) && !r.stderr.includes(secret));
    }
    const assertRejected = (
      target = 'first',
      preparedPath = prepared,
      confirmationPath = conf,
    ) => {
      const before = readFileSync(log, 'utf8').trim().split('\n').length;
      const result = invoke(
        target,
        'published',
        preparedPath,
        confirmationPath,
      );
      assert.equal(result.status, 1);
      assert.equal(result.stdout, '');
      assert.equal(readFileSync(log, 'utf8').trim().split('\n').length, before);
    };
    chmodSync(conf, 0o644);
    assertRejected();
    chmodSync(conf, 0o600);
    const confLink = path.join(dir, 'confirmation-link');
    symlinkSync(conf, confLink);
    assertRejected('first', prepared, confLink);
    assertRejected('first', prepared, path.join(dir, 'missing'));
    assertRejected('first', prepared, 'relative-confirmation');
    writeFileSync(conf, 'not-a-64-digit-hex-value');
    assertRejected();
    writeFileSync(conf, proof);
    assertRejected('unregistered');

    chmodSync(prepared, 0o755);
    assertRejected();
    chmodSync(prepared, 0o700);
    const preparedLink = path.join(dir, 'prepared-link');
    symlinkSync(prepared, preparedLink);
    assertRejected('first', preparedLink);
    assertRejected('first', 'relative-prepared');

    const candidate = path.join(prepared, 'first.json');
    chmodSync(candidate, 0o644);
    assertRejected();
    chmodSync(candidate, 0o600);
    unlinkSync(candidate);
    symlinkSync(path.join(prepared, 'second.json'), candidate);
    assertRejected();
    unlinkSync(candidate);
    writeFileSync(
      candidate,
      JSON.stringify({ target: 'second', status: 'prepared' }),
      { mode: 0o600 },
    );
    assertRejected();
    writeFileSync(
      candidate,
      JSON.stringify({ target: 'first', status: 'prepared' }),
    );

    const before = readFileSync(log, 'utf8').trim().split('\n').length;
    assert.equal(invoke('second', 'preview-fail').status, 1);
    const calls = readFileSync(log, 'utf8')
      .trim()
      .split('\n')
      .map((line) => JSON.parse(line));
    assert.equal(calls.length, before + 1);
    for (const call of calls) {
      assert.ok(!JSON.stringify(call.args).includes(secret));
      assert.ok(!JSON.stringify(call.env).includes(secret));
      assert.equal(call.input, call.args.includes('--confirm') ? issued : '');
      assert.ok(
        call.args.includes(
          path.join(
            prepared,
            call.args[call.args.indexOf('--target') + 1] + '.json',
          ),
        ),
      );
    }
    assert.equal(existsSync(path.join(prepared, 'first.merged.json')), false);
    writeFileSync(conf, 'b'.repeat(64), { mode: 0o600 });
    const n = calls.length;
    assert.equal(invoke('second', 'published').status, 1);
    assert.equal(readFileSync(log, 'utf8').trim().split('\n').length, n + 1);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
