#!/bin/sh
# NAS の受付（server）のブルーグリーンの入れ替え（docs/server-blue-green.md）。deploy/nas/update.sh が、受付が有効なとき
# （ISSUE_DESK_INTAKE=1）にイメージを作った後で呼ぶ。手で呼ぶときも、先にイメージ（issue-desk-intake:latest）を作っておく。
#
# 受付のコンテナは blue・green の2色で（compose.yaml の issue-desk-intake-blue・-green）、入口（issue-desk-intake-entry の
# Caddy）が volume issue-desk-intake-entry の active.caddy に書いた色へ転送する。書き手（タイマー・受付AI）は
# 共有DBの書き手のロックを取った1つだけ（server/internal/leadership/leader.go）。手順:
#   1. 入口の active.caddy から動いている色（A）と止まっている色（B）を決める。
#   2. B を新しいイメージで作り直して起動する（A がロックを持つので B は待機で、/healthz だけを返す）。
#   3. B の /healthz（docker exec。DB・スキーマの版・画面のファイル・版）が通り、版が更新先になるのを待つ。
#   4. A に入れ替えの要求を置き（server/internal/runtimegate。update.sh の従来の方式と同じ）、止めた印と busy でない heartbeat を待つ。
#   5. 入口に両色の active 健康確認と再試行を設定し、A に SIGUSR2 を送る。A は HTTP・後ろの処理・資源を閉じてロックを返す。
#   6. B の heartbeat（版が更新先、時刻が引き継ぎ後）と /healthz の role が active になるのを待つ。A のコンテナは残す。
#   7. 入口の優先色を B にして reload し、入口を通した /healthz を確認した後に A のコンテナを止める。
#   6・7 で確かめられなければ、B を止めて A を起動し直し、入口を A へ戻して、A が書き手として答えることを確かめる。
# DB は戻さない（マイグレーションは足すだけ。docs/shared-db.md）。
# 2・3 で失敗したときは A を止めていないので、B を止めるだけで終わる。4 で止められなければ何も変えずに見送る。
#
# 終了コード: 0 入れ替えた / 1 新しい版を確かめられず、前の版のまま（戻して確かめた） / 2 前の版へ戻せていない（手で復旧）
#             3 A が新しい処理を止められない（応答中・起票中）ため見送った（何も変えていない） / 4 入口・色を確かめられない
#             （何も変えていない） / 64 使い方の誤り
# journal の行は [server:<結果>] で見分ける。
#
#   sh deploy/nas/deploy-server.sh [--revision <コミット>] [--dry-run]
#   --revision  新しいイメージの版（ISSUE_DESK_REVISION）。省略すると配置先の git rev-parse HEAD。
set -eu

usage() {
  echo '使い方: sh deploy/nas/deploy-server.sh [--revision <40桁のコミット>] [--dry-run]' >&2
  exit 64
}
DRY_RUN=0
EXPECT=''
while [ "$#" -gt 0 ]; do
  case "$1" in
  --dry-run) DRY_RUN=1 ;;
  --revision)
    [ "$#" -ge 2 ] || usage
    EXPECT=$2
    shift
    ;;
  *) usage ;;
  esac
  shift
