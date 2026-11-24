#!/bin/sh
set -eu
DIR=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
DRY_RUN=0
BYPASS_WAIT=0
for argument in "$@"; do
  case "$argument" in
    --dry-run) DRY_RUN=1 ;;
    --intake-only) : ;;
    --intake-now) BYPASS_WAIT=1 ;;
    *) echo '使い方: update.sh [--dry-run] [--intake-now]' >&2; exit 64 ;;
  esac
done
say() { printf '%s %s\n' "$(date -Is)" "$*"; }
plan() { say "$*"; }
NL='
'
if [ "$(id -u)" = 0 ]; then
  echo 'NASの配置担当ユーザーで実行してください。' >&2
  exit 64
fi
mkdir -p "$DIR/runtime/locks"
exec 9>"$DIR/runtime/locks/intake-update.lock"
if ! flock -n 9; then
  say '[update:busy] 別のServer更新が進行中です。'
  exit 0
fi
for executable in git docker timeout; do
  command -v "$executable" >/dev/null || { say "[update:failed] $executable がありません。"; exit 1; }
done
if [ ! -f "$DIR/deploy/nas/.env" ]; then
  say '[update:failed] deploy/nas/.env がありません。'
  exit 1
fi
setting() {
  setting_key=$1
  setting_fallback=$2
  setting_value=''
  while IFS= read -r setting_line || [ -n "$setting_line" ]; do
    case "$setting_line" in
      "$setting_key="*) setting_value=${setting_line#*=} ;;
    esac
  done <"$DIR/deploy/nas/.env"
  case "$setting_value" in
    \"*\") setting_value=${setting_value#\"}; setting_value=${setting_value%\"} ;;
    \'*\') setting_value=${setting_value#\'}; setting_value=${setting_value%\'} ;;
  esac
  [ -n "$setting_value" ] || setting_value=$setting_fallback
  printf '%s' "$setting_value"
}
TOOLS_IMAGE=$(setting ISSUE_DESK_TOOLS_IMAGE issue-desk-tools:bc0356ff9e2e)
case "$TOOLS_IMAGE" in ''|*[!a-zA-Z0-9._/@:-]*) say '[update:failed] CLIツールのイメージ名が不正です。'; exit 64 ;; esac
IMAGE=$(setting ISSUE_DESK_INTAKE_IMAGE issue-desk-intake:latest)
case "$IMAGE" in ''|*[!a-zA-Z0-9._/@:-]*) say '[update:failed] Serverのイメージ名が不正です。'; exit 64 ;; esac
if [ -f "$DIR/deploy/nas/update-checkout-guard.sh" ] && [ "$DRY_RUN" = 0 ]; then
  . "$DIR/deploy/nas/update-checkout-guard.sh"
  checkout_guard_install || say '[update:guard-failed] 配置先の保護を置けませんでした（更新は続けます）'
  checkout_guard_restore || exit 1
fi
current_branch=$(git -C "$DIR" symbolic-ref --short HEAD) || { say '[update:failed] 配置先が作業ブランチではありません。'; exit 1; }
if [ "$current_branch" != trunk ]; then
  say '[update:failed] 配置先はtrunkのチェックアウトにしてください。'
  exit 1
fi
if [ -n "$(git -C "$DIR" status --porcelain --untracked-files=no)" ]; then
  say '[update:failed] 未保存の変更があります。作業ツリーを保持して更新を止めました。'
  exit 1
fi
if ! git -C "$DIR" fetch origin trunk; then
  say '[update:failed] 配布元の版を取得できません。現在の版を保持します。'
  exit 1
fi
if [ -n "$(git -C "$DIR" log --format=%H origin/trunk..HEAD)" ]; then
  if [ "$DRY_RUN" = 1 ] || ! command -v checkout_guard_drop_local_commits >/dev/null || ! checkout_guard_drop_local_commits; then
    say '[update:failed] 配布元にないコミットがあります。現在の版を保持します。'
    exit 1
  fi
fi
TARGET=$(git -C "$DIR" rev-parse origin/trunk)
if [ "$DRY_RUN" = 1 ]; then
  plan "[update:plan] Serverを $TARGET でビルドし、待機色の確認・書き手の交代・入口確認を行います。"
  . "$DIR/deploy/nas/update-remote-control-single.sh"
  . "$DIR/deploy/nas/rc-cleanup.sh"
  rc_cleanup
  exit 0
fi
ISSUE_DESK_DEPLOY_UPDATE=1 git -C "$DIR" merge --ff-only origin/trunk >/dev/null
ACTIVE_REVISION=$(sh "$DIR/deploy/nas/intake.sh" status 2>/dev/null | sed -n 's/.*"revision":"\([0-9a-f]*\)".*/\1/p') || ACTIVE_REVISION=''
case "$ACTIVE_REVISION" in ''|*[!0-9a-f]*) ACTIVE_REVISION='' ;; esac
[ "${#ACTIVE_REVISION}" -eq 40 ] || ACTIVE_REVISION=''
deploy=1
if [ "$ACTIVE_REVISION" = "$TARGET" ]; then
  deploy=0
