import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

// 分割仕様で使う YAML flow 形式の部分集合を読み、同じ mapping の重複キーを拒否する。
function parseFlow(source) {
  let i = 0;
  function space() {
    while (/\s/.test(source[i] ?? '') && i < source.length) i++;
  }
  function string() {
    const quote = source[i++];
    let value = '';
    while (i < source.length) {
      const ch = source[i++];
      if (ch === quote) {
        if (quote === "'" && source[i] === "'") {
          value += "'";
          i++;
          continue;
        }
        return value;
      }
      if (quote === '"' && ch === '\\') {
        const start = i - 1;
        i++;
        if (source[i - 1] === 'u') i += 4;
        value += JSON.parse('"' + source.slice(start, i) + '"');
      } else value += ch;
    }
    throw Error('文字列が閉じていません');
  }
  function value() {
    space();
    const ch = source[i];
    if (ch === "'" || ch === '"') return string();
    if (ch === '{') {
      i++;
      const out = Object.create(null);
      space();
      while (source[i] !== '}') {
        const key = value();
        space();
        assert.equal(typeof key, 'string');
        assert.equal(source[i++], ':');
        assert.ok(!Object.hasOwn(out, key), `重複キー: ${key}`);
        out[key] = value();
        space();
        if (source[i] !== '}') {
          assert.equal(source[i++], ',');
          space();
        }
      }
      i++;
      return out;
    }
    if (ch === '[') {
      i++;
      const out = [];
      space();
      while (source[i] !== ']') {
        out.push(value());
        space();
        if (source[i] !== ']') {
          assert.equal(source[i++], ',');
          space();
        }
      }
      i++;
      return out;
    }
    const start = i;
    while (i < source.length && !/[\s,\]}:]/.test(source[i])) i++;
    const token = source.slice(start, i);
    if (token === 'true') return true;
    if (token === 'false') return false;
    if (token === 'null' || token === '~') return null;
    if (/^-?(?:0|[1-9]\d*)(?:\.\d+)?$/.test(token)) return Number(token);
    throw Error(`未対応の YAML 値: ${token} at ${start}`);
  }
  const result = value();
  space();
  assert.equal(i, source.length);
  return result;
}

test('出荷する分割 OpenAPI の重複キーを拒み stats の問い合わせを保持する', () => {
  const dir = new URL('../api/openapi/', import.meta.url);
  let stats;
  for (const file of readdirSync(dir).filter((name) =>
    name.endsWith('.yaml'),
  )) {
    const document = parseFlow(readFileSync(new URL(file, dir), 'utf8'));
    if (document.paths?.['/api/stats'])
      stats = document.paths['/api/stats'].get;
  }
  assert.equal(stats.operationId, 'get_api_stats');
  for (const name of ['details', 'project', 'tables'])
    assert.equal(
      stats.parameters.filter((p) => p.name === name).length,
      1,
      name,
    );
  const details = stats.parameters.find((p) => p.name === 'details');
  assert.equal(details.schema.type, 'boolean');
  assert.equal(details.schema.default, false);
  assert.throws(
    () => parseFlow("{'get': {'parameters': [], 'parameters': []}}"),
    /重複キー/,
  );
});

// 契約に使われる型・境界・必須項目を検査し、項目間の関係は別途検証する。
function accepts(schema, value) {
  if (schema.oneOf)
    return schema.oneOf.filter((branch) => accepts(branch, value)).length === 1;
  const types = Array.isArray(schema.type) ? schema.type : [schema.type];
  const type =
    value === null ? 'null' : Array.isArray(value) ? 'array' : typeof value;
  if (
    !types.some(
      (t) =>
        t === type ||
        (t === 'integer' && type === 'number' && Number.isInteger(value)),
    )
  )
    return false;
  if (schema.enum && !schema.enum.includes(value)) return false;
  if (
    type === 'number' &&
    ((schema.minimum !== undefined && value < schema.minimum) ||
      (schema.maximum !== undefined && value > schema.maximum))
  )
    return false;
  if (type === 'object') {
    if ((schema.required ?? []).some((k) => !Object.hasOwn(value, k)))
      return false;
    for (const [key, item] of Object.entries(value)) {
      const property = schema.properties?.[key];
      if (property && !accepts(property, item)) return false;
      if (!property && schema.additionalProperties === false) return false;
      if (
        !property &&
        typeof schema.additionalProperties === 'object' &&
        !accepts(schema.additionalProperties, item)
      )
        return false;
    }
  }
  if (
    type === 'array' &&
    schema.items &&
    !value.every((item) => accepts(schema.items, item))
  )
    return false;
  return true;
}

