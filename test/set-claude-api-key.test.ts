import { test } from 'node:test';
import { strict as assert } from 'node:assert';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, chmodSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const docker = `#!/usr/bin/env python3
import os, sys, json
args = sys.argv[1:]
if args[0] == 'compose':
 print('writer' if args[-1].endswith('blue') else '')
elif 'node' in args:
 sys.exit(1 if os.environ['CASE'] == 'writer' else 0)
else:
 data = sys.stdin.read()
 key = 'sk-' + 'ant-' + 'A' * 24
 with open(os.environ['RECORD'], 'a') as f:
  f.write(json.dumps({'args': args, 'input': data, 'envLeak': any(key in x for x in os.environ.values())}) + '\\n')
 if os.environ['CASE'] == 'child':
  print(data, file=sys.stderr)
  sys.exit(1)
`;
const runner = String.raw`
import os, pty, subprocess, sys, termios, time, select, signal, json
case, script = sys.argv[1:]
master, slave = pty.openpty()
before = termios.tcgetattr(slave)
p = subprocess.Popen(['bash', script, 'clear' if case == 'clear' else 'set'], stdin=slave, stdout=slave, stderr=slave, start_new_session=True, preexec_fn=lambda: __import__('fcntl').ioctl(slave, termios.TIOCSCTTY, 0))
out = b''
if case != 'clear':
 deadline = time.monotonic() + 10
 while b': ' not in out and p.poll() is None and time.monotonic() < deadline:
  if select.select([master], [], [], .1)[0]: out += os.read(master, 4096)
 hidden = not bool(termios.tcgetattr(slave)[3] & termios.ECHO)
 if case in ('signal', 'interrupt', 'hangup'):
  os.kill(p.pid, {'signal': signal.SIGTERM, 'interrupt': signal.SIGINT, 'hangup': signal.SIGHUP}[case])
 elif case == 'cancel': os.write(master, b'\x04')
 else:
  key = ('sk-' + 'ant-' + ('B' if case == 'replace' else 'A') * 24) if case != 'invalid' else 'bad'
  if case == 'slow':
   os.write(master, key[:10].encode())
   time.sleep(1.2)
   os.write(master, key[10:].encode() + b'\n')
  else: os.write(master, key.encode() + b'\n')
else: hidden = True
try:
 p.wait(timeout=15)
except subprocess.TimeoutExpired:
 p.kill()
 p.wait()
 raise RuntimeError('timed out in case: ' + case)
while select.select([master], [], [], .1)[0]:
 try: out += os.read(master, 4096)
 except OSError: break
print(json.dumps({'code': p.returncode, 'hidden': hidden, 'restored': before == termios.tcgetattr(slave), 'output': out.decode(errors='replace')}))
`;
function run(cases) {
  const dir = mkdtempSync(join(tmpdir(), 'nas-key-test-'));
  try {
    const bin = join(dir, 'docker');
    const record = join(dir, 'record');
    writeFileSync(bin, docker);
    chmodSync(bin, 0o755);
    writeFileSync(record, '');
    const results = cases.map((c) => {
      const p = spawnSync(
        'python3',
        ['-c', runner, c, resolve('deploy/nas/set-claude-api-key.sh')],
        {
          encoding: 'utf8',
          env: {
            ...process.env,
            PATH: `${dir}:${process.env.PATH}`,
            CASE: c,
            RECORD: record,
          },
        },
      );
      assert.equal(p.status, 0, p.stderr);
      return JSON.parse(p.stdout);
    });
    return { results, records: requireRead(record) };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}
import { readFileSync } from 'node:fs';
function requireRead(path) {
  return readFileSync(path, 'utf8')
    .trim()
    .split('\n')
    .filter(Boolean)
    .map((line) => JSON.parse(line));
}

test('登録・置換・消去は管理CLIに標準入力だけで渡す', () => {
  const { results, records } = run(['set', 'replace', 'clear']);
  const keys = ['A', 'B'].map((c) => 'sk-' + 'ant-' + c.repeat(24));
  assert.equal(records.length, 3);
  for (const [i, result] of results.entries()) {
    assert.equal(result.code, 0);
    assert.ok(result.hidden && result.restored);
    assert.ok(keys.every((k) => !result.output.includes(k)));
    assert.equal(records[i].args.at(-1), i === 2 ? 'clear' : 'set');
    assert.deepEqual(records[i].args, [
      'exec',
      '-i',
      'writer',
      '/usr/local/bin/intake',
      'admin',
      '--config',
      '/state/config.local.json',
      '--application-dir',
      '/app',
      'claude-api-key',
      i === 2 ? 'clear' : 'set',
    ]);
    assert.equal(records[i].input, i === 2 ? '' : keys[i] + '\n');
    assert.equal(records[i].envLeak, false);
    assert.ok(keys.every((k) => !JSON.stringify(records[i].args).includes(k)));
  }
  assert.deepEqual(records[0].args.slice(-3), [
    '/app',
    'claude-api-key',
    'set',
  ]);
});

test('取消し・シグナル・不正入力・書き手確認失敗・子処理失敗は復元して失敗する', () => {
  const { results, records } = run([
    'cancel',
    'signal',
    'interrupt',
    'hangup',
    'invalid',
    'writer',
    'child',
  ]);
  const key = 'sk-' + 'ant-' + 'A'.repeat(24);
  for (const result of results) {
    assert.notEqual(result.code, 0);
    assert.ok(result.hidden && result.restored);
    assert.ok(!result.output.includes(key));
  }
  assert.equal(records.length, 1);
  assert.equal(records[0].args.at(-1), 'set');
});

test('入力途中で待ってもキーを保持して登録する', () => {
  const { results, records } = run(['slow']);
  assert.equal(results[0].code, 0);
  assert.ok(results[0].hidden && results[0].restored);
  assert.equal(records.length, 1);
  assert.equal(records[0].input, 'sk-' + 'ant-' + 'A'.repeat(24) + '\n');
  assert.equal(records[0].envLeak, false);
  assert.ok(!results[0].output.includes(records[0].input.trim()));
});
