#!/bin/sh
# Ubuntu の PC（a9max・gt12・x1-pro1）へ、チャット用の配布（正本の写し・配布タイマー・Skill・窓口）を初めて入れる
# （何度実行してもよく、済んだ段は飛ばす）。Remote Control は NAS にシステムごとに1つだけ置くので、PC では claude-rc@ を
# 起動しない（rc-update.sh が動いているものを止める。docs/chat-operations.md）。
# 使い方: RC_GITEA_SSH_BASE=ssh://git@<Gitea のホスト>[:ポート]/mofua-suki sh rc-setup.sh <リポジトリ名>...
#   （正本の写し ~/.issue-desk/remote-control/source が既にあれば、RC_GITEA_SSH_BASE は省略できる）
# 資料: docs/chat-deploy.md の「他のPC（Ubuntu）」。root・sudo は使わない。鍵・トークンは表示も保存もしない。
# 再起動すると、そのとき進んでいるチャットが途切れる（NAS と同じ）。Ctrl+C は押さない。
# 監視の準備（user-setup.sh）は呼ばない。

set -u

SRC=$HOME/.issue-desk/remote-control/source
KEY=$HOME/.ssh/gitea_rc_ed25519
RC_SSH="ssh -i $KEY -o IdentitiesOnly=yes -o BatchMode=yes"
UD=$HOME/.config/systemd/user

stop() {
  printf '[rc-setup-failed] %s\n' "$*"
  exit 1
}
step() { printf '[rc-setup] %s\n' "$*"; }

