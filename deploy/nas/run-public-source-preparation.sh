#!/bin/sh
set -eu
umask 077
PATH="$HOME/.local/bin:$PATH"
export PATH
fail() { echo '公開候補の準備設定または実行状態が不正です' >&2; exit 1; }
case ${JOBS_REGISTRY:-} in /*) ;; *) fail ;; esac
case ${PREPARATION_ROOT:-} in /*) ;; *) fail ;; esac
case ${PREVIOUS_DIR:-} in ''|/*) ;; *) fail ;; esac
[ ! -L "$PREPARATION_ROOT" ] && [ -d "$PREPARATION_ROOT" ] || fail
[ "$(realpath -e -- "$PREPARATION_ROOT")" = "$PREPARATION_ROOT" ] || fail
owner=$(stat -c %u -- "$PREPARATION_ROOT") || fail
mode=$(stat -c %a -- "$PREPARATION_ROOT") || fail
[ "$owner" = "$(id -u)" ] || fail
# Require exactly owner-only permissions, including no special bits.
[ "$mode" = 700 ] || fail
exec 9< "$PREPARATION_ROOT" || fail
flock -n 9 || { echo '公開候補の準備は実行中です' >&2; exit 1; }
rm -f -- "$PREPARATION_ROOT/.latest-prepared" || fail
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ -n "${MIRROR_REFRESH_REGISTRY:-}" ]; then
  case $MIRROR_REFRESH_REGISTRY in /*) ;; *) fail ;; esac
  node "$SCRIPT_DIR/../../scripts/refresh-public-source-mirrors.cjs" "$MIRROR_REFRESH_REGISTRY" "$JOBS_REGISTRY" || fail
fi
output="$PREPARATION_ROOT/run-$(date +%s)-$$"
if [ -n "${PREVIOUS_DIR:-}" ]; then
  sh "$SCRIPT_DIR/../../scripts/run-public-source-jobs.sh" --prepare --registry "$JOBS_REGISTRY" --output "$output" --previous-dir "$PREVIOUS_DIR"
else
  sh "$SCRIPT_DIR/../../scripts/run-public-source-jobs.sh" --prepare --registry "$JOBS_REGISTRY" --output "$output"
fi
if [ -n "${PUBLIC_SOURCE_SCAN_CONFIG:-}" ]; then
  case $PUBLIC_SOURCE_SCAN_CONFIG in /*) ;; *) fail ;; esac
  node "$SCRIPT_DIR/../../scripts/scan-public-source-candidates.cjs" "$PUBLIC_SOURCE_SCAN_CONFIG" "$JOBS_REGISTRY" "$output" || fail
fi
record=$(mktemp "$PREPARATION_ROOT/.latest-prepared.XXXXXXXX") || fail
trap 'rm -f -- "$record"' EXIT HUP INT TERM
printf '%s\n' "${output##*/}" > "$record" || fail
mv -f -- "$record" "$PREPARATION_ROOT/.latest-prepared" || fail
