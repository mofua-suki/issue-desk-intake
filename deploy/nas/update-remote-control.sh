# deploy/nas/update.sh が . で読み込む: remote-control の起動定義（deploy/nas/claude-rc@.service）と許可一覧
# （deploy/nas/remote-control-settings.json）を NAS の本体のホームへ配る（docs/chat-deploy.md の
# 「NAS の remote-control の起動定義と許可一覧」）。単独では実行しない。定義だけで、読み込んでも何もしない。
# update.sh の say・plan・DIR・DRY_RUN を使う。
#
# - ~/.config/systemd/user/claude-rc@.service が既にあるホストだけで働く。無ければ何も書かず、何も出さない。
# - 内容が違うときだけ、一時ファイルから mv で置き換える（cmp）。ユニットは初めて置き換えるとき、元を
#   claude-rc@.service.before-issue-desk に1回だけ写す。jq・node は使わない。
# - どちらかを書き換えた回だけ、daemon-reload の後に動いている claude-rc@*.service を再起動する。プロジェクト設定
#   （~/rc/<名前>/.claude/settings.json）だけが変わった回は、その名前の窓口（claude-rc@<名前>）だけを再起動し、
#   窓口の無い作業ディレクトリ（~/rc に新しく clone した場所など）に配っただけなら再起動しない。ユニットの差が
#   コメント行と空行だけなら、配っても再起動しない（remote_control_same_unit）。許可一覧の変更は、動いている
#   Claude Code が読み直すと確かめていないので、今までどおり再起動する。再起動の後に各
#   インスタンスの起動を確かめ（RC_WAIT_SECONDS 秒まで。既定30）、起動しなければ前の内容へ戻して再起動する。
#   restart の前には reset-failed で起動回数の制限を解き、restart の結果と戻した後の起動も確かめる。
# - 配った・再起動したとき（[update:remote-control]）と失敗したとき（[update:remote-control-failed]）だけ1行出す。
#   失敗しても更新・巻き戻しの結果（終了コード）は変えない。
# - 前の内容へ戻す mv が失敗したときは、退避ファイル（.prev.<pid>）を消さずに残し、失敗行にそのパスを出す
#   （「戻しました」とは言わない）。

# 内容が違うときだけ $1 を $2 へ写す（一時ファイルから mv）。写したら RC_WROTE=1。写せなければ 1。
remote_control_put() {
  RC_WROTE=0
  if [ -f "$2" ] && cmp -s "$1" "$2"; then
    return 0
  fi
  if [ -n "${3:-}" ]; then
    touch "$3" || return 1
  fi
  if cp "$1" "$2.$$.tmp" 2>/dev/null && mv -f "$2.$$.tmp" "$2" 2>/dev/null; then
    RC_WROTE=1
    return 0
  fi
  rm -f "$2.$$.tmp" 2>/dev/null || :
  return 1
}

# $1 と $2 のユニットが、コメント行（空白の後が # か ;）と空行を除いて同じなら 0。どちらかが無ければ 1。
# コメントだけの差は systemd の動作を変えないので、配っても窓口を再起動しない（チャットを切らない）。
remote_control_same_unit() {
  [ -f "$1" ] && [ -f "$2" ] || return 1
  # grep の 1（残る行が無い）は空として比べ、2（読めない）は再起動の側に倒す
  rc_body_a=$(grep -v -E '^[[:space:]]*([#;]|$)' "$1") || [ $? = 1 ] || return 1
  rc_body_b=$(grep -v -E '^[[:space:]]*([#;]|$)' "$2") || [ $? = 1 ] || return 1
  # 行末が \ の続き行があると、空行の有無で systemd の解釈が変わるので、同じとみなさない
  if grep -q -E '\\[[:space:]]*$' "$1" "$2"; then
    return 1
  fi
  [ "$rc_body_a" = "$rc_body_b" ]
}

