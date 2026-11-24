import test from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { sharedClientBuild } from './shared-client-build.ts';

test('画面テストは配布形式の成果物を同じプロセスで共有する', async () => {
  const first = sharedClientBuild();
  const second = sharedClientBuild();
  assert.strictEqual(first, second);
  assert.deepEqual(
    JSON.parse(
      readFileSync(join(first.destination, 'ui-version.json'), 'utf8'),
    ),
    { ui: first.ui },
  );
  assert.ok(
    existsSync(join(first.destination, 'v', first.ui, 'request-list.js')),
  );
  const { requestGroups } = await import(
    pathToFileURL(join(first.destination, 'v', first.ui, 'request-list.js'))
      .href
  );
  assert.equal(
    requestGroups([], [{ id: 'handoff', status: 'project_handoff' }])
      .completed[0].id,
    'handoff',
  );
});

test('配布される一覧の状態順は既知の状態を順に並べ未知の状態を末尾にする', async () => {
  const { destination, ui } = sharedClientBuild();
  const { sortRows, statusOrder } = await import(
    pathToFileURL(join(destination, 'v', ui, 'request-list.js')).href
  );
  assert.equal(statusOrder.includes('project_handoff'), false);
  const rows = [
    { id: 'unknown', status: 'unknown' },
    { id: 'running', status: 'running' },
    { id: 'pending', status: 'pending' },
  ];
  assert.deepEqual(
    sortRows(rows, { field: 'status', direction: 'asc' }).map(
      (row: { id: string }) => row.id,
    ),
    ['pending', 'running', 'unknown'],
  );
  assert.deepEqual(
    sortRows(rows, { field: 'status', direction: 'desc' }).map(
      (row: { id: string }) => row.id,
    ),
    ['running', 'pending', 'unknown'],
  );
});

test('別プロセスは完成した同じ配布物を共有する', async (t) => {
  const { mkdtempSync, rmSync } = await import('node:fs');
  const { tmpdir } = await import('node:os');
  const { spawn } = await import('node:child_process');
  const run = mkdtempSync(join(tmpdir(), 'shared-build-test-'));
  t.after(() => rmSync(run, { recursive: true, force: true }));
  const module = new URL('./shared-client-build.ts', import.meta.url).href;
  const code = `import { sharedClientBuild } from ${JSON.stringify(module)}; console.log(JSON.stringify(sharedClientBuild()));`;
  const launch = () =>
    new Promise<string>((resolve, reject) => {
      const child = spawn(
        process.execPath,
        ['--input-type=module', '-e', code],
        {
          env: { ...process.env, TMPDIR: run, ISSUE_DESK_RUN_TMPDIR: run },
        },
      );
      let output = '';
      let errors = '';
      child.stdout.on('data', (chunk) => {
        output += chunk;
      });
      child.stderr.on('data', (chunk) => {
        errors += chunk;
      });
      child.on('error', reject);
      child.on('close', (status) =>
        status === 0 ? resolve(output) : reject(new Error(errors)),
      );
    });
  const results = (await Promise.all(Array.from({ length: 4 }, launch))).map(
    (s) => JSON.parse(s),
  );
  for (const result of results) {
    assert.deepEqual(result, results[0]);
    assert.deepEqual(
      JSON.parse(
        readFileSync(join(result.destination, 'ui-version.json'), 'utf8'),
      ),
      { ui: result.ui },
    );
    assert.ok(
      existsSync(join(result.destination, 'v', result.ui, 'request-list.js')),
    );
  }
});

test('先行ビルドの強制終了後に途中成果物を公開せず有限時間で失敗する', async (t) => {
  const { mkdtempSync, readdirSync, rmSync } = await import('node:fs');
  const { tmpdir } = await import('node:os');
  const { spawn } = await import('node:child_process');
  const run = mkdtempSync(join(tmpdir(), 'shared-build-failure-'));
  t.after(() => rmSync(run, { recursive: true, force: true }));
  const module = new URL('./shared-client-build.ts', import.meta.url).href;
  const code = `import { sharedClientBuild } from ${JSON.stringify(module)}; sharedClientBuild();`;
  const launch = () =>
    spawn(process.execPath, ['--input-type=module', '-e', code], {
      env: { ...process.env, TMPDIR: run, ISSUE_DESK_RUN_TMPDIR: run },
    });
  const leader = launch();
  const closed = new Promise<void>((resolve) =>
    leader.on('close', () => resolve()),
  );
  while (
    !readdirSync(run).some((name) => name.startsWith('client-build-staging-'))
  ) {
    if (leader.exitCode !== null)
      throw new Error('先行プロセスが途中終了前に終了しました');
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  leader.kill('SIGKILL');
  await closed;
  const follower = launch();
  let errors = '';
  follower.stderr.on('data', (chunk) => {
    errors += chunk;
  });
  const status = await new Promise<number | null>((resolve, reject) => {
    follower.on('error', reject);
    follower.on('close', resolve);
  });
  assert.notEqual(status, 0);
  assert.match(errors, /Timed out waiting for shared client build/);
  assert.equal(existsSync(join(run, 'client-build')), false);
});
