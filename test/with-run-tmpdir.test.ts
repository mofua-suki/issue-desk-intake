import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { writePostgresFixture } from './helpers/test-postgres.ts';

const script = fileURLToPath(
  new URL('../scripts/with-run-tmpdir.sh', import.meta.url),
);

function base(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'with-run-tmpdir-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}
const env = (dir, extra = {}) => ({
  ...process.env,
  TMPDIR: dir,
  ISSUE_DESK_RUN_TMPDIR: '',
  ...extra,
});
const runs = (dir) =>
  fs.readdirSync(dir).filter((name) => name.startsWith('issue-desk-run-'));
const run = (dir, command, extra?) =>
  spawnSync('sh', [script, 'sh', '-c', command], {
    env: env(dir, extra),
    encoding: 'utf8',
  });

test('作業ツリー専用の一時ディレクトリでコマンドを動かし、残った物ごと消して終了コードを返す', (t) => {
  const dir = base(t);
  const result = run(
    dir,
    'printf "%s\\n" "$TMPDIR"; mkdir "$TMPDIR/leftover"; : > "$TMPDIR/leftover/file"; exit 7',
  );
  assert.equal(result.status, 7, result.stderr);
  const used = result.stdout.trim();
  assert.equal(path.dirname(used), dir);
  assert.match(
    path.basename(used),
    /^issue-desk-run-[A-Za-z0-9._-]+-\d+\.[A-Za-z0-9]{6}$/,
  );
  assert.equal(fs.existsSync(used), false);
  assert.deepEqual(runs(dir), []);
});

test('同じ作業ツリーの名前は実行ごとに同じ接頭辞になり、入れ子の呼び出しは親の場所を使う', (t) => {
  const dir = base(t);
  const first = run(dir, 'printf "%s" "$TMPDIR"').stdout;
  const nested = run(
    dir,
    `sh "${script}" sh -c 'printf "%s|%s" "$TMPDIR" "$ISSUE_DESK_RUN_TMPDIR"'`,
  ).stdout;
  const [inner, marker] = nested.split('|');
  assert.equal(inner, marker);
  assert.equal(path.dirname(inner), dir);
  assert.equal(
    path.basename(first).replace(/\.[^.]+$/, ''),
    path.basename(inner).replace(/\.[^.]+$/, ''),
  );
});

test('同じ作業ツリーの持ち主のいない残り物だけを消し、ほかの作業の物は残す', (t) => {
  const dir = base(t);
  const prefix = path
    .basename(run(dir, 'printf "%s" "$TMPDIR"').stdout)
    .replace(/\.[^.]+$/, '');
  const dead = '999999999';
  const stale = path.join(dir, `${prefix}.stale1`);
  const alive = path.join(dir, `${prefix}.alive1`);
  const other = path.join(dir, 'issue-desk-run-other-1.abcdef');
  const unrelated = path.join(dir, 'boot-parallel-abcdef');
  for (const target of [stale, alive, other, unrelated]) fs.mkdirSync(target);
  fs.writeFileSync(path.join(stale, '.owner'), `${dead}\n`);
  fs.writeFileSync(path.join(alive, '.owner'), `${process.pid}\n`);
  fs.writeFileSync(path.join(other, '.owner'), `${dead}\n`);
  const result = run(dir, 'exit 0');
  assert.equal(result.status, 0, result.stderr);
  assert.equal(fs.existsSync(stale), false);
  for (const target of [alive, other, unrelated])
    assert.equal(fs.existsSync(target), true, target);
});

test('TERMを受けたらコマンドを止めて一時ディレクトリを消す', async (t) => {
  const dir = base(t);
  const ready = path.join(dir, 'ready');
  const child = spawn(
    'sh',
    [
      script,
      'sh',
      '-c',
      `trap 'exit 143' TERM; printf "%s" "$TMPDIR" > "${ready}"; while :; do sleep 0.05; done`,
    ],
    { env: env(dir), stdio: 'ignore' },
  );
  t.after(() => {
    if (child.exitCode === null && child.signalCode === null)
      child.kill('SIGKILL');
  });
  const deadline = Date.now() + 10000;
  while (!fs.existsSync(ready) || !fs.readFileSync(ready, 'utf8')) {
    if (Date.now() > deadline) assert.fail('コマンドが起動しなかった');
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  const used = fs.readFileSync(ready, 'utf8');
  assert.equal(fs.existsSync(used), true);
  child.kill('SIGTERM');
  const code = await new Promise((resolve) =>
    child.once('close', (status) => resolve(status)),
  );
  assert.equal(code, 143);
  assert.equal(fs.existsSync(used), false);
  assert.deepEqual(runs(dir), []);
});

test('書込権の無い残り物があっても終了コードを変えず、次の実行も止めない', (t) => {
  const dir = base(t);
  const first = run(
    dir,
    'mkdir -p "$TMPDIR/locked/inner"; chmod 500 "$TMPDIR/locked"; exit 0',
  );
  assert.equal(first.status, 0, first.stderr);
  assert.deepEqual(runs(dir), []);
  const prefix = path
    .basename(run(dir, 'printf "%s" "$TMPDIR"').stdout)
    .replace(/\.[^.]+$/, '');
  const stale = path.join(dir, `${prefix}.locked`);
  fs.mkdirSync(path.join(stale, 'inner'), { recursive: true });
  fs.writeFileSync(path.join(stale, '.owner'), '999999999\n');
  fs.chmodSync(stale, 0o500);
  const next = run(dir, 'echo ran');
  assert.equal(next.status, 0, next.stderr);
  assert.equal(next.stdout.trim(), 'ran');
  assert.equal(fs.existsSync(stale), false);
});

test('持ち主の記録がまだ無い作りたての物と、コマンドだけが生きている物は消さない', (t) => {
  const dir = base(t);
  const prefix = path
    .basename(run(dir, 'printf "%s" "$TMPDIR"').stdout)
    .replace(/\.[^.]+$/, '');
  const fresh = path.join(dir, `${prefix}.fresh1`);
  const orphan = path.join(dir, `${prefix}.orphan`);
  fs.mkdirSync(fresh);
  fs.mkdirSync(orphan);
  fs.writeFileSync(path.join(orphan, '.owner'), `999999999\n${process.pid}\n`);
  const result = run(dir, 'exit 0');
  assert.equal(result.status, 0, result.stderr);
  assert.equal(fs.existsSync(fresh), true);
  assert.equal(fs.existsSync(orphan), true);
});

test('標準入力が閉じていてもコマンドを動かす', (t) => {
  const dir = base(t);
  const result = spawnSync(
    'sh',
    ['-c', `sh "${script}" sh -c 'echo ran' 0<&-`],
    { env: env(dir), encoding: 'utf8' },
  );
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), 'ran');
});

