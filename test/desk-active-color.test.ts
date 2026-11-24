import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { execFileSync, spawnSync } from 'node:child_process';

function fixture() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'desk-active-color-'));
  const home = path.join(dir, 'home'),
    bin = path.join(dir, 'bin');
  fs.mkdirSync(home);
  fs.mkdirSync(bin);
  const source = path.resolve('deploy/clients/remote-control/desk-access.sh');
  const wrapper = path.join(home, 'desk');
  fs.writeFileSync(
    wrapper,
    execFileSync('sh', ['-c', '. "$1"; linux_desk_access_body', 'sh', source]),
  );
  fs.chmodSync(wrapper, 0o700);
  const docker = path.join(bin, 'docker');
  fs.writeFileSync(
    docker,
    `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
fs.appendFileSync(process.env.CALL_LOG, JSON.stringify(args) + '\\n');
if (args[0] === 'cp' || args.some(a => a.includes('worker-colors'))) process.exit(9);
const blue = 'b'.repeat(12), green = 'c'.repeat(12);
if (args[0] === 'ps') {
 const prefix = 'label=com.docker.compose.service=';
 const service = args.find(a => a.startsWith(prefix)).slice(prefix.length);
 const wanted = service.endsWith('-green') ? green : blue;
 if (process.env.ABSENT !== service) {
  console.log(wanted);
  if (process.env.DUPLICATE === '1') console.log('d'.repeat(12));
 }
} else if (args[0] === 'inspect') {
 const template = args[2];
 if (template.includes('.Config.Image')) {
  const selected = args.at(-1) === green ? 'green' : 'blue';
  let volume = 'issue-desk-worker-state' + (selected === 'green' ? '-green' : '');
  if (process.env.WRONG_VOLUME === '1') volume = 'other-state';
  console.log('fixture:' + selected + '|volume:' + volume + '|true');
 } else process.exit(9);
} else if (args[0] === 'run') {
 fs.writeFileSync(process.env.RUN_LOG, JSON.stringify(args));
 console.log('{}');
} else process.exit(9);
`,
  );
  fs.chmodSync(docker, 0o700);
  const log = path.join(dir, 'run.json'),
    calls = path.join(dir, 'calls.jsonl');
  const env = {
    ...process.env,
    HOME: home,
    PATH: bin + ':' + process.env.PATH,
    RUN_LOG: log,
    CALL_LOG: calls,
  };
  return { dir, home, wrapper, log, calls, env };
}
test('窓口は色の状態やgreenの稼働に関係なくblueを選ぶ', () => {
  const f = fixture();
  try {
    for (const extra of [
      { MISSING_COLOR: '1', WRONG_COLORS: '1' },
      {},
      { ABSENT: 'issue-desk-client-green' },
    ]) {
      fs.rmSync(f.log, { force: true });
      fs.rmSync(f.calls, { force: true });
      const result = spawnSync(
        'sh',
        [f.wrapper, 'doctor', '--path', '/api/meta'],
        { cwd: f.home, env: { ...f.env, ...extra }, encoding: 'utf8' },
      );
      assert.equal(result.status, 0, result.stderr);
      const calls = fs
        .readFileSync(f.calls, 'utf8')
        .trim()
        .split('\n')
        .map((line) => JSON.parse(line));
      assert.ok(
        calls.every(
          (call) =>
            call[0] !== 'cp' &&
            call.every((arg) => !arg.includes('worker-colors')),
        ),
      );
      const args = JSON.parse(fs.readFileSync(f.log, 'utf8'));
      assert.ok(args.includes('fixture:blue'));
      assert.ok(args.includes('issue-desk-worker-state:/state:ro'));
      assert.deepEqual(args.slice(-5), [
        'doctor',
        '--config',
        '/state/config.local.json',
        '--path',
        '/api/meta',
      ]);
    }
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});
test('blueの稼働なし・重複・state不整合は実行しない', () => {
  const f = fixture();
  try {
    for (const extra of [
      { DUPLICATE: '1' },
      { WRONG_VOLUME: '1' },
      { ABSENT: 'issue-desk-client' },
    ]) {
      fs.rmSync(f.log, { force: true });
      const result = spawnSync('sh', [f.wrapper, 'doctor'], {
        cwd: f.home,
        env: { ...f.env, ...extra },
        encoding: 'utf8',
      });
      assert.notEqual(result.status, 0);
      assert.equal(fs.existsSync(f.log), false);
    }
  } finally {
    fs.rmSync(f.dir, { recursive: true, force: true });
  }
});
