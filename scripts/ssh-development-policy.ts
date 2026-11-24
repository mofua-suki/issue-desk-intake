// 固定SSHコマンドへ、実行を許すコードのハッシュを埋め込む。
import fs from 'node:fs';
import { createHash } from 'node:crypto';

const hash = (file) =>
  createHash('sha256')
    .update(fs.readFileSync(new URL(file, import.meta.url)))
    .digest('hex');
const quote = (shell) => "/bin/sh -c '" + shell.replaceAll("'", "'\\''") + "'";

// 標準入力の先頭2行（base64）で受け取った2つのファイルを、埋め込んだハッシュと一致したときだけ置いて host を動かす。
// 3行目以降の標準入力は host がそのまま読む。
function pinnedCommand(prefix, host, second) {
  return quote(
    `set -eu; umask 077; work=$(mktemp -d /tmp/${prefix}.XXXXXXXX); trap 'rm -rf "$work"' EXIT; trap 'exit 1' HUP INT TERM; IFS= read -r helper; IFS= read -r runner; printf '%s' "$helper" | base64 -d > "$work/${host}"; printf '%s' "$runner" | base64 -d > "$work/${second}"; printf '%s  %s\\n' '${hash('./' + host)}' "$work/${host}" '${hash('./' + second)}' "$work/${second}" | sha256sum -c - >&2; sh "$work/${host}"`,
  );
}
export function developmentCommand() {
  return pinnedCommand(
    'issue-desk-develop',
    'ssh-development-host.sh',
    'ssh-development-runner.ts',
  );
}
// 固定SSH操作 verify。どのリポジトリでも、呼出元が送る固定テストの指定と作業ツリーを稼働Workerのイメージで検証する。
export function verifyCommand() {
  return pinnedCommand(
    'issue-desk-verify',
    'ssh-verify-host.sh',
    'ssh-verify-runner.ts',
  );
}
