// ルートの型検査は moduleResolution: Bundler なので、Node で読めない指定子は scripts/typecheck-root.ts の追加の検査が止める。
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createRequire } from 'node:module';
import type * as TypeScript from 'typescript';
import { specifierProblems } from '../scripts/typecheck-root.ts';

const ts: typeof TypeScript = createRequire(
  path.resolve(import.meta.dirname, '../client/package.json'),
)('typescript');

test('拡張子のない相対指定・無いファイル・type: json の無い JSON の import を止め、Node で読める指定は通す', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'typecheck-root-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  for (const name of ['ok.ts', 'data.json', 'plain.mjs'])
    fs.writeFileSync(path.join(dir, name), name.endsWith('.json') ? '{}' : '');
  const source = [
    "import { a } from './ok.ts';",
    "import { b } from './ok';",
    "import { c } from './missing.ts';",
    "import data from './data.json';",
    "import typed from './data.json' with { type: 'json' };",
    "import type { T } from './types';",
    "export { d } from './plain.mjs';",
    "export * from './other';",
    "const later = await import('./later');",
    "const json = await import('./data.json', { with: { type: 'json' } });",
    "import fs from 'node:fs';",
  ].join('\n');
  const file = path.join(dir, 'main.ts');
  const problems = specifierProblems(
    ts.createSourceFile(file, source, ts.ScriptTarget.ES2024, true),
  ).map((line) => line.replace(/^.*main\.ts:/, ''));
  assert.deepEqual(problems, [
    "2: 相対指定子 './ok' に拡張子がありません（Node では読めません）",
    "3: 相対指定子 './missing.ts' のファイルがありません",
    "4: JSON の import に with { type: 'json' } がありません",
    "8: 相対指定子 './other' に拡張子がありません（Node では読めません）",
    "9: 相対指定子 './later' に拡張子がありません（Node では読めません）",
  ]);
});
