// ルートの TypeScript（scripts・shared・test・deploy・client の scripts と test・雛形など）を型検査する。出力はしない。
// TypeScript と Node の型は client の依存（client/node_modules）を使う。対象と設定はルートの tsconfig.json。
// ルートに package.json（"type": "module"）を置かないので、NodeNext では .ts が CommonJS と見なされて検査できない。
// そのため moduleResolution は Bundler にし、Bundler が通してしまう Node で動かない指定子（拡張子なしの相対指定、
// type: 'json' の無い JSON の import）は、下の import の検査で止める。
import fs from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import type * as TypeScript from 'typescript';

const root = fileURLToPath(new URL('../', import.meta.url));
const ts: typeof TypeScript = createRequire(
  path.join(root, 'client/package.json'),
)('typescript');

// Node（型の除去）で実行できる相対指定子か。拡張子を省いた指定・無いファイルは Node では読めない。
const runnable = /\.(?:ts|mts|cts|mjs|cjs|js|json)$/;
export function specifierProblems(file: TypeScript.SourceFile): string[] {
  const found: string[] = [];
  const check = (
    node: TypeScript.Node,
    specifier: TypeScript.Expression | undefined,
    attributes: boolean,
    typeOnly: boolean,
  ) => {
    if (!specifier || !ts.isStringLiteralLike(specifier) || typeOnly) return;
    const text = specifier.text;
    if (!text.startsWith('./') && !text.startsWith('../')) return;
    const line =
      file.getLineAndCharacterOfPosition(node.getStart(file)).line + 1;
    const where = `${path.relative(root, file.fileName)}:${line}`;
    if (!runnable.test(text))
      found.push(
        `${where}: 相対指定子 '${text}' に拡張子がありません（Node では読めません）`,
      );
    else if (!fs.existsSync(path.resolve(path.dirname(file.fileName), text)))
      found.push(`${where}: 相対指定子 '${text}' のファイルがありません`);
    else if (text.endsWith('.json') && !attributes)
      found.push(
        `${where}: JSON の import に with { type: 'json' } がありません`,
      );
  };
  const visit = (node: TypeScript.Node) => {
    if (ts.isImportDeclaration(node)) {
      const typeOnly = !!node.importClause?.isTypeOnly;
      const hasJson = !!node.attributes?.elements.some(
        (e) =>
          e.name.text === 'type' &&
          ts.isStringLiteralLike(e.value) &&
          e.value.text === 'json',
      );
      check(node, node.moduleSpecifier, hasJson, typeOnly);
    } else if (ts.isExportDeclaration(node)) {
      const hasJson = !!node.attributes?.elements.some(
        (e) =>
          e.name.text === 'type' &&
          ts.isStringLiteralLike(e.value) &&
          e.value.text === 'json',
      );
      check(node, node.moduleSpecifier, hasJson, node.isTypeOnly);
    } else if (
      ts.isCallExpression(node) &&
      node.expression.kind === ts.SyntaxKind.ImportKeyword
    ) {
      check(node, node.arguments[0], node.arguments.length > 1, false);
    }
    ts.forEachChild(node, visit);
  };
  visit(file);
  return found;
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  const configuration = ts.readConfigFile(
    path.join(root, 'tsconfig.json'),
    ts.sys.readFile,
  );
  if (configuration.error)
    throw Error(
      ts.flattenDiagnosticMessageText(configuration.error.messageText, '\n'),
    );
  const parsed = ts.parseJsonConfigFileContent(
    configuration.config,
    ts.sys,
    root,
  );
  if (parsed.errors.length)
    throw Error(
      parsed.errors
        .map((error) =>
          ts.flattenDiagnosticMessageText(error.messageText, '\n'),
        )
        .join('\n'),
    );
  const program = ts.createProgram(parsed.fileNames, parsed.options);
  const problems: string[] = [];
  for (const diagnostic of ts.getPreEmitDiagnostics(program)) {
    const file = diagnostic.file;
    const where = file
      ? `${path.relative(root, file.fileName)}:${file.getLineAndCharacterOfPosition(diagnostic.start ?? 0).line + 1}`
      : '';
    problems.push(
      `${where} TS${diagnostic.code}: ${ts.flattenDiagnosticMessageText(diagnostic.messageText, '\n')}`,
    );
  }
  const roots = new Set(parsed.fileNames.map((name) => path.resolve(name)));
  for (const file of program.getSourceFiles())
    if (roots.has(path.resolve(file.fileName)))
      problems.push(...specifierProblems(file));
  for (const problem of problems) console.error(problem);
  if (problems.length) {
    console.error(`ルートの型検査で${problems.length}件の指摘があります。`);
    process.exitCode = 1;
  } else
    console.log(
      `ルートの型検査が通りました（${parsed.fileNames.length}ファイル）。`,
    );
}
