import { readFileSync } from 'node:fs';
import assert from 'node:assert/strict';
import test from 'node:test';

const spec = JSON.parse(
  readFileSync(new URL('../api/openapi.json', import.meta.url), 'utf8'),
);
const exceptions = JSON.parse(
  readFileSync(
    new URL('./list-limit-exceptions.json', import.meta.url),
    'utf8',
  ),
);
const named = [
  '/api/issues/search',
  '/api/todos',
  '/api/close-requests',
  '/api/attention/alerts',
  '/api/v1/jobs',
  '/api/ai-decisions',
  '/api/decisions',
  '/api/cpu-profiles',
  '/api/provider-disables',
  '/api/model-overrides',
];

function resolve(doc, value) {
  const seen = new Set();
  while (value?.$ref) {
    const ref = value.$ref;
    assert.ok(ref.startsWith('#/'), `ローカル参照ではありません: ${ref}`);
    assert.ok(!seen.has(ref), `循環参照: ${ref}`);
    seen.add(ref);
    value = ref
      .slice(2)
      .split('/')
      .reduce(
        (node, key) => node?.[key.replaceAll('~1', '/').replaceAll('~0', '~')],
        doc,
      );
    assert.ok(value, `参照先がありません: ${ref}`);
  }
  return value;
}

function hasLimit(doc, pathItem) {
  return [...(pathItem.parameters ?? []), ...(pathItem.get.parameters ?? [])]
    .map((parameter) => resolve(doc, parameter))
    .some(
      (parameter) => parameter.in === 'query' && parameter.name === 'limit',
    );
}

function isList(doc, operation) {
  const response = resolve(doc, operation.responses?.['200']);
  return Object.values<any>(response?.content ?? {}).some((content) => {
    const schema = resolve(doc, content.schema);
    if (schema?.type === 'array' && schema.items) return true;
    return Object.values(schema?.properties ?? {}).some(
      (property) => resolve(doc, property)?.type === 'array',
    );
  });
}

function validate(doc, entries) {
  assert.ok(Array.isArray(entries), '例外は配列で指定してください');
  const excluded = new Map();
  for (const entry of entries) {
    assert.ok(
      typeof entry.path === 'string' && entry.path,
      '例外の経路が必要です',
    );
    assert.ok(
      typeof entry.reason === 'string' && entry.reason.trim(),
      `${entry.path}: 例外の理由が必要です`,
    );
    if ('removeWhen' in entry)
      assert.ok(
        typeof entry.removeWhen === 'string' && entry.removeWhen.trim(),
        `${entry.path}: 外す条件が空です`,
      );
    assert.ok(!excluded.has(entry.path), `${entry.path}: 例外が重複しています`);
    const pathItem = doc.paths[entry.path];
    assert.ok(pathItem?.get, `${entry.path}: GET が仕様にありません`);
    assert.ok(
      !hasLimit(doc, pathItem),
      `${entry.path}: limit があるため例外を削除してください`,
    );
    excluded.set(entry.path, entry);
  }
  for (const [path, pathItem] of Object.entries<any>(doc.paths)) {
    if (pathItem.get && isList(doc, pathItem.get)) {
      assert.ok(
        hasLimit(doc, pathItem) || excluded.has(path),
        `${path}: limit または理由つき例外が必要です`,
      );
    }
  }
  const drafts = doc.paths['/api/drafts'];
  assert.ok(drafts?.get, '/api/drafts: GET が仕様にありません');
  assert.ok(hasLimit(doc, drafts), '/api/drafts: limit が必要です');
  assert.ok(!excluded.has('/api/drafts'), '/api/drafts: 例外にできません');
  for (const path of named) {
    const item = doc.paths[path];
    assert.ok(item?.get, `${path}: GET が仕様にありません`);
    assert.notEqual(
      hasLimit(doc, item),
      excluded.has(path),
      `${path}: limit または理由つき例外のいずれかが必要です`,
    );
  }
}

test('出荷する OpenAPI の一覧 GET は limit または理由つき例外を持つ', () => {
  validate(spec, exceptions);
});

test('参照された query と配列応答を判定し、古い例外や不足を拒否する', () => {
  const copy = structuredClone(spec);
  const path = '/api/draft-withdrawals';
  copy.components.parameters ??= {};
  copy.components.parameters.GuardLimit = { name: 'limit', in: 'query' };
  copy.paths[path].parameters = [
    { $ref: '#/components/parameters/GuardLimit' },
  ];
  assert.throws(() => validate(copy, exceptions), /limit があるため例外を削除/);
  const remaining = exceptions.filter((entry) => entry.path !== path);
  assert.doesNotThrow(() => validate(copy, remaining));
  delete copy.paths[path].parameters;
  assert.throws(
    () => validate(copy, remaining),
    /limit または理由つき例外が必要/,
  );
  for (const detail of [
    '/api/v1/jobs/{id}',
    '/api/drafts/{id}',
    '/api/todos/{id}',
    '/api/diagnostics/db',
  ]) {
    assert.throws(
      () =>
        validate(
          spec,
          exceptions.filter((entry) => entry.path !== detail),
        ),
      /limit または理由つき例外が必要/,
    );
  }
  assert.throws(
    () => validate(spec, [...exceptions, { path: '/missing', reason: '理由' }]),
    /仕様にありません/,
  );
  assert.throws(
    () =>
      validate(
        spec,
        exceptions.map((entry) =>
          entry.path === path ? { ...entry, reason: '  ' } : entry,
        ),
      ),
    /理由が必要/,
  );
});
