#!/bin/sh
# NAS_UPDATE_RECORD: 状態 volume 内で実行する。本文・出力は保存せず、固定形式だけを読む。
set -eu
umask 022
revision=$1 action=$2 stage=$3
d=${4:-/state/runtime/state}
file=$d/intake-nas-update.json
case "$revision" in '' | *[!0-9a-f]*) exit 64 ;; esac
[ "${#revision}" -eq 40 ] || exit 64
case "$action" in begin | failure | success) ;; *) exit 64 ;; esac
case "$stage" in entry | start | healthz | drain | entry-prepare | handoff | takeover | entry-check | stop-old) ;; *) exit 64 ;; esac
previous=$(head -c 257 "$file" 2>/dev/null) || previous=''
nl='
'
case "$previous" in *"$nl"*) previous='' ;; esac
count=0
pattern='^[{]"revision":"[0-9a-f]{40}","failures":(0|[1-9][0-9]{0,8}),"stage":"(none|entry|start|healthz|drain|entry-prepare|handoff|takeover|entry-check|stop-old)"[}]$'
if [ "${#previous}" -le 256 ] && printf '%s\n' "$previous" | grep -Eq "$pattern"; then
  prior_revision=${previous#*\"revision\":\"}
  prior_revision=${prior_revision%%\"*}
  prior_count=${previous#*\"failures\":}
  prior_count=${prior_count%%,*}
  case "$previous" in
  *'"failures":0,"stage":"none"}' | *'"stage":"none"}') ;;
  *) [ "$prior_count" = 0 ] || { [ "$prior_revision" != "$revision" ] || count=$prior_count; } ;;
  esac
fi
if [ "$action" = begin ] && [ "$count" -gt 0 ]; then exit 0; fi
if [ "$action" = failure ]; then
  [ "$count" -ge 999999999 ] || count=$((count + 1))
else
  count=0 stage=none
fi
[ -d "$d" ] || exit 3
trap 'rm -f "$file.tmp"' 0
printf '{"revision":"%s","failures":%s,"stage":"%s"}\n' "$revision" "$count" "$stage" >"$file.tmp"
mv -f "$file.tmp" "$file"
