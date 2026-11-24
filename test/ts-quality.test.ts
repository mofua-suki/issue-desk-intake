import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { checkTypeScriptQuality } from '../scripts/check-ts-quality.ts';

test(
  '実品質道具は結合UIの参照を保持し、未整形と未使用コードを検出する',
  { timeout: 60_000 },
  async (t) => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'ts-quality-fixture-'));
    t.after(() => fs.rmSync(root, { recursive: true, force: true }));
    execFileSync('git', ['init', '-q', root]);
    const write = (file: string, text: string) => {
      const destination = path.join(root, file);
      fs.mkdirSync(path.dirname(destination), { recursive: true });
      fs.writeFileSync(destination, text);
    };
    write('.gitignore', '.tmp/\n');
    write('client/package.json', '{"type":"module","devDependencies":{}}');
    write('client/scripts/app-parts.json', '["app-boot.ts"]');
    write(
      'client/src/ui/app.ts',
      "import { answer } from './answer.js';\nboot();\n",
    );
    write(
      'client/src/ui/app-boot.ts',
      'function boot() {\n  console.log(answer);\n}\n',
    );
    const module = 'client/src/ui/answer.ts';
    const valid = 'export const answer = 42;\n';
    write(module, valid);
    for (const mode of ['format', 'lint', 'exports'] as const) {
      assert.equal(await checkTypeScriptQuality(mode, root), 0, mode);
    }
    write(module, 'export const answer=42');
    assert.equal(await checkTypeScriptQuality('format', root), 1);
    write(module, valid);
    write(
      'client/src/ui/app-boot.ts',
      'function boot() {\n  const unused = 1;\n  console.log(answer);\n}\n',
    );
    assert.equal(await checkTypeScriptQuality('lint', root), 1);
    write(
      'client/src/ui/app-boot.ts',
      'function boot() {\n  console.log(answer);\n}\n',
    );
    write(module, valid + 'export const abandoned = 0;\n');
    assert.equal(await checkTypeScriptQuality('exports', root), 1);
    write(module, valid);
    write('scripts/public-source-send.cjs', "require('./send-helper.cjs');\n");
    write(
      'scripts/run-public-source-jobs.cjs',
      "require('./send-helper.cjs');\n",
    );
    write(
      'scripts/refresh-public-source-mirrors.cjs',
      "require('./send-helper.cjs');\n",
    );
    write('scripts/send-helper.cjs', 'module.exports = 1;\n');
    assert.equal(await checkTypeScriptQuality('exports', root), 0);
    write('scripts/unused-helper.cjs', 'module.exports = 1;\n');
    assert.equal(await checkTypeScriptQuality('exports', root), 1);
    fs.unlinkSync(path.join(root, 'scripts/unused-helper.cjs'));
    write('client/src/ui/orphan.ts', 'export const orphan = 0;\n');
    assert.equal(await checkTypeScriptQuality('exports', root), 1);
    // 未ステージの削除はgit ls-filesに残るが、検査する実ファイルではない。
    execFileSync('git', ['add', 'client/src/ui/orphan.ts'], { cwd: root });
    fs.unlinkSync(path.join(root, 'client/src/ui/orphan.ts'));
    assert.equal(await checkTypeScriptQuality('exports', root), 0);
    // 公開互換入口だけは外部の読取を表し、同じ実装moduleの非公開exportは隠さない。
    write(
      'client/src/ui/request-list.ts',
      "export { retained } from './public-source.js';\n",
    );
    write('client/src/ui/public-source.ts', 'export const retained = 1;\n');
    write('api/types.ts', 'export interface Wire { value: string; }\n');
    assert.equal(await checkTypeScriptQuality('exports', root), 0);
    write(
      'client/src/ui/public-source.ts',
      'export const retained = 1;\nexport const abandoned = 2;\n',
    );
    assert.equal(await checkTypeScriptQuality('exports', root), 1);
    assert.deepEqual(fs.readdirSync(path.join(root, '.tmp')), []);
  },
);

