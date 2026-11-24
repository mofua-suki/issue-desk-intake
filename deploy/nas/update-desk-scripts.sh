# 稼働Serverの版を配置先のGitから写す。呼出元のTARGETは未稼働の場合がある。
desk_scripts_pin() (
  base=$HOME/.issue-desk
  checkout=$base/checkout
  revision=$(sh "$DIR/deploy/nas/intake.sh" status 2>/dev/null | sed -n 's/.*"revision":"\([0-9a-f]*\)".*/\1/p') || revision=
  case "$revision" in ''|*[!0-9a-f]*) revision=;; esac
  [ "${#revision}" -eq 40 ] || { printf '%s\n' '[update:desk-scripts] 稼働版を確認できないため写しを保持します' >&2; return 0; }
  mkdir -p "$checkout" || return 1
  [ ! -L "$checkout" ] && [ ! -L "$base/scripts-root" ] && [ ! -d "$base/scripts-root" ] || return 1
  lock=$checkout/.paced-lock
  mkdir "$lock" 2>/dev/null || { say '[update:desk-scripts] checkoutは使用中のため更新を見送ります'; return 0; }
  work=
  root_tmp=
  trap '[ -z "$work" ] || rm -rf -- "$work"; [ -z "$root_tmp" ] || rm -f -- "$root_tmp"; rmdir "$lock"' EXIT
  dest=$checkout/$revision
  if [ ! -d "$dest" ] || [ -L "$dest" ] || [ ! -f "$dest/.desk-scripts-snapshot" ]; then
    [ ! -e "$dest" ] && [ ! -L "$dest" ] || return 1
    work=$(mktemp -d "$checkout/.snapshot.XXXXXXXX") || return 1
    git -C "$DIR" archive --format=tar -o "$work/archive.tar" "$revision" || return 1
    mkdir "$work/tree" || return 1
    tar -xf "$work/archive.tar" -C "$work/tree" || return 1
    [ -f "$work/tree/scripts/desk-client.ts" ] && [ -d "$work/tree/shared" ] && [ -f "$work/tree/deploy/versions.json" ] || return 1
    : >"$work/tree/.desk-scripts-snapshot" || return 1
    mv "$work/tree" "$dest" || return 1
  fi
  [ -f "$dest/scripts/desk-client.ts" ] && [ -d "$dest/shared" ] && [ -f "$dest/deploy/versions.json" ] || return 1
  previous=
  if [ -f "$base/scripts-root" ]; then
    IFS= read -r previous < "$base/scripts-root" || return 1
    if [ "$previous" = "$dest" ]; then
      previous=$(sed -n '2p' "$base/scripts-root") || return 1
    fi
  fi
  root_tmp=$(mktemp "$base/.scripts-root.XXXXXXXX") || return 1
  printf '%s\n' "$dest" >"$root_tmp" || return 1
  [ -z "$previous" ] || printf '%s\n' "$previous" >>"$root_tmp" || return 1
  mv -f "$root_tmp" "$base/scripts-root" || return 1
  root_tmp=
  # この仕組みの印がある40桁のフォルダーだけを消す。使用記録は空でも保守的に残す。
  for candidate in "$checkout"/*; do
    name=${candidate##*/}
    case "$name" in ''|*[!0-9a-f]*) continue;; esac
    [ "${#name}" -eq 40 ] || continue
    [ "$candidate" != "$dest" ] && [ "$candidate" != "$previous" ] || continue
    [ -d "$candidate" ] && [ ! -L "$candidate" ] && [ -f "$candidate/.desk-scripts-snapshot" ] || continue
    [ ! -e "$checkout/.paced-use/$name" ] && [ ! -L "$checkout/.paced-use/$name" ] || continue
    rm -rf -- "$candidate" || say "[update:desk-scripts] 古い写しを削除できません: $candidate"
  done
)