# 動いている claude-rc@*.service を空白区切りで RC_UNITS に入れる。systemctl が失敗したら 1。
remote_control_active() {
  RC_UNITS=''
  rc_listed=$(systemctl --user list-units 'claude-rc@*.service' --state=active --plain --no-legend --no-pager 2>/dev/null) || return 1
  while IFS= read -r rc_row || [ -n "$rc_row" ]; do
    rc_name=${rc_row%% *}
    case "$rc_name" in
    claude-rc@?*.service) RC_UNITS=$RC_UNITS${RC_UNITS:+ }$rc_name ;;
    esac
  done <<EOF
$rc_listed
EOF
  return 0
}

# RC_UNITS のうち active でないものを RC_BAD に入れる（インスタンス名。.service は付けない）。
remote_control_check() {
  RC_BAD=''
  for rc_u in $RC_UNITS; do
    if ! systemctl --user is-active --quiet "$rc_u" 2>/dev/null; then
      rc_i=${rc_u#claude-rc@}
      RC_BAD=$RC_BAD${RC_BAD:+, }${rc_i%.service}
    fi
  done
}

# RC_UNITS を、プロジェクト設定を置き換えた名前（RC_PROJECT_DIRS）の窓口だけに絞る。
remote_control_project_units() {
  rc_only=''
  for rc_u in $RC_UNITS; do
    rc_i=${rc_u#claude-rc@}
    case " ${RC_PROJECT_DIRS:-} " in
    *" ${rc_i%.service} "*) rc_only=$rc_only${rc_only:+ }$rc_u ;;
    esac
  done
  RC_UNITS=$rc_only
}

# 再起動の後、全部が active になるまで最大 RC_WAIT_SECONDS 秒待ち、続けて落ちていないかを確かめ直す。
remote_control_wait() {
  rc_waited=0
  remote_control_check
  while [ -n "$RC_BAD" ] && [ "$rc_waited" -lt "${RC_WAIT_SECONDS:-30}" ]; do
    sleep 1
    rc_waited=$((rc_waited + 1))
    remote_control_check
  done
  if [ -z "$RC_BAD" ]; then
    sleep "${RC_SETTLE_SECONDS:-2}"
    remote_control_check
  fi
}

# $1 の各ユニットの起動回数の制限を解いてから restart し、起動を確かめる。restart が拒まれたものと起動しなかった
# ものを RC_BAD に入れる（Restart=on-failure で落ち続けると start-limit-hit になり、解かないと restart が拒まれる）。
remote_control_restart() {
  rc_rejected=''
  rc_started=''
  for rc_u in $1; do
    systemctl --user reset-failed "$rc_u" >/dev/null 2>&1 || :
    if systemctl --user restart "$rc_u" >/dev/null 2>&1; then
      rc_started=$rc_started${rc_started:+ }$rc_u
    else
      rc_i=${rc_u#claude-rc@}
      rc_rejected=$rc_rejected${rc_rejected:+, }${rc_i%.service}
    fi
  done
  RC_UNITS=$rc_started
  remote_control_wait
  if [ -n "$rc_rejected" ]; then
    RC_BAD=$rc_rejected${RC_BAD:+, }$RC_BAD
  fi
}

# 前の内容（退避した写し）へ戻す。許可一覧が元から無ければ消す。
# 戻せなかった退避ファイルは RC_KEPT（空白区切り）に入れ、1つでも戻せなければ 1 を返す。
remote_control_restore() {
  RC_KEPT=''
  if command -v remote_control_guard >/dev/null; then
    remote_control_guard restore || return 1
  fi
  if ! mv -f "$rc_prev_unit" "$rc_unit" 2>/dev/null; then
    RC_KEPT=$rc_prev_unit
  fi
  if [ "$rc_had_set" = 1 ]; then
    if ! mv -f "$rc_prev_set" "$rc_set" 2>/dev/null; then
      RC_KEPT=$RC_KEPT${RC_KEPT:+ }$rc_prev_set
    fi
  else
    rm -f "$rc_set" 2>/dev/null || :
  fi
  [ -z "$RC_KEPT" ]
}

# 退避ファイルを消す。remote_control_restore が戻せず RC_KEPT に入れたものは、唯一の復旧材料なので消さない。
remote_control_cleanup() {
  case " ${RC_KEPT:-} " in
  *" $rc_prev_unit "*) ;;
  *) rm -f "$rc_prev_unit" 2>/dev/null || : ;;
  esac
  case " ${RC_KEPT:-} " in
  *" $rc_prev_set "*) ;;
  *) rm -f "$rc_prev_set" 2>/dev/null || : ;;
  esac
}

