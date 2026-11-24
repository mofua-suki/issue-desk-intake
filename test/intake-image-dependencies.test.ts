import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

// Reproduce context COPYs to /app in the final stage, without access to the checkout.
function imageTree(context, destination, dockerfile) {
  const finalStage = dockerfile.split(/^FROM\s+/im).at(-1);
  for (const line of finalStage.split('\n')) {
    if (!/^COPY\s/i.test(line) || /\s--from[=\s]/.test(line)) continue;
    const words = line
      .trim()
      .split(/\s+/)
      .slice(1)
      .filter((word) => !word.startsWith('--'));
    const target = words.pop();
    if (!target.startsWith('/app/')) continue;
    for (const source of words) {
      const src = path.join(context, source);
      const dest = path.join(
        destination,
        target.slice('/app/'.length),
        target.endsWith('/') && !source.endsWith('/')
          ? path.basename(source)
          : '',
      );
      fs.mkdirSync(path.dirname(dest), { recursive: true });
      fs.cpSync(src, dest, { recursive: true });
    }
  }
}

function files(dir) {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const name = path.join(dir, entry.name);
    return entry.isDirectory() ? files(name) : [name];
  });
}

// Resolve literal local imports (including dynamic imports and re-exports), and
// literal URL-based resources from every script. Do not execute management commands.
function missingDeployDependencies(sourceRoot, imageRoot) {
  const problems = [];
  for (const entry of files(path.join(sourceRoot, 'scripts')).filter((f) =>
    /\.(?:mjs|js|ts)$/.test(f),
  )) {
    const visited = new Set();
    function visit(file) {
      if (visited.has(file) || !fs.existsSync(file)) return;
      visited.add(file);
      const text = fs.readFileSync(file, 'utf8');
      const imports = /(?:\bfrom\s*|\bimport\s*(?:\(\s*)?)["'](\.[^"']+)["']/g;
      for (const match of text.matchAll(imports)) {
        const dependency = path.resolve(path.dirname(file), match[1]);
        if (/\.(?:mjs|js|ts)$/.test(dependency)) visit(dependency);
      }
      const urls =
        /new\s+URL\s*\(\s*["']([^"']+)["']\s*,\s*import\.meta\.url\s*\)/g;
      for (const match of text.matchAll(urls)) {
        const resource = path.resolve(path.dirname(file), match[1]);
        const relative = path.relative(sourceRoot, resource);
        if (
          relative.startsWith(`deploy${path.sep}`) &&
          !fs.existsSync(path.join(imageRoot, relative))
        ) {
          problems.push(`${path.relative(sourceRoot, entry)}: ${relative}`);
        }
      }
    }
    visit(entry);
  }
  return problems;
}

function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'intake-image-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const image = path.join(dir, 'app');
  fs.mkdirSync(image);
  imageTree(
    root,
    image,
    fs.readFileSync(path.join(root, 'deploy/nas/Dockerfile'), 'utf8'),
  );
  return { dir, image };
}

test('受付イメージの隔離配置で返信CLIが使い方を表示する', (t) => {
  const { image } = fixture(t);
  const result = spawnSync(
    process.execPath,
    [path.join(image, 'scripts/reply-issue.ts'), '--help'],
    {
      cwd: image,
      encoding: 'utf8',
      env: { PATH: process.env.PATH, HOME: image },
    },
  );
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /(?:使い方|Usage)/i);
  assert.doesNotMatch(result.stderr, /ENOENT/);
});

test('全スクリプトの間接deploy依存を隔離配置と照合し欠落を診断する', (t) => {
  const { dir, image } = fixture(t);
  assert.deepEqual(missingDeployDependencies(root, image), []);
  fs.rmSync(path.join(image, 'deploy/versions.json'));

  // A separate dependency through a shared module must also identify its entry script.
  const source = path.join(dir, 'source');
  fs.mkdirSync(path.join(source, 'scripts'), { recursive: true });
  fs.mkdirSync(path.join(source, 'shared/src'), { recursive: true });
  fs.writeFileSync(
    path.join(source, 'scripts/example.mjs'),
    "import '../shared/src/example.mjs';\n",
  );
  fs.writeFileSync(
    path.join(source, 'shared/src/example.mjs'),
    "import fs from 'node:fs';\nfs.readFileSync(new URL('../../deploy/other.json', import.meta.url));\n",
  );
  assert.deepEqual(missingDeployDependencies(source, image), [
    'scripts/example.mjs: deploy/other.json',
  ]);
});