else
  success_file=$DIR/deploy/nas/.update-intake-last-success
  if [ "$BYPASS_WAIT" = 0 ] && [ -f "$success_file" ]; then
    success_line=$(head -c 100 "$success_file")
    success_revision=${success_line%% *}
    success_at=${success_line#* }
    case "$success_revision:$success_at" in *[!0-9a-f:]*|:*|*:) success_at=0 ;; esac
    case "$success_at" in *[!0-9]*|'') success_at=0 ;; esac
    [ "${#success_at}" -le 13 ] || success_at=0
    [ "${#success_revision}" -eq 40 ] || success_at=0
    now=$(date +%s)
    if [ "$success_revision" = "$ACTIVE_REVISION" ] && [ "$success_at" -le "$((now * 1000))" ] && [ "$success_at" -gt "$(((now - 1800) * 1000))" ]; then
      say '[update:waiting] 前回の入れ替えから30分の間隔を置いています。稼働版は保持します。'
      deploy=0
    fi
  fi
fi
if [ "$deploy" = 1 ]; then
# 窓口（~/.issue-desk/desk）は新旧どちらのイメージでも動くので、イメージを入れ替える前に書き直す。
# 入れ替えの後だけで書き直すと、その間は新しいイメージに古い窓口の読み込み役の名前が届かず失敗する。
# 写しの固定（稼働版）は入れ替えの後の desk_access でもう一度行う。ここで失敗しても入れ替えは止めない。
if [ -f "$DIR/deploy/nas/update-desk-access.sh" ]; then
  . "$DIR/deploy/nas/update-desk-access.sh"
  desk_access || say '[update:desk-access-failed] 入れ替えの前に窓口を書き直せませんでした（入れ替えの後にもう一度試します）'
fi
if ! docker build --build-arg "TOOLS_IMAGE=$TOOLS_IMAGE" --build-arg "ISSUE_DESK_REVISION=$TARGET" \
  -f "$DIR/deploy/nas/Dockerfile" -t "$IMAGE" "$DIR"; then
  say '[update:failed] Serverのビルドに失敗しました。稼働中のServerを保持します。'
  exit 1
fi
result=0
ISSUE_DESK_INTAKE_IMAGE=$IMAGE sh "$DIR/deploy/nas/deploy-server.sh" --revision "$TARGET" || result=$?
if [ "$result" -ne 0 ]; then
  say '[update:failed] 新しいServerの稼働を確認できませんでした。切り戻しの結果を確認してください。'
  exit "$result"
fi
fi
. "$DIR/deploy/nas/update-skills.sh"
skills_listed
skills_guard
. "$DIR/deploy/nas/update-remote-control.sh"
. "$DIR/deploy/nas/update-remote-control-guard.sh"
. "$DIR/deploy/nas/update-remote-control-single.sh"
. "$DIR/deploy/nas/update-consultations.sh"
claude_ready=1
if ! claude_consultations; then
  claude_ready=0
  say '[update:remote-control-failed] image-review-client の相談用フォルダーを準備できません'
fi
remote_control
remote_control_single "$claude_ready"
codex_consultations || say '[update:remote-control-failed] Codex の相談用フォルダーまたはデーモンを準備できません'
. "$DIR/deploy/nas/rc-cleanup.sh"
rc_cleanup
. "$DIR/deploy/nas/update-units.sh"
scheduled_units
if [ -f "$DIR/deploy/nas/update-desk-access.sh" ]; then
  . "$DIR/deploy/nas/update-desk-access.sh"
  desk_access
fi
# NASのGo（~/.local/bin/go）を、PATHに無くても使える形で置く。失敗しても更新は止めない。
if [ -f "$DIR/deploy/nas/update-go-tools.sh" ]; then
  . "$DIR/deploy/nas/update-go-tools.sh"
  go_tools || say '[update:go-tools-failed] NASのGoラッパーを置けませんでした（更新は続けます）'
fi
# NAS本体のNode（~/.local/bin/node）を deploy/versions.json の版にそろえる。失敗しても更新は止めない。
if [ -f "$DIR/deploy/nas/update-host-node.sh" ]; then
  . "$DIR/deploy/nas/update-host-node.sh"
  host_node || say '[update:host-node-failed] NAS本体のNodeを deploy/versions.json の版にできませんでした（更新は続けます）'
fi
if [ "$deploy" = 1 ] || [ "$ACTIVE_REVISION" = "$TARGET" ]; then
  say "[update:success] Server $TARGET の書き手と入口を確認しました。"
fi
# Workerのtrunkの発行と登録（Server更新の成功のあとに行う。失敗してもここまでの更新は止めない）。
if [ -f "$DIR/deploy/nas/update-worker-release.sh" ]; then
  . "$DIR/deploy/nas/update-worker-release.sh"
  worker_release || say '[update:worker-release-failed] 発行・登録の段が異常終了しました。'
fi
