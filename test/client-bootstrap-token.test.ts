import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

function fixture(t, existing, green = true) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'intake-token-bootstrap-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  fs.mkdirSync(path.join(dir, 'bin'));
  fs.writeFileSync(
    path.join(dir, 'compose.yaml'),
    green
      ? 'volumes:\n  issue-desk-worker-state-green: {}\n  issue-desk-worker-colors: {}\nservices:\n  issue-desk-client-green:\n    profiles: [green]\n'
      : 'services:\n  issue-desk-client: {}\n',
  );
  fs.writeFileSync(
    path.join(dir, '.env'),
    'REGISTRY=registry.test\nFLAVOR=\nISSUE_DESK_CLIENT_TAG=0123456789ab\n',
  );
  const docker = path.join(dir, 'bin', 'docker');
  fs.writeFileSync(
    docker,
    `#!/bin/sh
phase= kind= command=
while [ "$#" -gt 0 ]; do
 case "$1" in --phase) phase=$2; shift;; --kind) kind=$2; shift;; doctor|get) command=$1;; esac
 shift
done
if [ "$command" = doctor ]; then exit 0; fi
if [ "$command" = get ]; then printf '%s\\n' '{"status":200,"body":{"config":{"assignments":{"workerAccounts":{"test-host":"worker-account"}}}}}'; exit 0; fi
printf '%s %s\\n' "$phase" "$kind" >> "$BOOTSTRAP_TRACE"
if [ "$phase" = existing ]; then exit ${existing ? '0' : '1'}; fi
IFS= read -r token || exit 1
[ "$token" = fixture-input-token ] || exit 1
exit 0
`,
  );
  fs.chmodSync(docker, 0o755);
  return {
    dir,
    env: {
      ...process.env,
      PATH: path.join(dir, 'bin') + ':' + process.env.PATH,
      BOOTSTRAP_HOST: 'test-host',
      BOOTSTRAP_TRACE: path.join(dir, 'trace'),
    },
  };
}
test('両色の既存tokenが本人一致なら入力せず再配置もしない', (t) => {
  const { dir, env } = fixture(t, true);
  const result = spawnSync(
    'bash',
    ['deploy/clients/worker-token-linux.sh', dir],
    { env, input: '', encoding: 'utf8' },
  );
  assert.equal(result.status, 0, result.stderr);
  const phases = fs
    .readFileSync(path.join(dir, 'trace'), 'utf8')
    .trim()
    .split('\n');
  assert.deepEqual(phases, ['existing worker-token', 'existing worker-token']);
});
test('tokenは両色をcheckしてからwrite、verifyし、値を出さない', (t) => {
  const { dir, env } = fixture(t, false);
  const result = spawnSync(
    'bash',
    ['deploy/clients/worker-token-linux.sh', dir],
    { env, input: 'fixture-input-token\n', encoding: 'utf8' },
  );
  assert.equal(result.status, 0, result.stderr);
  assert.doesNotMatch(result.stdout + result.stderr, /fixture-input-token/);
  const phases = fs
    .readFileSync(path.join(dir, 'trace'), 'utf8')
    .trim()
    .split('\n');
  assert.deepEqual(phases, [
    'existing worker-token',
    'existing worker-token',
    'check worker-token',
    'check worker-token',
    'write worker-token',
    'write worker-token',
    'verify worker-token',
    'verify worker-token',
  ]);
  assert.doesNotMatch(phases.join('\n'), /gitea-token/);
});

test('greenの定義がないcomposeではblueだけにtokenを配置する', (t) => {
  const { dir, env } = fixture(t, false, false);
  const result = spawnSync(
    'bash',
    ['deploy/clients/worker-token-linux.sh', dir],
    { env, input: 'fixture-input-token\n', encoding: 'utf8' },
  );
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(
    fs.readFileSync(path.join(dir, 'trace'), 'utf8').trim().split('\n'),
    [
      'existing worker-token',
      'check worker-token',
      'write worker-token',
      'verify worker-token',
    ],
  );
});

test('導入のvolume処理は取得した現行・次版のcomposeを使う', (t) => {
  const source = fs.readFileSync(
    'deploy/clients/bootstrap-ubuntu-client.sh',
    'utf8',
  );
  const block = source.slice(
    source.indexOf('volumes=(issue-desk-worker-state'),
    source.indexOf('# --- 段③: 設定3項目 ---'),
  );
  assert.ok(block.includes('docker volume create'));
  for (const green of [true, false]) {
    const { dir } = fixture(t, true, green);
    const update = path.join(dir, 'update-linux.sh');
    fs.writeFileSync(
      update,
      green ? 'issue-desk-worker-colors\n' : '# blue only\n',
    );
    const runner = `set -eu
work=$1; src=$1; image=fixture
skip() { :; }; ok() { :; }
docker() { printf '%s\\n' "$*" >> "$work/calls"; [ "$1" != volume ] || [ "$2" != inspect ]; }
run_c() { printf '%s\\n' "$*" >> "$work/calls"; return 1; }
${block}`;
    // 持ち主の変更も記録したうえで成功させる。
    const fixed = runner.replace(
      'return 1; }',
      'case "$*" in *"--entrypoint chown"*) return 0;; *) return 1;; esac; }',
    );
    const r = spawnSync('bash', ['-c', fixed, 'test', dir], {
      encoding: 'utf8',
    });
    assert.equal(r.status, 0, r.stderr);
    const calls = fs.readFileSync(path.join(dir, 'calls'), 'utf8');
    for (const volume of [
      'issue-desk-worker-state-green',
      'issue-desk-worker-colors',
    ])
      assert.equal(calls.includes(volume), green, calls);
    assert.match(calls, /volume create issue-desk-worker-state/);
    assert.doesNotMatch(calls, /volume (rm|remove)/);
  }
});

test('Webhook入口が定義された場合だけ稼働を必須にする', () => {
  const source = fs.readFileSync(
    'deploy/clients/bootstrap-ubuntu-start.sh',
    'utf8',
  );
  const block = source.slice(
    source.indexOf('both_running() {'),
    source.indexOf('# --- 段⑥の前'),
  );
  for (const [defined, running, expected] of [
    ['issue-desk-client', 'issue-desk-client', 0],
    ['issue-desk-client\nissue-desk-webhook-entry', 'issue-desk-client', 1],
    [
      'issue-desk-client\nissue-desk-webhook-entry',
      'issue-desk-client\nissue-desk-webhook-entry',
      0,
    ],
    ['issue-desk-client', '', 1],
  ] as [string, string, number][]) {
    const script = `dc() { if [ "$1" = config ]; then printf '%s\\n' "$DEFINED"; else printf '%s\\n' "$RUNNING"; fi; }\n${block}\nboth_running`;
    const result = spawnSync('bash', ['-c', script], {
      env: { ...process.env, DEFINED: defined, RUNNING: running },
      encoding: 'utf8',
    });
    assert.equal(result.status, expected, result.stderr);
  }
});
