# rc-update.sh が . で読み込む定義だけ。Ubuntu の PC に受付の窓口 ~/.issue-desk/desk を置く。
# say・plan・DIR・HOME・DRY_RUN を使う。資料: docs/chat-deploy.md の「Ubuntu の PC の窓口」。
#
# 窓口は、稼働中のblue監視コンテナの
# イメージを使い捨てで起動し、その中の /app/scripts/<スクリプト名> を、blueのstate volumeの
# /state/config.local.json（K2）とそのトークンのファイルで動かす。トークンの写しは作らない（環境変数にはパスだけ）。
# 呼び出しの検査は deploy/nas/update-desk-access.sh の窓口と同じ。違いは、作業ディレクトリがホームの外なら止めること。

# 窓口の sh 本体。内容が変わらない限り書き直さない。
linux_desk_access_body() {
  cat <<'WRAPPER'
#!/bin/sh
# issue-desk-desk: deploy/clients/remote-control/desk-access.sh が書き直すので、手で変えない。
usage() { echo '使い方: ~/.issue-desk/desk doctor|get|operation [引数…] または <受付スクリプト名.ts> [引数…]' >&2; exit 2; }
case "${1:-}" in
'' | *[!A-Za-z0-9._-]* | *..*) usage ;;
esac
# 入口は .ts だけを受け付ける。権限保護で .mjs のまま残る scripts/ssh-*-runner.mjs はホスト作業用SSHの固定操作が動かし、この窓口からは呼ばない。
case "$1" in
*.ts | doctor | get | operation) ;;
*) usage ;;
esac
script=$1
shift
physical_home=$(CDPATH= cd -- "$HOME" 2>/dev/null && pwd -P)
[ -n "$physical_home" ] || { echo 'ホームを確認できません。' >&2; exit 1; }
if [ "$script" = follow-issues.ts ]; then
  state_next=0
  for argument in "$@"; do
    if [ "$state_next" = 1 ]; then
      case "$argument" in /*) ;; *) echo '--state はホームの下の絶対パスを指定してください。' >&2; exit 1;; esac
      state_path=$(realpath -m -- "$argument" 2>/dev/null) || { echo '--state を解決できません。' >&2; exit 1; }
      case "$state_path/" in "${physical_home%/}/"*) ;; *) echo '--state はホームの下に置いてください。' >&2; exit 1;; esac
      ancestor=$state_path
      while [ ! -e "$ancestor" ] && [ ! -L "$ancestor" ]; do
        ancestor=${ancestor%/*}; [ -n "$ancestor" ] || ancestor=/
      done
      if [ -L "$ancestor" ] && ! realpath -e -- "$ancestor" >/dev/null 2>&1; then
        echo '--state のリンクを解決できません。' >&2; exit 1
      fi
      [ -d "$ancestor" ] || ancestor=${ancestor%/*}
      [ -n "$ancestor" ] || ancestor=/
      [ -x "$ancestor" ] && realpath -e -- "$ancestor" >/dev/null 2>&1 || { echo '--state の親を確認できません。' >&2; exit 1; }
      state_next=0
    elif [ "$argument" = --state ]; then state_next=1
    fi
  done
fi
check_visible_path() {
  candidate=$1
  [ -e "$candidate" ] || return 0
  physical_path=$(readlink -f -- "$candidate" 2>/dev/null)
  case "$physical_path/" in
  "${physical_home%/}/"*) return 0 ;;
  esac
  printf '%s はホーム（%s）の外にあり、コンテナから見えません。ホームの下（例: ~/.issue-desk/requests/）に置いてから呼んでください。\n' "$1" "$HOME" >&2
  exit 1
}
for argument in "$@"; do
  case "$argument" in
  --*=*) check_visible_path "${argument#*=}" ;;
  *) check_visible_path "$argument" ;;
  esac
done
check_visible_path "${ISSUE_DESK_TOKEN_FILE:-}"
physical_cwd=$(pwd -P 2>/dev/null)
case "$physical_cwd/" in
"${physical_home%/}/"*) ;;
*)
  printf '作業ディレクトリ %s はホーム（%s）の外にあり、コンテナから見えません。ホームの下に移ってから呼んでください。\n' "$PWD" "$HOME" >&2
  exit 1
  ;;
esac
if ! command -v docker >/dev/null 2>&1; then
  echo '受付の窓口を動かせません: docker が見つかりません。' >&2
  exit 1
fi
# blue の稼働コンテナが一意であり、期待する state を使うことを確認する。
color_error() { echo '受付の窓口を動かせません: blue の image・state volumeを一意に確認できません。' >&2; exit 1; }
state_volume=issue-desk-worker-state
ids=$(docker ps --filter label=com.docker.compose.project=issue-desk-client --filter label=com.docker.compose.service=issue-desk-client --filter status=running --format '{{.ID}}') || color_error
case "$ids" in ''|*[!a-f0-9]*) color_error ;; esac
metadata=$(docker inspect --format '{{.Config.Image}}|{{range .Mounts}}{{if eq .Destination "/state"}}{{.Type}}:{{.Name}}{{end}}{{end}}|{{.State.Running}}' "$ids") || color_error
image=${metadata%%|*}
case "$image" in ''|*[!a-zA-Z0-9._/@:-]*) color_error ;; esac
[ "${metadata#*|}" = "volume:$state_volume|true" ] || color_error
case "$script" in
  doctor|get|operation)
    exec docker run --rm -i --pull never --network host -v "$state_volume:/state:ro" -v "$HOME:$HOME" -e HOME="$HOME" -w "$PWD" --entrypoint /usr/local/bin/desk "$image" "$script" --config /state/config.local.json "$@"
    ;;
esac
chat_source=$HOME/.issue-desk/remote-control/source
if [ -f "$HOME/.issue-desk/scripts-root" ]; then
  IFS= read -r chat_source < "$HOME/.issue-desk/scripts-root" || exit 1
fi
[ -d "$chat_source" ] && [ -f "$chat_source/scripts/desk-client.ts" ] || { echo 'チャット用の受付スクリプトの正本がありません。' >&2; exit 1; }
[ -f "$chat_source/scripts/$script" ] || { echo '未対応の受付スクリプトです。' >&2; exit 2; }
# コンテナの中で動かす sh。K2 から接続先とトークンのファイルを決め、受付のスクリプトを node で動かす。
inner=$(cat <<'INNER'
if [ -z "${ISSUE_DESK_URL:-}" ] || [ -z "${ISSUE_DESK_TOKEN_FILE:-}" ]; then
  cfg=/state/config.local.json
  if [ ! -r "$cfg" ]; then
    echo '受付の窓口を動かせません: 監視の設定（K2、/state/config.local.json）を読めません。' >&2
    exit 1
  fi
  out=$(node -e '
const fs = require("fs");
const path = require("path");
const file = process.argv[1];
let c;
try {
  c = JSON.parse(fs.readFileSync(file, "utf8").replace(/^﻿/, ""));
} catch {
  process.stderr.write("受付の窓口を動かせません: 監視の設定（K2）がJSONとして読めません。\n");
  process.exit(1);
}
const text = (v) => typeof v === "string" && v.trim() !== "";
if (!c || Object.keys(c).length !== 3 || !text(c.serverUrl) || !text(c.serverTokenFile) || !text(c.workerId)) {
  process.stderr.write("受付の窓口を動かせません: 監視の設定（K2）が serverUrl・serverTokenFile・workerId の3項目ではありません。\n");
  process.exit(1);
}
process.stdout.write(c.serverUrl.trim() + "\n" + path.resolve(path.dirname(file), c.serverTokenFile.trim()) + "\n");
' "$cfg") || exit 1
  k2_url=$(printf '%s\n' "$out" | sed -n 1p)
  k2_token=$(printf '%s\n' "$out" | sed -n 2p)
  ISSUE_DESK_URL=${ISSUE_DESK_URL:-$k2_url}
  ISSUE_DESK_TOKEN_FILE=${ISSUE_DESK_TOKEN_FILE:-$k2_token}
fi
if [ ! -f "$ISSUE_DESK_TOKEN_FILE" ] || [ ! -r "$ISSUE_DESK_TOKEN_FILE" ]; then
  echo "受付の窓口を動かせません: トークンのファイル（$ISSUE_DESK_TOKEN_FILE）が無い、または読めません。" >&2
  exit 1
fi
export ISSUE_DESK_URL ISSUE_DESK_TOKEN_FILE
script=$1
shift
exec node "/chat/scripts/$script" "$@"
INNER
)
# deploy/versions.json も渡す（update-desk-scripts.sh の配布条件と同じく、正本がそろっていることの確認）。無いと止める。
[ -f "$chat_source/deploy/versions.json" ] || { echo 'チャット用の受付スクリプトの正本に deploy/versions.json がありません。' >&2; exit 1; }
exec docker run --rm -i --pull never --network host \
  -v "$state_volume:/state:ro" -v "$HOME:$HOME" -v "$chat_source/scripts:/chat/scripts:ro" -v "$chat_source/shared:/chat/shared:ro" -v "$chat_source/deploy/versions.json:/chat/deploy/versions.json:ro" -e HOME="$HOME" \
  -e ISSUE_DESK_URL -e ISSUE_DESK_TOKEN_FILE -w "$PWD" \
  --entrypoint sh "$image" -c "$inner" sh "$script" "$@"
WRAPPER
}

# 許可一覧の Bash の許可と hooks の command に現れる ~/.issue-desk/<名前> の入口が、ホームにあるかを確かめる。
linux_desk_access_entries() {
  da_settings=$DIR/deploy/nas/remote-control-settings.json
  [ -f "$da_settings" ] || return 0
  da_names=$(awk '/"allow"/{a=1;next} /"deny"/{a=0;next} /"command"/{print;next} a && /"Bash\(/{print}' "$da_settings" |
    grep -o '~/\.issue-desk/[A-Za-z0-9._-]*' | sed 's#^~/\.issue-desk/##' | sort -u)
  for da_name in $da_names; do
    if [ ! -f "$HOME/.issue-desk/$da_name" ] || [ ! -x "$HOME/.issue-desk/$da_name" ]; then
      say "[rc-update-failed] $HOME/.issue-desk/$da_name が無い、または実行できません（チャットの許可一覧が案内する入口）"
    fi
  done
}

linux_desk_access() {
  case "${HOME:-}" in
  /*) ;;
  *)
    say '[rc-update-failed] HOME が絶対パスではないため、~/.issue-desk/desk を置きません'
    return 1
    ;;
  esac
  da_base=$HOME/.issue-desk
  da_dest=$da_base/desk
  if [ "${DRY_RUN:-0}" = 1 ]; then
    plan "予定: $da_dest（受付の窓口）を置く"
    return 0
  fi
  if [ -L "$da_dest" ] || [ -d "$da_dest" ]; then
    say "[rc-update-failed] $da_dest はリンクまたはディレクトリのため、置き換えません"
  else
    da_tmp=$(mkdir -p "$da_base" 2>/dev/null && mktemp "$da_base/.desk.XXXXXX" 2>/dev/null) || {
      say "[rc-update-failed] $da_dest を書けません"
      return 1
    }
    if ! linux_desk_access_body >"$da_tmp" 2>/dev/null || ! chmod 755 "$da_tmp" 2>/dev/null; then
      rm -f -- "$da_tmp" 2>/dev/null || :
      say "[rc-update-failed] $da_dest を書けません"
      return 1
    fi
    if [ -f "$da_dest" ] && [ -x "$da_dest" ] && cmp -s "$da_tmp" "$da_dest"; then
      rm -f -- "$da_tmp" 2>/dev/null || :
    else
      da_kind=作成
      [ ! -e "$da_dest" ] || da_kind=更新
      if mv -f -- "$da_tmp" "$da_dest" 2>/dev/null; then
        say "[rc-update] $da_dest を${da_kind}しました"
      else
        rm -f -- "$da_tmp" 2>/dev/null || :
        say "[rc-update-failed] $da_dest を書けません"
        return 1
      fi
    fi
  fi
  linux_desk_access_entries
  return 0
}
