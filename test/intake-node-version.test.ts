import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

// 受付の実行時のNodeは、.env で固定したツールイメージではなく最後の段の基本イメージから決まる。
test('受付イメージの実行時のNodeは deploy/versions.json の版に固定する', () => {
  const versions = JSON.parse(
    fs.readFileSync(path.join(root, 'deploy/versions.json'), 'utf8'),
  );
  const dockerfile = fs.readFileSync(
    path.join(root, 'deploy/nas/Dockerfile'),
    'utf8',
  );
  const finalStage = dockerfile.split(/^FROM\s+/im).at(-1);
  assert.equal(
    finalStage.split('\n')[0].trim(),
    `node:${versions.node}-bookworm-slim AS intake`,
  );
  assert.doesNotMatch(
    finalStage,
    /^COPY\s+--from=\S+\s+\/usr\/local\/bin\/node\b/m,
  );
});
