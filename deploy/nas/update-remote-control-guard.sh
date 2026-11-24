# update.sh が読み込む定義だけ。失敗した配布の組と、プロジェクト設定の退避を管理する。
remote_control_guard() {
  case "$1" in
  allow) remote_control_guard_allow ;;
  begin)
    remote_control_guard_begin && return 0
    remote_control_projects_restore || return 1
    return 1
    ;;
  changed) [ "${RC_PROJECT_CHANGED:-0}" = 0 ] || rc_changed=1 ;;
  restore)
    if ! remote_control_projects_restore; then
      RC_KEPT="${rc_prev_unit:-} ${rc_prev_set:-}"
      say '[update:remote-control-failed] プロジェクト設定を戻せないため、退避と未完了記録を残しました'
      return 1
    fi
    ;;
  success)
    rm -rf "$rc_guard_txn" 2>/dev/null || return 1
    rm -f "$rc_guard_failed" 2>/dev/null
    ;;
  failed)
    # 失敗記録の直後に止まっても、次回に送信待ちを拾えるよう先に保存する。
    remote_control_notice_queue >/dev/null 2>&1 || :
    printf '%s\n' "$rc_guard_key" >"$rc_guard_failed.$$.tmp" 2>/dev/null &&
      mv -f "$rc_guard_failed.$$.tmp" "$rc_guard_failed" 2>/dev/null || return 1
    # 通知の失敗で配布の戻り値・既存の出力を変えない。
    remote_control_notice_send >/dev/null 2>&1 || :
    ;;
  esac
}

# ファイルは実行せず、比較するだけ。cksum のファイル名は含めない。
remote_control_guard_allow() {
  rc_guard_root=$HOME/.issue-desk/remote-control
  rc_guard_failed=$rc_guard_root/failed-definition
  rc_guard_txn=$rc_guard_root/project-pending
  remote_control_notice_send >/dev/null 2>&1 || :
  [ -f "$rc_src_unit" ] && [ -f "$rc_src_set" ] || return 0
  rc_guard_version=$(timeout 5 "$HOME/.local/bin/claude" --version 2>/dev/null) || return 1
  rc_guard_version=$(printf '%s\n' "$rc_guard_version" | head -n 1)
  rc_guard_key=$(cksum <"$rc_src_unit") || return 1
  rc_guard_set=$(cksum <"$rc_src_set") || return 1
  rc_guard_key="$rc_guard_key
$rc_guard_set
$rc_guard_version"
  if [ -f "$rc_guard_failed" ] && [ "$(cat "$rc_guard_failed")" = "$rc_guard_key" ]; then
    return 1
  fi
  return 0
}

remote_control_guard_begin() {
  command -v git >/dev/null || return 1
  RC_PROJECT_CHANGED=0
  # 置き換えたプロジェクト（~/rc/<名前>）の名前。空白区切り。再起動はこの名前の窓口だけにする。
  RC_PROJECT_DIRS=''
  mkdir -p "$rc_guard_root" 2>/dev/null || return 1
  # 配置中に止まった場合も、控えを書き換える前の設定へ戻す。
  remote_control_projects_restore || return 1
  for rg_dir in "$HOME"/rc/* "$HOME"/rc/.[!.]* "$HOME"/rc/..?*; do
    [ -d "$rg_dir" ] || continue
    rg_dest=$rg_dir/.claude/settings.json
    # 本人用の既存PC更新では、新しいproject設定や許可一覧を導入しない。
    if [ "${ISSUE_DESK_RC_EXISTING_ONLY:-0}" = 1 ] && [ ! -f "$rg_dest" ]; then continue; fi
    rg_reason=''
    rg_git=0
    rg_rel=.claude/settings.json
    if [ -L "$rg_dir" ] || [ -L "$rg_dir/.claude" ] || [ -L "$rg_dest" ]; then
      rg_reason=link
    elif rg_git_info=$(git -C "$rg_dir" rev-parse --show-prefix --git-path info/exclude 2>/dev/null); then
      rg_git=1
      {
        IFS= read -r rg_prefix
        IFS= read -r rg_exclude
      } <<EOF
$rg_git_info
EOF
      [ -n "$rg_exclude" ] || return 1
      rg_rel=$rg_prefix.claude/settings.json
      if git -C "$rg_dir" ls-files --error-unmatch -- .claude/settings.json >/dev/null 2>&1; then
        rg_reason=tracked
      else
        rg_status=$?
        [ "$rg_status" = 1 ] || return 1
      fi
    fi
    if [ -z "$rg_reason" ] && [ -e "$rg_dest" ] &&
      ! cmp -s "$rc_src_set" "$rg_dest" && { [ ! -f "$rc_set" ] || ! cmp -s "$rc_set" "$rg_dest"; } &&
      ! remote_control_granted "$rg_dir"; then
      rg_reason=custom
    fi
    rg_notice=$rc_guard_root/rejected-${rg_dir##*/}
    if [ -n "$rg_reason" ]; then
      rg_content=$( (cksum <"$rg_dest") 2>/dev/null) || rg_content=unreadable
      rg_notice_key="$rg_reason $rg_content"
      if [ ! -f "$rg_notice" ] || [ "$(cat "$rg_notice")" != "$rg_notice_key" ]; then
        printf '%s\n' "$rg_notice_key" >"$rg_notice.$$.tmp" && mv -f "$rg_notice.$$.tmp" "$rg_notice" || return 1
        say "[update:remote-control-failed] プロジェクト設定を置き換えません ($rg_reason): $rg_dest"
      fi
      continue
    fi
    rm -f "$rg_notice" 2>/dev/null || return 1
    if [ "$rg_git" = 1 ]; then
      case "$rg_exclude" in /*) ;; *) rg_exclude=$rg_dir/$rg_exclude ;; esac
      mkdir -p "${rg_exclude%/*}" || return 1
      rg_rule=$(printf '%s\n' "/$rg_rel" | sed 's/[\\*?[]/\\&/g')
      if [ ! -f "$rg_exclude" ] || ! grep -Fqx "$rg_rule" "$rg_exclude"; then
        printf '\n%s\n' "$rg_rule" >>"$rg_exclude" || return 1
      fi
    fi
    cmp -s "$rc_src_set" "$rg_dest" && continue
    # 反映係が置いた内容は、反映係が次の周期に元の許可から作り直すので置き換えない（再起動もしない）。
    remote_control_granted "$rg_dir" && continue
    rg_backup=$rc_guard_txn/${rg_dir##*/}
    mkdir -p "$rg_backup" "$rg_dir/.claude" || return 1
    if [ -f "$rg_dest" ]; then
      cp "$rg_dest" "$rg_backup/settings.json" || return 1
    fi
    cp "$rc_src_set" "$rg_backup/applied.json" || return 1
    # ready は退避完了を示す。中断時に未退避の既存ファイルを消さない。
    : >"$rg_backup/ready" || return 1
    remote_control_put "$rc_src_set" "$rg_dest" || return 1
    RC_PROJECT_CHANGED=1
    RC_PROJECT_DIRS=$RC_PROJECT_DIRS${RC_PROJECT_DIRS:+ }${rg_dir##*/}
  done
}

