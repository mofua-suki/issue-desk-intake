import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';

test('導入は無期限停止と期限切れを受付応答から判定する', () => {
  const source = readFileSync(
    'deploy/clients/bootstrap-ubuntu-start.sh',
    'utf8',
  );
  const block = source.slice(
    source.indexOf('read_view() {'),
    source.indexOf('desk_drain() {'),
  );
  for (const [drain, expected] of [
    [{ indefinite: true }, 1],
    [{ until: '2000-01-01T00:00:00Z' }, 0],
  ]) {
    const script = `host=pc\ndc() { printf '%s' "$RESPONSE"; }\n${block}\nread_view 0; printf '%s' "$drained"`;
    const response = JSON.stringify({
      body: {
        hosts: [
          {
            name: 'pc',
            updatedAt: new Date().toISOString(),
            ready: true,
            drain,
          },
        ],
      },
    });
    const result = spawnSync('bash', ['-c', script], {
      env: { ...process.env, RESPONSE: response },
      encoding: 'utf8',
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout, String(expected));
  }
});

test('更新は無期限停止を認識し安全条件不足なら更新を実行しない', () => {
  const script = `set -e\nhost=pc; work=/tmp; src=/tmp; tag=fixture\nstop() { printf '%s' "$1" >&2; exit 42; }\ndocker() { case " $* " in *' get '*) printf '%s' "$RESPONSE";; *) cat >/dev/null;; esac; }\nnas_ssh() { echo update-should-not-run >&2; }\nsource deploy/clients/bootstrap-ubuntu-update.sh`;
  const response = JSON.stringify({
    body: {
      hosts: [
        {
          name: 'pc',
          updatedAt: new Date().toISOString(),
          drain: { indefinite: true },
          restartSafe: false,
          implementing: 1,
          publishing: 0,
        },
      ],
    },
  });
  const result = spawnSync('bash', ['-c', script], {
    env: {
      ...process.env,
      RESPONSE: response,
      BOOTSTRAP_WAIT_ATTEMPTS: '1',
      BOOTSTRAP_POLL_SECONDS: '0',
    },
    encoding: 'utf8',
  });
  assert.equal(result.status, 42, result.stderr);
  assert.match(result.stderr, /安全|時間切れ/);
  assert.doesNotMatch(
    result.stderr,
    /停止印が消失|有効な停止印|update-should-not-run/,
  );
});

test('導入完了時も既存の無期限手動停止を自動解除しない', () => {
  const source = readFileSync(
    'deploy/clients/bootstrap-ubuntu-start.sh',
    'utf8',
  );
  const block = source.slice(
    source.indexOf('step_resume() {'),
    source.indexOf('\nstep_prerequisites\n'),
  );
  const script = `drained=1; indefinite=1; host=pc\ndesk_drain() { echo unexpected-resume; }\nskip() { :; }; ok() { :; }\n${block}\nstep_resume`;
  const result = spawnSync('bash', ['-c', script], { encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  assert.doesNotMatch(result.stdout, /unexpected-resume/);
});
