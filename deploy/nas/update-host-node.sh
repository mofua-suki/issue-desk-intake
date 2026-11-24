# NAS本体のNode（~/.local/bin/node）を deploy/versions.json の node の版にそろえる。update.shからsourceし、DIR・sayを受け取る（update.sh の --dry-run はここへ届く前に終わる）。
# 公式の配布物を SHASUMS256.txt で照合し、~/.local/lib/nodejs/node-v<版>-linux-<arch> に展開してから current を切り替える。
# ~/.local/bin の node・npm・npx・corepack・pnpm・pnpx は current を指す。前の版のディレクトリは消さない（切り戻しに使う）。
# OS付属の /usr/bin/node は対象外（UGOSが管理する）。
host_node_link() {
 # $1: ~/.local/bin の名前、$2: 指す先。通常のファイルは上書きしない。
 if [ -e "$bin/$1" ] && [ ! -L "$bin/$1" ]; then
  say "[update:host-node-failed] $bin/$1 がリンクではないので置き換えません"
  return 1
 fi
 [ "$(readlink "$bin/$1" 2>/dev/null)" = "$2" ] && return 0
 rm -f "$bin/.$1.tmp"
 ln -s "$2" "$bin/.$1.tmp" && mv -fT "$bin/.$1.tmp" "$bin/$1" || { rm -f "$bin/.$1.tmp"; return 1; }
}
host_node_links() {
 for tool in node npm npx corepack pnpm pnpx; do
  host_node_link "$tool" "$lib/current/bin/$tool" || return 1
 done
}
host_node_validate() (
 candidate=$1
 export PATH="$candidate/bin:$PATH"
 [ "$("$candidate/bin/node" --version)" = "v$version" ] &&
 [ "$("$candidate/bin/corepack" --version)" = "0.36.0" ] &&
 [ "$("$candidate/bin/pnpm" --version)" = "$pnpm_version" ] &&
 "$candidate/bin/npm" --version >/dev/null && "$candidate/bin/npx" --version >/dev/null &&
 help=$("$candidate/bin/pnpx" --help) &&
 printf '%s\n' "$help" | grep -Fx "Version $pnpm_version" >/dev/null &&
 printf '%s\n' "$help" | grep -E '^Usage: pnpm dlx([[:space:]]|$)' >/dev/null
)
host_node() (
 case "${HOME:-}" in /*) ;; *) say '[update:host-node-failed] HOMEが絶対パスではありません'; return 1;; esac
 version=$(sed -n 's/^[[:space:]]*"node":[[:space:]]*"\([0-9][0-9.]*\)".*/\1/p' "$DIR/deploy/versions.json" | sed -n '1p')
 case "$version" in
  *[!0-9.]* | '') say '[update:host-node-failed] deploy/versions.json から node の版を読めません'; return 1 ;;
 esac
 pnpm_version=$(sed -n 's/^[[:space:]]*"pnpm":[[:space:]]*"\([0-9][0-9.]*\)".*/\1/p' "$DIR/deploy/versions.json" | sed -n '1p')
 case "$pnpm_version" in *[!0-9.]* | '') say '[update:host-node-failed] pnpm の版を読めません'; return 1;; esac
 case "$(uname -m)" in
  x86_64) arch=x64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) say "[update:host-node-failed] 対応していないCPUです: $(uname -m)"; return 1 ;;
 esac
 name=node-v$version-linux-$arch
 lib=$HOME/.local/lib/nodejs
 bin=$HOME/.local/bin
 umask 022
 mkdir -p "$lib" "$bin" || return 1
 backup=''
 if ! host_node_validate "$lib/$name"; then
  work=$(mktemp -d "$lib/.download.XXXXXXXX") || return 1
  archive=$name.tar.xz
  if ! (cd "$work" &&
   timeout "${ISSUE_DESK_NODE_FETCH_TIMEOUT:-900}" curl -fsSL --connect-timeout 30 --proto '=https' --proto-redir '=https' -O "https://nodejs.org/dist/v$version/$archive" &&
   timeout "${ISSUE_DESK_NODE_FETCH_TIMEOUT:-900}" curl -fsSL --connect-timeout 30 --proto '=https' --proto-redir '=https' -O "https://nodejs.org/dist/v$version/SHASUMS256.txt" &&
   grep "  $archive\$" SHASUMS256.txt | sha256sum -c - >/dev/null &&
   tar -xJf "$archive" &&
   [ "$("./$name/bin/node" --version)" = "v$version" ] &&
   PATH="$work/$name/bin:$PATH" "$work/$name/bin/npm" install --prefix "$work/$name" -g --no-save --ignore-scripts --no-audit --no-fund corepack@0.36.0 "pnpm@$pnpm_version") >"$work/log" 2>&1; then
   say "[update:host-node-failed] Node v$version の取得・照合・展開に失敗しました（今のNodeを保持します）: $(tail -n 3 "$work/log" | tr '\n' ' ')"
   rm -rf "$work"
   return 1
  fi
  if ! host_node_validate "$work/$name"; then
   say "[update:host-node-failed] 候補のコマンドを起動できません（今のNodeを保持します）"
   rm -rf "$work"
   return 1
  fi
  if [ -e "$lib/$name" ] || [ -L "$lib/$name" ]; then
   backup=$(mktemp -d "$lib/.replaced.XXXXXXXX") || { rm -rf "$work"; return 1; }
   if ! mv -T "$lib/$name" "$backup/$name"; then
    rmdir "$backup"
    rm -rf "$work"
    return 1
   fi
  else
   backup=''
  fi
  if ! mv -T "$work/$name" "$lib/$name"; then
   if [ -n "$backup" ]; then
    mv -T "$backup/$name" "$lib/$name" || say "[update:host-node-failed] 元の候補を $backup/$name から復旧してください"
   fi
   rm -rf "$work"
   return 1
  fi
  rm -rf "$work"
 fi
 # 既存候補も毎回検証し、切り替え前に通常ファイルとの衝突を検出する。
 if ! host_node_validate "$lib/$name"; then
  say "[update:host-node-failed] 候補のコマンドを起動できません（今のNodeを保持します）"
  return 1
 fi
 [ -z "$backup" ] || rm -rf "$backup" || return 1
 for tool in node npm npx corepack pnpm pnpx; do
  if [ -e "$bin/$tool" ] && [ ! -L "$bin/$tool" ]; then
   say "[update:host-node-failed] $bin/$tool がリンクではないので置き換えません"
   return 1
  fi
 done
 if [ "$(readlink "$lib/current" 2>/dev/null)" = "$lib/$name" ]; then
  host_node_links || return 1
  return 0
 fi
 previous=$(readlink "$lib/current" 2>/dev/null) || previous=''
 rm -f "$lib/.current.tmp"
 ln -s "$lib/$name" "$lib/.current.tmp" && mv -fT "$lib/.current.tmp" "$lib/current" || { rm -f "$lib/.current.tmp"; return 1; }
 host_node_links || return 1
 say "[update:host-node] NAS本体のNodeを v$version にしました（前: ${previous:-なし}。前の版は消していません）"
)