test('検証用DBの停止に失敗したデータを現在と次の実行で保持する', (t) => {
  const dir = base(t);
  const bin = writePostgresFixture(dir);
  const marker = path.join(dir, 'database-directory');
  const postgres = fileURLToPath(
    new URL('../scripts/with-test-postgres.ts', import.meta.url),
  );
  const result = spawnSync(
    'sh',
    [script, process.execPath, postgres, '--', 'true'],
    {
      env: env(dir, {
        ISSUE_DESK_TEST_POSTGRES_DSN: '',
        ISSUE_DESK_TEST_POSTGRES_BIN: bin,
        VERIFY_PG_CALLS: path.join(dir, 'commands'),
        VERIFY_PG_DIRECTORY_MARKER: marker,
        VERIFY_PG_STOP_EXIT: '1',
      }),
      encoding: 'utf8',
    },
  );
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr + result.stdout, /データは保持しています/);
  const database = fs.readFileSync(marker, 'utf8').trim();
  const pid = path.join(database, 'data', 'postmaster.pid');
  assert.ok(fs.existsSync(pid));
  assert.equal(run(dir, 'true').status, 0);
  assert.ok(fs.existsSync(pid));
  fs.unlinkSync(pid);
  assert.equal(run(dir, 'true').status, 0);
  assert.ok(!fs.existsSync(database));
});
