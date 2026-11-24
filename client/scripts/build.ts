import fs from 'node:fs';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { applicationSource, compileSource } from './source.ts';

const root = fileURLToPath(new URL('../', import.meta.url));

// ページが実行時に読むモジュール。app.js から静的な import・再exportでたどれるものを、近い順に返す（app.js 自身は除く）。
// 型だけのモジュールは import が出力に残らず、sw.js はページが import しないので入らない。
function runtimeModules(files: Map<string, Buffer>): string[] {
  const order = ['app.js'];
  for (const name of order) {
    const bytes = files.get(name);
    if (!bytes) throw Error(`${name}: import 先が出力にありません。`);
    for (const [, next] of bytes
      .toString('utf8')
      .matchAll(/(?:\bfrom\s*|\bimport\s*)['"]\.\/([\w.-]+\.js)['"]/g))
      if (!order.includes(next)) order.push(next);
  }
  return order.slice(1);
}

/** Builds into an explicit destination so verification never replaces a running UI. */
export function buildClient(destination: string = path.join(root, 'dist')): {
  ui: string;
  files: string[];
} {
  const source = path.join(root, 'src/ui');
  const application = applicationSource();
  const { parts } = application;
  const files = new Map<string, Buffer>();
  let attentionReplaced = false;
  const replaceAttention = (text: string, name: string) =>
    text.replace(
      /(\bfrom\s*['"])\.\.\/\.\.\/\.\.\/shared\/src\/desk-attention\.mts(['"])/g,
      (_match, before: string, after: string) => {
        if (name === 'request-classification.ts') attentionReplaced = true;
        return `${before}./desk-attention.js${after}`;
      },
    );
  for (const name of fs.readdirSync(source).sort()) {
    if (name === 'app.ts' || parts.includes(name) || name === 'types.ts')
      continue;
    if (name.endsWith('.ts')) {
      const text = replaceAttention(
        fs.readFileSync(path.join(source, name), 'utf8'),
        name,
      );
      files.set(
        name.replace(/\.ts$/, '.js'),
        Buffer.from(compileSource(text, name)),
      );
    }
  }
  if (!attentionReplaced)
    throw Error(
      'request-classification.ts: desk-attention の置換対象が見つかりません。',
    );
  files.set(
    'desk-attention.js',
    Buffer.from(
      compileSource(
        fs.readFileSync(
          path.join(root, '../shared/src/desk-attention.mts'),
          'utf8',
        ),
        'desk-attention.mts',
      ),
    ),
  );
  for (const name of fs.readdirSync(path.join(root, 'public')).sort()) {
    files.set(name, fs.readFileSync(path.join(root, 'public', name)));
  }
  files.set(
    'app.js',
    Buffer.from(
      compileSource(replaceAttention(application.source, 'app.ts'), 'app.ts'),
    ),
  );
  const typedApi = fs.readFileSync(
    path.join(root, 'src/api/client.ts'),
    'utf8',
  );
  files.set(
    'api-client.js',
    Buffer.from(
      compileSource(
        replaceAttention(typedApi, 'api-client.ts'),
        'api-client.ts',
      ),
    ),
  );
  for (const [name, bytes] of files) {
    if (
      name.endsWith('.js') &&
      /['"`]\.\.?\/(?:[^'"`\r\n]*\/)?shared(?:\/|['"`])/.test(
        bytes.toString('utf8'),
      )
    ) {
      throw Error(`${name}: shared への相対パスが出力に残っています。`);
    }
  }
  // Hash names and bytes in deterministic order, before adding the hash to HTML.
  const hash = createHash('sha256');
  for (const [name, bytes] of [...files].sort(([a], [b]) =>
    a.localeCompare(b, 'en'),
  )) {
    hash.update(`${name}\0${bytes.length}\0`).update(bytes);
  }
  const ui = hash.digest('hex');
  let html = files
    .get('index.html')!
    .toString('utf8')
    .replace('data-ui=""', `data-ui="${ui}"`);
  // app.js が import するモジュールは、取得して初めて次の段の import が分かる（app.js → 子 → 孫）。
  // 先に index.html へ並べ、段を待たずに同時に取得させる。
  const entry = '<script type="module" src="/app.js"></script>';
  if (!html.includes(entry))
    throw Error('index.html: app.js の読み込み位置が見つかりません。');
  const modules = runtimeModules(files);
  html = html.replace(entry, () =>
    [
      ...modules.map((name) => `<link rel="modulepreload" href="/${name}" />`),
      entry,
    ].join('\n    '),
  );
  for (const name of [
    'icon.svg',
    'manifest.webmanifest',
    'style.css',
    'app.js',
    ...modules,
  ]) {
    html = html.replaceAll(`="/${name}"`, `="/v/${ui}/${name}"`);
  }
  files.set('index.html', Buffer.from(html));
  fs.mkdirSync(destination, { recursive: true });
  for (const [name, bytes] of files) {
    if (name === 'index.html') continue;
    fs.writeFileSync(path.join(destination, name), bytes);
    // Relative JS imports stay inside the same immutable version directory.
    if (name !== 'sw.js') {
      const versioned = path.join(destination, 'v', ui, name);
      fs.mkdirSync(path.dirname(versioned), { recursive: true });
      fs.writeFileSync(versioned, bytes);
    }
  }
  fs.writeFileSync(path.join(destination, 'index.html'), html);
  fs.writeFileSync(
    path.join(destination, 'ui-version.json'),
    JSON.stringify({ ui }) + '\n',
  );
  return { ui, files: [...files.keys()].sort() };
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  const result = buildClient();
  console.log(
    `Clientをビルドしました: ${result.files.length}ファイル、画面の版 ${result.ui}`,
  );
}
