// 手書きのスクリプトの「直接実行されたか」の判定（scripts/lib/invoked.ts の invokedAs）。
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { invokedAs } from '../scripts/lib/invoked.ts';

test('入口が同じファイル（リンク経由を含む）のときだけ真になる', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'invoked-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'tool.mjs');
  fs.writeFileSync(file, '');
  const other = path.join(dir, 'other.mjs');
  fs.writeFileSync(other, '');
  const link = path.join(dir, 'link.mjs');
  fs.symlinkSync(file, link);
  const url = pathToFileURL(file).href;

  assert.equal(invokedAs(url, file), true);
  assert.equal(invokedAs(url, link), true);
  assert.equal(invokedAs(url, path.relative(process.cwd(), file)), true);
  assert.equal(invokedAs(url, other), false);
  assert.equal(invokedAs(url, path.join(dir, 'missing.mjs')), false);
  assert.equal(invokedAs(url, ''), false);
});

test('入口の引数が無い（process.argv[1] が undefined）ときは偽になる', (t) => {
  const saved = process.argv[1];
  t.after(() => {
    process.argv[1] = saved;
  });
  process.argv[1] = undefined;
  const url = import.meta.url;
  assert.equal(invokedAs(url), false);
  assert.equal(invokedAs(url, undefined), false);
  assert.equal(invokedAs(url, undefined, { ignoreCase: true }), false);
});

test('ignoreCase は大文字小文字の違う入口も同じファイルとみなす', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'invoked-case-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'tool.mjs');
  fs.writeFileSync(file, '');
  // 名前の大文字小文字だけが違う入口。大文字小文字を区別するファイルシステムでは別のファイルになる。
  const upper = path.join(dir, 'TOOL.mjs');
  fs.writeFileSync(upper, '');
  const url = pathToFileURL(file).href;
  assert.equal(invokedAs(url, upper, { ignoreCase: true }), true);
  assert.equal(
    invokedAs(url, upper),
    fs.realpathSync.native(upper) === fs.realpathSync.native(file),
  );
});
