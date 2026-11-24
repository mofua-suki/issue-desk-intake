import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { execFileSync } from 'node:child_process';
import {
  buildSpec,
  specMarkdown,
  specModule,
} from '../scripts/generate-consultation-spec.ts';
import {
  intakeRequestSize,
  requestSizeWarning,
} from '../shared/src/intake-request-size.ts';

const assignments = JSON.parse(fs.readFileSync('assignments.json', 'utf8'));

test('公開した相談の仕様は正本と一致する', () => {
  const spec = buildSpec(assignments);
  assert.equal(
    fs.readFileSync('docs/consultation-spec.md', 'utf8'),
    specMarkdown(spec),
  );
  assert.equal(
    fs.readFileSync('shared/src/consultation-spec.generated.ts', 'utf8'),
    specModule(spec),
  );
  assert.equal(
    spec.implementation.runLimit,
    assignments.implementationRunLimit,
  );
  for (const value of Object.values(spec.implementation))
    assert.equal(typeof value, 'number');
});

test('生成仕様は CLI に表示される', () => {
  const spec = buildSpec(assignments);
  const markdown = specMarkdown(spec);
  assert.equal(
    execFileSync(process.execPath, ['scripts/create-issue.ts', '--spec'], {
      encoding: 'utf8',
    }),
    `${markdown}\n`,
  );
});

test('対象ファイルの目安を超えても警告して継続し、資料参照は変更範囲と区別する', () => {
  const files = (n) =>
    Array.from({ length: n }, (_, i) => `server/a${i}.go`).join(' ');
  assert.equal(intakeRequestSize.files, 4);
  assert.equal(requestSizeWarning(files(4)), null);
  for (const count of [5, 16]) {
    assert.match(
      requestSizeWarning(files(count)),
      new RegExp(`${count}件.*送信は続けます`),
    );
  }
  const scoped = `## 変更範囲\n${files(4)} server/a0.go\n## 資料\n${files(16)}`;
  assert.equal(requestSizeWarning(scoped), null);
});
