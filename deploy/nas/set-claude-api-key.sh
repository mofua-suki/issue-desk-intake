#!/usr/bin/env bash
set +x
set -u
export LC_ALL=C
if [[ $# != 1 || ( $1 != set && $1 != clear ) ]]; then
  echo '使い方: set-claude-api-key.sh set|clear' >&2
  exit 64
fi
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
action=$1
if [[ $action == set ]]; then
  if ! [[ -t 0 && -t 2 ]]; then
    echo '端末から実行してください。' >&2
    exit 1
  fi
  terminal=$(stty -g < /dev/tty) || exit 1
  restore() { stty "$terminal" < /dev/tty; }
  trap 'restore' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap 'exit 129' HUP
  stty -echo < /dev/tty || exit 1
  printf 'Claude APIキー（取消しはCtrl-D）: ' >&2
  # Periodically return to the shell so a signal arriving just before read
  # blocks cannot leave a pending trap waiting indefinitely. Preserve partial
  # input across timeouts; there is no deadline for the user to enter the key.
  key=
  while :; do
    part=
    IFS= read -r -t 1 part < /dev/tty
    result=$?
    key+=$part
    if (( result == 0 )); then
      break
    elif (( result <= 128 )); then
      printf '\n入力を取り消しました。\n' >&2
      exit 1
    fi
  done
  unset part
  restore || exit 1
  trap - EXIT
  printf '\n' >&2
  if [[ ${#key} -gt 4096 || ! $key =~ ^sk-ant-[A-Za-z0-9_-]{20,}$ ]]; then
    echo 'キーの形式が不正です。' >&2
    exit 1
  fi
  if ! printf '%s\n' "$key" | sh "$script_dir/intake.sh" exec -T /usr/local/bin/intake admin --config /state/config.local.json --application-dir /app claude-api-key set >/dev/null 2>&1; then
    unset key
    echo '登録に失敗しました。書き手と受付の状態を確認してください。' >&2
    exit 1
  fi
  unset key
else
  if ! sh "$script_dir/intake.sh" exec -T /usr/local/bin/intake admin --config /state/config.local.json --application-dir /app claude-api-key clear </dev/null >/dev/null 2>&1; then
    echo '消去に失敗しました。書き手と受付の状態を確認してください。' >&2
    exit 1
  fi
fi
echo '操作が完了しました。'