function filingSchema(node) {
  if (!node || typeof node !== 'object') return undefined;
  if (node.properties?.filingCheck) return node.properties.filingCheck;
  for (const child of Object.values(node)) {
    const found = filingSchema(child);
    if (found) return found;
  }
}

function validFiling(schema, response) {
  if (!accepts(schema, response)) return false;
  const reduction = response.inputReduction;
  if (!reduction) return true;
  return ['before', 'after'].every((name) => {
    const { known, reduced, rate } = reduction[name];
    return (
      reduced <= known &&
      (known === 0 ? rate === null : rate === reduced / known)
    );
  });
}

test('出荷する両仕様で入力縮小率の応答と算術関係を検証する', () => {
  const yaml = parseFlow(
    readFileSync(
      new URL('../api/openapi/70-providers-stats.yaml', import.meta.url),
      'utf8',
    ),
  );
  const json = JSON.parse(
    readFileSync(new URL('../api/openapi.json', import.meta.url), 'utf8'),
  );
  const event = {
    counts: { total: 3 },
    triggers: {},
    outcomes: {},
    diagnostics: { invalidLines: 0 },
  };
  const base = {
    project: 'p',
    unavailable: [],
    before: event,
    after: event,
    reasons: { before: null, after: null },
  };
  const zero = { known: 0, reduced: 0, unknown: 0, rate: null };
  const make = (before, after = zero) => ({
    before,
    after,
    coverage: 'retained_checks_only',
  });
  const roundtrip = (value) => JSON.parse(JSON.stringify(value));
  for (const spec of [yaml, json]) {
    const schema = filingSchema(spec.paths['/api/stats']);
    assert.ok(schema);
    for (const reduction of [
      undefined,
      null,
      make(zero),
      make({ ...zero, unknown: 4 }),
      make({ known: 4, reduced: 1, unknown: 2, rate: 0.25 }),
      make({ known: 2, reduced: 0, unknown: 0, rate: 0 }),
      make({ known: 2, reduced: 2, unknown: 0, rate: 1 }),
    ]) {
      const response = roundtrip({ ...base, inputReduction: reduction });
      assert.ok(validFiling(schema, response), JSON.stringify(response));
    }
    for (const period of [
      { ...zero, known: -1 },
      { ...zero, unknown: 0.5 },
      { ...zero, reduced: -1 },
      { known: 1, reduced: 0, unknown: 0, rate: -0.1 },
      { known: 1, reduced: 1, unknown: 0, rate: 1.1 },
      { known: 1, reduced: 2, unknown: 0, rate: 1 },
      { known: 2, reduced: 1, unknown: 0, rate: 0.4 },
      { ...zero, rate: 0 },
      { known: 1, reduced: 0, unknown: 0, rate: null },
    ])
      assert.equal(
        validFiling(
          schema,
          roundtrip({ ...base, inputReduction: make(period) }),
        ),
        false,
        JSON.stringify(period),
      );
    const failed = roundtrip({
      ...base,
      inputReduction: null,
      unavailable: ['inputReduction before: retained checks unreadable'],
    });
    assert.ok(validFiling(schema, failed));
    assert.match(failed.unavailable[0], /inputReduction.*before.*unreadable/);
    assert.deepEqual(failed.before, event);
    assert.deepEqual(failed.after, event);
    assert.deepEqual(failed.reasons, base.reasons);
  }
});
