#!/bin/sh
# Run as the SSH account. Stdin contains the repository name and a registration token.
set +x
set -eu
umask 077

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

platform=$(uname -s)
machine=$(hostname -s | tr '[:upper:]' '[:lower:]')
case "$platform:$machine" in
  Linux:gt12-pro|Linux:nab-9s) labels="$machine,pc-ci" ;;
  Darwin:user-macbook-pro) machine=macbook-pro; labels=$machine ;;
  *) fail 'このPCはGitHub Actionsランナーの導入対象外です。' ;;
esac
IFS= read -r repository || fail '対象リポジトリがありません。'
case "$repository" in
  # 既存ランナーの保存先とサービス名には旧名を使い続ける。
  explicit-decimal|big-decimal) repository=explicit-decimal; github_repo=big-decimal ;;
  flutter-architecture)
    [ "$platform" = Linux ] || fail 'flutter-architectureはLinuxランナーの対象です。'
    github_repo=$repository ;;
  blocking-queue|date-time-pattern|immutable-collections|issue-desk-intake|issue-desk-worker|receipt-print|validated-numbers|value-group)
    github_repo=$repository ;;
  *) fail 'このリポジトリはランナーの導入対象外です。' ;;
esac
IFS= read -r registration || fail 'GitHubの登録トークンがありません。'
case "$registration" in
  ''|*[!A-Za-z0-9_-]*) fail 'GitHubの登録トークンの形式が不正です。' ;;
esac

version=2.338.2
case "$platform:$(uname -m)" in
  Linux:x86_64)
    asset=linux-x64
    digest=baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40 ;;
  Linux:aarch64)
    asset=linux-arm64
    digest=670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945 ;;
  Darwin:arm64)
    asset=osx-arm64
    digest=25a065af409ffa317ab4d5a3b614dd15fc9c40f2172bc1b35a1b1258e919a388 ;;
  Darwin:x86_64)
    asset=osx-x64
    digest=109fff505a34014a7e8d1acb850c33d404811102f572b8fb9788e2fff013cc49 ;;
  *) fail 'このOSとCPU向けのランナー配布物がありません。' ;;
esac

root="$HOME/.local/share/github-actions-runners"
directory="$root/$repository"
mkdir -p "$root"
lock="$root/.$repository.lock"
mkdir "$lock" || fail 'このランナーのセットアップが既に実行中です。'
tmp=
cleanup() {
  [ -z "$tmp" ] || rm -rf -- "$tmp"
  rmdir "$lock"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

if [ "$platform" = Linux ]; then
  XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
  export XDG_RUNTIME_DIR
  systemctl --user show-environment >/dev/null ||
    fail 'systemdのユーザーサービスに接続できません。'
  unit="github-actions-$repository.service"
  unit_dir="$HOME/.config/systemd/user"
  unit_file="$unit_dir/$unit"
else
  domain="gui/$(id -u)"
  launchctl print "$domain" >/dev/null 2>&1 ||
    fail 'macOSのユーザーセッションに接続できません。'
fi

if [ ! -e "$directory" ]; then
  tmp=$(mktemp -d "$root/.download.XXXXXXXX")
  archive="actions-runner-$asset-$version.tar.gz"
  curl -q --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --connect-timeout 15 --max-time 180 --output "$tmp/$archive" \
    "https://github.com/actions/runner/releases/download/v$version/$archive"
  if [ "$platform" = Linux ]; then
    actual=$(sha256sum "$tmp/$archive")
  else
    actual=$(shasum -a 256 "$tmp/$archive")
  fi
  [ "${actual%% *}" = "$digest" ] || fail '公式ランナー配布物のSHA-256が一致しません。'
  mkdir "$tmp/runner"
  tar -xzf "$tmp/$archive" -C "$tmp/runner"
  mv "$tmp/runner" "$directory"
fi

cd "$directory"
url="https://github.com/mofua-suki/$github_repo"
runner_url_regex="($url|https://github.com/mofua-suki/$repository)"
if [ -f .runner ]; then
  # GitHubの設定ファイル上の登録先と端末名を照合する。
  grep -Eq '"gitHubUrl":[[:space:]]*"'"$runner_url_regex"'/?".*' .runner &&
    grep -Eq '"agentName":[[:space:]]*"'"$machine"'".*' .runner ||
    fail '既存のランナー登録が対象のリポジトリまたは端末名と一致しません。'
else
  if ! ./config.sh --unattended --url "$url" --token "$registration" \
    --name "$machine" --labels "$labels" </dev/null >/dev/null 2>&1; then
    fail 'GitHubランナーの登録に失敗しました。トークンの期限と同名ランナーの有無を確認してください。'
  fi
fi
unset registration

if [ "$platform" = Linux ]; then
  [ -x ./runsvc.sh ] || install -m 755 ./bin/runsvc.sh ./runsvc.sh
  mkdir -p "$unit_dir"
  if [ ! -e "$unit_file" ]; then
    (
      set -C
      cat > "$unit_file" <<UNIT
[Unit]
Description=GitHub Actions runner for mofua-suki/$repository
[Service]
WorkingDirectory=%h/.local/share/github-actions-runners/$repository
ExecStart=%h/.local/share/github-actions-runners/$repository/runsvc.sh
Restart=on-failure
[Install]
WantedBy=default.target
UNIT
    )
  fi
  systemctl --user daemon-reload
  [ "$(systemctl --user show "$unit" --property=FragmentPath --value)" = "$unit_file" ] ||
    fail '既存のユーザーサービスが対象のファイルと一致しません。'
  if [ "$(loginctl show-user "$(id -u)" --property=Linger --value)" != yes ]; then
    loginctl --no-ask-password enable-linger ||
      fail 'ログアウト後の稼働を有効にできません。'
  fi
  [ "$(loginctl show-user "$(id -u)" --property=Linger --value)" = yes ] ||
    fail 'ログアウト後もランナーが稼働する設定を確認できません。'
  systemctl --user enable --now "$unit"
  systemctl --user is-active --quiet "$unit"
else
  [ -f .service ] || ./svc.sh install
  plist=$(cat .service)
  case "$plist" in
    "$HOME"/Library/LaunchAgents/actions.runner.*.plist) ;;
    *) fail 'ランナーのlaunchd設定ファイルが想定の配置先と一致しません。' ;;
  esac
  label=${plist##*/}
  service="$domain/${label%.plist}"
  if ! status=$(launchctl print "$service" 2>/dev/null); then
    launchctl bootstrap "$domain" "$plist"
  elif ! printf '%s\n' "$status" | grep -Eq '^[[:space:]]*pid = [0-9]+$'; then
    launchctl kickstart "$service"
  fi
  launchctl print "$service" >/dev/null
fi
printf 'GitHub Actionsランナーを起動しました（%s / %s）。\n' "$machine" "$repository"
