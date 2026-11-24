# update.sh と setup-admin.sh が読み込む。比較・配置・reload は共通部品に任せる。
scheduled_units() (
  if [ "${DRY_RUN:-0}" = 1 ]; then
    plan '予定: 予約タスクの利用者ユニットを配り、新規・変更タイマーだけ有効にする'
    return 0
  fi
  if [ "${ISSUE_DESK_SETUP_MIGRATION:-0}" != 1 ] &&
    [ ! -f "$HOME/.config/systemd/user/.issue-desk-units-migrated" ]; then
    say '[update:units-skipped:migration-pending] 管理セットアップで旧ユニットを移行してください'
    return 0
  fi
  su_tmp=$(mktemp -d 2>/dev/null) || { say '[update:units-failed] 一時フォルダーを作れません'; return 1; }
  trap 'rm -rf "$su_tmp" 2>/dev/null || :' EXIT
  su_sources='deploy/nas/issue-desk-intake-update deploy/nas/issue-desk-db-backup deploy/nas/issue-desk-permission-grants deploy/nas/issue-desk-codex-watch'
  # 週次のSSH練習は廃止した（他マシンへのSSHは利用者の明示の許可があるときだけ）。残っている旧ユニットを止めて消す。
  if [ -f "$HOME/.config/systemd/user/issue-desk-ssh-practice.timer" ]; then
    systemctl --user disable --now issue-desk-ssh-practice.timer >/dev/null 2>&1 || :
    rm -f "$HOME/.config/systemd/user/issue-desk-ssh-practice.timer" "$HOME/.config/systemd/user/issue-desk-ssh-practice.service"
    systemctl --user daemon-reload >/dev/null 2>&1 || :
  fi
  if ! su_dir=$(printf '%s\n' "$DIR" | sed 's/[\\&|]/\\&/g' 2>/dev/null); then
    say '[update:units-failed] 配置先の置換を準備できません'
    return 1
  fi
  for su_base in $su_sources; do
    for su_ext in service timer; do
      if ! sed "s|__DIR__|$su_dir|g" "$DIR/$su_base.$su_ext" >"$su_tmp/${su_base##*/}.$su_ext" 2>/dev/null; then
        say '[update:units-failed] ユニットの写しを作れません'
        return 1
      fi
    done
  done
  if ! su_changed=$(user_units_place "$su_tmp" 2>/dev/null); then
    say '[update:units-failed] 共通部品による配置に失敗しました'
    return 1
  fi
  [ -n "$su_changed" ] || return 0
  for su_name in $su_changed; do
    case "$su_name" in
    *.timer)
      if ! systemctl --user enable --now "$su_name" >/dev/null 2>&1; then
        say '[update:units-failed] タイマーの有効化に失敗しました'
        return 1
      fi
      ;;
    esac
  done
  # 有効化の完了まで印を保持し、中断した回は次の更新でやり直す。
  if ! user_units_ack $su_changed 2>/dev/null; then
    say '[update:units-failed] 完了の記録に失敗しました'
    return 1
  fi
  say '[update:units] 予約タスクの利用者ユニットを更新しました'
)
