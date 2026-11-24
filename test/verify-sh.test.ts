import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, writeFile, rm, readdir, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { requiredTests } from '../scripts/verify-review-notes.ts';

const script = resolve('scripts/verify.sh');

async function fixture(t) {
  const dir = await mkdtemp(join(tmpdir(), 'verify-test-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const command = async (name, body) => {
    const path = join(dir, name);
    await writeFile(path, `#!/bin/sh\n${body}\n`, { mode: 0o755 });
    return path;
  };
  const node = await command(
    'node',
    `if [ "${'$'}{2:-}" = --normal-go ]; then exec "${process.execPath}" "${'$'}@"; fi
echo fake-node "${'$'}@"`,
  );
  await writeFile(
    join(dir, 'go-events'),
    requiredTests
      .filter(([pkg]) => !pkg.endsWith('/consultations'))
      .map(([Package, Test]) =>
        JSON.stringify({ Action: 'pass', Package, Test }),
      )
      .join('\n') + '\n',
  );
  const pnpm = await command('pnpm', 'echo fake-client');
  const gofmt = await command('gofmt', 'exit 0');
  const go = await command(
    'go',
    `case "$1" in run) exit 0 ;; esac
case " $* " in
  " list ./... ") printf '%s\\n' issue-desk-intake/server/internal/other; exit 0 ;;
  *" test -json -count=1 -timeout 8m "*)
    printf '%s\\n' "$@" > "$FAKE_DIR/go-args"
    if [ "\${FAKE_GO_OK:-}" = 1 ]; then cat "$FAKE_DIR/go-events"; exit 0; fi
    printf '%s\\n' '{"Action":"output","Package":"issue-desk-intake/server/internal/other","Output":"fake-go test issue-desk-intake/server/internal/other\\n"}' '{"Action":"fail","Package":"issue-desk-intake/server/internal/other","Test":"TestFakeServer"}'
    if [ "\${BLOCK_GO:-}" = 1 ]; then exec sleep 300; fi
    exit 1 ;;
  *) echo fake-go "$@" ;;
esac`,
  );
  return {
    dir,
    env: {
      ...process.env,
      NODE: node,
      PNPM: pnpm,
      GO: go,
      GOFMT: gofmt,
      ISSUE_DESK_TEST_POSTGRES_DSN:
        'postgresql://fixture@localhost/issue_desk_test',
      TMPDIR: dir,
      ISSUE_DESK_RUN_TMPDIR: dir,
      FAKE_DIR: dir,
    },
  };
}

function run(args, env) {
  const child = spawn('sh', [script, ...args], {
    env,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let output = '';
  child.stdout.on('data', (data) => {
    output += data;
  });
  child.stderr.on('data', (data) => {
    output += data;
  });
  const done = new Promise<any>((resolveDone, reject) => {
    child.on('error', reject);
    child.on('close', (code, signal) => resolveDone({ code, signal, output }));
  });
  return { child, done, getOutput: () => output };
}

async function interrupted(t) {
  const { dir, env } = await fixture(t);
  const { child, done, getOutput } = run([], { ...env, BLOCK_GO: '1' });
  t.after(() => {
    if (child.exitCode === null && child.signalCode === null)
      child.kill('SIGTERM');
  });
  // Wait for the Go child output to reach the server log before interrupting.
  const deadline = performance.now() + 30000;
  let observed = '';
  for (;;) {
    if (child.exitCode !== null || child.signalCode !== null)
      throw new Error(
        `verify exited before interruption: ${JSON.stringify(await done)}`,
      );
    if (performance.now() >= deadline) {
      child.kill('SIGTERM');
      throw new Error(
        `timed out waiting for verify stages; output: ${getOutput()}\n${observed}`,
      );
    }
    const dirs = (await readdir(dir)).filter((name) =>
      name.startsWith('issue-desk-verify.'),
    );
    if (dirs.length) {
      const logs = join(dir, dirs[0]);
      const server = await readFile(join(logs, 'server'), 'utf8').catch(
        () => '',
      );
      const client = await readFile(join(logs, 'client'), 'utf8').catch(
        () => '',
      );
      const go = await readFile(join(logs, 'go'), 'utf8').catch(() => '');
      const state = await readFile(join(logs, 'client-stage'), 'utf8').catch(
        () => 'missing',
      );
      const args = await readFile(join(dir, 'go-args'), 'utf8').catch(() => '');
      observed = `server: ${server}\ngo: ${go}\nclient: ${client}\nclient-stage: ${state}\ngo-args: ${args}`;
      if (
        go.includes('stage=go-test start=') &&
        go.includes('fake-go test issue-desk-intake/server/internal/other') &&
        args &&
        client.includes('stage=root-test end=') &&
        state === ''
      )
        break;
    }
    await delay(20);
  }
  child.kill('SIGTERM');
  return done;
}

test('打ち切り時の出力を両系統から残す', async (t) => {
  const result = await interrupted(t);
  assert.notEqual(result.code, 0);
  assert.match(result.output, /stage=go-test start=/);
  assert.match(
    result.output,
    /fake-go test .*issue-desk-intake\/server\/internal\/other/,
  );
  assert.match(result.output, /fake-client/);
  assert.match(result.output, /stage=root-test end=.*exit=0/);
});

test('実行中の段階名を打ち切り時に一度出す', async (t) => {
  const result = await interrupted(t);
  assert.notEqual(result.code, 0);
  const stages = result.output.match(/^stage=.* interrupted$/gm) ?? [];
  assert.ok(stages.includes('stage=go-test interrupted'));
  assert.equal(
    new Set(stages).size,
    stages.length,
    '同じ段階を重複出力しました',
  );
  assert.ok(
    stages.every((stage) =>
      /^stage=(go-test|consultations-race) interrupted$/.test(stage),
    ),
  );
});

test('go test の timeout が外側の期限より短い', async (t) => {
  const { dir, env } = await fixture(t);
  const result = await run(['--server'], env).done;
  assert.notEqual(result.code, 0);
  assert.match(result.output, /stage=go-test end=.*exit=1/);
  assert.equal(
    (await readdir(dir)).filter((name) =>
      name.startsWith('issue-desk-verify-server.'),
    ).length,
    0,
  );
  const args = (await readFile(join(dir, 'go-args'), 'utf8'))
    .trim()
    .split('\n');
  const index = args.indexOf('-timeout');
  assert.ok(index >= 0);
  const match = /^(\d+)(s|m)$/.exec(args[index + 1]);
  assert.ok(match);
  const seconds = Number(match[1]) * (match[2] === 'm' ? 60 : 1);
  assert.ok(seconds > 0 && seconds < 900);
});

test('固定検証が実行中の段階を終了前に出力する', async (t) => {
  const { env } = await fixture(t);
  const { child, done, getOutput } = run([], {
    ...env,
    BLOCK_GO: '1',
    ISSUE_DESK_VERIFY_PROGRESS_SECONDS: '1',
  });
  t.after(() => {
    if (child.exitCode === null && child.signalCode === null)
      child.kill('SIGTERM');
  });
  const deadline = performance.now() + 10000;
  while (
    !getOutput().includes('stage=progress') &&
    performance.now() < deadline
  )
    await delay(50);
  assert.match(getOutput(), /stage=progress[^\n]*go=go-test/);
  child.kill('SIGTERM');
  const result = await done;
  assert.equal(result.code, 1);
});

test('子の検証へ親の段階とログの書込先を渡さない', async (t) => {
  const { env } = await fixture(t);
  await writeFile(
    env.NODE,
    `#!/bin/sh
case "\${2:-}" in
 --normal-go) exec "${process.execPath}" "$@" ;;
esac
case "$1" in
 --test)
  if [ -n "\${VERIFY_STAGE_FILE:-}\${VERIFY_LOG_DIR:-}" ]; then
   printf '%s\\n' 'parent-stage-leaked'
   [ -z "\${VERIFY_STAGE_FILE:-}" ] || printf '%s\\n' nested > "$VERIFY_STAGE_FILE"
   exit 88
  fi ;;
esac
echo fake-node "$@"
`,
    { mode: 0o755 },
  );
  const result = await run([], env).done;
  assert.match(result.output, /stage=root-test end=.*exit=0/);
  assert.doesNotMatch(result.output, /parent-stage-leaked/);
});

test('失敗したときは失敗した段とテストを client の出力の後ろにまとめて出す', async (t) => {
  const { env } = await fixture(t);
  const result = await run([], env).done;
  assert.equal(result.code, 1);
  const tail = result.output.slice(
    result.output.lastIndexOf('stage=root-test end='),
  );
  assert.match(tail, /失敗の要約:/);
  assert.match(tail, /stage=go-test end=.*exit=1/);
  assert.match(tail, /--- FAIL: TestFakeServer/);
});

test('server だけが失敗したときは server の出力を client の後ろに出す', async (t) => {
  const { env } = await fixture(t);
  const result = await run([], env).done;
  assert.equal(result.code, 1);
  assert.ok(
    result.output.indexOf('stage=root-test end=') <
      result.output.indexOf('stage=go-test end='),
    result.output,
  );
});

test('成功したときは失敗の要約を出さない', async (t) => {
  const { env } = await fixture(t);
  const result = await run([], { ...env, FAKE_GO_OK: '1' }).done;
  assert.equal(result.code, 0, result.output);
  assert.doesNotMatch(result.output, /失敗の要約:/);
});
