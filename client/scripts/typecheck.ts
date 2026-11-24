import fs from 'node:fs';
import path from 'node:path';
import ts from 'typescript';
import { applicationSource, clientRoot } from './source.ts';

const application = applicationSource();
const virtual = path.join(clientRoot, 'src/ui/application.ts');
const included = new Set(['app.ts', ...application.parts, 'sw.ts']);
const ui = path.join(clientRoot, 'src/ui');
const roots = [
  virtual,
  ...fs
    .readdirSync(ui)
    .filter((name) => name.endsWith('.ts') && !included.has(name))
    .map((name) => path.join(ui, name)),
];
const configuration = ts.readConfigFile(
  path.join(clientRoot, 'tsconfig.json'),
  ts.sys.readFile,
);
if (configuration.error)
  throw Error(
    ts.flattenDiagnosticMessageText(configuration.error.messageText, '\n'),
  );
const parsed = ts.parseJsonConfigFileContent(
  configuration.config,
  ts.sys,
  clientRoot,
);
if (parsed.errors.length)
  throw Error(
    parsed.errors
      .map((error) => ts.flattenDiagnosticMessageText(error.messageText, '\n'))
      .join('\n'),
  );
const options: ts.CompilerOptions = parsed.options;
const host = ts.createCompilerHost(options);
const read = host.readFile.bind(host);
const exists = host.fileExists.bind(host);
host.readFile = (file) => (file === virtual ? application.source : read(file));
host.fileExists = (file) => file === virtual || exists(file);
const browser = ts.createProgram(roots, options, host);
const worker = ts.createProgram([path.join(ui, 'sw.ts')], {
  ...options,
  lib: ['lib.es2024.d.ts', 'lib.webworker.d.ts'],
});
const diagnostics = [
  ...ts.getPreEmitDiagnostics(browser),
  ...ts.getPreEmitDiagnostics(worker),
];
for (const diagnostic of diagnostics) {
  let file = diagnostic.file?.fileName ?? '';
  let start = diagnostic.start ?? 0;
  if (file === virtual) {
    const location = application.locations.find(
      (part) => start >= part.start && start < part.end,
    );
    if (location) {
      file = location.file;
      start -= location.start;
    }
  }
  const text = file && fs.existsSync(file) ? fs.readFileSync(file, 'utf8') : '';
  const line = text.slice(0, start).split('\n').length;
  console.error(
    `${path.relative(clientRoot, file)}:${line} TS${diagnostic.code}: ${ts.flattenDiagnosticMessageText(diagnostic.messageText, '\n')}`,
  );
}
if (diagnostics.length) process.exitCode = 1;
else console.log('Clientの型検査が通りました。');
