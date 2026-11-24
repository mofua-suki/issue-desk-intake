# rc-update.sh が . で読み込む定義だけ。窓口の出力には資格情報がありうるため捨てる。
rc_update_report() {
  rr_timer=disabled
  systemctl --user is-enabled --quiet issue-desk-rc-update.timer >/dev/null 2>&1 && rr_timer=enabled
  if [ "${DRY_RUN:-0}" = 1 ]; then
    printf '予定: 受付へ配布の報告を送る（timer: %s）\n' "$rr_timer"
    return 0
  fi
  rr_desk=$HOME/.issue-desk/desk
  rr_reason=''
  if [ ! -e "$rr_desk" ]; then
    rr_reason=窓口なし
  elif [ ! -x "$rr_desk" ]; then
    rr_reason=実行不可
  else
    rr_status=0
    if command -v timeout >/dev/null 2>&1; then
      { timeout -s KILL 60 "$rr_desk" desk-ops.ts notice rc-update --timer "$rr_timer" >/dev/null 2>&1; } 2>/dev/null || rr_status=$?
    else
      { "$rr_desk" desk-ops.ts notice rc-update --timer "$rr_timer" >/dev/null 2>&1; } 2>/dev/null || rr_status=$?
    fi
    case "$rr_status" in
      0) return 0 ;;
      124|137) rr_reason=時間切れ ;;
      126|127) rr_reason=実行不可 ;;
      *) rr_reason=非0 ;;
    esac
  fi
  printf '[rc-update-failed] 受付へ配布の報告を送れません（%s）\n' "$rr_reason"
  return 1
}
