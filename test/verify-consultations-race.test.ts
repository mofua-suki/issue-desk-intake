import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../', import.meta.url));

function checkFailure(t, response, expectedError) {
  const dir = fs.mkdtempSync(
    path.join(os.tmpdir(), 'consultations-race-test-'),
  );
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const temp = path.join(dir, 'temp');
  fs.mkdirSync(temp);
  const preload = path.join(dir, 'fetch.mjs');
  fs.writeFileSync(preload, `globalThis.fetch = async () => ${response};\n`);
  const run = spawnSync(
    process.execPath,
    [
      '--import',
      preload,
      path.join(root, 'scripts/verify-consultations-race.ts'),
    ],
    {
      cwd: path.join(root, 'server'),
      env: { ...process.env, TMPDIR: temp },
      encoding: 'utf8',
    },
  );
  assert.equal(run.error, undefined);
  assert.notEqual(run.status, 0, `${run.stdout}\n${run.stderr}`);
  assert.match(run.stderr, expectedError);
  assert.deepEqual(fs.readdirSync(temp), []);
}

test('相談のrace検証で取得失敗を理由付きで失敗し一時ファイルを片付ける', (t) => {
  checkFailure(
    t,
    "new Response('', { status: 503 })",
    /Cコンパイラーの取得に失敗しました: HTTP 503/,
  );
});

test('相談のrace検証で破損した配布物を理由付きで失敗し一時ファイルを片付ける', (t) => {
  checkFailure(
    t,
    "new Response('corrupt archive')",
    /Cコンパイラーの整合性が一致しません/,
  );
});
