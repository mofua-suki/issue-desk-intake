#!/bin/sh
set -eu
# チャットのシェルに削除済みの GODEBUG（tlskyber=0 など）が残っていると、go とそれを呼ぶテストが起動時に落ちる。
unset GODEBUG
# チャットのシェルの既定の umask は 077 で、資格情報の権限を確かめるテストが配置と違う権限で落ちる。配置と同じ 022 に揃える。
umask 022
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
# /tmp はほかの作業と共有するので、この作業ツリー専用の一時ディレクトリで流し、終わったら消す。
if [ -z "${ISSUE_DESK_RUN_TMPDIR:-}" ] || [ "${TMPDIR:-}" != "$ISSUE_DESK_RUN_TMPDIR" ] || [ ! -d "$ISSUE_DESK_RUN_TMPDIR" ]; then
  exec sh "$ROOT/scripts/with-run-tmpdir.sh" sh "$ROOT/scripts/verify.sh" "$@"
fi
# NASの利用者が入れた~/.local/binのnode(24)・goを優先する。PATHのnodeは古い(18)ことがあり、非対話のシェルはPATHにgoを含まない。
tool() {
  if [ -x "$HOME/.local/bin/$1" ]; then printf '%s' "$HOME/.local/bin/$1"; else printf '%s' "$1"; fi
}
NODE=${NODE:-$(tool node)}
PNPM=${PNPM:-$(tool pnpm)}
GO=${GO:-$(tool go)}
GOFMT=${GOFMT:-$(tool gofmt)}
# pnpm などの `#!/usr/bin/env node` は PATH の node で動くため、選んだ node を PATH の先頭に置く。
case "$NODE" in */*) PATH=$(dirname -- "$NODE"):$PATH; export PATH ;; esac
for executable in "$NODE" "$PNPM" "$GO" "$GOFMT" setsid; do
  command -v "$executable" >/dev/null || { printf '必要なコマンドがありません: %s\n' "$executable" >&2; exit 1; }
done
"$NODE" -e 'if(Number(process.versions.node.split(".")[0])<24){console.error("Node.js 24以上が必要です");process.exit(1)}'
if [ -z "${ISSUE_DESK_TEST_POSTGRES_DSN:-}" ]; then
  exec "$NODE" "$ROOT/scripts/with-test-postgres.ts" -- sh "$ROOT/scripts/verify.sh" "$@"
fi
stage() {
  name=$1; shift
  start=$(date +%s)
  if [ -n "${VERIFY_STAGE_FILE:-}" ]; then printf '%s\n' "$name" > "$VERIFY_STAGE_FILE"; fi
  printf 'stage=%s start=%s\n' "$name" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  # 子の検証へ親の段階・並行ログの書込先を引き継がない。
  if (unset VERIFY_STAGE_FILE VERIFY_LOG_DIR; "$@"); then code=0; else code=$?; fi
  end=$(date +%s)
  printf '\nstage=%s end=%s duration=%ss exit=%s\n' "$name" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$((end - start))" "$code"
  if [ -n "${VERIFY_STAGE_FILE:-}" ]; then : > "$VERIFY_STAGE_FILE"; fi
  return "$code"
}
format_check() {
  unformatted=$("$GOFMT" -l .) || return $?
  if [ -n "$unformatted" ]; then
    printf 'gofmtが必要です:\n%s\n' "$unformatted" >&2
    return 1
  fi
}
consultations_race() {
  if [ -n "${CC:-}" ] || command -v gcc >/dev/null 2>&1; then
    CGO_ENABLED=1 GO="$GO" "$NODE" "$ROOT/scripts/verify-review-notes.ts" --consultations-race
  elif command -v clang >/dev/null 2>&1; then
    CGO_ENABLED=1 CC=clang GO="$GO" "$NODE" "$ROOT/scripts/verify-review-notes.ts" --consultations-race
  else
    GO="$GO" "$NODE" "$ROOT/scripts/verify-consultations-race.ts"
  fi
}
go_tests() {
  packages=$("$GO" list ./...) || return $?
  set -f
  set -- $packages
  selected=
  for package do
    [ "$package" = issue-desk-intake/server/internal/consultations ] && continue
    selected="${selected}${selected:+ }${package}"
  done
  if [ -z "$selected" ]; then
    printf '%s\n' 'go-test: 対象パッケージがありません' >&2
    return 1
  fi
  set -- $selected
  GO="$GO" "$NODE" "$ROOT/scripts/verify-review-notes.ts" --normal-go "$@"
}
parallel_go_checks() {
  (VERIFY_STAGE_FILE="$VERIFY_LOG_DIR/go-stage"; stage go-test go_tests) >"$VERIFY_LOG_DIR/go" 2>&1 &
  go_pid=$!
  (VERIFY_STAGE_FILE="$VERIFY_LOG_DIR/consultations-stage"; stage consultations-race consultations_race) >"$VERIFY_LOG_DIR/consultations" 2>&1 &
  consultations_pid=$!
  parallel_status=0
  wait "$go_pid" || parallel_status=1
  wait "$consultations_pid" || parallel_status=1
  cat "$VERIFY_LOG_DIR/go" "$VERIFY_LOG_DIR/consultations"
  rm -f "$VERIFY_LOG_DIR/go" "$VERIFY_LOG_DIR/consultations"
  return "$parallel_status"
}
server_checks() {
  stage api-manifest "$NODE" "$ROOT/scripts/api-manifest.ts" &&
  cd "$ROOT/server" &&
  stage gofmt format_check &&
  stage go-quality env GO="$GO" sh "$ROOT/scripts/check-go-quality.sh" &&
  stage test-postgres "$NODE" "$ROOT/scripts/verify-review-notes.ts" --check-dsn &&
  stage nas-node env GO="$GO" NODE="$NODE" "$NODE" "$ROOT/scripts/verify-nas-node.ts" &&
  stage nas-caddy "$GO" test ./internal/startup -run '^TestNASCaddyHandoff$' -count=1 -v &&
  parallel_go_checks &&
  stage openapi-race env GO="$GO" "$NODE" "$ROOT/scripts/verify-openapi-race.ts" &&
  stage go-vet "$GO" vet ./...
}
read_node_test_targets() {
  CLIENT_TEST_TARGETS=
  ROOT_TEST_TARGETS=
  while IFS=' ' read -r suite patterns || [ -n "$suite" ]; do
    case "$patterns" in
      ''|*[!a-zA-Z0-9_./\*\ -]*) printf '不正なNodeテスト対象: %s\\n' "$suite" >&2; return 1 ;;
    esac
    case " $patterns" in
      *" -"*) printf 'Nodeのオプションをテスト対象には指定できません。\n' >&2; return 1 ;;
    esac
    case "$suite" in
      client) [ -z "$CLIENT_TEST_TARGETS" ] || return 1; CLIENT_TEST_TARGETS=$patterns ;;
      root) [ -z "$ROOT_TEST_TARGETS" ] || return 1; ROOT_TEST_TARGETS=$patterns ;;
      *) printf '不明なNodeテストの区分: %s\n' "$suite" >&2; return 1 ;;
    esac
  done < "$ROOT/scripts/verify-node-test-targets.list"
  if [ -z "$CLIENT_TEST_TARGETS" ] || [ -z "$ROOT_TEST_TARGETS" ]; then
    printf '%s\n' 'Nodeテストの対象はclientとrootの両方が必要です。' >&2
    return 1
  fi
}
client_test() {
  client_timeout=${ISSUE_DESK_CLIENT_TEST_TIMEOUT_SECONDS:-1200}
  case "$client_timeout" in ''|*[!0-9]*|0) printf 'client-test: 不正な期限です\n' >&2; return 1 ;; esac
  if [ "$client_timeout" -lt 1 ] || [ "$client_timeout" -gt 1200 ]; then printf 'client-test: 期限は1〜1200秒です\n' >&2; return 1; fi
  if (cd "$ROOT/client" && timeout -k 5s "${client_timeout}s" "$NODE" --test --test-concurrency=4 $CLIENT_TEST_TARGETS); then return 0
  else
    code=$?
    if [ "$code" -eq 124 ] || [ "$code" -eq 137 ]; then printf 'client-test: テスト結果待機が%s秒で期限切れになりました\n' "$client_timeout" >&2; fi
    return "$code"
  fi
}
root_test() { "$NODE" --test --test-concurrency=4 $ROOT_TEST_TARGETS; }
client_checks() {
  cd "$ROOT"
  read_node_test_targets || return
  stage pnpm-install "$PNPM" --dir client install --frozen-lockfile --ignore-scripts --ignore-pnpmfile --config.minimum-release-age=20160 --config.confirmModulesPurge=false &&
  stage desk-contract "$NODE" scripts/generate-desk-contract.ts &&
  stage ts-format "$NODE" scripts/check-ts-quality.ts format &&
  stage ts-lint "$NODE" scripts/check-ts-quality.ts lint &&
  stage ts-unused "$NODE" scripts/check-ts-quality.ts exports &&
  stage client-typecheck "$NODE" client/scripts/typecheck.ts &&
  stage root-typecheck "$NODE" scripts/typecheck-root.ts &&
  stage client-test client_test &&
  stage client-build "$NODE" client/scripts/build.ts &&
  stage root-test root_test
}
case "${1:-}" in
  --server)
    if [ -z "${VERIFY_LOG_DIR:-}" ]; then
      VERIFY_LOG_DIR=$(mktemp -d "${TMPDIR:-/tmp}/issue-desk-verify-server.XXXXXXXX")
      trap 'rm -rf "$VERIFY_LOG_DIR"' EXIT
    fi
    server_checks; exit $? ;;
  --client) client_checks; exit $? ;;
esac
# 各系統の出力を分離し、終了後にまとめて表示する。
logs=$(mktemp -d "${TMPDIR:-/tmp}/issue-desk-verify.XXXXXXXX")
progress_pid=
stop_progress() {
  if [ -n "$progress_pid" ]; then kill -TERM "$progress_pid" 2>/dev/null || :; wait "$progress_pid" 2>/dev/null || :; progress_pid=; fi
}
progress_watch() {
  progress_sleep=
  trap '[ -z "$progress_sleep" ] || kill "$progress_sleep" 2>/dev/null || :; exit 0' TERM INT
  progress_seconds=${ISSUE_DESK_VERIFY_PROGRESS_SECONDS:-15}
  case "$progress_seconds" in ''|*[!0-9]*) progress_seconds=15 ;; esac
  if [ "$progress_seconds" -lt 1 ] || [ "$progress_seconds" -gt 60 ]; then progress_seconds=15; fi
  while :; do
    sleep "$progress_seconds" & progress_sleep=$!
    wait "$progress_sleep" || exit 0
    progress_sleep=
    server_stage=idle; client_stage=idle; go_stage=idle; race_stage=idle
    if [ -s "$logs/server-stage" ]; then IFS= read -r server_stage < "$logs/server-stage" || :; fi
    if [ -s "$logs/client-stage" ]; then IFS= read -r client_stage < "$logs/client-stage" || :; fi
    if [ -s "$logs/go-stage" ]; then IFS= read -r go_stage < "$logs/go-stage" || :; fi
    if [ -s "$logs/consultations-stage" ]; then IFS= read -r race_stage < "$logs/consultations-stage" || :; fi
    printf 'stage=progress at=%s server=%s go=%s consultations=%s client=%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$server_stage" "$go_stage" "$race_stage" "$client_stage"
  done
}
trap 'stop_progress; rm -rf "$logs"' EXIT
server_pid=
client_pid=
stop_checks() {
  trap '' TERM INT
  for pid in "$server_pid" "$client_pid"; do
    [ -n "$pid" ] || continue
    kill -TERM "-$pid" 2>/dev/null || :
    kill -TERM "$pid" 2>/dev/null || :
  done
  for pid in "$server_pid" "$client_pid"; do
    [ -n "$pid" ] && wait "$pid" 2>/dev/null || :
  done
  cat "$logs/server" "$logs/client" 2>/dev/null || :
  cat "$logs/go" "$logs/consultations" 2>/dev/null || :
  for state in "$logs/server-stage" "$logs/client-stage" "$logs/go-stage" "$logs/consultations-stage"; do
    if [ -s "$state" ]; then
      IFS= read -r interrupted_stage < "$state" || :
      printf 'stage=%s interrupted\n' "$interrupted_stage"
    fi
  done
  exit 1
}
trap stop_checks TERM INT
VERIFY_LOG_DIR="$logs" VERIFY_STAGE_FILE="$logs/server-stage" setsid sh "$ROOT/scripts/verify.sh" --server >"$logs/server" 2>&1 &
server_pid=$!
VERIFY_STAGE_FILE="$logs/client-stage" setsid sh "$ROOT/scripts/verify.sh" --client >"$logs/client" 2>&1 &
client_pid=$!
progress_watch &
progress_pid=$!
server_status=0
client_status=0
wait "$server_pid" || server_status=1
wait "$client_pid" || client_status=1
stop_progress
# 別PCの検証は出力の末尾（64KB）だけを持ち帰る。片方だけ失敗したときは失敗した系統のログを後ろに出し、
# テストの失敗に限らず、ビルド・vet・整形・型検査の失敗の原因も末尾に残るようにする。
if [ "$server_status" -ne 0 ] && [ "$client_status" -eq 0 ]; then
  cat "$logs/client" "$logs/server"
else
  cat "$logs/server" "$logs/client"
fi
# 両方が失敗して前の系統のログが切れても失敗した段とテストが読めるよう、最後に要約を出す。
if [ "$server_status" -ne 0 ] || [ "$client_status" -ne 0 ]; then
  printf '\n%s\n' '失敗の要約:'
  grep -h -B 4 -A 8 -E '^stage=.* exit=[1-9]|^--- FAIL|^FAIL|^panic:|^✖|^not ok' "$logs/server" "$logs/client" 2>/dev/null | tail -c 16384 || :
  exit 1
fi
exit 0
