// host-ssh.sh は PATH を固定するので、出力の整理（host-ssh-output.ts）を動かす node は deploy/nas-host/host-node.sh が選ぶ。
// 型の除去を持たない古い node（NAS本体の /usr/bin/node v18）を選ばず、見つからなければ探した場所と理由を示して止まるかを見る。
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import type { TestContext } from 'node:test';

const root = path.resolve(import.meta.dirname, '..');
const chooser = path.join(root, 'deploy/nas-host/host-node.sh');

function fixture(t: TestContext) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'host-ssh-node-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const log = path.join(dir, 'calls');
  // 古い node の代役: 型の除去の確かめには失敗し、版だけ答える。
  const old = path.join(dir, 'old-node');
  fs.writeFileSync(
    old,
    `#!/bin/sh\nprintf 'old %s\\n' "$*" >> '${log}'\n[ "$1" = --version ] && { echo v18.20.4; exit 0; }\nexit 1\n`,
    { mode: 0o755 },
  );
  const unusable = path.join(dir, 'not-executable');
  fs.writeFileSync(unusable, '', { mode: 0o644 });
  // 新しい node の代役: 実際の Node 24 を呼ぶ。
  const current = path.join(dir, 'node24');
  fs.writeFileSync(
    current,
    `#!/bin/sh\nprintf 'new %s\\n' "$*" >> '${log}'\nexec '${process.execPath}' "$@"\n`,
    { mode: 0o755 },
  );
  const choose = (...candidates: string[]) =>
    spawnSync(
      'sh',
      ['-c', '. "$0"; typescript_node "$@"', chooser, ...candidates],
      { encoding: 'utf8' },
    );
  const calls = () => (fs.existsSync(log) ? fs.readFileSync(log, 'utf8') : '');
  return { dir, old, unusable, current, choose, calls };
}

test('型の除去を持たない古い node を飛ばし、TypeScript を実行できる node を選ぶ', (t) => {
  const f = fixture(t);
  const result = f.choose(path.join(f.dir, 'missing'), f.old, f.current);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, f.current + '\n');
  assert.match(f.calls(), /^old -e /m);
  // 選んだ node で実際に .ts の出力の整理が動く。
  const finished = path.join(f.dir, 'output');
  fs.writeFileSync(finished, 'verify=0\n');
  const run = spawnSync(
    result.stdout.trim(),
    [
      path.join(root, 'deploy/nas-host/host-ssh-output.ts'),
      '--finished',
      finished,
      '0',
    ],
    { encoding: 'utf8' },
  );
  assert.equal(run.status, 0, run.stderr);
});

test('実行できる node が無ければ、探した場所とそれぞれの理由を示して失敗する', (t) => {
  const f = fixture(t);
  const missing = path.join(f.dir, 'missing');
  const result = f.choose(missing, f.unusable, f.old);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '');
  assert.match(result.stderr, /TypeScriptを実行できる node が見つかりません/);
  assert.ok(result.stderr.includes(`${missing}: ありません`), result.stderr);
  assert.ok(
    result.stderr.includes(`${f.unusable}: 実行できません`),
    result.stderr,
  );
  assert.ok(
    result.stderr.includes(
      `${f.old}: TypeScriptを実行できません（v18.20.4。Node 24 以上が必要）`,
    ),
    result.stderr,
  );
});

test('host-ssh.sh は申請の前に、ホームの ~/.local/bin/node を最初の候補として確かめる', (t) => {
  const f = fixture(t);
  const home = path.join(f.dir, 'home');
  fs.mkdirSync(path.join(home, '.local/bin'), { recursive: true });
  fs.copyFileSync(f.current, path.join(home, '.local/bin/node'));
  fs.chmodSync(path.join(home, '.local/bin/node'), 0o755);
  // 登録の無い接続先なので、方針の照合で止まり、受付への申請や接続はしない。
  const run = spawnSync(
    'sh',
    [
      path.join(root, 'deploy/nas-host/host-ssh.sh'),
      '--host',
      'no-such-host',
      '--operation',
      'verify',
      '--reason',
      'test',
    ],
    {
      env: { HOME: home, PATH: '/usr/bin:/bin', TMPDIR: f.dir },
      encoding: 'utf8',
    },
  );
  assert.notEqual(run.status, 0);
  assert.doesNotMatch(run.stderr, /node がありません/);
  assert.match(
    f.calls(),
    /^new -e process\.exit\(process\.features\.typescript/m,
  );
});
