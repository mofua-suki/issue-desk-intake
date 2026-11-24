#!/usr/bin/env bash
# 段⑧。秘密は SSH の標準出力から install.sh の標準入力へ直接渡す。
set +x
set -euo pipefail
umask 077
ok() { printf '[済み] 段⑧ %s\n' "$*"; }
skip() { printf '[飛ばした] 段⑧ %s\n' "$*"; }
stop() { printf '[止めた] 段⑧ %s\n' "$*" >&2; exit 1; }
[[ $(id -u) -ne 0 ]] || stop 'root では実行しません。'
[[ $# -eq 1 ]] || stop '使い方: bootstrap-ubuntu-hostd.sh <作業フォルダー>'
work=$(cd -- "$1" && pwd) || stop '作業フォルダーがありません。'
nas=${BOOTSTRAP_NAS:-user@192.168.0.10}
nas_dir=${BOOTSTRAP_NAS_DIR:-Workspace/issue-desk-intake}
server_url=${BOOTSTRAP_SERVER_URL:-https://issue-desk.example.ts.net/}
ctl=${BOOTSTRAP_SSH_CTL:-}
# 担当表の名前。入口が段②で決めて渡す（単独で叩いたときは OS のホスト名）。
host=${BOOTSTRAP_HOST:-$(hostname | tr '[:upper:]' '[:lower:]')}
[[ $nas =~ ^[A-Za-z0-9][A-Za-z0-9._@:-]*$ && $nas_dir =~ ^[A-Za-z0-9._][A-Za-z0-9._/-]*$ &&
   $server_url =~ ^https?://[A-Za-z0-9._:/-]+$ && $host =~ ^[a-z0-9][a-z0-9-]{0,30}$ ]] || stop 'NAS・受付 URL・PC名の形が不正です。'
nas_ssh() {
  if [[ -n $ctl ]]; then
    local status=0
    ssh -F /dev/null -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o "ControlPath=$ctl/ctl" "$nas" "$@" || status=$?
    if [[ $status -ne 0 ]]; then
      printf '%s\n' 'SSH の診断を確認してください。NAS のホスト鍵を確認できない場合は、入口取得の1行で本人が先に鍵を確認・記録してください。' >&2
    fi
    return "$status"
  else
    ssh "$nas" "$@"
  fi
}
intake_exec() { printf 'sh ~/%s/deploy/nas/intake.sh exec -T /usr/local/bin/intake admin --config /state/config.local.json --application-dir /app hostd-token %s %s' "$nas_dir" "$1" "$host"; }
src="$work/worker/deploy"
registry=$(sed -n 's/^REGISTRY=//p' "$work/.env" | tail -n 1)
flavor=$(sed -n 's/^FLAVOR=//p' "$work/.env" | tail -n 1)
tag=$(sed -n 's/^ISSUE_DESK_CLIENT_BLUE_TAG=//p' "$work/.env" | tail -n 1)
[[ -n $tag ]] || tag=$(sed -n 's/^ISSUE_DESK_CLIENT_TAG=//p' "$work/.env" | tail -n 1)
image=$registry/issue-desk-client$flavor:$tag
if cmp -s -- "$src/update-linux.sh" "$work/update-linux.sh"; then
  skip 'update-linux.sh は同じ内容'
else
  cp -- "$src/update-linux.sh" "$work/update-linux.sh" || stop 'update-linux.sh を置けません。取得した一式を確認してください。'
  ok 'update-linux.sh を作業フォルダー直下に置いた'
fi
if [[ -x $work/hostd/hostd ]] && crontab -l 2>/dev/null | grep -F "'$work/hostd/hostd' --config '$work/hostd/config.json'" | grep -F '# issue-desk-linux-hostd' >/dev/null &&
  node - "$work/hostd/config.json" "$host" <<'JS'
const fs = require('node:fs');
try { process.exit(JSON.parse(fs.readFileSync(process.argv[2], 'utf8')).host === process.argv[3] ? 0 : 1); }
catch { process.exit(1); }
JS
then
  if [[ -s $work/hostd/token && -s $work/hostd/registry-password ]]; then
    skip 'Linux hostd（本人の crontab・この PC の config・token・registry-password）'
    exit 0
  fi
fi
[[ -f $src/client/linux-hostd/install.sh ]] || stop '取得した一式に linux-hostd/install.sh がありません。'
# 発行CLIは重複とDB障害を区別しない。41は発行失敗、42はパスワード取得失敗。
# 出力を捕捉するのは秘密を読まない install.sh の固定コードだけ。SSH の出力は捕捉しない。
install_once() {
  set +e
  if [[ -s $work/hostd/token && -s $work/hostd/registry-password ]]; then
    { printf '%s\n' "$(cat -- "$work/hostd/token")" "$(cat -- "$work/hostd/registry-password")"; } |
      bash "$src/client/linux-hostd/install.sh" "$work" "$host" "$server_url" "$image"
    statuses=("${PIPESTATUS[@]}")
    set -e
    return 0
  fi
  nas_ssh "$(intake_exec issue) || exit 41; cat ~/.config/issue-desk/registry.password || exit 42; printf '\\n'" </dev/null 2>&3 |
    bash "$src/client/linux-hostd/install.sh" "$work" "$host" "$server_url" "$image"
  statuses=("${PIPESTATUS[@]}")
  set -e
  return 0
}
# install_once の状態は親シェルに残し、表示用の固定コードだけを一時フォルダーで受け取る。
# 秘密の転送用ファイルは作らない。
result_dir=$(mktemp -d)
trap 'rm -rf -- "$result_dir"' EXIT
install_once 3>&2 >"$result_dir/result" 2>"$result_dir/reason"
if [[ ${statuses[0]} -eq 41 ]]; then
  printf '段⑧ %s の hostd トークンを発行できません（同名の有効なトークン、または NAS の共有DB障害）。NAS の稼働を確認してください。%s の既存トークンを失効して発行し直すと、古いトークンの接続は止まります。よければ y を入力してください: ' "$host" "$host" >&2
  answer=''
  read -r answer || true
  [[ ${answer//$'\r'/} == y ]] || stop "$host の失効・再発行は行わずに止めました。"
  nas_ssh "$(intake_exec revoke)" </dev/null >/dev/null || stop 'hostd_token_revoke_failed: NAS の受付を確認してください。'
  ok "$host の既存 hostd トークンを失効させた"
  install_once 3>&2 >"$result_dir/result" 2>"$result_dir/reason"
fi
reason=''
if [[ ${statuses[1]} -ne 0 ]]; then
  # install.sh の固定の理由コードだけを表示し、未知の出力をログへ漏らさない。
  reason=$(grep -E '^(install_arguments: client-dir host intake-url \[hostd-dir\]|crontab_(unavailable|read_failed|write_failed)|flock_unavailable|node_(unavailable|24_required)|invalid_(directory|config)|client_files_missing|cycle_busy|hostd_owner_invalid|token_(input_failed|invalid)|password_(input_failed|empty)|install_prepare_failed)$' "$result_dir/reason" | tail -n 1) || reason=install_failed
fi
if [[ ${statuses[0]} -ne 0 ]]; then
  stop "${reason:+$reason: }hostd_secret_transfer_failed（SSH 終了 ${statuses[0]}）: NAS の SSH・受付・registry.password を確認し、同じ入口を叩き直してください。"
fi
[[ ${statuses[1]} -eq 0 ]] || stop "$reason: 原因を直し、同じ入口を叩き直してください。"
grep -qxF linux_hostd_installed "$result_dir/result" || stop 'install_completion_missing: install.sh の完了を確認できません。'
ok 'linux_hostd_installed'
