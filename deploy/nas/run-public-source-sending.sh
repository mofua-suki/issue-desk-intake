#!/bin/sh
set -eu
umask 077
# No sending configuration means preparation only. Partial configuration is an error.
if [ -z "${SEND_PREPARED_DIR:-}${SEND_PREPARATION_ROOT:-}${SEND_CONFIRMATIONS_DIR:-}${SEND_ISSUER_REGISTRY:-}" ]; then
  exit 0
fi
fail() { echo '公開送信の設定が不正です' >&2; exit 1; }
for value in "${JOBS_REGISTRY:-}" "${SEND_CONFIRMATIONS_DIR:-}" "${SEND_ISSUER_REGISTRY:-}"; do
  case "$value" in /*) ;; *) fail ;; esac
done
if [ -n "${SEND_PREPARATION_ROOT:-}" ]; then
  [ -z "${SEND_PREPARED_DIR:-}" ] || fail
  root=$SEND_PREPARATION_ROOT
  case $root in /*) ;; *) fail ;; esac
  [ ! -L "$root" ] && [ -d "$root" ] || fail
  [ "$(realpath -e -- "$root")" = "$root" ] || fail
  [ "$(stat -c %u -- "$root")" = "$(id -u)" ] && [ "$(stat -c %a -- "$root")" = 700 ] || fail
  exec 9< "$root" || fail
  flock -n 9 || fail
  record=$root/.latest-prepared
  [ ! -L "$record" ] && [ -f "$record" ] || fail
  [ "$(stat -c %u:%a:%h -- "$record")" = "$(id -u):600:1" ] || fail
  now=$(date +%s) || fail
  modified=$(stat -c %Y -- "$record") || fail
  [ "$modified" -le "$now" ] && [ $((now - modified)) -le 86400 ] || fail
  name=$(cat -- "$record") || fail
  [ "$(printf '%s\n' "$name" | wc -l)" -eq 1 ] || fail
  printf '%s\n' "$name" | LC_ALL=C grep -Eq '^run-[0-9]+-[0-9]+$' || fail
  [ "$(wc -c < "$record")" -eq "$((${#name} + 1))" ] || fail
  SEND_PREPARED_DIR=$root/$name
  [ ! -L "$SEND_PREPARED_DIR" ] && [ -d "$SEND_PREPARED_DIR" ] || fail
  [ "$(realpath -e -- "$SEND_PREPARED_DIR")" = "$SEND_PREPARED_DIR" ] || fail
  [ "$(stat -c %u:%a -- "$SEND_PREPARED_DIR")" = "$(id -u):700" ] || fail
else
  case ${SEND_PREPARED_DIR:-} in /*) ;; *) fail ;; esac
fi
if [ "${SEND_WAIT_SECONDS+x}" = x ]; then
  case $SEND_WAIT_SECONDS in ''|*[!0-9]*|0|0*) fail ;; esac
  [ ${#SEND_WAIT_SECONDS} -le 4 ] && [ "$SEND_WAIT_SECONDS" -le 3600 ] || fail
  set -- --wait-seconds "$SEND_WAIT_SECONDS"
else
  set --
fi
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PATH="$HOME/.local/bin:$PATH"
export PATH
# Pass only paths and a minimal environment; issuance stays inside the approved entry.
exec env -i PATH="$PATH" HOME=/nonexistent node "$SCRIPT_DIR/../../scripts/run-approved-public-source-jobs.cjs" \
  --registry "$JOBS_REGISTRY" --prepared-dir "$SEND_PREPARED_DIR" \
  --confirmations-dir "$SEND_CONFIRMATIONS_DIR" --issuer-registry "$SEND_ISSUER_REGISTRY" "$@"