done
case "$0" in
*/*) SCRIPT_DIR=${0%/*} ;;
*) SCRIPT_DIR=. ;;
esac
DIR=$(CDPATH= cd -- "${SCRIPT_DIR:-/}/../.." && pwd)
if [ -z "$EXPECT" ]; then
  EXPECT=$(git -C "$DIR" rev-parse HEAD) || usage
fi
case "$EXPECT" in
*[!0-9a-f]*) usage ;;
esac
[ "${#EXPECT}" -eq 40 ] || usage

PREFIX=issue-desk-intake
ENTRY_SERVICE=issue-desk-intake-entry
ENTRY_VOLUME=issue-desk-intake-entry
STATE_VOLUME=issue-desk-intake-state
# 待つ上限（秒。/healthz・入れ替えの要求・書き手の交代・前の版の起動し直しのそれぞれ）と確認の間隔（update.sh と同じ変数）。
# temporary-code: {"id":"nas-startup-timeout-4193","until":"mofua-suki/issue-desk#4257 merged / mofua-suki/issue-desk#4266 merged / NAS 更新の受付の入れ替えで、起動から heartbeat までが180秒内に収まることを更新の記録で確かめた（2026-10-25 にも受付の CPU 張り付きが再発）"}
# 一時的に600秒へ延長。受付の負荷に関する根本修正後は180秒へ戻す。
TIMEOUT=${ISSUE_DESK_UPDATE_TIMEOUT:-600}
INTERVAL=${ISSUE_DESK_UPDATE_INTERVAL:-5}
for n in "$TIMEOUT" "$INTERVAL"; do
  case "$n" in
  '' | *[!0-9]* | ???????*)
    echo "待機の秒数は0以上の整数（6桁まで）で指定してください: $n" >&2
    exit 64
    ;;
  esac
done
# 1回の docker の読み取り・確認に待つ上限（秒）。
READ_LIMIT=30
# 旧色へ引き継ぎを要求してから、新色が書き手になり入口を通して active を答えるまでの期限（ミリ秒）。旧色が受け付け済みの
# 要求と後ろの処理を終えてロックを返すまでの数秒、新色がロックを試す間隔2秒（server/internal/leadership/leader.go）、取った後に
# 待ち受けを始めるまでの約3秒（2026-10-24 の受付のログ）と、入口の切り替え・確認を含める。2026-10-24 は8秒で旧色の
# 返却が間に合わず3回とも切り戻した。打ち切って切り戻すと旧色の起動し直しの分だけ断が延びるので、入口断30秒未満の方針で
# 切り戻しの実際の所要（約10秒）を残せる長さにする。
# 一時（2026-10-24、利用者の明示）: 旧色は各PCの POST /api/v1/jobs/next（最長60秒）の終了を待ってからロックを返すため、
# 15秒では毎回 switched-back:takeover になる。60秒の問い合わせと旧色の終了・新色の起動の余裕を足して75秒にし、入口断30秒
# 未満の方針から一時的に外れる。交代の合図で jobs/next を即時に終える根本の直しが入ったら 15000 に戻す。
# 戻し条件は temporary.json の nas-handoff-75s-4655（このファイルは別の項目で登録済みのため目印は置かない）。
HANDOFF_MS=75000
# 切り戻しの期限（ミリ秒。切り戻し開始から）。両色の停止確認の後に旧色を起動し直し、heartbeat・active・入口を確かめる。
RECOVERY_MS=45000
# 切り戻しの確認（heartbeat・直接の /healthz・入口）1回の上限（秒）。docker exec で node を起動して読むので1秒では足りない。
RECOVERY_CHECK_LIMIT=3
# 入れ替えの要求の有効期限（秒）の上限。受付は15分より先の期限を壊れた要求として扱う（server/internal/runtimegate）。
DRAIN_MAX_SECONDS=840
# 受付の heartbeat の1行（server/internal/runtimegate の intakeHeartbeatBody）。update.sh の INTAKE_HB_FORMAT と同じ。
HB_FORMAT='^[{]"time":(0|[1-9][0-9]{0,14}),"pid":(0|[1-9][0-9]{0,9}),"revision":("[0-9a-f]{40}"|null),"busy":(true|false)[}]$'
# heartbeat を読むコンテナの中の処理。$1（入れ替えの要求の期限）があれば、止めた印が一致するときだけ読む（違えば 4）。
HB_READ='if [ -n "$1" ]; then h=$(cat /state/runtime/state/intake-drain.held 2>/dev/null) || h=""; [ "$h" = "$1" ] || exit 4; fi; f=/state/runtime/state/intake-heartbeat.json; [ -e "$f" ] || exit 3; cat "$f"'
# 受付のコンテナの中で /healthz を読む（1行の JSON を出し、200 なら 0）。
HEALTH_JS='fetch("http://127.0.0.1:8787/healthz").then(async (r) => { process.stdout.write((await r.text()).replace(/\n/g, "")); process.exit(r.ok ? 0 : 1); }, () => process.exit(2))'
CADDYFILE=/tmp/issue-desk-intake-Caddyfile
NL='
'

say() { echo "$(date -Is) $*"; }
compose() { docker compose -f "$DIR/deploy/nas/compose.yaml" --profile intake "$@"; }

. "$DIR/deploy/nas/server-entry-probe.sh"
. "$DIR/deploy/nas/update-intake-batch.sh"

# timeout -s KILL で $1 秒を上限に、終わらないコマンドを強制終了する。
# ENTRY_DEADLINE があれば、その残り時間によって期限はさらに短くなる。
bounded() {
  limit=$1
  shift
  if [ -n "${ENTRY_DEADLINE:-}" ]; then
    budget=$(((ENTRY_DEADLINE - $(now_ms)) / 1000))
    [ "$budget" -gt 0 ] || return 124
    [ "$limit" -le "$budget" ] || limit=$budget
  fi
  code=0
  timeout -s KILL "$limit" "$@" || code=$?
  if [ -n "${ENTRY_DEADLINE:-}" ]; then
    [ "$(now_ms)" -lt "$ENTRY_DEADLINE" ] || return 124
  fi
  return "$code"
}

# 出力は固定の操作名・色・状態と数値だけ。Dockerのstderrや要求本文は記録しない。
operation_result() {
  result_kind=command-failed
  [ "$4" != 0 ] || result_kind=ok
  if [ -n "${ENTRY_DEADLINE:-}" ] && [ "$(now_ms)" -ge "$ENTRY_DEADLINE" ]; then
    result_kind=deadline
  elif [ "$4" = 124 ] || [ "$4" = 137 ]; then
    result_kind=timeout
  fi
  say "[server:operation] op=$1 color=$2 state=$3 code=$4 result=$result_kind deadline=${ENTRY_DEADLINE:-none} now=$(now_ms) budget=${5:-$READ_LIMIT}" >&2
}
operation() {
  operation_name=$1 operation_color=$2 operation_state=$3
  shift 3
  operation_code=0
  bounded "$READ_LIMIT" "$@" >/dev/null 2>&1 || operation_code=$?
  if [ "$operation_code" != 0 ]; then
    operation_result "$operation_name" "$operation_color" "$operation_state" "$operation_code"
  fi
  return "$operation_code"
}
# 停止済みなら停止待ちを省く。読めない状態は推測せず復旧失敗とする。
recovery_stop() {
  stop_state=unknown stop_code=0
  stop_state=$(bounded 1 docker inspect --format '{{.State.Running}}' "$2" 2>/dev/null) || stop_code=$?
  if [ "$stop_code" != 0 ]; then
    operation_result "inspect-$1" "$3" unknown "$stop_code"
    return 1
  fi
  case "$stop_state" in
    false) operation_result "stop-$1" "$3" already-stopped 0; return 0 ;;
    true) ;;
    *) operation_result "inspect-$1" "$3" unknown 1; return 1 ;;
  esac
  # Docker側の猶予1秒、クライアント2秒。打ち切り後もdaemonの停止完了を必ず読む。
  stop_code=0
  say "[server:stop-budget] color=$3 client=2 grace=1 inspect=1 deadline=$ENTRY_DEADLINE" >&2
  bounded 2 docker stop --time 1 "$2" >/dev/null 2>&1 || stop_code=$?
  operation_result "stop-$1" "$3" running "$stop_code" 2
  while :; do
    stop_code=0
    stop_state=$(bounded 1 docker inspect --format '{{.State.Running}}' "$2" 2>/dev/null) || stop_code=$?
    if [ "$stop_code" != 0 ]; then
      operation_result "inspect-$1" "$3" unknown "$stop_code"; return 1
    fi
    case "$stop_state" in
      false) operation_result "inspect-$1" "$3" stopped 0; return 0 ;;
      true) ;;
      *) operation_result "inspect-$1" "$3" unknown 1; return 1 ;;
    esac
    if [ "$(now_ms)" -ge "$((ENTRY_DEADLINE - 1000))" ]; then
      operation_result "inspect-$1" "$3" running 124; return 1
    fi
    sleep 1
  done
}

# 間隔を置く（残りが間隔より短ければ残りだけ）。$1 は期限（ミリ秒）。期限を過ぎていれば 1。
pause_until() {
  left=$(($1 - $(now_ms)))
  [ "$left" -gt 0 ] || return 1
  if [ $((INTERVAL * 1000)) -lt "$left" ]; then
    sleep "$INTERVAL"
  else
    sleep $(((left + 999) / 1000))
  fi
}

# サービスのコンテナの ID（止まっていても。無ければ空）。最初の行だけを使う。
cid_of() {
  c=$(compose ps -a -q "$1" 2>/dev/null) || c=''
  printf '%s' "${c%%"$NL"*}"
}

# 入口の優先色（blue・green。旧1行形式も読む）。読めない・形が違うときは 1。
read_active() {
  line=$(bounded "$READ_LIMIT" docker run --rm -v "$ENTRY_VOLUME":/entry:ro alpine cat /entry/active.caddy 2>/dev/null) || return 1
  line=${line%%"$NL"*}
  line=${line%" {"}
  case "$line" in
  "reverse_proxy $PREFIX-blue:8787") echo blue ;;
  "reverse_proxy $PREFIX-green:8787") echo green ;;
  *) return 1 ;;
  esac
}

# 入口の転送先を $1 の色にして reload する。
switch_entry() {
  operation entry-write "$1" entry docker exec "$ENTRY" sh -c \
    'set -e
    if [ "${2:-0}" = 1 ]; then
      printf "reverse_proxy issue-desk-intake-%s:8787\n" "$1" >/entry/active.caddy.tmp
    else
      if [ "$1" = blue ]; then
        other=green
      else
        other=blue
      fi
      printf "reverse_proxy issue-desk-intake-%s:8787 {\n  to issue-desk-intake-%s:8787\n  import intake_retry\n}\n" "$1" "$other" >/entry/active.caddy.tmp
    fi
    mv -f /entry/active.caddy.tmp /entry/active.caddy' \
    sh "$1" "${2:-0}" || return 1
  operation entry-config "$1" entry docker cp "$DIR/deploy/nas/entry/Caddyfile" "$ENTRY:$CADDYFILE" || return 1
  operation entry-validate "$1" entry docker exec "$ENTRY" caddy validate --config "$CADDYFILE" --adapter caddyfile || return 1
  operation entry-reload "$1" entry docker exec "$ENTRY" caddy reload --config "$CADDYFILE" --adapter caddyfile
}

# $1 の /healthz が通り、版が $2（any なら問わない）、役割が $3（any なら問わない）かを1回確かめる。
health_ok() {
  body=$(bounded "$READ_LIMIT" docker exec "$1" node -e "$HEALTH_JS" 2>/dev/null) || return 1
  matches "$body" "$2" "$3"
}
# 入口を通した /healthz（入口のコンテナの中から 127.0.0.1:8787 へ。Host は 127.0.0.1:8787）。
entry_ok() {
  body=$(bounded "${3:-$READ_LIMIT}" docker exec "$ENTRY" wget -q -O - http://127.0.0.1:8787/healthz 2>/dev/null) || return 1
  matches "$body" "$1" "$2"
}
matches() {
  printf '%s' "$1" | bounded "$READ_LIMIT" docker run --rm -i --pull never --network none --read-only \
    --cap-drop ALL --security-opt no-new-privileges --user 1000:1000 --entrypoint node \
    "${ISSUE_DESK_INTAKE_IMAGE:-issue-desk-intake:latest}" -e '
      let input="";
      process.stdin.setEncoding("utf8");
      process.stdin.on("data",chunk=>{input+=chunk;if(input.length>65536)process.exit(1)});
      process.stdin.on("end",()=>{try{
        const value=JSON.parse(input),[revision,role]=process.argv.slice(1);
        process.exit(value&&value.ok===true&&(revision==="any"||value.revision===revision)&&(role==="any"||value.role===role)?0:1);
      }catch{process.exit(1)}});
    ' "$2" "$3" >/dev/null 2>&1
}

# $1 の確かめ（health_ok・entry_ok と引数）が通るまで、今から TIMEOUT 秒まで待つ。
wait_for() {
  deadline=$(($(now_ms) + TIMEOUT * 1000))
  if [ -n "${ENTRY_DEADLINE:-}" ] && [ "$ENTRY_DEADLINE" -lt "$deadline" ]; then deadline=$ENTRY_DEADLINE; fi
  while :; do
    if [ -n "${ENTRY_DEADLINE:-}" ]; then [ "$(now_ms)" -lt "$ENTRY_DEADLINE" ] || return 1; fi
    "$@" && return 0
    pause_until "$deadline" || return 1
  done
}

# 受付の heartbeat を読み、drain の要求を操作する部品。
. "$DIR/deploy/nas/server-drain.sh"

# 新しい版を確かめられなかったので、B を止めて A を起動し直し、入口を A へ戻して、A が書き手として答えるのを確かめる。
switch_back() {
  why=$1
  UPDATE_STAGE=$why
  recovery_begin=$(now_ms)
  READ_LIMIT=1 INTERVAL=1
  ENTRY_DEADLINE=$((recovery_begin + RECOVERY_MS))
  say "[server:recovery] color=$ACTIVE deadline=$ENTRY_DEADLINE now=$(now_ms)"
  failed=''
  ENTRY_DEADLINE=$((recovery_begin + 5000))
  recovery_stop new "$NEW" "$IDLE" || failed="$failed stop-new"
  if [ -z "$failed" ] && [ -n "$OLD" ]; then
    ENTRY_DEADLINE=$((recovery_begin + 10000))
    recovery_stop old "$OLD" "$ACTIVE" || failed="$failed stop-old"
    if [ -z "$failed" ]; then
      READ_LIMIT=2
      ENTRY_DEADLINE=$((recovery_begin + 12000))
      restart_ms=$(now_ms start)
      operation start-old "$ACTIVE" stopped docker start "$OLD" || failed="$failed start-old"
      [ -n "$failed" ] || operation_result start-old "$ACTIVE" started 0
    fi
  elif [ -z "$OLD" ]; then
    operation_result start-old "$ACTIVE" missing 1
    failed="$failed start-old"
  fi
  ENTRY_DEADLINE=$((recovery_begin + 13000))
  clear_drain
  ENTRY_DEADLINE=$((recovery_begin + 15000))
  switch_entry "$ACTIVE" "$LEGACY_ENTRY" || failed="$failed entry"
  # 2026-10-24 13:38 は直接の active の確認に1秒（17〜18秒）しか残らず、heartbeat は通ったのに check:active で
  # 期限切れになった。確認の3段にはそれぞれ1回3秒の確認を2回以上できる枠を、終わりから数えて取る。
  READ_LIMIT=$RECOVERY_CHECK_LIMIT
  if [ -z "$failed" ]; then
    ENTRY_DEADLINE=$((recovery_begin + RECOVERY_MS - 10000))
    wait_for heartbeat_ok any "$restart_ms" || { operation_result check:heartbeat "$ACTIVE" unknown 1; failed="$failed check:heartbeat"; }
  fi
  if [ -z "$failed" ]; then
    ENTRY_DEADLINE=$((recovery_begin + RECOVERY_MS - 5000))
    wait_for health_ok "$OLD" any active || { operation_result check:active "$ACTIVE" unknown 1; failed="$failed check:active"; }
  fi
  if [ -z "$failed" ]; then
    ENTRY_DEADLINE=$((recovery_begin + RECOVERY_MS))
    wait_for entry_ok any active || { operation_result check:entry "$ACTIVE" unknown 1; failed="$failed check:entry"; }
  fi
  if [ -z "$failed" ]; then
    say "[server:switched-back:$why] 新しい版（$EXPECT、$IDLE）を確かめられないため、前の色（$ACTIVE）へ戻し、入口を通して書き手として答えることを確かめました"
    exit 1
  fi
  say "[server:switch-back-failed:$why] 前の色（$ACTIVE）へ戻せていません。失敗した操作:$failed。手で復旧してください: docs/server-blue-green.md『Manual rollback』"
  exit 2
}

if [ "$DRY_RUN" = 1 ]; then
  say '[server:dry-run] 表示だけで、コンテナ・入口・受付の volume は変更しません'
  say "[server:dry-run] 予定: docker compose --profile intake up -d --no-deps --no-build $ENTRY_SERVICE（入口が停止中の場合だけ起動）"
  say "[server:dry-run] 予定: 入口の volume $ENTRY_VOLUME の active.caddy から動いている色（A）と止まっている色（B）を決める"
  say "[server:dry-run] 予定: docker compose --profile intake up -d --no-deps --no-build --force-recreate $PREFIX-<B>（待機で起動）"
  say "[server:dry-run] 予定: B の /healthz が通り版が $EXPECT になるのを最大${TIMEOUT}秒待つ（だめなら B を止めて終わる）"
  say "[server:dry-run] 予定: A の応答の切れ目を最大 min(${TIMEOUT},300)秒待ち、要求を置いて止めた印と idle を最大 min(${TIMEOUT},120)秒待つ（だめなら B を止めて見送り）"
  say "[server:dry-run] 予定: 入口の再試行を準備、A に SIGUSR2（旧版は停止）、B の heartbeat（版 $EXPECT）と /healthz の role active を最大${TIMEOUT}秒待つ"
  say '[server:dry-run] 予定: 入口の active.caddy を B にして caddy reload、入口の /healthz を確かめてから docker stop A'
  say '[server:dry-run] 予定（確かめられないとき）: docker stop B、docker start A、入口を A へ戻して A の heartbeat と入口の /healthz を確かめる'
  exit 0
fi

# 入口を読めない早期終了も記録する。記録の失敗は入れ替え・巻き戻しの終了コードに影響させない。
update_record() {
  bounded "$READ_LIMIT" docker run --rm -v "$STATE_VOLUME":/state alpine \
    sh -c "$(cat "$DIR/deploy/nas/server-update-record.sh")" sh "$EXPECT" "$1" "$UPDATE_STAGE" >/dev/null 2>&1
}
UPDATE_STAGE=entry
PROBE_DIR='' PROBE_PID=''
update_record begin || :
trap 'update_status=$?; ENTRY_DEADLINE=; trap - 0; if [ "$update_status" = 0 ]; then update_record success || :; else update_record failure || :; fi; finish_probe || :; exit "$update_status"' 0
trap 'exit 143' TERM
trap 'exit 130' INT

# 動いている入口は再作成しない。イメージ・定義の変更は別の保守作業で反映する。設定だけ reload する。
entry_up=0
ENTRY=$(cid_of "$ENTRY_SERVICE")
entry_running=''
[ -z "$ENTRY" ] || entry_running=$(docker inspect --format '{{.State.Running}}' "$ENTRY" 2>/dev/null)
if [ "$entry_running" != true ]; then
  compose up -d --no-deps --no-build "$ENTRY_SERVICE" >/dev/null 2>&1 || entry_up=1
  ENTRY=$(cid_of "$ENTRY_SERVICE")
fi
if [ "$entry_up" = 1 ] || [ -z "$ENTRY" ] || ! ACTIVE=$(read_active); then
  say "[server:aborted:entry] 受付の入口（$ENTRY_SERVICE）か転送先（$ENTRY_VOLUME の active.caddy）を確かめられないため、入れ替えません。復旧は docs/server-blue-green.md『Manual rollback』"
  exit 4
fi
if [ "$ACTIVE" = blue ]; then IDLE=green; else IDLE=blue; fi
OLD=$(cid_of "$PREFIX-$ACTIVE")
OLD_RUNNING=0
if [ -n "$OLD" ]; then
  state=$(docker inspect --format '{{.State.Running}}' "$OLD" 2>/dev/null) || state=''
  [ "$state" != true ] || OLD_RUNNING=1
fi

say "[server:started] 受付を入れ替えます: $ACTIVE（動いている）-> $IDLE（新しい版 $EXPECT）"
start_probe || :
UPDATE_STAGE=start
up_ms=$(now_ms start)
if ! compose up -d --no-deps --no-build --force-recreate "$PREFIX-$IDLE" >/dev/null 2>&1; then
  NEW=$(cid_of "$PREFIX-$IDLE")
  [ -z "$NEW" ] || docker stop "$NEW" >/dev/null 2>&1 || :
  say "[server:failed:start] 新しい版の受付（$IDLE）を起動できません。動いている $ACTIVE はそのままです"
  exit 1
fi
NEW=$(cid_of "$PREFIX-$IDLE")
UPDATE_STAGE=healthz
if [ -z "$NEW" ] || ! wait_for health_ok "$NEW" "$EXPECT" any; then
  [ -z "$NEW" ] || docker stop "$NEW" >/dev/null 2>&1 || :
  say "[server:failed:healthz] 新しい版の受付（$IDLE）の /healthz が${TIMEOUT}秒以内に通りません（DB・スキーマの版・画面のファイル・版）。止めました。動いている $ACTIVE はそのままです"
  exit 1
fi

UPDATE_STAGE=drain
wait_drain

# 旧色の設定にも再試行と両色の健康確認を入れてから、書き手を譲る。
UPDATE_STAGE=entry-prepare
LEGACY_ENTRY=0
if [ "$OLD_RUNNING" = 1 ] &&
  ! bounded "$READ_LIMIT" docker exec "$OLD" test -x /usr/local/bin/intake >/dev/null 2>&1 &&
  ! bounded "$READ_LIMIT" docker exec "$OLD" test -f /app/server/src/intake-handoff.mjs >/dev/null 2>&1; then
  LEGACY_ENTRY=1
fi
if ! switch_entry "$ACTIVE"; then
  # 初回の失敗で旧版のスクリプトへ戻しても読める形に戻す。
  switch_entry "$ACTIVE" 1 || :
  clear_drain
  docker stop "$NEW" >/dev/null 2>&1 || :
  say '[server:aborted:entry] 引き継ぎ前の入口の再読込に失敗しました。旧色は停止していません'
  exit 4
fi
# 初回の健康確認（間隔1秒・上限2秒）を待つ。
sleep 3
stop_ms=$(now_ms start)
# 新色の交代・入口確認は HANDOFF_MS（引き継ぎが終わるまで待つ）。切り戻しは開始時から独立した RECOVERY_MS。
entry_deadline=$(($(now_ms) + HANDOFF_MS))
ENTRY_DEADLINE=$entry_deadline
UPDATE_STAGE=handoff
if [ "$OLD_RUNNING" = 1 ]; then
  if [ "$LEGACY_ENTRY" = 0 ]; then
    bounded "$READ_LIMIT" docker kill --signal=SIGUSR2 "$OLD" >/dev/null 2>&1 || switch_back handoff
  else
    say '[server:legacy-handoff] 旧版を停止して書き手を交代します（初回の移行）'
    bounded "$READ_LIMIT" docker stop "$OLD" >/dev/null 2>&1 || switch_back stop-old
  fi
fi
clear_drain
# 書き手の heartbeat は A がロックを譲った後のもの（A が動いていなければ、B はロックをすぐ取るので B を起動した後のもの）。
since_ms=$stop_ms
UPDATE_STAGE=takeover
[ "$OLD_RUNNING" = 1 ] || since_ms=$up_ms
# 入口断の間なので、共通の確認間隔（既定5秒）を待たず1秒おきに確かめる。
INTERVAL=1
if ! wait_for heartbeat_ok "$EXPECT" "$since_ms" || ! wait_for health_ok "$NEW" "$EXPECT" active; then
  switch_back takeover
fi
UPDATE_STAGE=entry
# 引き継ぎから使った期限を更新せず、その残りで入口を確認する。
if ! switch_entry "$IDLE"; then
  switch_back entry
fi
ENTRY_DEADLINE=$entry_deadline
UPDATE_STAGE=entry-check
# 新色の確認で切り戻しの時間を使い切らない。各読取も残り時間内に収める。
while :; do
  entry_left=$(((entry_deadline - $(now_ms)) / 1000))
  [ "$entry_left" -gt 0 ] || switch_back entry-check
  [ "$entry_left" -le 5 ] || entry_left=5
  if entry_ok "$EXPECT" active "$entry_left"; then break; fi
  [ "$(now_ms)" -lt "$entry_deadline" ] || switch_back entry-check
  sleep 1
done
ENTRY_DEADLINE=''
UPDATE_STAGE=stop-old
if [ "$OLD_RUNNING" = 1 ] && ! docker stop --time "$TIMEOUT" "$OLD" >/dev/null 2>&1; then
  switch_back stop-old
fi
say "[server:success] 受付を $IDLE（$EXPECT）へ入れ替えました（書き手の交代と入口を通した /healthz を確認）。$ACTIVE は止めたまま残します"
record_intake_success
exit 0
