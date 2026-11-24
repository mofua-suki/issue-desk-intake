// 正本から配布用の契約moduleと常設指示の生成範囲を再現する。
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
import {
  deskContract,
  deskContractVersion,
  deskContractHeader,
  contractError,
  contractInstructions,
} from '../shared/src/desk-contract.ts';

const root = path.resolve(import.meta.dirname, '..');
const require = createRequire(path.join(root, 'client/package.json'));
const begin = '<!-- issue-desk:desk-contract:begin -->';
const end = '<!-- issue-desk:desk-contract:end -->';

export async function syncDeskContract(
  directory: string,
  write = false,
): Promise<boolean> {
  const file = path.join(directory, 'chat-instructions.md');
  const current = fs.readFileSync(file, 'utf8');
  const start = current.indexOf(begin);
  const finish = current.indexOf(end);
  if (
    start < 0 ||
    finish < start ||
    current.indexOf(begin, start + begin.length) >= 0 ||
    current.indexOf(end, finish + end.length) >= 0
  )
    throw Error('常設指示の契約生成範囲が不正です');
  const newline = current.includes('\r\n') ? '\r\n' : '\n';
  const block = [
    '',
    '',
    '### 依頼側と受付の共通の取り決め（生成）',
    '',
    ...contractInstructions()
      .split('\n')
      .map((line) => `- ${line}`),
    '',
    '',
  ].join(newline);
  const instructions =
    current.slice(0, start + begin.length) + block + current.slice(finish);
  // 実関数の本体を配布する。引数の型はNodeのtype strippingで消えるため出力側で明示する。
  const implementation = contractError.toString();
  const body = implementation.slice(implementation.indexOf('{'));
  const module = await require('prettier').format(
    `// scripts/generate-desk-contract.ts が shared/src/desk-contract.ts から生成。直接編集しない。
export const deskContract = ${JSON.stringify(deskContract)};
export const deskContractVersion = ${JSON.stringify(deskContractVersion)};
export const deskContractHeader = ${JSON.stringify(deskContractHeader)};
export function contractError(code: string) ${body}
`,
    { parser: 'typescript', singleQuote: true, printWidth: 120 },
  );
  // version宣言の単独行はdeploy/nas/update-intake-batch.shのデータ読取契約。
  const outputs: [string, string][] = [
    [file, instructions],
    [path.join(directory, 'scripts/lib/desk-contract.generated.ts'), module],
  ];
  let matched = true;
  for (const [output, text] of outputs) {
    if (fs.existsSync(output) && fs.readFileSync(output, 'utf8') === text)
      continue;
    matched = false;
    if (write) {
      fs.mkdirSync(path.dirname(output), { recursive: true });
      fs.writeFileSync(output, text);
    }
  }
  return write || matched;
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href
) {
  if (process.argv.slice(2).some((arg) => arg !== '--write'))
    throw Error('使い方: node scripts/generate-desk-contract.ts [--write]');
  if (!(await syncDeskContract(root, process.argv.includes('--write')))) {
    console.error(
      '契約生成物が正本と一致しません。node scripts/generate-desk-contract.ts --write で更新してください。',
    );
    process.exitCode = 1;
  }
}
