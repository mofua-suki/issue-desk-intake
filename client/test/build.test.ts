import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { buildClient } from '../scripts/build.ts';

test('independent builds preserve versioned imports, entry ordering, and all page assets', (t) => {
  const temporary = fs.mkdtempSync(
    path.join(os.tmpdir(), 'intake-client-build-'),
  );
  t.after(() => fs.rmSync(temporary, { recursive: true, force: true }));
  const first = buildClient(path.join(temporary, 'first'));
  const second = buildClient(path.join(temporary, 'second'));
  assert.equal(first.ui, second.ui);
  assert.match(first.ui, /^[a-f0-9]{64}$/);
  const dist = path.join(temporary, 'first');
  const html = fs.readFileSync(path.join(dist, 'index.html'), 'utf8');
  assert.match(html, new RegExp(`data-ui="${first.ui}"`));
  assert.ok(html.includes(`/v/${first.ui}/app.js`));
  const app = fs.readFileSync(path.join(dist, 'app.js'), 'utf8');
  assert.match(app, /boot\(\);\s*$/);
  assert.equal([...app.matchAll(/^boot\(\);$/gm)].length, 1);
  assert.ok(app.indexOf('function api(') < app.indexOf('function home('));
  assert.ok(
    app.includes(
      "requestJson(url, { method, data, csrf: session?.csrf || '' })",
    ),
  );
  assert.ok(app.includes("new EventSource('/api/events')"));
  for (const name of first.files.filter((file) => file.endsWith('.js'))) {
    const source = fs.readFileSync(path.join(dist, name), 'utf8');
    assert.doesNotMatch(source, /\.\.\/\.\.\/(?:server|shared|client)\//);
    for (const match of source.matchAll(
      /(?:from\s*|import\s*)['"](\.\/[\w.-]+\.js)['"]/g,
    )) {
      assert.ok(
        fs.existsSync(path.join(dist, 'v', first.ui, match[1])),
        `${name}: ${match[1]}`,
      );
    }
  }
  for (const name of [
    'sw.js',
    'manifest.webmanifest',
    'icon.svg',
    'style.css',
    'request-classification.js',
    'api-client.js',
  ]) {
    assert.ok(fs.existsSync(path.join(dist, name)), name);
  }
  assert.deepEqual(
    JSON.parse(fs.readFileSync(path.join(dist, 'ui-version.json'), 'utf8')),
    { ui: first.ui },
  );
});

test('index.html は app.js が実行時に読むモジュールを版つきパスで先読みさせる', (t) => {
  const dist = fs.mkdtempSync(path.join(os.tmpdir(), 'intake-client-build-'));
  t.after(() => fs.rmSync(dist, { recursive: true, force: true }));
  const { ui, files } = buildClient(dist);
  const html = fs.readFileSync(path.join(dist, 'index.html'), 'utf8');
  const versioned = `/v/${ui}/`;
  const preloaded = [
    ...html.matchAll(/<link rel="modulepreload" href="([^"]+)"/g),
  ].map((match) => {
    assert.ok(match[1].startsWith(versioned), match[1]);
    return match[1].slice(versioned.length);
  });
  assert.equal(new Set(preloaded).size, preloaded.length);
  // 先読みの指定は、app.js を読む script より前に置く。
  assert.ok(
    html.lastIndexOf('rel="modulepreload"') <
      html.indexOf(`<script type="module" src="${versioned}app.js">`),
  );
  const imports = (name: string) =>
    [
      ...fs
        .readFileSync(path.join(dist, 'v', ui, name), 'utf8')
        .matchAll(/(?:from\s*|import\s*)['"]\.\/([\w.-]+\.js)['"]/g),
    ].map((match) => match[1]);
  // ブラウザーは app.js の import を、子、孫の順にたどって取得する。たどり着くモジュールがすべて先読みに並んでいれば、
  // 段ごとの取得待ちが起きない。
  const reached = new Set<string>();
  const visit = (name: string) => {
    for (const next of imports(name)) {
      if (reached.has(next)) continue;
      reached.add(next);
      visit(next);
    }
  };
  visit('app.js');
  assert.deepEqual([...preloaded].sort(), [...reached].sort());
  // 子（app.js が直接 import）だけでなく、孫（子が import）も含む。
  assert.ok(imports('app.js').includes('request-classification.js'));
  assert.ok(!imports('app.js').includes('desk-attention.js'));
  assert.ok(preloaded.includes('request-classification.js'));
  assert.ok(preloaded.includes('desk-attention.js'));
  // ページが読まないものは先読みしない: app.js 自身、Service Worker、型だけの空モジュール。
  assert.ok(files.includes('sw.js') && files.includes('draft-types.js'));
  for (const name of ['app.js', 'sw.js', 'draft-types.js'])
    assert.ok(!preloaded.includes(name), name);
});
