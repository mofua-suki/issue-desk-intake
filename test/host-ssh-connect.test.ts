import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const root = path.resolve(import.meta.dirname, '..');
const modulePath = path.join(root, 'deploy/nas-host/host-ssh-connect.sh');

function run(t, mode, input, code = 0) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'host-ssh-connect-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const bin = path.join(dir, 'bin');
  const work = path.join(dir, 'work');
  const publicWork = path.join(dir, 'public');
  fs.mkdirSync(bin);
  fs.mkdirSync(work);
  fs.mkdirSync(publicWork);
  fs.symlinkSync(process.execPath, path.join(bin, 'node'));
  fs.writeFileSync(
    path.join(bin, 'timeout'),
    `#!/bin/sh\nif [ "$MODE" = timeout ]; then shift 3; exec /usr/bin/timeout -k 1 0.2 "$@"; fi\nexec /usr/bin/timeout "$@"\n`,
    { mode: 0o755 },
  );
  fs.writeFileSync(
    path.join(bin, 'ssh'),
    `#!/bin/sh\ncase "$MODE" in\n shell) for arg do last=$arg; done; exec /bin/sh -c "$last" ;;\n forced) cat > "$RECEIVED"; exit "$FORCED_CODE" ;;\n failure) exit 255 ;;\n failure-output) printf '%s\\n' 'Connection closed by authenticating user' 'private key confidential' 'token confidential' >&2; exit 255 ;;\n timeout) exec sleep 10 ;;\nesac\n`,
    { mode: 0o755 },
  );
  const inputFile = path.join(dir, 'input');
  fs.writeFileSync(inputFile, input);
  const reportFile = path.join(dir, 'report');
  const received = path.join(dir, 'received');
  const script = `set -eu
. "$MODULE"
work=$WORK public_work=$PUBLIC_WORK report=1 connection=failed exit_code=null summary=initial ticket=test
api_call() { cp "$work/result" "$REPORT_FILE"; }
trap cleanup EXIT
ssh_timeout=5600 destination=test known_dir=$WORK input=$INPUT repo=$ROOT ts_node=$NODE
run_connection
`;
  const result = spawnSync('sh', ['-c', script], {
    encoding: 'utf8',
    env: {
      ...process.env,
      PATH: `${bin}:/usr/bin:/bin`,
      MODE: mode,
      FORCED_CODE: String(code),
      RECEIVED: received,
      MODULE: modulePath,
      WORK: work,
      PUBLIC_WORK: publicWork,
      REPORT_FILE: reportFile,
      INPUT: inputFile,
      ROOT: root,
      NODE: process.execPath,
    },
  });
  assert.ok(fs.existsSync(reportFile), result.stderr);
  const report = JSON.parse(fs.readFileSync(reportFile, 'utf8'));
  assert.ok(!fs.existsSync(work));
  assert.ok(!fs.existsSync(publicWork));
  assert.match(result.stdout, /結果を報告しました/);
  return { result, report, received, dir };
}

test('固定操作がない通常シェルは入力を実行せず非0を報告して片付ける', (t) => {
  const marker = path.join(os.tmpdir(), `host-ssh-marker-${process.pid}`);
  t.after(() => fs.rmSync(marker, { force: true }));
  const { result, report } = run(t, 'shell', `touch '${marker}'\n`);
  assert.notEqual(result.status, 0);
  assert.equal(report.exitCode, result.status);
  assert.equal(report.connectionResult, 'connected');
  assert.ok(!fs.existsSync(marker));
});

test('強制操作は入力を受け取り成功と失敗の終了結果を保つ', (t) => {
  for (const code of [0, 7]) {
    const input = 'helper-base64\nrunner-base64\narchive payload\n';
    const { result, report, received } = run(t, 'forced', input, code);
    assert.equal(fs.readFileSync(received, 'utf8'), input);
    assert.equal(result.status, code);
    assert.equal(report.exitCode, code);
    assert.equal(report.connectionResult, 'connected');
  }
});

test('接続失敗と時間切れは失敗を報告して一時ファイルを片付ける', (t) => {
  for (const [mode, code] of [
    ['failure', 255],
    ['timeout', 124],
  ]) {
    const { result, report } = run(t, mode, 'input\n');
    assert.equal(result.status, code);
    assert.equal(report.exitCode, code);
    assert.equal(report.connectionResult, 'failed');
    assert.match(report.summary, /接続に失敗または時間切れ/);
  }
});

test('接続失敗時はsshの理由を秘密を除いて呼び出し側へ表示する', (t) => {
  const { result, report } = run(t, 'failure-output', 'input\n');
  const output = result.stdout + result.stderr;
  assert.equal(result.status, 255);
  assert.match(output, /Connection closed by authenticating user/);
  assert.match(result.stderr, /Connection closed by authenticating user/);
  assert.doesNotMatch(output, /private key|token/i);
  assert.match(report.summary, /Connection closed by authenticating user/);
  assert.doesNotMatch(report.summary, /private key|token/i);
});
