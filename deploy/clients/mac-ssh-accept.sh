#!/bin/sh
# MacBook Pro（ホスト作業用SSHのPC名 macbook-pro）で、NASのホスト作業用SSHを受け入れる準備をする。
# MacのTerminalで、ふだん使う利用者から sudo で実行する（例: sudo sh mac-ssh-accept.sh）。目安1分。完了表示までCtrl+Cを押さない。
# 受け入れるのは、NASの署名役（CA）が発券した短命の証明書のうち、principal が issue-desk-repair-macbook-pro のものだけ。
# 実行できる操作は証明書に固定され（host-ssh.json の operations）、パスワード・個人の鍵の設定は変えない。何度実行してもよい。
set -eu
PC_NAME=macbook-pro
REGISTERED_USER=user
CA_PUBLIC_KEY='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDC3aeBe5g7h1beb/Hp52rOwcHImsvtrZaojxzh2jODi issue-desk-signer@DXP480TPLUS-D04A'
# テストだけが別の場所を指す。実機では空のまま /etc/ssh を使う。
ROOT=${ISSUE_DESK_SSH_ACCEPT_ROOT:-}
ssh_dir="$ROOT/etc/ssh"
ca_file="$ssh_dir/issue-desk-repair-ca.pub"
principals_dir="$ssh_dir/issue-desk-principals"
drop_in="$ssh_dir/sshd_config.d/050-issue-desk.conf"

stage='前提確認'
finished=0
finish() {
  result=$?
  [ "$finished" = 1 ] || printf '\n失敗: %s（終了コード %s）。直前の表示が原因です。直してから同じコマンドを実行し直してください。\n' "$stage" "$result" >&2
}
trap finish EXIT
fail() { printf '%s\n' "$1" >&2; exit 1; }
step() { stage=$1; printf '\n== %s\n' "$1"; }

step '前提確認'
[ "$#" = 0 ] || fail "使い方: sudo sh $0"
[ "$(uname -s)" = Darwin ] || fail 'このスクリプトはMacで実行してください。'
[ "$(id -u)" = 0 ] || fail 'sudo を付けて実行してください（例: sudo sh mac-ssh-accept.sh）。'
user=${SUDO_USER:-}
[ -n "$user" ] && [ "$user" != root ] || fail 'rootのシェルではなく、ふだん使う利用者から sudo で実行してください。'
printf '%s\n' "$user" | grep -Eq '^[a-zA-Z0-9][a-zA-Z0-9._-]*$' || fail "対応していない利用者名です: $user"
[ -f "$ssh_dir/sshd_config" ] || fail "$ssh_dir/sshd_config がありません。"
grep -Eq '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d/\*' "$ssh_dir/sshd_config" ||
  fail "$ssh_dir/sshd_config が sshd_config.d を読み込んでいません。macOSを更新してから実行し直してください。"
# sshdは最初に読んだ値を使うので、ほかの場所の同じ指示があると証明書の受け入れが効かない。消さずに止める。
others=$(grep -EHin '^[[:space:]]*(TrustedUserCAKeys|AuthorizedPrincipalsFile|AuthorizedPrincipalsCommand)[[:space:]]' \
  "$ssh_dir/sshd_config" "$ssh_dir"/sshd_config.d/* 2>/dev/null | grep -Fv "$drop_in:" || true)
[ -z "$others" ] || fail "既存のSSH設定に同じ指示があります。上書きしないので、内容を確かめてから実行し直してください:
$others"

step '証明書の受け入れ設定'
# 一度もリモートログインを有効にしていないMacにはホスト鍵が無く、設定の検査が通らない。足りない鍵だけを作る。
[ -n "$ROOT" ] || ssh-keygen -A >/dev/null || fail 'SSHのホスト鍵を作れません。直前の表示が原因です。'
mkdir -p "$ssh_dir/sshd_config.d" "$principals_dir"
previous=''
[ ! -f "$drop_in" ] || previous=$(cat "$drop_in")
chmod 755 "$principals_dir"
printf '%s\n' "$CA_PUBLIC_KEY" > "$ca_file.new"
printf 'issue-desk-repair-%s\n' "$PC_NAME" > "$principals_dir/$user.new"
printf '%s\n' '# issue-desk のホスト作業用SSH（deploy/clients/mac-ssh-accept.sh が管理）' \
  "TrustedUserCAKeys /etc/ssh/issue-desk-repair-ca.pub" \
  "AuthorizedPrincipalsFile /etc/ssh/issue-desk-principals/%u" > "$drop_in.new"
for file in "$ca_file" "$principals_dir/$user" "$drop_in"; do
  chmod 644 "$file.new"
  [ -n "$ROOT" ] || chown root:wheel "$file.new"
  mv "$file.new" "$file"
done
# 検査に通らなければ、この設定を前の状態に戻してから止める（次の接続から壊れた設定を読ませない）。
if [ -z "$ROOT" ] && ! /usr/sbin/sshd -t; then
  if [ -n "$previous" ]; then printf '%s\n' "$previous" > "$drop_in"; else rm -f "$drop_in"; fi
  fail 'SSHの設定の検査に失敗したので、この設定を戻しました。直前の表示が原因です。'
fi
printf '受け入れる証明書: issue-desk-repair-%s（ログインする利用者 %s）\n' "$PC_NAME" "$user"

step 'リモートログイン'
if [ -z "$ROOT" ]; then
  launchctl enable system/com.openssh.sshd ||
    fail 'リモートログインを有効にできません。システム設定 → 一般 → 共有 → リモートログイン をオンにしてから実行し直してください。'
  launchctl bootstrap system /System/Library/LaunchDaemons/ssh.plist 2>/dev/null || true
  launchctl kickstart -k system/com.openssh.sshd 2>/dev/null || true
  listening=0
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if /usr/bin/nc -z 127.0.0.1 22 2>/dev/null; then listening=1; break; fi
    sleep 1
  done
  [ "$listening" = 1 ] || fail 'リモートログインを有効にできません。システム設定 → 一般 → 共有 → リモートログイン をオンにしてから実行し直してください。'
  # 「アクセスを許可」を一部の利用者に絞っている場合は、この利用者も入れる必要がある。
  if dscl . -read /Groups/com.apple.access_ssh >/dev/null 2>&1 && ! dseditgroup -o checkmember -m "$user" com.apple.access_ssh >/dev/null 2>&1; then
    fail "リモートログインが一部の利用者に絞られていて、$user が入っていません。システム設定 → 一般 → 共有 → リモートログイン の「アクセスを許可」に $user を足してから実行し直してください。"
  fi
fi
printf 'リモートログインは有効です。\n'

finished=1
printf '\n完了: %s のホスト作業用SSHの受け入れを準備しました。\n' "$PC_NAME"
if [ "$user" != "$REGISTERED_USER" ]; then
  printf '注意: 受付の登録（host-ssh.json の %s）の利用者名は %s ですが、このMacは %s です。\n' "$PC_NAME" "$REGISTERED_USER" "$user"
  printf '      このままではNASから接続できません。チャットのAIに「Macの利用者名は %s」と伝えてください。\n' "$user"
fi
