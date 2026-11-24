# host-ssh.sh から読み込む。出力の整理（host-ssh-output.ts）に使う、TypeScript を直接実行できる node を選ぶ。
# host-ssh.sh は PATH を固定するので、PATH の node（NAS本体では /usr/bin/node の v18）は .ts を実行できない。
# 引数の候補を順に試し、型の除去（process.features.typescript）がある最初のものを標準出力へ返す。
# 見つからなければ、試した場所とそれぞれの理由を標準エラーへ出して失敗する。
typescript_node() {
  typescript_node_tried=
  for typescript_node_candidate do
    if [ ! -e "$typescript_node_candidate" ]; then
      typescript_node_reason='ありません'
    elif [ ! -x "$typescript_node_candidate" ]; then
      typescript_node_reason='実行できません'
    elif "$typescript_node_candidate" -e 'process.exit(process.features.typescript ? 0 : 1)' >/dev/null 2>&1; then
      printf '%s\n' "$typescript_node_candidate"
      return 0
    else
      typescript_node_version=$("$typescript_node_candidate" --version 2>/dev/null) || typescript_node_version='版を確認できない'
      typescript_node_reason="TypeScriptを実行できません（$typescript_node_version。Node 24 以上が必要）"
    fi
    typescript_node_tried="$typescript_node_tried
  $typescript_node_candidate: $typescript_node_reason"
  done
  printf '%s%s\n' 'TypeScriptを実行できる node が見つかりません。探した場所:' "$typescript_node_tried" >&2
  return 1
}
