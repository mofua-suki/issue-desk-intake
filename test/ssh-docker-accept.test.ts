import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

const rejected = [
  ['9999\n', /許可された検証番号/],
  ['6355X', /区切りが不正/],
  ['6355\nextra', /許可された検証番号/],
  ['6293\nwrong snapshot', /固定アーカイブの指紋が一致しません/],
  ['6358\nwrong snapshot', /固定アーカイブの指紋が一致しません/],
] as const;

for (const [input, message] of rejected) {
  test(
    '隔離Docker検証は不正な入力をDockerに渡す前に拒否する ' +
      JSON.stringify(input),
    () => {
      const result = spawnSync('sh', ['scripts/ssh-docker-accept-host.sh'], {
        input,
        encoding: 'utf8',
        timeout: 10_000,
        env: { ...process.env, PATH: '/usr/bin:/bin' },
      });
      assert.ifError(result.error);
      assert.equal(result.status, 1);
      assert.match(result.stderr, message);
    },
  );
}
