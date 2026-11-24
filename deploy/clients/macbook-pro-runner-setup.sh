#!/bin/sh
# MacBook Pro（受付のID macbook-pro-test）で、受付からiOSの検証要求を引き取るrunner（issue-desk-worker の worker apple run）を常駐させる。
# MacのTerminalで一般ユーザーとして実行する。初回はWorkerのビルドで数分かかる（未測定）。完了表示までCtrl+Cを押さない。
# 前提: 受付のPC別トークンとGiteaの資格情報を、利用者が ~/.issue-desk/apple/private/ に置いてあること（docs/mac-roles.md）。
# このスクリプトはトークンを読まず、発行・権限の変更もしない。何度実行してもよい（Workerをビルドし直し、常駐中なら再起動する）。
# --source <dir> を付けると、GiteaからWorkerを取らず、そのフォルダーのソースでビルドする（NASからホスト作業用SSHの
# deploy-worker-apple-runner で、送ったソースで入れ替えるときに使う。SSHのセッションではGiteaのログインができないため）。
set -eu
HOST_ID=macbook-pro-test
SERVER_URL=https://issue-desk.example.ts.net
WORKER_REPO=http://192.168.0.10:3000/mofua-suki/issue-desk-worker.git

stage='前提確認'
finished=0
finish() {
  result=$?
  [ "$finished" = 1 ] || printf '\n失敗: %s（終了コード %s）。直前の表示が原因です。直してから同じコマンドを実行し直してください。\n' "$stage" "$result" >&2
}
trap finish EXIT
fail() { printf '%s\n' "$1" >&2; exit 1; }
step() { stage=$1; printf '\n== %s\n' "$1"; }

source_dir=
case "$#:${1:-}" in
  0:) ;;
  2:--source) source_dir=$2 ;;
  *) fail "使い方: sh $0 [--source <Workerのソースのフォルダー>]" ;;
esac

step '前提確認'
[ "$(uname -s)" = Darwin ] || fail 'このスクリプトはMacで実行してください。'
[ "$(id -u)" != 0 ] || fail 'sudoや管理者シェルではなく、一般ユーザーで実行してください。'
case "$(uname -m)" in
  arm64) arch=arm64 ;;
  x86_64) arch=amd64 ;;
  *) fail "未対応のCPUです: $(uname -m)" ;;
esac
go="$HOME/.issue-desk/tools/go/bin/go"
[ -x "$go" ] || fail "$go がありません。先に macbook-pro-setup.sh を実行してください。"
command -v python3 >/dev/null 2>&1 || fail 'python3 がありません。Xcodeの導入を確認してください。'
git --version >/dev/null || fail 'git がありません。Xcodeの導入を確認してください。'
state="$HOME/.issue-desk/apple"
server_token="$state/private/pc-token"
[ -s "$server_token" ] || fail "$server_token がありません。受付のPC別トークンを置いてから実行し直してください（docs/mac-roles.md）。"

step '受付への接続設定（K2）'
k2="$state/apple-k2.json"
(umask 077 && printf '{\n  "serverUrl": "%s",\n  "serverTokenFile": "%s",\n  "workerId": "%s"\n}\n' "$SERVER_URL" "$server_token" "$HOST_ID" > "$k2.new")
mv "$k2.new" "$k2"
printf '%s を書きました（トークンの値は含みません）。\n' "$k2"

step 'Workerの取得とビルド'
src="$HOME/Workspace/issue-desk-worker"
if [ -n "$source_dir" ]; then
  [ -f "$source_dir/go.mod" ] && [ -d "$source_dir/cmd/worker" ] || fail "$source_dir にWorkerのソースがありません。"
  src=$source_dir
elif [ -e "$src" ] && [ ! -d "$src/.git" ]; then
  fail "$src がGitのcloneではありません。別の場所へ移してから実行し直してください。"
elif [ ! -e "$src" ]; then
  mkdir -p "$HOME/Workspace"
  git clone "$WORKER_REPO" "$src" || fail "$WORKER_REPO をcloneできません。NASへの接続とGiteaのログインを確認してください。"
else
  git -C "$src" pull --ff-only || fail "$src を最新にできません。手元の変更が無いか確かめてください。"
fi
bin="$state/bin"
mkdir -p "$bin"
(cd "$src" && GOOS=darwin GOARCH=$arch CGO_ENABLED=0 "$go" build -o "$bin/worker.new" ./cmd/worker) || fail 'Workerをビルドできません。直前の表示が原因です。'
mv "$bin/worker.new" "$bin/worker"
if [ -n "$source_dir" ]; then
  printf '%s を、送られたソースで作りました。\n' "$bin/worker"
else
  printf '%s を作りました（%s）。\n' "$bin/worker" "$(git -C "$src" rev-parse --short HEAD)"
fi

step 'runnerの検査（worker apple check）'
"$bin/worker" apple check --config "$k2" || fail 'runnerの検査に失敗しました。直前の表示が原因です（トークン・Giteaの権限・受付の登録を確かめてください）。'

step 'runnerの常駐'
label="com.issue-desk.apple.$HOST_ID"
domain="gui/$(id -u)"
prepared="$HOME/Library/Application Support/issue-desk/apple-prepared/$HOST_ID/$label.plist"
if launchctl print "$domain/$label" >/dev/null 2>&1; then
  launchctl kickstart -k "$domain/$label" || fail 'runnerを再起動できません。'
  printf '登録済みのrunnerを、新しくビルドしたWorkerで再起動しました。\n'
elif [ -f "$prepared" ]; then
  # 再起動・ログアウトで登録だけが消えた状態。prepare.sh は既存の定義を上書きしないので、残っている定義を読み込み直す。
  launchctl bootstrap "$domain" "$prepared" || fail 'runnerを登録し直せません。'
  launchctl kickstart "$domain/$label" || fail 'runnerを起動できません。'
  printf '準備済みのrunnerを登録し直して起動しました。\n'
else
  bash "$src/deploy/client/macos-apple/prepare.sh" --start --role test-only --config "$k2" --worker "$bin/worker" ||
    fail 'runnerを常駐させられません。直前の表示が原因です。'
fi
# prepare.sh の定義はログイン時に読まれない場所にあるので、同じ内容を ~/Library/LaunchAgents にも置き、再起動後も自動で起動させる。
step 'ログイン時の起動'
agents="$HOME/Library/LaunchAgents"
mkdir -p "$agents"
if ! cmp -s "$prepared" "$agents/$label.plist"; then
  cp "$prepared" "$agents/$label.plist.new" && mv "$agents/$label.plist.new" "$agents/$label.plist" || fail 'ログイン時の起動の定義を置けません。'
fi
printf '%s を置きました（ログイン時に起動します）。\n' "$agents/$label.plist"

finished=1
printf '\n完了: %s のrunnerが常駐しました。ログは ~/Library/Application Support/issue-desk/apple-prepared/%s/logs にあります。\n' "$HOST_ID" "$HOST_ID"
printf '受付がこのMacへ検証を送るのは、精算Clientの実装担当に nativeTestHost が付いてからです。\n'
