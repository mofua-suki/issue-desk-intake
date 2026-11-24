// host-ssh.sh が接続先の出力を扱う部品。秘密らしい行を除いた全文を呼び出し元へ見せ、
// 受付への結果報告には末尾1500文字だけを残す（固定テストの合否は末尾に出る）。
import fs from 'node:fs';
import { pathToFileURL } from 'node:url';

const artifact = /^ISSUE_DESK_DEVELOPMENT_RESULT=[A-Za-z0-9+/]+=*$/;
const secret = /idk_|idh_|PRIVATE KEY|password|secret|token/i;

export const cleanOutput = (text) =>
  text
    .split(/\r?\n/)
    .filter((line) => artifact.test(line) || !secret.test(line))
    .join('\n')
    .trim();

export const reportSummary = (fixed, clean) => {
  const logs = clean
    .split('\n')
    .filter((line) => !artifact.test(line))
    .join('\n');
  return fixed + (logs ? '\n' + Array.from(logs).slice(-1500).join('') : '');
};

// 接続先の操作が最後まで動いて自分の終了コードを verify=<終了コード> で返したか（最後の空でない行で見る）。
// 固定テストの段の時間切れ（124）やシグナル終了（137）を、接続の失敗・時間切れと取り違えないために使う。
export const finishedWith = (text, code) =>
  text
    .split(/\r?\n/)
    .filter((line) => line.trim())
    .at(-1)
    ?.trim() === `verify=${code}`;

// 使い方: node host-ssh-output.ts --finished <接続先の出力> <終了コード>（最後まで動いたなら終了コード0）
// 使い方: node host-ssh-output.ts <接続先の出力> <固定の要約> <整えた出力の書き出し先>
// 整えた全文を書き出し先へ、受付へ送る要約を標準出力へ書く。
if (
  import.meta.url === pathToFileURL(process.argv[1] ?? '').href &&
  process.argv[2] === '--finished'
) {
  process.exitCode = finishedWith(
    fs.readFileSync(process.argv[3], 'utf8'),
    process.argv[4],
  )
    ? 0
    : 1;
} else if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  const [file, fixed, out] = process.argv.slice(2);
  const clean = cleanOutput(fs.readFileSync(file, 'utf8'));
  fs.writeFileSync(out, clean ? clean + '\n' : '');
  process.stdout.write(reportSummary(fixed, clean));
}
