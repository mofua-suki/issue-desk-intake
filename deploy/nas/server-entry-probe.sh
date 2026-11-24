#!/bin/sh
# deploy-server.shから読み込む入口計測。関数は呼出元のシェルで公開し、既存の呼出しを保つ。
# 今の時刻（ミリ秒）。update.sh の now_ms と同じ（ミリ秒を出せなければ $1 が start のとき次の秒の頭）。
now_ms() {
  ms=$(date +%s%3N 2>/dev/null) || ms=''
  case "$ms" in
  '' | *[!0-9]*) ms='' ;;
  esac
  if [ "${#ms}" -ge 13 ] && [ "${#ms}" -le 15 ]; then
    echo "$ms"
  elif [ "${1:-}" = start ]; then
    echo $((($(date +%s) + 1) * 1000))
  else
    echo $(($(date +%s) * 1000))
  fi
}

# 測定は別のシェルで行い、失敗しても入れ替えの判定には使わない。
# 応答の本文は捨て、HTTP 200 だけを成功とする。wget と docker の両方に短い期限を置く。
probe_once() {
  probe_start=$(now_ms)
  probe_total=$((probe_total + 1))
  probe_headers=$(bounded 1 docker exec "$ENTRY" wget -S -T 1 -O /dev/null http://127.0.0.1:8787/healthz 2>&1) && probe_ok=1 || probe_ok=0
  probe_http=$(printf '%s\n' "$probe_headers" | grep -m 1 -E '^  HTTP/[0-9.]+ ') || probe_http=''
  if [ "$probe_ok" = 1 ] && printf '%s\n' "$probe_http" | grep -Eq '^  HTTP/[0-9.]+ 200 '; then
    if [ "$probe_since" -gt 0 ]; then
      probe_span=$(($(now_ms) - probe_since))
      [ "$probe_span" -le "$probe_longest" ] || probe_longest=$probe_span
    fi
    probe_since=0
  else
    probe_failures=$((probe_failures + 1))
    [ "$probe_since" -ne 0 ] || probe_since=$probe_start
  fi
}
start_probe() {
  PROBE_DIR=$(mktemp -d) || return 0
  probe_total=0 probe_failures=0 probe_longest=0 probe_since=0
  probe_once
  (
    set -e
    while [ ! -e "$PROBE_DIR/stop" ]; do
      command -p sleep 1 || exit 1
      [ ! -e "$PROBE_DIR/stop" ] || break
      probe_once
    done
    printf '%s %s %s %s\n' "$probe_total" "$probe_failures" "$probe_longest" "$probe_since" >"$PROBE_DIR/counts"
  ) >/dev/null 2>&1 &
  PROBE_PID=$!
}
finish_probe() {
  [ -n "$PROBE_DIR" ] || return 0
  [ -n "$PROBE_PID" ] || { rm -rf "$PROBE_DIR"; return 0; }
  if ! printf '' >"$PROBE_DIR/stop"; then
    kill "$PROBE_PID" 2>/dev/null || :
    wait "$PROBE_PID" 2>/dev/null || :
    rm -rf "$PROBE_DIR"
    return 0
  fi
  if ! wait "$PROBE_PID" 2>/dev/null; then
    rm -rf "$PROBE_DIR"
    return 0
  fi
  (
  # このサブシェルの失敗も元の終了コードへ伝えない。失敗時も一時ファイルを片付ける。
  trap 'rm -rf "$PROBE_DIR"' 0
  read -r probe_total probe_failures probe_longest probe_since <"$PROBE_DIR/counts" || exit 0
  probe_end=$(now_ms)
  if [ "$probe_since" -gt 0 ]; then
    probe_span=$((probe_end - probe_since))
    [ "$probe_span" -le "$probe_longest" ] || probe_longest=$probe_span
  fi
  probe_json=$(printf '{"total":%s,"failures":%s,"longestFailureMs":%s,"revision":"%s","completedAt":%s}' "$probe_total" "$probe_failures" "$probe_longest" "$EXPECT" "$probe_end")
  bounded "$READ_LIMIT" docker run --rm -v "$STATE_VOLUME":/state alpine \
    sh -c 'd=/state/runtime/state; [ -d "$d" ] || exit 3; printf "%s\n" "$1" >"$d/intake-handoff.json.tmp" && mv -f "$d/intake-handoff.json.tmp" "$d/intake-handoff.json"' \
    sh "$probe_json" >/dev/null 2>&1 || exit 0
  say "[server:probe] $probe_json"
  )
}

