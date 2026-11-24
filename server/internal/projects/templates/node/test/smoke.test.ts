// 固定テストの雛形（issue-desk の受付で作成）。最初の機能を実装したら、その機能のテストをこのフォルダーへ加える。
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));

test('the project documents and package manifest are in place', () => {
  const readme = fs.readFileSync(path.join(root, 'README.md'), 'utf8');
  assert.match(readme, /## 設計書の目次/);
  const manifest = JSON.parse(
    fs.readFileSync(path.join(root, 'package.json'), 'utf8'),
  );
  assert.equal(manifest.type, 'module');
});

test('the runtime is Node.js 24 or later', () => {
  assert.ok(Number(process.versions.node.split('.')[0]) >= 24);
});
