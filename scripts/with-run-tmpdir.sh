#!/bin/sh
# 作業ツリーごとの一時ディレクトリを TMPDIR にしてコマンドを動かし、終わったら消す。
# テストが os.tmpdir()・t.TempDir()・mktemp で作って消し忘れた物も、まとめてここで消える。
# 同じ作業ツリーの前回の実行が強制終了で残した物（持ち主のプロセスが居ないもの）は始める前に消す。
# ほかの作業ツリー・ほかの作業の一時ファイルには触れない。
set -u
if [ "$#" -eq 0 ]; then
  printf '%s\n' '使い方: sh scripts/with-run-tmpdir.sh <コマンド> [引数...]' >&2
  exit 2
fi
# 入れ子の呼び出しは親の一時ディレクトリをそのまま使う。TMPDIR を別の場所へ変えた呼び出しは、その下に作り直す。
if [ -n "${ISSUE_DESK_RUN_TMPDIR:-}" ] && [ "${TMPDIR:-}" = "$ISSUE_DESK_RUN_TMPDIR" ] && [ -d "$ISSUE_DESK_RUN_TMPDIR" ]; then
  exec "$@"
fi
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
base=${TMPDIR:-/tmp}
base=${base%/}
[ -n "$base" ] || base=/
key=$(printf '%s' "$ROOT" | cksum | cut -d ' ' -f 1)
# Unixソケットのパス長（108 bytes）に余裕を残すため、名前は短く切る。
name=$(printf '%s' "$(basename -- "$ROOT")" | LC_ALL=C tr -c 'A-Za-z0-9._-' '_' | cut -c 1-12)
prefix="issue-desk-run-$name-$key"
# テストが書込権を外したまま残した物も消せるようにし、消せなくても本体は止めない。
remove() {
  # PostgreSQL の停止に失敗したときは、PIDファイルが残る間は外側でも保持する。
  for postgres in "$1"/issue-desk-test-pg-*; do
    if [ -f "$postgres/.managed-postgres" ] && [ -f "$postgres/data/postmaster.pid" ]; then
      printf '停止を確認できない検証用PostgreSQLの一時ディレクトリを保持します: %s\n' "$1" >&2
      return 0
    fi
  done
  chmod -R u+rwx -- "$1" 2>/dev/null || :
  rm -rf -- "$1" 2>/dev/null || printf '一時ディレクトリを消せませんでした: %s\n' "$1" >&2
}
# .owner に書いた持ち主（このスクリプトとコマンド）が1つでも生きていれば使用中とみなす。
# .owner がまだ無い作りたての物は、ほかの実行が始めたところかもしれないので10分は残す。
in_use() {
  if [ -r "$1/.owner" ]; then
    while IFS= read -r owner; do
      case "$owner" in ''|*[!0-9]*) continue ;; esac
      kill -0 "$owner" 2>/dev/null && return 0
    done < "$1/.owner"
    return 1
  fi
  [ -n "$(find "$1" -maxdepth 0 -mmin -10 2>/dev/null)" ]
}
for stale in "$base/$prefix".*; do
  [ -d "$stale" ] && [ ! -L "$stale" ] || continue
  in_use "$stale" && continue
  remove "$stale"
done
dir=$(mktemp -d "$base/$prefix.XXXXXX") || {
  printf '一時ディレクトリを作れません: %s（df -h %s で空きを確かめてください）\n' "$base" "$base" >&2
  exit 1
}
child=
cleanup() { remove "$dir"; }
trap cleanup EXIT
forward() { [ -z "$child" ] || kill -TERM "$child" 2>/dev/null || :; }
trap forward INT TERM HUP
printf '%s\n' "$$" > "$dir/.owner"
TMPDIR=$dir ISSUE_DESK_RUN_TMPDIR=$dir
export TMPDIR ISSUE_DESK_RUN_TMPDIR
# 裏で動かすと標準入力が /dev/null になるので、元の標準入力を渡し直す（閉じていればそのまま）。
if (exec 3<&0) 2>/dev/null; then
  exec 3<&0
  "$@" 0<&3 3<&- &
  child=$!
  exec 3<&-
else
  "$@" &
  child=$!
fi
# このスクリプトだけが強制終了されても、動き続けるコマンドの場所をほかの実行が消さないようにする。
printf '%s\n' "$child" >> "$dir/.owner"
status=0
while :; do
  if wait "$child"; then status=0; else status=$?; fi
  kill -0 "$child" 2>/dev/null || break
done
exit "$status"
