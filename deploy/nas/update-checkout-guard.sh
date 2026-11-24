# NASの配置先を trunk に保つ。update.sh から source し、DIR・say を受け取る。
# 配置先で作業されると更新が失敗し続け、受付とWorkerの配布が止まる（2026-11-03 に約1時間半止まった）。
# - 断る: git-hooks/ をこの作業ツリーの hooks へ置き、update.sh 以外のコミット・枝の作成を断る。
# - 戻す: 未保存の変更が無ければ trunk へ戻す。手元のコミットは消さず、枝か refs/preserved/ に残す。
# - 未保存の変更は捨てない。その場合は戻さず失敗にする（更新は止まるが理由を出す）。
checkout_guard_install() {
  [ -e "$DIR/.git" ] || return 0
  cg_hooks=$(git -C "$DIR" rev-parse --git-path hooks) || return 1
  case "$cg_hooks" in /*) ;; *) cg_hooks=$DIR/$cg_hooks ;; esac
  mkdir -p "$cg_hooks" || return 1
  # pack-refs も参照の更新としてhookを通り、枝の作成と区別できない。gc が止まらないよう、参照は束ねない。
  git -C "$DIR" config gc.packRefs false || return 1
  for cg_hook in reference-transaction post-checkout; do
    cmp -s "$DIR/deploy/nas/git-hooks/$cg_hook" "$cg_hooks/$cg_hook" && continue
    command cp -f "$DIR/deploy/nas/git-hooks/$cg_hook" "$cg_hooks/$cg_hook" && chmod 755 "$cg_hooks/$cg_hook" || return 1
  done
}
checkout_guard_preserve() {
  cg_name=$1-$(date +%Y%m%dT%H%M%S)
  ISSUE_DESK_DEPLOY_UPDATE=1 git -C "$DIR" update-ref "refs/preserved/$cg_name" HEAD || return 1
  say "[update:recovered] 手元のコミットを refs/preserved/$cg_name に残しました。"
}
# 別の枝や切り離した状態にあれば trunk へ戻す。
checkout_guard_restore() {
  [ -e "$DIR/.git" ] || return 0
  cg_branch=$(git -C "$DIR" symbolic-ref --short -q HEAD || true)
  [ "$cg_branch" != trunk ] || return 0
  if [ -n "$(git -C "$DIR" status --porcelain --untracked-files=no)" ]; then
    say "[update:failed] 配置先が trunk でなく（${cg_branch:-切り離し}）、未保存の変更があります。作業ツリーを保持して更新を止めました。"
    return 1
  fi
  if [ -z "$cg_branch" ]; then
    checkout_guard_preserve detached || return 1
  fi
  if ! ISSUE_DESK_DEPLOY_UPDATE=1 git -C "$DIR" switch -q trunk; then
    say '[update:failed] 配置先を trunk へ戻せません。'
    return 1
  fi
  say "[update:recovered] 配置先を trunk へ戻しました（元: ${cg_branch:-切り離し}。枝は残しています）。"
}
# trunk に配布元にないコミットがあれば、残してから配布元へ合わせる。fetch の後に呼ぶ。
checkout_guard_drop_local_commits() {
  [ -e "$DIR/.git" ] || return 1
  checkout_guard_preserve trunk || return 1
  ISSUE_DESK_DEPLOY_UPDATE=1 git -C "$DIR" reset -q --keep origin/trunk || return 1
  say '[update:recovered] trunk を配布元に合わせました。'
}
