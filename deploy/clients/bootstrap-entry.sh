#!/bin/sh
# NASで実行し、稼働受付と同じ固定コミットのUbuntu導入入口だけを標準出力へ返す。
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
container=$(sh "$root/deploy/nas/intake.sh" active) || exit 1
revision=$(docker exec "$container" sh -c 'printf "%s" "$ISSUE_DESK_REVISION"') || exit 1
case "$revision" in ''|*[!0-9a-f]*) echo '稼働受付の版を確認できません。' >&2;exit 1;; esac
[ "${#revision}" = 40 ] || { echo '稼働受付の版が40桁SHAではありません。' >&2;exit 1; }
source=$(git -C "$root" show "$revision:deploy/clients/bootstrap-ubuntu.sh") || exit 1
case "$source" in *'# bootstrap-pinned-v1:'*) ;; *) echo '固定版の導入入口を取得できません。' >&2;exit 1;; esac
printf 'BOOTSTRAP_REVISION=%s\n%s\n' "$revision" "$source"
