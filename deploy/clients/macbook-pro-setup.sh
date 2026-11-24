#!/bin/sh
# MacBook Pro（受付のID macbook-pro-test）の初期セットアップ。MacのTerminalで一般ユーザーとして実行する。
# mofua-suki/sample-expense-client のMacネイティブテスト専用。実装・PR・マージは担当しない。
# 何度実行してもよい。管理者権限の操作・Xcodeのライセンス同意・資格情報の発行は代行しない。
# 初回はFlutter・Goの取得で長くかかる（未測定。目安20〜40分）。完了表示までCtrl+Cを押さない。
# 精算ClientのiOSテスト（scripts/verify-apple.sh ios）はここでは流さない。NASからホスト作業用SSHの操作で流す（docs/mac-roles.md）。
#
#   sh macbook-pro-setup.sh
set -eu
HOST_ID=macbook-pro-test
REGISTERED_HOME=/Users/user
FLUTTER_VERSION=3.47.9
GO_VERSION=1.27.0
GITEA=http://192.168.0.10:3000
REPO=mofua-suki/sample-expense-client

stage='前提確認'
finished=0
work=''
finish() {
  result=$?
  [ -z "$work" ] || rm -rf "$work"
  [ "$finished" = 1 ] || printf '\n失敗: %s（終了コード %s）。直前の表示が原因です。直してから同じコマンドを実行し直してください。\n' "$stage" "$result" >&2
}
trap finish EXIT
fail() { printf '%s\n' "$1" >&2; exit 1; }
step() { stage=$1; printf '\n== %s\n' "$1"; }

[ "$#" = 0 ] || fail "使い方: sh $0（引数はありません）"

step '前提確認'
[ "$(uname -s)" = Darwin ] || fail 'このスクリプトはMacで実行してください。'
[ "$(id -u)" != 0 ] || fail 'sudoや管理者シェルではなく、一般ユーザーで実行してください。'
case "$(uname -m)" in
  arm64) arch=arm64 ;;
  x86_64) arch=amd64 ;;
  *) fail "未対応のCPUです: $(uname -m)" ;;
esac
command -v curl >/dev/null 2>&1 || fail 'curl がありません。'
if [ "$HOME" != "$REGISTERED_HOME" ]; then
  printf '注意: このMacのホームは %s ですが、受付の登録（fleet.json の %s）は %s です。\n' "$HOME" "$HOST_ID" "$REGISTERED_HOME"
  printf '      稼働させる前に fleet.json の runtimeDir・workerTokenFile・nativeToolRoots をこのホームに合わせる必要があります。\n'
fi
developer=$(xcode-select -p 2>/dev/null) || fail 'Xcodeが選ばれていません。App StoreでXcodeを入れ、sudo xcode-select -s /Applications/Xcode.app/Contents/Developer を実行してください。'
case "$developer" in
  */CommandLineTools) fail 'コマンドラインツールだけが選ばれています。Xcode本体を入れ、sudo xcode-select -s /Applications/Xcode.app/Contents/Developer を実行してください。' ;;
esac
first_launch='Xcodeの初回設定が済んでいません。sudo xcodebuild -license accept と sudo xcodebuild -runFirstLaunch を実行してください（ライセンスへの同意は利用者が行う）。'
xcodebuild -version || fail "$first_launch"
xcodebuild -checkFirstLaunchStatus || fail "$first_launch"
xcrun --sdk macosx --show-sdk-path >/dev/null || fail 'macOS SDKが見つかりません。Xcodeの導入を確認してください。'
git --version || fail 'git がありません。Xcodeの導入を確認してください。'

step 'Go'
go_root="$HOME/.issue-desk/tools/go"
current_go=''
if [ -x "$go_root/bin/go" ]; then current_go=$("$go_root/bin/go" env GOVERSION 2>/dev/null || true); fi
if [ "$current_go" != "go$GO_VERSION" ]; then
  archive="go$GO_VERSION.darwin-$arch.tar.gz"
  work=$(mktemp -d)
  curl -fL --retry 3 -o "$work/$archive" "https://dl.google.com/go/$archive" || fail "Goを取得できません: https://dl.google.com/go/$archive"
  expected=$(curl -fsSL --retry 3 "https://dl.google.com/go/$archive.sha256") || fail "Goのチェックサムを取得できません: https://dl.google.com/go/$archive.sha256"
  actual=$(shasum -a 256 "$work/$archive" | cut -d ' ' -f 1)
  [ "$actual" = "$expected" ] || fail "Goのチェックサムが一致しません（期待 $expected、実際 $actual）。"
  mkdir -p "$HOME/.issue-desk/tools"
  rm -rf "$go_root.new"
  mkdir "$go_root.new"
  tar -xzf "$work/$archive" -C "$go_root.new" --strip-components 1
  rm -rf "$work" "$go_root"
  work=''
  mv "$go_root.new" "$go_root"