# 未完了記録（$rc_pend）を書く。$1=段階（placing=配置前後・restarting=再起動中）、$2=再起動の対象。一時ファイルから mv。
remote_control_journal() {
  {
    printf 'stage=%s\n' "$1"
    printf 'prev_unit=%s\n' "$rc_prev_unit"
    printf 'prev_set=%s\n' "$rc_prev_set"
    printf 'had_set=%s\n' "$rc_had_set"
    printf 'units=%s\n' "${2:-}"
    # scope=project は、プロジェクト設定だけが変わった回。復旧でも projects の名前の窓口だけを再起動する。
    printf 'scope=%s\n' "${rc_scope:-all}"
    printf 'projects=%s\n' "${RC_PROJECT_DIRS:-}"
  } >"$rc_pend.$$.tmp" 2>/dev/null && mv -f "$rc_pend.$$.tmp" "$rc_pend" 2>/dev/null && return 0
  rm -f "$rc_pend.$$.tmp" 2>/dev/null || :
  return 1
}

# systemctl --user に要る環境変数を整える。
remote_control_env() {
  if [ -z "${XDG_RUNTIME_DIR:-}" ]; then
    XDG_RUNTIME_DIR=/run/user/$(id -u)
  fi
  if [ -z "${DBUS_SESSION_BUS_ADDRESS:-}" ]; then
    DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
  fi
  export XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS
}

