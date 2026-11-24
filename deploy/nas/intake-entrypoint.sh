#!/bin/sh
set -eu
config=${ISSUE_DESK_CONFIG:-/state/config.local.json}
if [ ! -f "$config" ]; then
  echo 'Serverの設定ファイルがありません。' >&2
  exit 1
fi
mkdir -p /data/codex /data/claude
exec /usr/local/bin/intake --config "$config" --application-dir /app --assets /app/client/dist
