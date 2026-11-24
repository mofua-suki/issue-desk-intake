#!/bin/sh
set -eu
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
compose() { docker compose --project-directory "$script_dir" -f "$script_dir/compose.yaml" --profile intake "$@"; }
if [ "${1:-}" = probe ]; then
  [ "$#" -eq 1 ] || exit 64
  active=''
  uncertain=0
  for color in blue green; do
    container=$(compose ps -q "issue-desk-intake-$color") || { echo '書き手の確認に失敗しました。' >&2; exit 1; }
    [ -n "$container" ] || continue
    case "$container" in *[!A-Za-z0-9_.-]*|'') echo 'コンテナの確認応答が不正です。' >&2; exit 1;; esac
    code=0
    timeout 5 docker exec "$container" node -e 'fetch("http://127.0.0.1:8787/healthz",{signal:AbortSignal.timeout(2000)}).then(async r=>{let b;try{b=await r.json()}catch{process.exit(12)};if(!r.ok||!b||b.ok!==true||!['\''active'\'','\''standby'\''].includes(b.role)||b.ready!==(b.role==="active"))process.exit(12);process.exit(b.role==="active"?0:10)},()=>process.exit(11)).catch(()=>process.exit(11))' >/dev/null 2>&1 || code=$?
    case "$code" in
      0) [ -z "$active" ] || { echo '書き手が複数あります。' >&2; exit 1; }; active=$container;;
      10) ;;
      11|124|137) uncertain=1;;
      *) echo '書き手の確認に失敗したか応答が不正です。' >&2; exit 1;;
    esac
  done
  if [ -z "$active" ]; then
    [ "$uncertain" = 0 ] || exit 4
    exit 3
  fi
  printf '%s\n' "$active"
  exit 0
fi
active=''
for color in blue green; do
  container=$(compose ps -q "issue-desk-intake-$color") || exit 1
  [ -n "$container" ] || continue
  if timeout 5 docker exec "$container" node -e 'fetch("http://127.0.0.1:8787/healthz",{signal:AbortSignal.timeout(2000)}).then(async r=>{const b=await r.json();process.exit(r.ok&&b.role==="active"?0:1)},()=>process.exit(1)).catch(()=>process.exit(1))' >/dev/null 2>&1; then
    if [ -n "$active" ]; then
      echo '書き手のServerを一意に確認できません。' >&2
      exit 1
    fi
    active=$container
  fi
done
if [ -z "$active" ]; then
  echo '稼働中の書き手のServerを確認できません。' >&2
  exit 1
fi
command=${1:-status}
[ "$#" -eq 0 ] || shift
case "$command" in
  active)
    [ "$#" -eq 0 ] || exit 64
    printf '%s\n' "$active"
    ;;
  exec)
    if [ "${1:-}" = -T ]; then shift; fi
    [ "$#" -gt 0 ] || { echo '使い方: intake.sh exec [-T] <コマンド> [引数…]' >&2; exit 64; }
    exec docker exec -i "$active" "$@"
    ;;
  status)
    [ "$#" -eq 0 ] || exit 64
    exec docker exec "$active" node -e 'fetch("http://127.0.0.1:8787/healthz",{signal:AbortSignal.timeout(2000)}).then(async r=>{process.stdout.write(await r.text());process.exit(r.ok?0:1)},()=>process.exit(1))'
    ;;
  *) echo '使い方: intake.sh status|active|exec [-T] <コマンド> [引数…]' >&2; exit 64 ;;
esac
