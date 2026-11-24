import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { requestPayload } from '../shared/src/request-rules.ts';

test('full_test は正しい形式のリポジトリ名と空の payload だけを受け付ける', () => {
  assert.deepEqual(
    requestPayload('full_test', 'mofua-suki/issue-desk-intake', {}),
    { payload: {} },
  );
  assert.ok(requestPayload('full_test', 'o/r', { extra: true }).problem);
  assert.ok(requestPayload('full_test', 'not-a-repo', {}).problem);
});

test('対象の本番コードに selfRepo と旧リポジトリ名が無い', () => {
  const legacy = /mofua-suki\/issue-desk(?![-A-Za-z0-9_.])/;
  for (const file of [
    'shared/src/request-rules.ts',
    'server/internal/dispatch/validation.go',
    'server/internal/operations/full_tests.go',
  ]) {
    const text = fs.readFileSync(file, 'utf8');
    assert.doesNotMatch(text, legacy, file);
    assert.doesNotMatch(text, /selfRepo/, file);
  }
});