# 許可の付与の反映係（deploy/nas/permission-grants-apply.ts）が置いた内容か。反映係の記録（置いた内容の写しと、
# 置き換えの途中の写し）のどちらかと一致すれば 0（docs/permission-grants.md『NAS Rule Applier』）。
remote_control_granted() {
  for rg_record in "$rc_guard_root/permission-grants-applied/${1##*/}/settings.json" \
    "$rc_guard_root/permission-grants-applied/${1##*/}/next.json"; do
    [ -f "$rg_record" ] && cmp -s "$rg_record" "$1/.claude/settings.json" && return 0
  done
  return 1
}

remote_control_projects_restore() {
  # recover からは begin より先に呼ばれる。
  rc_guard_txn=$HOME/.issue-desk/remote-control/project-pending
  [ -d "$rc_guard_txn" ] || return 0
  for rg_backup in "$rc_guard_txn"/* "$rc_guard_txn"/.[!.]* "$rc_guard_txn"/..?*; do
    [ -f "$rg_backup/ready" ] || continue
    rg_dest=$HOME/rc/${rg_backup##*/}/.claude/settings.json
    [ ! -L "${rg_dest%/*}" ] && [ ! -L "$HOME/rc/${rg_backup##*/}" ] && [ ! -L "$rg_dest" ] || return 1
    # 中断後に利用者が変えた設定も上書きしない。復元済みの設定は再復元できる。
    if [ -e "$rg_dest" ] && ! cmp -s "$rg_dest" "$rg_backup/applied.json" &&
      { [ ! -f "$rg_backup/settings.json" ] || ! cmp -s "$rg_dest" "$rg_backup/settings.json"; }; then
      return 1
    fi
    if [ -f "$rg_backup/settings.json" ]; then
      remote_control_put "$rg_backup/settings.json" "$rg_dest" || return 1
    else
      rm -f "$rg_dest" || return 1
    fi
  done
  rm -rf "$rc_guard_txn" 2>/dev/null
}

# 送信待ちは失敗記録とは別に残す。定義が変わっても前の通知を失わない。
remote_control_notice_queue() (
  rn_signature=$(printf '%s\n' "$rc_guard_key" | sha256sum) || return 1
  rn_signature=${rn_signature%% *}
  mkdir -p "$rc_guard_root/notice-pending" || return 1
  [ ! -f "$rc_guard_root/notice-sent/$rn_signature" ] || return 0
  rn_tmp=$rc_guard_root/notice-pending/$rn_signature.$$.tmp
  trap 'rm -f "$rn_tmp" 2>/dev/null || :' EXIT
  printf '%s\n' "${rc_bad_new:-}" | tr -d ' ' >"$rn_tmp" || return 1
  mv -f "$rn_tmp" "$rc_guard_root/notice-pending/$rn_signature" || return 1
  return 0
)

remote_control_notice_send() (
  for rn_file in "$rc_guard_root"/notice-pending/*; do
    [ -f "$rn_file" ] || continue
    rn_signature=${rn_file##*/}
    case "$rn_signature" in *[!a-f0-9]*|'') continue ;; esac
    [ "${#rn_signature}" = 64 ] || continue
    if [ ! -f "$rc_guard_root/notice-sent/$rn_signature" ]; then
      rn_instances=$(cat "$rn_file") || continue
      timeout 30 "$HOME/.issue-desk/desk" desk-ops.ts notice remote-control \
        --signature "$rn_signature" --instances "$rn_instances" >/dev/null 2>&1 || continue
      mkdir -p "$rc_guard_root/notice-sent" || continue
      : >"$rc_guard_root/notice-sent/$rn_signature" || continue
    fi
    rm -f "$rn_file" || :
  done
)