fi
"$go_root/bin/go" version

step 'Flutter'
flutter_root="$HOME/development/flutter"
if [ ! -e "$flutter_root" ]; then
  mkdir -p "$HOME/development"
  rm -rf "$flutter_root.new"
  git clone --depth 1 --branch "$FLUTTER_VERSION" https://github.com/flutter/flutter.git "$flutter_root.new" || fail 'Flutterを取得できません。'
  mv "$flutter_root.new" "$flutter_root"
fi
[ -x "$flutter_root/bin/flutter" ] || fail "$flutter_root はFlutterのSDKではありません。移動してから実行し直してください。"
"$flutter_root/bin/flutter" --disable-analytics || fail 'Flutterを起動できません。直前の表示が原因です（初回はDart SDKの取得にネットワークが要る）。'
flutter_version=$("$flutter_root/bin/flutter" --version --machine) || fail 'Flutterの版を読めません。直前の表示が原因です。'
printf '%s\n' "$flutter_version" | grep -Eq "\"frameworkVersion\"[[:space:]]*:[[:space:]]*\"$FLUTTER_VERSION\"" ||
  fail "$flutter_root がFlutter $FLUTTER_VERSION ではありません。上書きしないので、別の場所へ移してから実行し直してください。"
"$flutter_root/bin/flutter" precache --macos

step 'PATH'
profile="$HOME/.zprofile"
marker='# issue-desk Macテスト用（deploy/clients のMacセットアップが追加）'
touch "$profile"
if ! grep -Fq "$marker" "$profile"; then
  printf '\n%s\nexport PATH="$HOME/development/flutter/bin:$HOME/.issue-desk/tools/go/bin:$PATH"\n' "$marker" >> "$profile"
  printf '%s に PATH を追加しました。新しいTerminalから有効になります。\n' "$profile"
fi
PATH="$flutter_root/bin:$go_root/bin:$PATH"
export PATH
command -v pod >/dev/null 2>&1 || printf '注意: CocoaPods（pod）がありません。プラグインが要求したときは入れてください。\n'

step '受付用の状態フォルダー'
state="$HOME/.issue-desk/apple"
mkdir -p "$state/runtime" "$state/private"
chmod 700 "$HOME/.issue-desk" "$state" "$state/runtime" "$state/private"
printf '%s を作りました（資格情報は稼働させるときに private/ へ置く）。\n' "$state"

step 'sample-expense-client'
client="$HOME/Workspace/sample-expense-client"
if [ -e "$client" ] && [ ! -d "$client/.git" ]; then
  fail "$client がGitのcloneではありません。別の場所へ移してから実行し直してください。"
elif [ ! -e "$client" ]; then
  mkdir -p "$HOME/Workspace"
  git clone "$GITEA/$REPO.git" "$client" || fail "$GITEA/$REPO.git をcloneできません。NASへの接続とGiteaのログインを確認してください。"
else
  origin=$(git -C "$client" remote get-url origin 2>/dev/null || true)
  case "$origin" in
    *"$REPO"|*"$REPO.git") ;;
    *) fail "$client のoriginが $REPO ではありません（$origin）。別の場所へ移してから実行し直してください。" ;;
  esac
  git -C "$client" fetch --quiet origin || fail "$client の取得に失敗しました。NASへの接続を確認してください。"
  printf '%s は既にあるので、ブランチは変えません。\n' "$client"
fi

finished=1
printf '\n完了: %s の準備が終わりました。\n' "$HOST_ID"
printf '受付でこのMacが検証を担当するかは fleet.json の nativeTestHost で決まります（登録時は停止中）。稼働は docs/mac-roles.md の「停止中のMacを稼働させる」で行います。\n'
