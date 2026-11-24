import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';

const routes = JSON.parse(
  fs.readFileSync('deploy/nas/desk-pc-routes.json', 'utf8'),
);
const entries = new Map<string, string[]>(
  routes.map((line) => {
    const [script, ...flags] = line.split('|');
    return [script, flags];
  }),
);
const root = path.resolve('scripts');
// 窓口が動かすのは .ts の入口だけ（権限保護で .mjs のまま残る ssh-*-runner.mjs は窓口から呼ばない）。
const scripts = fs.readdirSync(root).filter((name) => /\.ts$/.test(name));
function sources(file, seen = new Set()) {
  if (
    seen.has(file) ||
    !fs.existsSync(file) ||
    path.basename(file) === 'desk-client.ts'
  )
    return '';
  seen.add(file);
  const text = fs.readFileSync(file, 'utf8');
  let result = text;
  for (const match of text.matchAll(
    /(?:from\s+|import\s*)['"](\.\.?\/[^'"]+)['"]/g,
  )) {
    const target = path.resolve(path.dirname(file), match[1]);
    if (target.startsWith(root + path.sep))
      result += '\n' + sources(target, seen);
  }
  return result;
}

test('一時キー非対応のAPIを呼ぶスクリプトは一覧にある', () => {
  const api = JSON.parse(fs.readFileSync('api/openapi.json', 'utf8'));
  const restricted = Object.entries(api.paths).flatMap(([route, methods]) =>
    Object.entries(methods)
      .filter(
        ([, op]) =>
          Array.isArray(op.security) && !op.security.some((s) => 'cliKey' in s),
      )
      .map(([method]) => ({ route, method: method.toUpperCase() })),
  );
  assert.ok(
    restricted.some(({ route }) => route === '/api/desk-cleanup/inspect'),
  );
  for (const script of scripts) {
    const text =
      sources(path.join(root, script)).replace(
        /\$\{close \? 'close' : 'inspect'\}/g,
        'inspect',
      ) +
      '\n' +
      sources(path.join(root, script)).replace(
        /\$\{close \? 'close' : 'inspect'\}/g,
        'close',
      );
    for (const { route, method } of restricted) {
      const pattern = route
        .split(/(\{[^}]+\})/)
        .map((part) =>
          part.startsWith('{')
            ? '(?:\\$\\{[^}]+\\}|[^/]+)'
            : part.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'),
        )
        .join('');
      const direct = new RegExp(
        `(?:call|send)\\(\\s*['"]${method}['"]\\s*,\\s*['"\\x60]${pattern}(?:[\\x60'"?$]|\\$\\{)`,
      ).test(text);
      if (direct)
        assert.ok(entries.has(script), `${script}: ${method} ${route}`);
    }
  }
});

test('トークン必須の検査と承認項目は一覧に含まれる', () => {
  for (const script of scripts) {
    const text = fs.readFileSync(path.join(root, script), 'utf8');
    if (/noTokenMessage|トークン接続でだけ|運用代行トークン専用/.test(text))
      assert.ok(entries.has(script), script);
    if (/overLimitStatement|implementationLimitRetry/.test(text)) {
      assert.ok(entries.has(script), script);
      for (const flag of [
        '--over-limit-statement',
        '--implementation-limit-retry',
      ])
        assert.ok(entries.get(script).includes(flag), `${script}: ${flag}`);
    }
    if (/applyChoiceApproval/.test(text)) {
      assert.ok(entries.has(script), script);
      // 引数なしの行はスクリプト全体を振り分けるので、記号の回答の項目も通る。
      if (entries.get(script).length)
        for (const flag of ['--approval-question', '--approval-answer'])
          assert.ok(entries.get(script).includes(flag), `${script}: ${flag}`);
    }
  }
});

test('一覧の名前は実在の .ts の入口に当たり、改名前の .mjs の名前では当たらない', () => {
  for (const line of routes) {
    const [script] = line.split('|');
    assert.match(script, /\.ts$/);
    assert.ok(scripts.includes(script), script);
    assert.ok(!entries.has(script.replace(/\.ts$/, '.mjs')), script);
  }
  assert.ok(!entries.has('desk-ops.js'));
});
