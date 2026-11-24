import fs from 'node:fs';
import path from 'node:path';
import { createHash } from 'node:crypto';

const root = path.resolve(import.meta.dirname, '../api');
const mode = process.argv[2];
if (process.argv.length > 3 || (mode && !['--write', '--print'].includes(mode)))
  throw Error('使い方: node scripts/api-manifest.ts [--print|--write]');
if (mode === '--write') {
  console.error(
    'api/manifest.json は廃止。削除し、再生成は不要です。API定義と api/openapi.json だけを変更してください。',
  );
  process.exit(1);
}
const contract = JSON.parse(
  fs.readFileSync(path.join(root, 'contract.json'), 'utf8'),
);
if (!/^[a-f0-9]{64}$/.test(contract.version)) throw Error('契約の版が不正です');
if (!mode && fs.readdirSync(root).includes('manifest.json')) {
  console.error('api/manifest.json は廃止。削除し、再生成は不要です。');
  process.exit(1);
}
if (mode === '--print') {
  const files = [
    'contract.json',
    'openapi.json',
    ...fs
      .readdirSync(path.join(root, 'openapi'))
      .filter((name) => name.endsWith('.yaml'))
      .sort()
      .map((name) => 'openapi/' + name),
  ];
  const manifest = {
    contractVersion: contract.version,
    files: files.map((file) => ({
      path: file,
      sha256: createHash('sha256')
        .update(fs.readFileSync(path.join(root, file)))
        .digest('hex'),
    })),
  };
  console.log(JSON.stringify(manifest, null, 2));
}
