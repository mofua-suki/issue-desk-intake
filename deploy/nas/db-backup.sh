#!/bin/sh
# 共有DB（compose.yaml の issue-desk-db。docs/shared-db.md）のバックアップ。NAS 本体の systemd タイマー
# （deploy/nas/issue-desk-db-backup.timer、毎日 03:40 JST）が実行する。
#
# DB のコンテナの中で pg_dump -Fc（独自形式。pg_restore で戻す）を実行し、出力を配置先の runtime/db-backups/
# （Git 管理外。環境変数 ISSUE_DESK_DB_BACKUP_DIR で変えられる）へ issue-desk-db-<日時>.dump として書く。書き終えるまでは
# .partial の名前で置き、成功したときだけ本来の名前へ移す。新しいものから KEEP（14）世代を残し、それより古いものを消す。
# DB のコンテナが動いていない（受付を動かさない NAS など）ときは何もせずに 0 で終わる。
# journal の行は [db-backup:<結果>] で見分ける。
#
#   sh deploy/nas/db-backup.sh
set -eu

case "$0" in
*/*) SCRIPT_DIR=${0%/*} ;;
*) SCRIPT_DIR=. ;;
esac
DIR=$(CDPATH= cd -- "${SCRIPT_DIR:-/}/../.." && pwd)
CONTAINER=${ISSUE_DESK_DB_CONTAINER:-issue-desk-db}
BACKUP_DIR=${ISSUE_DESK_DB_BACKUP_DIR:-$DIR/runtime/db-backups}
KEEP=14
DB_USER=issue_desk
DB_NAME=issue_desk

say() { echo "$(date -Is) $*"; }

running=$(docker inspect --format '{{.State.Running}}' "$CONTAINER" 2>/dev/null) || running=''
if [ "$running" != true ]; then
  say "[db-backup:skipped:not-running] 共有DBのコンテナ（$CONTAINER）が動いていないため、バックアップしません"
  exit 0
fi

mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR"
STAMP=$(date +%Y%m%d-%H%M%S)
FILE=$BACKUP_DIR/issue-desk-db-$STAMP.dump
PARTIAL=$FILE.partial
umask 077
if ! docker exec "$CONTAINER" pg_dump -U "$DB_USER" -d "$DB_NAME" -Fc >"$PARTIAL"; then
  rm -f "$PARTIAL"
  say "[db-backup:failed] pg_dump に失敗しました（$CONTAINER）"
  exit 1
fi
if [ ! -s "$PARTIAL" ]; then
  rm -f "$PARTIAL"
  say "[db-backup:failed] pg_dump の出力が空です（$CONTAINER）"
  exit 1
fi
mv "$PARTIAL" "$FILE"

# 古い世代を消す（名前の日時の順＝作った順。新しいものから KEEP 個を残す）。
count=0
for old in $(ls -1r "$BACKUP_DIR" | grep -E '^issue-desk-db-[0-9]{8}-[0-9]{6}\.dump$' || :); do
  count=$((count + 1))
  [ "$count" -le "$KEEP" ] || rm -f "$BACKUP_DIR/$old"
done
kept=$count
[ "$kept" -le "$KEEP" ] || kept=$KEEP
say "[db-backup:success] $FILE に保存しました（残す世代: $kept/$KEEP）"
