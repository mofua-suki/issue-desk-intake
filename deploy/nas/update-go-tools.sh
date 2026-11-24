# NASのGoを「入っている」と分かる形で置く。update.shからsourceし、DIR・DRY_RUN・say・planを受け取る。
# NASのGoは ~/.local/bin/go にあり、PATHには無い。さらにチャットのシェルの GODEBUG（tlskyber など）が残っていると、
# Go は起動時に落ちる。そのため「NASにGoが無い」と誤解されてきた。~/.issue-desk/go と ~/.issue-desk/gofmt は、
# 場所を探し、GODEBUG を外して呼ぶ。見つからないときは探した場所を表示して止まる。
go_tool_body() {
 sed "s/__TOOL__/$1/g" <<'WRAPPER'
#!/bin/sh
# issue-desk-__TOOL__: NASのGoを呼ぶ（deploy/nas/update-go-tools.sh が配る。手で変えない）。
# PATH に __TOOL__ が無くても、Goが入っていないという意味ではない。「NASにGoが無い」と判断する前に: ~/.issue-desk/__TOOL__ version
# 探す場所は ISSUE_DESK_GO_DIRS（コロン区切り）で上書きできる（テスト用。通常は使わない）。
dirs=${ISSUE_DESK_GO_DIRS:-"$HOME/.local/bin:$HOME/.local/lib/go/bin:/usr/local/go/bin:/usr/lib/go/bin"}
found=''
old_ifs=$IFS
IFS=:
for dir in $dirs; do
 if [ -x "$dir/__TOOL__" ]; then found=$dir/__TOOL__; break; fi
done
IFS=$old_ifs
if [ -z "$found" ]; then found=$(command -v __TOOL__ 2>/dev/null) || found=''; fi
# PATH に ~/.issue-desk がある場合に、自分自身を呼んで無限に再帰しない。
self=$(readlink -f "$0" 2>/dev/null) || self=''
real=$(readlink -f "$found" 2>/dev/null) || real=''
if [ -n "$found" ] && [ -n "$self" ] && [ "$self" = "$real" ]; then found=''; fi
if [ -z "$found" ]; then
 echo "__TOOL__ が見つかりません。探した場所: $dirs と PATH" >&2
 exit 127
fi
# GODEBUG に削除済みの値（tlskyber=0 など）が残っていると、Go は起動時に落ちる。
unset GODEBUG
exec "$found" "$@"
WRAPPER
}
go_tools() (
 case "${HOME:-}" in /*) ;; *) say '[update:go-tools-failed] HOMEが絶対パスではありません'; return 1;; esac
 if [ "${DRY_RUN:-0}" = 1 ]; then plan "$HOME/.issue-desk/go と gofmt（NASのGoを呼ぶラッパー）を配置する"; return 0; fi
 umask 077
 base=$HOME/.issue-desk
 mkdir -p "$base" || return 1
 for tool in go gofmt; do
  [ ! -L "$base/$tool" ] && [ ! -d "$base/$tool" ] || return 1
  temporary=$(mktemp "$base/.$tool.XXXXXXXX") || return 1
  if ! { go_tool_body "$tool" >"$temporary" && chmod 755 "$temporary"; }; then rm -f "$temporary"; return 1; fi
  if [ -x "$base/$tool" ] && cmp -s "$temporary" "$base/$tool"; then rm -f "$temporary"; continue; fi
  mv -f "$temporary" "$base/$tool" || { rm -f "$temporary"; return 1; }
  say "[update:go-tools] NASの $tool ラッパー（~/.issue-desk/$tool）を更新しました"
 done
)
