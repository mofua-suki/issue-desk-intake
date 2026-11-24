import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import ts from 'typescript';

export const clientRoot = fileURLToPath(new URL('../', import.meta.url));
interface SourcePart {
  file: string;
  start: number;
  end: number;
}

export function applicationSource(root = clientRoot) {
  const directory = path.join(root, 'src/ui');
  const parts: string[] = JSON.parse(
    fs.readFileSync(path.join(root, 'scripts/app-parts.json'), 'utf8'),
  );
  if (
    parts.length !== new Set(parts).size ||
    parts.some((name) => !/^app-[\w-]+\.ts$/.test(name))
  ) {
    throw Error('画面の結合順が不正です。');
  }
  let source = '';
  const locations: SourcePart[] = [];
  for (const name of ['app.ts', ...parts]) {
    const file = path.join(directory, name);
    let text = fs.readFileSync(file, 'utf8');
    if (name === 'app.ts') {
      if (!/boot\(\);\s*$/.test(text))
        throw Error('画面の起動位置を確認できません。');
      text = text.replace(/boot\(\);\s*$/, '');
    }
    locations.push({
      file,
      start: source.length,
      end: source.length + text.length,
    });
    source += text + '\n';
  }
  return { source: source + 'boot();\n', parts, locations };
}

export function compileSource(source: string, file: string): string {
  const result = ts.transpileModule(source, {
    fileName: file,
    reportDiagnostics: true,
    compilerOptions: {
      target: ts.ScriptTarget.ES2024,
      module: ts.ModuleKind.ES2022,
      verbatimModuleSyntax: true,
      // 配信する JS にコメントは要らない（結合元の約2割）。行の位置は typecheck が元の TS で示す。
      removeComments: true,
    },
  });
  const errors = (result.diagnostics ?? []).filter(
    (item) => item.category === ts.DiagnosticCategory.Error,
  );
  if (errors.length)
    throw Error(
      `${file}: ${errors.map((item) => ts.flattenDiagnosticMessageText(item.messageText, '\n')).join('\n')}`,
    );
  return result.outputText;
}