for (const metadata of ['absent', 'unreadable'] as const)
  test(
    `Git管理領域が${metadata}でも実ソースを検査し、cacheだけ除外する`,
    { timeout: 60_000 },
    async (t) => {
      const root = fs.mkdtempSync(
        path.join(os.tmpdir(), 'ts-quality-archive-'),
      );
      const gitDirectory = path.join(root, '.git');
      t.after(() => {
        if (metadata === 'unreadable') fs.chmodSync(gitDirectory, 0o700);
        fs.rmSync(root, { recursive: true, force: true });
      });
      if (metadata === 'unreadable') {
        execFileSync('git', ['init', '-q', root]);
        fs.chmodSync(gitDirectory, 0);
        assert.throws(() => fs.readdirSync(gitDirectory), { code: 'EACCES' });
      }
      const write = (file: string, text: string) => {
        const destination = path.join(root, file);
        fs.mkdirSync(path.dirname(destination), { recursive: true });
        fs.writeFileSync(destination, text);
      };
      write('client/package.json', '{"type":"module","devDependencies":{}}');
      write('client/scripts/app-parts.json', '["app-boot.ts"]');
      write(
        'client/src/ui/app.ts',
        "import { answer } from './answer.js';\nboot();\n",
      );
      write(
        'client/src/ui/app-boot.ts',
        'function boot() {\n  console.log(answer);\n}\n',
      );
      const module = 'client/src/ui/answer.ts';
      write(module, 'export const answer = 42;\n');
      // Ordinary source must remain checked even when .gitignore matches it.
      write('.gitignore', 'client/src/ui/answer.ts\n.tmp/\n');
      for (const directory of [
        'node_modules',
        'client/node_modules',
        '.tmp',
        '.pnpm-store',
        'client/dist',
        'server/bin',
      ])
        write(`${directory}/invalid.ts`, 'not valid TypeScript !!!');
      for (const mode of ['format', 'lint', 'exports'] as const)
        assert.equal(await checkTypeScriptQuality(mode, root), 0, mode);
      write(module, 'export const answer=42');
      assert.equal(await checkTypeScriptQuality('format', root), 1);
      write(module, 'export const answer = 42;\nexport const abandoned = 0;\n');
      assert.equal(await checkTypeScriptQuality('exports', root), 1);
      write(module, 'export const answer = 42;\nconst unused = 0;\n');
      assert.equal(await checkTypeScriptQuality('lint', root), 1);
      write(module, 'export const answer = 42;\n');
      write('shared/src/orphan.ts', 'export const orphan = 1;\n');
      assert.equal(await checkTypeScriptQuality('exports', root), 1);
      fs.unlinkSync(path.join(root, 'shared/src/orphan.ts'));
      for (const mode of ['format', 'lint', 'exports'] as const)
        assert.equal(await checkTypeScriptQuality(mode, root), 0, mode);
    },
  );

test('到達不能コードは結合UIと通常moduleのどちらでも拒否する', async (t) => {
  const root = fs.mkdtempSync(
    path.join(os.tmpdir(), 'ts-quality-unreachable-'),
  );
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const write = (file: string, text: string) => {
    const destination = path.join(root, file);
    fs.mkdirSync(path.dirname(destination), { recursive: true });
    fs.writeFileSync(destination, text);
  };
  write('client/package.json', '{"type":"module","devDependencies":{}}');
  write('client/scripts/app-parts.json', '["app-boot.ts"]');
  write(
    'client/src/ui/app.ts',
    "import { answer } from './answer.js';\nboot();\n",
  );
  const ui = 'client/src/ui/app-boot.ts';
  const module = 'client/src/ui/answer.ts';
  const validUI = 'function boot() { console.log(answer()); }\n';
  const validModule = 'export function answer() { return 42; }\n';
  const errors = t.mock.method(console, 'error', () => {});
  for (const file of [ui, module]) {
    write(ui, validUI);
    write(module, validModule);
    assert.equal(await checkTypeScriptQuality('lint', root), 0, file);
    for (const termination of ['return 1;', 'throw new Error();']) {
      write(
        file,
        file === ui
          ? `function boot() { ${termination} console.log(answer()); }\n`
          : `export function answer() { ${termination} return 42; }\n`,
      );
      errors.mock.resetCalls();
      assert.equal(
        await checkTypeScriptQuality('lint', root),
        1,
        `${file}: ${termination}`,
      );
      assert.ok(
        errors.mock.calls.some((call) =>
          String(call.arguments[0]).includes('no-unreachable'),
        ),
      );
    }
  }
});