# 前回の更新が配置の後で止まっていたら（未完了記録がある）、内容比較より先に前の内容へ戻し、daemon-reload と
# 記録された対象の再起動を終える。終えて起動も確かめられたときだけ記録を消す。記録が無ければ何もしない（0）。
# 復旧した・復旧に失敗したときは1行出して 1 を返す（呼び出し側はその回の配布を見送る）。
remote_control_recover() {
  [ -f "$rc_pend" ] || return 0
  rc_prev_unit=''
  rc_prev_set=''
  rc_had_set=0
  rc_targets=''
  rc_scope=all
  rc_projects=''
  while IFS= read -r rc_line || [ -n "$rc_line" ]; do
    case "$rc_line" in
    prev_unit=*) rc_prev_unit=${rc_line#prev_unit=} ;;
    prev_set=*) rc_prev_set=${rc_line#prev_set=} ;;
    had_set=*) rc_had_set=${rc_line#had_set=} ;;
    units=*) rc_targets=${rc_line#units=} ;;
    scope=project) rc_scope=project ;;
    projects=*) rc_projects=${rc_line#projects=} ;;
    esac
  done <"$rc_pend"
  case "$rc_prev_unit:$rc_prev_set:$rc_had_set" in
  "$rc_unit".prev.?*:"$rc_set".prev.?*:0 | "$rc_unit".prev.?*:"$rc_set".prev.?*:1) ;;
  *)
    say "[update:remote-control-failed] 前回の更新の未完了記録を読めないため、配りません: $rc_pend"
    return 1
    ;;
  esac
  for rc_u in $rc_targets; do
    case "$rc_u" in
    claude-rc@?*.service) ;;
    *)
      say "[update:remote-control-failed] 前回の更新の未完了記録の対象が不正なため、配りません: $rc_pend"
      return 1
      ;;
    esac
  done
  RC_KEPT=''
  if command -v remote_control_guard >/dev/null; then
    remote_control_guard restore || return 1
  fi
  if [ -f "$rc_prev_unit" ] && ! mv -f "$rc_prev_unit" "$rc_unit" 2>/dev/null; then
    RC_KEPT=$rc_prev_unit
  fi
  if [ "$rc_had_set" = 1 ]; then
    if [ -f "$rc_prev_set" ] && ! mv -f "$rc_prev_set" "$rc_set" 2>/dev/null; then
      RC_KEPT=$RC_KEPT${RC_KEPT:+ }$rc_prev_set
    fi
  else
    rm -f "$rc_set" 2>/dev/null || :
  fi
  if [ -n "$RC_KEPT" ]; then
    say "[update:remote-control-failed] 前回の更新を前の内容へ戻せませんでした。退避と未完了記録を残しました: $RC_KEPT"
    return 1
  fi
  remote_control_env
  RC_UNITS=''
  RC_BAD=''
  if ! user_units_reload; then
    say '[update:remote-control-failed] 前回の更新を戻しましたが、systemctl --user daemon-reload に失敗しました。未完了記録を残しました'
    return 1
  fi
  if [ -z "$rc_targets" ]; then
    if ! remote_control_active; then
      say '[update:remote-control-failed] 前回の更新を戻しましたが、systemctl --user list-units に失敗しました。未完了記録を残しました'
      return 1
    fi
    if [ "$rc_scope" = project ]; then
      RC_PROJECT_DIRS=$rc_projects
      remote_control_project_units
    fi
    rc_targets=$RC_UNITS
    # 再起動の途中で止まっても対象を失わないよう、再起動の前に対象を記録へ残す。残せなければ再起動しない。
    if ! remote_control_journal restarting "$rc_targets"; then
      say '[update:remote-control-failed] 前回の更新を戻しましたが、再起動する対象を未完了記録へ書けないため再起動していません。未完了記録を残しました'
      return 1
    fi
  fi
  remote_control_restart "$rc_targets"
  if [ -n "$RC_BAD" ]; then
    say "[update:remote-control-failed] 前回の更新を戻しましたが、起動しなかったインスタンス: $RC_BAD。未完了記録を残しました"
    return 1
  fi
  rm -f "$rc_prev_unit" "$rc_prev_set" "$rc_pend" 2>/dev/null || :
  say "[update:remote-control] 前回の更新が途中で止まっていたため、前の定義へ戻して ${rc_targets:-（動いているインスタンスなし）} を再起動しました"
  return 1
}