case "$HOME" in
/*) ;;
*) stop 'HOME が絶対パスではありません' ;;
esac
[ "$#" -gt 0 ] || stop '使い方: sh rc-setup.sh <リポジトリ名>...（例: issue-desk-intake）'
for rs_repo in "$@"; do
  case "$rs_repo" in
  '' | *[!A-Za-z0-9._-]* | .* | *..*) stop "リポジトリ名が不正です: $rs_repo（英数字・.・_・- のみ）" ;;
  esac
done

# --- 前提 ---
[ -f "$KEY" ] || stop "$KEY がありません。鍵を作って Gitea の mofua-suki に登録してから再実行してください（ssh-keygen -t ed25519 -f $KEY -N '' の後、公開鍵 $KEY.pub を登録）"
command -v git >/dev/null || stop 'git がありません。apt で git を入れてください'
if [ -d "$SRC/.git" ] && [ -z "${RC_GITEA_SSH_BASE:-}" ]; then
  rs_origin=$(git -C "$SRC" remote get-url origin 2>/dev/null) || rs_origin=''
  RC_GITEA_SSH_BASE=${rs_origin%/*}
fi
[ -n "${RC_GITEA_SSH_BASE:-}" ] || stop 'RC_GITEA_SSH_BASE（例: ssh://git@gitea.example:2222/mofua-suki）を指定してください'
RC_GITEA_SSH_BASE=${RC_GITEA_SSH_BASE%/}
rs_ls_error=$(GIT_SSH_COMMAND=$RC_SSH GIT_TERMINAL_PROMPT=0 git ls-remote "$RC_GITEA_SSH_BASE/issue-desk-intake.git" HEAD 2>&1 >/dev/null) || {
  case "$rs_ls_error" in
  *'Host key verification failed'*) stop 'Gitea のホスト鍵が known_hosts にありません。ssh -i ~/.ssh/gitea_rc_ed25519 -p <ポート> git@<ホスト> を1回実行して鍵を確かめてから再実行してください' ;;
  *) stop "$KEY で Gitea の mofua-suki/issue-desk-intake を読めません。公開鍵の登録と RC_GITEA_SSH_BASE を確かめてください" ;;
  esac
}
systemctl --user show-environment >/dev/null 2>&1 || stop 'systemctl --user が使えません。ログインし直すか、loginctl enable-linger を確かめてください'
command -v docker >/dev/null || stop 'docker がありません（受付の窓口に要ります）'
rs_mon=$(docker ps -q --filter label=com.docker.compose.project=issue-desk-client \
  --filter label=com.docker.compose.service=issue-desk-client 2>/dev/null) || rs_mon=''
[ -n "$rs_mon" ] || stop '監視のコンテナ（issue-desk-client）が動いていません。先に監視を起動してください'

# --- Claude Code の導入とログイン ---
CLAUDE=$HOME/.local/bin/claude
if [ ! -x "$CLAUDE" ]; then
  command -v curl >/dev/null || stop 'curl がありません。Claude Code を入れられません'
  step 'Claude Code を導入します'
  curl -fsSL https://claude.ai/install.sh | bash >/dev/null 2>&1 || stop 'Claude Code の導入に失敗しました'
  [ -x "$CLAUDE" ] || stop "$CLAUDE ができませんでした"
fi
if ! "$CLAUDE" auth status >/dev/null 2>&1; then
  step 'Claude Code にログインします（表示される手順に従ってください）'
  "$CLAUDE" auth login || stop 'Claude Code のログインに失敗しました'
fi

# --- linger ---
if [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null)" != yes ]; then
  loginctl enable-linger "$(id -un)" >/dev/null 2>&1 || stop 'linger を有効にできません。管理者に loginctl enable-linger を頼んでください'
  step 'linger を有効にしました'
fi

. "$(dirname -- "$0")/chat-source.sh" || stop 'チャット配布の選択定義を読めません'

# --- 正本の写し ---
# 旧リポジトリの写しは新しいIntakeの履歴へmergeしない。利用者の変更が無い場合だけ、
# clone完了後に旧sourceを退避し、新正本へ原子的に切り替える。
if [ -d "$SRC/.git" ]; then
 rs_origin=$(git -C "$SRC" remote get-url origin) || stop '正本のoriginを読めません'
 if [ "$rs_origin" != "$RC_GITEA_SSH_BASE/issue-desk-intake.git" ]; then
  [ -z "$(git -C "$SRC" status --porcelain)" ] || stop '旧正本の写しに手元の変更があります。上書きしません'
  rs_local=$(git -C "$SRC" rev-list HEAD --not --remotes) || stop '旧正本の未公開履歴を確認できません'
  [ -z "$rs_local" ] || stop '旧正本の写しに未公開コミットがあります。上書きしません'
  [ ! -L "$SRC" ] || stop '正本の写しがsymlinkです。切り替えません'
  rs_next=$(mktemp -d "${SRC%/*}/.source-intake.XXXXXXXX") || stop '新正本の準備フォルダーを作れません'
  if ! GIT_SSH_COMMAND=$RC_SSH git clone -q --no-checkout "$RC_GITEA_SSH_BASE/issue-desk-intake.git" "$rs_next"; then
   rmdir "$rs_next" 2>/dev/null || :
   stop 'Intakeの正本を取得できません。旧正本は保持しました'
  fi
  chat_sources "$rs_next" || { rm -rf -- "$rs_next"; stop 'チャット用の配布資産を選択できません'; }
  rs_previous=${SRC}.before-intake
  [ ! -e "$rs_previous" ] || { rm -rf -- "$rs_next"; stop '前回の正本退避が残っています。旧正本を重ねて置き換えません'; }
  mv "$SRC" "$rs_previous" || { rm -rf -- "$rs_next"; stop '旧正本を退避できません'; }
  if ! mv "$rs_next" "$SRC"; then
   if mv "$rs_previous" "$SRC"; then stop '新正本を配置できません。旧正本を戻しました';
   else stop "新正本も旧正本も配置できません。退避は $rs_previous に保持しています"; fi
  fi
  step "Intakeの正本へ切り替えました。前の写し: $rs_previous"
 fi
fi
if [ ! -d "$SRC/.git" ]; then
  mkdir -p "${SRC%/*}" || stop "${SRC%/*} を作れません"
  GIT_SSH_COMMAND=$RC_SSH git clone -q --no-checkout "$RC_GITEA_SSH_BASE/issue-desk-intake.git" "$SRC" 2>/dev/null || stop '正本の clone に失敗しました'
  chat_sources "$SRC" || stop 'チャット用の配布資産を選択できません'
  step "正本の写しを $SRC へ置きました"
fi
chat_sources "$SRC" || stop 'チャット用の配布資産を選択できません'
git -C "$SRC" config core.sshCommand "$RC_SSH" || stop 'core.sshCommand を設定できません'

# --- 作業フォルダー ---
mkdir -p "$HOME/rc" || stop "$HOME/rc を作れません"
for rs_repo in "$@"; do
  if [ ! -d "$HOME/rc/$rs_repo/.git" ]; then
    [ ! -e "$HOME/rc/$rs_repo" ] || [ -d "$HOME/rc/$rs_repo" ] || stop "$HOME/rc/$rs_repo が通常のフォルダーではありません"
    GIT_SSH_COMMAND=$RC_SSH git clone -q "$RC_GITEA_SSH_BASE/$rs_repo.git" "$HOME/rc/$rs_repo" 2>/dev/null ||
      stop "$rs_repo の clone に失敗しました（mofua-suki/$rs_repo の名前と権限を確かめてください）"
    step "$HOME/rc/$rs_repo を clone しました"
  fi
  git -C "$HOME/rc/$rs_repo" config core.sshCommand "$RC_SSH" || stop "$rs_repo の core.sshCommand を設定できません"
done

# --- ユニット（claude-rc@.service は無ければ置く。あれば rc-update.sh が更新する）とタイマー ---
mkdir -p "$UD" || stop "$UD を作れません"
for rs_unit in issue-desk-rc-update.service issue-desk-rc-update.timer; do
  cmp -s "$SRC/deploy/clients/remote-control/$rs_unit" "$UD/$rs_unit" 2>/dev/null ||
    cp "$SRC/deploy/clients/remote-control/$rs_unit" "$UD/$rs_unit" || stop "$UD/$rs_unit を置けません"
done
[ -f "$UD/claude-rc@.service" ] || cp "$SRC/deploy/nas/claude-rc@.service" "$UD/claude-rc@.service" || stop "$UD/claude-rc@.service を置けません"
systemctl --user daemon-reload || stop 'systemctl --user daemon-reload に失敗しました'
systemctl --user enable --now issue-desk-rc-update.timer >/dev/null 2>&1 || stop 'タイマーを有効にできません'

# --- 配布を1回（手置きの設定の引き継ぎ、hooks、窓口、必要な再起動を含む）---
systemctl --user start issue-desk-rc-update.service ||
  stop '配布（issue-desk-rc-update.service）に失敗しました（journalctl --user -u issue-desk-rc-update を確かめてください）'

# --- 最終確認 ---
systemctl --user is-enabled --quiet issue-desk-rc-update.timer || stop 'issue-desk-rc-update.timer が有効ではありません'
"$HOME/.issue-desk/desk" get --path '/api/issues/search?project=mofua-suki%2Fissue-desk-intake&countOnly=1' >/dev/null 2>&1 ||
  stop '窓口 ~/.issue-desk/desk get が通りません（journalctl --user -u issue-desk-rc-update を確かめてください）'
step "完了: タイマー有効、窓口 OK、作業フォルダー: $*（Remote Control は NAS だけで、この PC では起動しません）"
