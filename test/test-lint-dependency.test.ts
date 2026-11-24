import assert from 'node:assert/strict';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

test('依存未導入で検査を起動すると導入方法を案内する', () => {
  const root = mkdtempSync(path.join(tmpdir(), 'test-lint-dependency-'));
  try {
    mkdirSync(path.join(root, 'test'));
    mkdirSync(path.join(root, 'client'));
    copyFileSync(
      new URL('./test-lint.test.ts', import.meta.url),
      path.join(root, 'test/test-lint.test.ts'),
    );
    copyFileSync(
      new URL('../client/package.json', import.meta.url),
      path.join(root, 'client/package.json'),
    );
    const result = spawnSync(
      process.execPath,
      [path.join(root, 'test/test-lint.test.ts')],
      { encoding: 'utf8' },
    );
    assert.equal(result.status, 1, result.stderr);
    assert.match(
      result.stdout + result.stderr,
      /client\/ で pnpm install --frozen-lockfile を実行/,
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
