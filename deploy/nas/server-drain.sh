#!/bin/sh
# deploy-server.sh の heartbeat・drain ファイル操作。呼び出し元の bounded と設定を使う。

# 応答の切れ目は通常業務を止めず5分待つ（2026-10-24 の受付AIの決定）。
DRAIN_GAP_SECONDS=300
# 全枠・起票を止める待ちは2分までにする（同日の600秒の見送り反復を防ぐ）。
DRAIN_WAIT_SECONDS=120

# 要求前の観察と要求後の待機は、別々の絶対期限を使う。
# サブシェルで bounded に期限を渡し、交代処理の期限や READ_LIMIT を変更しない。
drain_heartbeat() (
  ENTRY_DEADLINE=$1
  shift
  heartbeat_ok "$@"
)
wait_drain() {
  [ "$OLD_RUNNING" = 1 ] || return 0
  gap_start=$(now_ms)
  gap_seconds=$DRAIN_GAP_SECONDS
  [ "$gap_seconds" -le "$TIMEOUT" ] || gap_seconds=$TIMEOUT
  gap_deadline=$((gap_start + gap_seconds * 1000))
  gap_found=false
  while [ "$(now_ms)" -lt "$gap_deadline" ]; do
    if drain_heartbeat "$gap_deadline" any 0 idle; then gap_found=true; break; fi
    pause_until "$gap_deadline" || break
  done
  say "[server:drain-gap] waited_ms=$(($(now_ms) - gap_start)) gap=$gap_found"

  drain_ms=$(now_ms start)
  drain_wait=$DRAIN_WAIT_SECONDS
  [ "$drain_wait" -le "$TIMEOUT" ] || drain_wait=$TIMEOUT
  drain_deadline=$((drain_ms + drain_wait * 1000))
  drain_seconds=$((drain_wait + 60))
  [ "$drain_seconds" -le "$DRAIN_MAX_SECONDS" ] || drain_seconds=$DRAIN_MAX_SECONDS
  token=$((drain_ms + drain_seconds * 1000))
  drain_put=0
  if (ENTRY_DEADLINE=$drain_deadline; put_drain "$token"); then
    drain_put=1
    while [ "$(now_ms)" -lt "$drain_deadline" ]; do
      if drain_heartbeat "$drain_deadline" any "$drain_ms" idle "$token"; then return 0; fi
      pause_until "$drain_deadline" || break
    done
  fi
  # 暫定（2026-10-26、利用者の明示）: 受付の相談・起票が途切れず見送りが続くと、受付の負荷の直しも入らない。
  # update-intake.sh が見送り2回以上で ISSUE_DESK_FORCE_DRAIN=1 を渡したときは、要求を置けていれば途中の処理が
  # あっても入れ替えへ進む（旧色は SIGUSR2 で後ろの処理を閉じ、相談は新色の起動時の再開で続く）。直ったら外す。
  if [ "${ISSUE_DESK_FORCE_DRAIN:-0}" = 1 ] && [ "$drain_put" = 1 ]; then
    say "[server:forced:drain] 見送りが続いたため、動いている受付（$ACTIVE）に途中の処理があっても入れ替えます"
    return 0
  fi
  # 診断と解除もDockerの作成・起動を伴うため、通常の読み取り予算を確保する。
  detail=$(drain_detail)
  clear_drain
  docker stop "$NEW" >/dev/null 2>&1 || :
  say "[server:skipped:drain] 動いている受付（$ACTIVE）が新しい処理を止められない（応答中・起票中）ため、入れ替えを見送ります。新しい版（$IDLE）は止めました。$detail"
  exit 3
}

# 受付の heartbeat を読み、版が $1（any なら問わない）、時刻が $2（ミリ秒）以降、$3 が idle なら busy でないかを1回確かめる。
# $4 は入れ替えの要求の期限（止めた印の一致も確かめる）。
heartbeat_ok() {
  hb=$(bounded "$READ_LIMIT" docker run --rm -v "$STATE_VOLUME":/state:ro alpine sh -c "$HB_READ" sh "${4:-}" 2>/dev/null) || return 1
  case "$hb" in *"$NL"*) return 1 ;; esac
  printf '%s\n' "$hb" | grep -Eq "$HB_FORMAT" || return 1
  hb_time=${hb#*:}
  hb_time=${hb_time%%,*}
  [ "$hb_time" -ge "$2" ] 2>/dev/null || return 1
  [ "$1" = any ] || case "$hb" in *"\"revision\":\"$1\""*) ;; *) return 1 ;; esac
  [ "${3:-}" != idle ] || case "$hb" in *'"busy":false}') ;; *) return 1 ;; esac
}

# 入れ替えの要求を置く・消す（受付の volume。受付が一度も起動していなければ置かない）。
put_drain() {
  bounded "$READ_LIMIT" docker run --rm -v "$STATE_VOLUME":/state alpine \
    sh -c 'd=/state/runtime/state; [ -d "$d" ] || exit 3; echo "$1" >"$d/intake-drain.request.tmp" && mv -f "$d/intake-drain.request.tmp" "$d/intake-drain.request"' \
    sh "$1" >/dev/null 2>&1
}
clear_drain() {
  bounded "$READ_LIMIT" docker run --rm -v "$STATE_VOLUME":/state alpine \
    sh -c 'rm -f /state/runtime/state/intake-drain.request /state/runtime/state/intake-drain.request.tmp' \
    >/dev/null 2>&1 || :
}

# 要求を消す前に読む。診断の失敗は見送り・後片付けの判定を変えない。
drain_detail() {
  detail_code=0
  detail=$(bounded "$READ_LIMIT" docker run --rm -v "$STATE_VOLUME":/state:ro alpine \
    sh -c 'f=/state/runtime/state/intake-drain.status; [ -e "$f" ] || exit 3; head -c 8193 "$f"' 2>/dev/null) || detail_code=$?
  if [ "$detail_code" != 0 ]; then
    echo "内訳なし（読み取り失敗 code=$detail_code）"
    return
  fi
  case "$detail" in
    ''|*"$NL"*) echo '内訳なし（空または複数行）'; return ;;
  esac
  if [ "${#detail}" -gt 8192 ]; then
    echo '内訳なし（大きさ超過）'
    return
  fi
  case "$detail" in
    '{"until":'*'}') printf '内訳=%s\n' "$detail" ;;
    *) echo '内訳なし（形式不正）' ;;
  esac
}