# 毎回の実行の最後に呼ぶ（update.sh の finish）。--dry-run では配らず予定だけを表示する。
remote_control() {
  case "${HOME:-}" in
  /*) ;;
  *)
    say '[update:remote-control-failed] HOME が絶対パスではないため、remote-control の起動定義と許可一覧を配りません'
    return 0
    ;;
  esac
  rc_unit=$HOME/.config/systemd/user/claude-rc@.service
  [ -f "$rc_unit" ] || return 0
  rc_set=$HOME/.issue-desk/remote-control/settings.json
  rc_src_unit=$DIR/deploy/nas/claude-rc@.service
  rc_src_set=$DIR/deploy/nas/remote-control-settings.json
  if [ "$DRY_RUN" = 1 ]; then
    plan "予定（最後に）: $rc_src_set を $rc_set へ、$rc_src_unit を $rc_unit へ配り、どちらかが変わるときだけ動いている claude-rc@*.service を再起動する（起動しなければ前の定義へ戻す）"
    return 0
  fi
  rc_pend=$rc_unit.update-pending
  if command -v remote_control_guard >/dev/null; then
    remote_control_guard allow || return 0
  fi
  remote_control_recover || return 0
  if [ ! -f "$rc_src_unit" ] || [ ! -f "$rc_src_set" ]; then
    say '[update:remote-control-failed] claude-rc@.service か remote-control-settings.json を読めないため、配りません'
    return 0
  fi
  if command -v remote_control_guard >/dev/null; then
    remote_control_guard begin || return 0
  fi
  rc_changed=0
  if [ ! -f "$rc_set" ] || ! cmp -s "$rc_src_set" "$rc_set"; then
    rc_changed=1
  fi
  rc_unit_comment_only=0
  if ! cmp -s "$rc_src_unit" "$rc_unit"; then
    if remote_control_same_unit "$rc_src_unit" "$rc_unit"; then
      rc_unit_comment_only=1
    else
      rc_changed=1
    fi
  fi
  rc_core_changed=$rc_changed
  rc_scope=all
  [ "$rc_core_changed" = 1 ] || rc_scope=project
  if command -v remote_control_guard >/dev/null; then
    remote_control_guard changed
  fi
  # コメントだけ違うユニットも配る（次の起動から同じ内容にする）が、再起動の対象はプロジェクト設定の分だけにする。
  [ "$rc_unit_comment_only" = 0 ] || rc_changed=1
  [ "$rc_changed" = 1 ] || return 0

  rc_prev_unit=$rc_unit.prev.$$
  rc_prev_set=$rc_set.prev.$$
  rc_had_set=0
  RC_KEPT=''
  if [ -f "$rc_set" ]; then
    rc_had_set=1
  fi
  if ! cp "$rc_unit" "$rc_prev_unit" 2>/dev/null || { [ "$rc_had_set" = 1 ] && ! cp "$rc_set" "$rc_prev_set" 2>/dev/null; }; then
    remote_control_cleanup
    say '[update:remote-control-failed] 前の内容を退避できないため、配りません'
    return 0
  fi
  rc_before=$rc_unit.before-issue-desk
  if [ ! -e "$rc_before" ] && ! cmp -s "$rc_src_unit" "$rc_unit"; then
    if ! cp "$rc_unit" "$rc_before.$$.tmp" 2>/dev/null || ! mv "$rc_before.$$.tmp" "$rc_before" 2>/dev/null; then
      rm -f "$rc_before.$$.tmp" 2>/dev/null || :
      remote_control_cleanup
      say '[update:remote-control-failed] 元のユニットを claude-rc@.service.before-issue-desk へ写せないため、配りません'
      return 0
    fi
  fi
  if ! remote_control_journal placing ''; then
    remote_control_cleanup
    say '[update:remote-control-failed] 未完了記録を書けないため、配りません'
    return 0
  fi
  if ! { mkdir -p "${rc_set%/*}" 2>/dev/null && remote_control_put "$rc_src_set" "$rc_set" && remote_control_put "$rc_src_unit" "$rc_unit"; }; then
    if remote_control_restore; then
      remote_control_cleanup
      rm -f "$rc_pend" 2>/dev/null || :
      say '[update:remote-control-failed] 書けなかったため、前の内容へ戻しました（再起動していません）'
    else
      remote_control_cleanup
      say "[update:remote-control-failed] 書けず、前の内容へも戻せませんでした（再起動していません）。退避を残しました: $RC_KEPT"
    fi
    return 0
  fi

  remote_control_env

  RC_UNITS=''
  RC_BAD=''
  rc_restarted=''
  rc_step=''
  if ! user_units_reload; then
    rc_step='daemon-reload'
  elif ! remote_control_active; then
    rc_step='list-units'
  else
    [ "$rc_core_changed" = 1 ] || remote_control_project_units
    rc_restarted=$RC_UNITS
    # 対象の記録は再起動の前に必須。残せなければ再起動せず、前の定義へ戻す（記録は placing のまま残る）。
    if remote_control_journal restarting "$rc_restarted"; then
      remote_control_restart "$rc_restarted"
    else
      rc_step='journal'
    fi
  fi
  if [ -z "$rc_step" ] && [ -z "$RC_BAD" ]; then
    if command -v remote_control_guard >/dev/null; then
      remote_control_guard success || return 0
    fi
    remote_control_cleanup
    rm -f "$rc_pend" 2>/dev/null || :
    if [ "$rc_core_changed" = 0 ] && [ "$rc_unit_comment_only" = 1 ] && [ -z "$rc_restarted" ] && [ -z "${RC_PROJECT_DIRS:-}" ]; then
      say '[update:remote-control] 起動定義はコメントだけが変わったため、配り直しましたが再起動していません'
    elif [ "$rc_core_changed" = 0 ]; then
      if [ -n "$rc_restarted" ]; then
        say "[update:remote-control] プロジェクト設定が変わったため、$rc_restarted を再起動しました"
      else
        say "[update:remote-control] プロジェクト設定を配りました（窓口の無い場所のため再起動していません）: ${RC_PROJECT_DIRS:-}"
      fi
    elif [ -n "$rc_restarted" ]; then
      say "[update:remote-control] 起動定義か許可一覧が変わったため、$rc_restarted を再起動しました"
    else
      say '[update:remote-control] 起動定義か許可一覧を更新しました（動いているインスタンスはありません）'
    fi
    return 0
  fi

  rc_bad_new=$RC_BAD
  rc_restored=1
  remote_control_restore || rc_restored=0
  remote_control_cleanup
  user_units_reload || :
  if [ "$rc_restored" = 0 ]; then
    if [ -n "$rc_step" ]; then
      if [ "$rc_step" = journal ]; then
        rc_what='未完了記録への対象の書き込み'
      else
        rc_what="systemctl --user $rc_step"
      fi
      say "[update:remote-control-failed] $rc_what に失敗し、前の定義へも戻せませんでした。退避を残しました: $RC_KEPT"
    else
      say "[update:remote-control-failed] 起動しなかったインスタンス: $rc_bad_new。前の定義へ戻せませんでした（再起動していません）。退避を残しました: $RC_KEPT"
    fi
    return 0
  fi
  if [ "$rc_step" = journal ]; then
    say '[update:remote-control-failed] 再起動する対象を未完了記録へ書けないため、再起動せず前の定義へ戻しました'
    return 0
  fi
  if [ -n "$rc_step" ]; then
    say "[update:remote-control-failed] systemctl --user $rc_step に失敗したため、前の定義へ戻しました"
    return 0
  fi
  if command -v remote_control_guard >/dev/null; then
    remote_control_guard failed || return 0
  fi
  remote_control_restart "$rc_restarted"
  if [ -n "$RC_BAD" ]; then
    say "[update:remote-control-failed] 起動しなかったインスタンス: $rc_bad_new。前の定義へ戻しましたが、戻した後も起動しなかったインスタンス: $RC_BAD"
    return 0
  fi
  rm -f "$rc_pend" 2>/dev/null || :
  say "[update:remote-control-failed] 起動しなかったインスタンス: $rc_bad_new。前の定義へ戻して再起動しました"
  return 0
}

# ユーザーユニットの共通配置口。出力は変更名のみ。未完了の印は呼び出し側が
# 有効化を終えてから消す。reload/有効化の失敗や中断は次回に再試行する。
user_units_reload() {
  systemctl --user daemon-reload >/dev/null 2>&1
}

user_units_place() (
  case "${HOME:-}" in /*) ;; *) return 1 ;; esac
  uu_dest=$HOME/.config/systemd/user
  uu_pending=$uu_dest/.issue-desk-units-pending
  mkdir -p "$uu_dest" "$uu_pending" || return 1
  for uu_src in "$1"/*; do
    [ -f "$uu_src" ] || continue
    uu_name=${uu_src##*/}
    case "$uu_name" in *.service|*.timer) ;; *) return 1 ;; esac
    remote_control_put "$uu_src" "$uu_dest/$uu_name" "$uu_pending/$uu_name" || return 1
  done
  uu_changed=''
  for uu_src in "$1"/*; do
    uu_name=${uu_src##*/}
    [ -f "$uu_pending/$uu_name" ] || continue
    uu_changed="$uu_changed${uu_changed:+ }$uu_name"
  done
  [ -n "$uu_changed" ] || return 0
  user_units_reload || return 1
  printf '%s\n' "$uu_changed"
)

user_units_ack() {
  for uu_name in "$@"; do
    rm -f "$HOME/.config/systemd/user/.issue-desk-units-pending/$uu_name" || return 1
  done
}
