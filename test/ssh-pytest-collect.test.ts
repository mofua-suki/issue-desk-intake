import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

test('固定のpytest比較は異なるアーカイブをDockerに渡す前に拒否する', () => {
  const result = spawnSync('sh', ['scripts/ssh-pytest-collect-host.sh'], {
    input: 'not the fixed archive',
    encoding: 'utf8',
    timeout: 10_000,
    env: { ...process.env, PATH: '/usr/bin:/bin' },
  });
  assert.ifError(result.error);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /固定アーカイブの指紋が一致しません: before/);
});
