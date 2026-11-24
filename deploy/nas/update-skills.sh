# NASの更新処理からsourceし、chat-deploy.jsonで指定したSkill・常設指示・フックを配る。
# 内容が違うときだけ原子的に置き換え、配布先だけにあるファイルは消さない。
# NASのNode launcherは受付イメージのNodeを使う。UbuntuはホストNode24を使う。Server本体は起動しない。
# say・plan・NL・DIR・DRY_RUNを受け取る。失敗は固定の理由で報告し更新の終了状態を変えない。

SKILLS_LIST=chat-deploy.json

# 文字列 $1 の先頭の空白を除いた値を SK_TRIM に入れる。
skills_trim() {
  SK_TRIM=${1#"${1%%[![:space:]]*}"}
}

# 項目の中身（sk_item）からキー $1 の文字列の値を SK_FIELD に入れる。無い・文字列でなければ 1。
skills_field() {
  case "$sk_item" in
  *"\"$1\""*) ;;
  *) return 1 ;;
  esac
  skills_trim "${sk_item#*\"$1\"}"
  case "$SK_TRIM" in
  :*) ;;
  *) return 1 ;;
  esac
  skills_trim "${SK_TRIM#:}"
  case "$SK_TRIM" in
  \"*\"*) ;;
  *) return 1 ;;
  esac
  SK_FIELD=${SK_TRIM#\"}
  SK_FIELD=${SK_FIELD%%\"*}
}

# 名前（フォルダー名・種類）が英数字で始まり、英数字・.・_・- だけなら 0。
skills_name_ok() {
  case "$1" in
  '' | [!A-Za-z0-9]* | *[!A-Za-z0-9._-]*) return 1 ;;
  esac
  return 0
}

# $1（一覧の JSON）の targets の各項目を「kind name」の1行ずつ SK_ITEMS へ入れる。形が正しくなければ 1。
skills_items() {
  SK_ITEMS=''
  case "$1" in
  *'"targets"'*) ;;
  *) return 1 ;;
  esac
  skills_trim "${1#*\"targets\"}"
  case "$SK_TRIM" in
  :*) ;;
  *) return 1 ;;
  esac
  skills_trim "${SK_TRIM#:}"
  case "$SK_TRIM" in
  \[*\]*) ;;
  *) return 1 ;;
  esac
  sk_rest=${SK_TRIM#\[}
  sk_rest=${sk_rest%%\]*}
  while :; do
    skills_trim "$sk_rest"
    sk_rest=$SK_TRIM
    [ -n "$sk_rest" ] || return 0
    case "$sk_rest" in
    \{*\}*) ;;
    *) return 1 ;;
    esac
    sk_item=${sk_rest#\{}
    sk_item=${sk_item%%\}*}
    skills_trim "${sk_rest#*\}}"
    case "$SK_TRIM" in
    ,*) sk_rest=${SK_TRIM#,} ;;
    '') sk_rest='' ;;
    *) return 1 ;;
    esac
    skills_field kind || return 1
    sk_kind=$SK_FIELD
    skills_field name || return 1
    skills_name_ok "$sk_kind" && skills_name_ok "$SK_FIELD" || return 1
    SK_ITEMS=$SK_ITEMS$sk_kind' '$SK_FIELD$NL
  done
}

# フォルダー $1 の下のファイルの相対パスを1行ずつ SW_FILES に足す（$2 は途中の相対パス。リンクはたどらない）。
skills_walk() {
  for sw_f in "$1/$2"* "$1/$2".[!.]* "$1/$2"..?*; do
    if [ -L "$sw_f" ] || [ ! -e "$sw_f" ]; then
      continue
    elif [ -d "$sw_f" ]; then
      skills_walk "$1" "${sw_f#"$1"/}/"
    elif [ -f "$sw_f" ]; then
      SW_FILES=$SW_FILES${sw_f#"$1"/}$NL
    fi
  done
}

# フォルダー $1 を、既にある一番近い上のフォルダーから1段ずつ作る（mkdir -p は上のフォルダーを順にたどるので、
# 入れない上のフォルダーがあると失敗する）。作れなければ 1。
skills_mkdir() {
  [ -n "$1" ] || return 1
  [ ! -d "$1" ] || return 0
  skills_mkdir "${1%/*}" || return 1
  mkdir "$1" 2>/dev/null || [ -d "$1" ]
}

# 内容が違うときだけ $1 を $2 へ写す（一時ファイルに写してから名前を変える）。写したら SC_WROTE=1。写せなければ 1。
skills_put() {
  SC_WROTE=0
  if [ -f "$2" ] && cmp -s "$1" "$2"; then
    return 0
  fi
  skills_mkdir "${2%/*}" || return 1
  if cp "$1" "$2.$$.tmp" 2>/dev/null && mv -f "$2.$$.tmp" "$2" 2>/dev/null; then
    SC_WROTE=1
    return 0
  fi
  rm -f "$2.$$.tmp" 2>/dev/null || :
  return 1
}

# Skill $1 を配置先 $2 へ写し、SK_CREATED・SK_UPDATED・SK_UNCHANGED と SK_ORPHANS（配置先にだけあるファイル）を進める。
skills_copy() {
  sc_source=$DIR/skills/$1
  sc_new=0
  [ -e "$2" ] || sc_new=1
  sc_changed=0
  SW_FILES=''
  skills_walk "$sc_source" ''
  sc_files=$SW_FILES
  sc_list=$sc_files
  while [ -n "$sc_list" ]; do
    sc_rel=${sc_list%%"$NL"*}
    sc_list=${sc_list#*"$NL"}
    if ! skills_put "$sc_source/$sc_rel" "$2/$sc_rel"; then
      SK_ERROR="$2/$sc_rel を書けません"
      return 1
    fi
    sc_changed=$((sc_changed + SC_WROTE))
  done
  if [ "$sc_new" = 1 ]; then
    SK_CREATED=$((SK_CREATED + 1))
  elif [ "$sc_changed" -gt 0 ]; then
    SK_UPDATED=$((SK_UPDATED + 1))
  else
    SK_UNCHANGED=$((SK_UNCHANGED + 1))
  fi
  SW_FILES=''
  skills_walk "$2" ''
  sc_list=$SW_FILES
  while [ -n "$sc_list" ]; do
    sc_rel=${sc_list%%"$NL"*}
    sc_list=${sc_list#*"$NL"}
    case "$NL$sc_files" in
    *"$NL$sc_rel$NL"*) ;;
    *) SK_ORPHANS="$SK_ORPHANS${SK_ORPHANS:+, }$2/$sc_rel" ;;
    esac
  done
  return 0
}

# ~/.issue-desk/nodeの中身。ホストのNode24でチャット補助を実行する。
skills_container_node_text() {
  SK_NODE="#!/bin/sh
# issue-desk の Skill のスクリプトを、NAS 本体に node が無くても受付のイメージの node で動かす。
# deploy/nas/update.sh（deploy/nas/update-skills.sh）が書き直すので、手で変えない。
# ホーム（\$HOME）だけを同じパスで見せるので、スクリプト・依頼のファイル・トークンのファイルはホームの下に置く。
# mktemp reserves a unique name even for simultaneous callers in different shells.
name_dir=\$(mktemp -d \"\${TMPDIR:-/tmp}/issue-desk-node.XXXXXXXXXX\") || exit 1
container=\${name_dir##*/}
run_pid=''
# Bound each Docker cleanup client to three seconds, even if the daemon hangs.
bounded_cleanup() {
  docker \"\$@\" >/dev/null 2>&1 &
  cleanup_pid=\$!
  remaining=3
  while kill -0 \"\$cleanup_pid\" 2>/dev/null && [ \"\$remaining\" -gt 0 ]; do
    sleep 1
    remaining=\$((remaining - 1))
  done
  kill -KILL \"\$cleanup_pid\" 2>/dev/null || :
  wait \"\$cleanup_pid\" 2>/dev/null || :
}
interrupted() {
  trap '' TERM INT HUP
  # A signal can arrive between the asynchronous launch and run_pid assignment.
  run_pid=\${run_pid:-\$!}
  bounded_cleanup stop -t 1 \"\$container\"
  bounded_cleanup rm -f \"\$container\"
  if [ -n \"\$run_pid\" ]; then
    kill -KILL \"\$run_pid\" 2>/dev/null || :
    wait \"\$run_pid\" 2>/dev/null || :
  fi
  rmdir \"\$name_dir\" 2>/dev/null || :
  exit \"\$1\"
}
trap 'interrupted 143' TERM
trap 'interrupted 130' INT
trap 'interrupted 129' HUP
# Save stdin before the asynchronous shell command defaults fd 0 to /dev/null.
exec 3<&0
docker run --rm -i --init --name \"\$container\" --network host --user \"\$(id -u):\$(id -g)\" -e HOME -e ISSUE_DESK_URL -e ISSUE_DESK_TOKEN_FILE -v \"\$HOME:\$HOME\" -w \"\$PWD\" --entrypoint node $IMAGE \"\$@\" <&3 3<&- &
run_pid=\$!
exec 3<&-
wait \"\$run_pid\"
status=\$?
rmdir \"\$name_dir\" 2>/dev/null || :
exit \"\$status\"
"
}


skills_node_text() {
 if [ "${SKILLS_HOST_NODE:-0}" != 1 ]; then skills_container_node_text; return; fi
 SK_NODE='#!/bin/sh
# issue-desk-rc-node: チャット補助用のホストNode24。Serverは起動しない。
usable() {
 [ -x "$1" ] || return 1
 version=$("$1" --version 2>/dev/null) || return 1
 version=${version#v}; version=${version%%.*}
 case "$version" in ""|*[!0-9]*) return 1;; esac
 [ "$version" -ge 24 ]
}
node_path=$(command -v node 2>/dev/null) || node_path=""
if [ -n "$node_path" ] && usable "$node_path"; then exec "$node_path" "$@"; fi
for dir in "$HOME/.local/bin" "$HOME/.local/node/bin" /usr/local/bin /usr/bin /opt/node/bin; do
 if usable "$dir/node"; then exec "$dir/node" "$@"; fi
done
echo "チャット補助にはホストのNode.js 24以上が必要です。" >&2
exit 1
'
}

# ~/.issue-desk/node を置く（内容が同じで実行できれば書かない）。書いたら SK_NODE_WROTE=作成・更新。書けなければ 1。
skills_node() {
  SK_NODE_WROTE=''
  [ "${SKILLS_NODE_MANAGED_EXTERNALLY:-0}" != 1 ] || return 0
  skills_node_text
  sn_file=$HOME/.issue-desk/node
  sn_have=''
  if [ -f "$sn_file" ]; then
    while IFS= read -r sn_line || [ -n "$sn_line" ]; do
      sn_have=$sn_have$sn_line$NL
    done <"$sn_file"
    if [ "$sn_have" = "$SK_NODE" ] && [ -x "$sn_file" ]; then
      return 0
    fi
    sn_kind=更新
  else
    sn_kind=作成
  fi
  skills_mkdir "$HOME/.issue-desk" || return 1
  if { printf '%s' "$SK_NODE" >"$sn_file.$$.tmp"; } 2>/dev/null && chmod 755 "$sn_file.$$.tmp" 2>/dev/null &&
    mv -f "$sn_file.$$.tmp" "$sn_file" 2>/dev/null; then
    SK_NODE_WROTE=$sn_kind
    return 0
  fi
  rm -f "$sn_file.$$.tmp" 2>/dev/null || :
  return 1
}

# 毎回の実行の最後に呼ぶ（update.sh の finish）。--dry-run では配らず予定だけを表示する。
skills_listed() {
  case "${HOME:-}" in
  /*) ;;
  *)
    say '[update:skills-failed] HOME が絶対パスではないため、Skill と ~/.issue-desk/node を配りません'
    return 0
    ;;
  esac
  sk_list=''
  sk_read=1
  if [ -f "$DIR/$SKILLS_LIST" ] && [ -r "$DIR/$SKILLS_LIST" ]; then
    while IFS= read -r sk_line || [ -n "$sk_line" ]; do
      sk_list="$sk_list$sk_line "
    done <"$DIR/$SKILLS_LIST" || sk_read=0
  else
    sk_read=0
  fi
  SK_NAMES=''
  SK_SKIPPED=''
  SK_ERROR=''
  if [ "$sk_read" = 0 ]; then
    SK_ERROR="$SKILLS_LIST を読めません"
  elif ! skills_items "$sk_list"; then
    SK_ERROR="$SKILLS_LIST が {\"targets\": [{\"kind\": …, \"name\": …}, …]} の形ではありません"
  else
    sk_rest=$SK_ITEMS
    while [ -n "$sk_rest" ]; do
      sk_line=${sk_rest%%"$NL"*}
      sk_rest=${sk_rest#*"$NL"}
      sk_kind=${sk_line%% *}
      sk_name=${sk_line#* }
      if [ "$sk_kind" != skill ]; then
        SK_SKIPPED="$SK_SKIPPED${SK_SKIPPED:+, }$sk_kind $sk_name"
        continue
      fi
      case "$NL$SK_NAMES" in
      *"$NL$sk_name$NL"*) continue ;;
      esac
      if [ ! -f "$DIR/skills/$sk_name/SKILL.md" ]; then
        SK_ERROR="skills/$sk_name/SKILL.md がないため、どの Skill も配りません"
        break
      fi
      SK_NAMES=$SK_NAMES$sk_name$NL
    done
  fi
  if [ "$DRY_RUN" = 1 ]; then
    plan "予定（最後に）: $SKILLS_LIST の Skill を $HOME/.claude/skills・$HOME/.codex/skills へ配り、$HOME/.issue-desk/node を置き、scripts/chat-stop-hook.ts を $HOME/.claude/hooks/chat-stop-hook.ts へ配り、scripts/guard-gitea.sh を $HOME/.claude/hooks/guard-gitea.sh へ配り、$CI_SOURCE を $HOME/.claude/CLAUDE.md・$HOME/.codex/AGENTS.md の目印の範囲へ配る（内容が同じなら書かない）${SK_ERROR:+。今の一覧では配れません: $SK_ERROR}${SK_SKIPPED:+。飛ばす項目: $SK_SKIPPED}"
    return 0
  fi
  SK_CREATED=0
  SK_UPDATED=0
  SK_UNCHANGED=0
  SK_ORPHANS=''
  sk_count=0
  if [ -z "$SK_ERROR" ]; then
    sk_rest=$SK_NAMES
    while [ -n "$sk_rest" ]; do
      sk_name=${sk_rest%%"$NL"*}
      sk_rest=${sk_rest#*"$NL"}
      sk_count=$((sk_count + 1))
      skills_copy "$sk_name" "$HOME/.claude/skills/$sk_name" && skills_copy "$sk_name" "$HOME/.codex/skills/$sk_name" || break
    done
  fi
  if [ -n "$SK_ERROR" ]; then
    say "[update:skills-failed] Skill を配れませんでした: $SK_ERROR"
  elif [ $((SK_CREATED + SK_UPDATED)) -gt 0 ]; then
    say "[update:skills] Skill ${sk_count}件（配置先 $((sk_count * 2))か所）: 作成 $SK_CREATED・更新 $SK_UPDATED・変更なし $SK_UNCHANGED${SK_ORPHANS:+。元にないファイル（消していません）: $SK_ORPHANS}${SK_SKIPPED:+。配り方の無い種類で飛ばした項目: $SK_SKIPPED}"
  fi
  if ! skills_node; then
    say "[update:skills-failed] $HOME/.issue-desk/node を書けません"
  elif [ -n "$SK_NODE_WROTE" ]; then
    say "[update:skills] $HOME/.issue-desk/node を${SK_NODE_WROTE}しました（チャット補助のNode launcher）"
  fi
  sk_hook=$HOME/.claude/hooks/chat-stop-hook.ts
  sk_hook_kind=作成
  [ ! -e "$sk_hook" ] || sk_hook_kind=更新
  # フックが読み込む検査の部品（同じ場所に置く）を先に配る。
  if ! skills_put "$DIR/scripts/chat-shell-parser.ts" "$HOME/.claude/hooks/chat-shell-parser.ts"; then
    say "[update:skills-failed] $HOME/.claude/hooks/chat-shell-parser.ts を書けません"
  elif ! skills_put "$DIR/scripts/chat-usage-report.ts" "$HOME/.claude/hooks/chat-usage-report.ts"; then
    say "[update:skills-failed] $HOME/.claude/hooks/chat-usage-report.ts を書けません"
  elif ! skills_put "$DIR/scripts/chat-turn-checks.ts" "$HOME/.claude/hooks/chat-turn-checks.ts"; then
    say "[update:skills-failed] $HOME/.claude/hooks/chat-turn-checks.ts を書けません"
  elif ! skills_put "$DIR/scripts/stop-check-record.ts" "$HOME/.claude/hooks/stop-check-record.ts"; then
    say "[update:skills-failed] $HOME/.claude/hooks/stop-check-record.ts を書けません"
  elif ! skills_put "$DIR/scripts/chat-stop-hook.ts" "$sk_hook"; then
    say "[update:skills-failed] $sk_hook を書けません"
  elif [ "$SC_WROTE" = 1 ]; then
    say "[update:skills] $sk_hook を$sk_hook_kindしました"
  fi
  skills_guard
  skills_instructions
  # 更新タイマーでも再送する。oneshot の終了前に最大60秒待つ。
  if [ -x "$HOME/.issue-desk/desk" ] && [ -s "$HOME/.issue-desk/stop-check-fires.jsonl" ]; then
    (cd "$HOME" && timeout 60 "$HOME/.issue-desk/desk" stop-check-send.ts >/dev/null 2>&1) </dev/null || :
  fi
  return 0
}

# scripts/guard-gitea.sh を ~/.claude/hooks/guard-gitea.sh へ配り、実行可にする。作成・更新した回だけ、正本の cksum と
# settings.json の PreToolUse に登録が無いこと（登録はしない）、配布元に無い hooks のファイル名（消さない）を添えて出す。
skills_guard() {
  sg_dest=$HOME/.claude/hooks/guard-gitea.sh
  sg_kind=作成
  [ ! -e "$sg_dest" ] || sg_kind=更新
  if ! skills_put "$DIR/scripts/guard-gitea.sh" "$sg_dest" || ! chmod 755 "$sg_dest" 2>/dev/null; then
    say "[update:skills-failed] $sg_dest を書けません"
    return 0
  fi
  [ "$SC_WROTE" = 1 ] || return 0
  sg_sum=$(cksum <"$DIR/scripts/guard-gitea.sh" 2>/dev/null) || sg_sum='不明'
  sg_note=''
  if ! grep -qs 'guard-gitea\.sh' "$HOME/.claude/settings.json" 2>/dev/null; then
    sg_note="。$HOME/.claude/settings.json の PreToolUse に guard-gitea.sh の登録がありません（登録はしません）"
  fi
  sg_extra=''
  for sg_f in "$HOME/.claude/hooks/"* "$HOME/.claude/hooks/".[!.]* "$HOME/.claude/hooks/"..?*; do
    { [ -e "$sg_f" ] || [ -L "$sg_f" ]; } || continue
    sg_base=${sg_f##*/}
    case "$sg_base" in
    chat-stop-hook.ts | chat-turn-checks.ts | chat-shell-parser.ts | chat-usage-report.ts | stop-check-record.ts | guard-gitea.sh) continue ;;
    esac
    sg_extra="$sg_extra${sg_extra:+, }$sg_base"
  done
  say "[update:skills] $sg_dest を${sg_kind}しました（正本の cksum: $sg_sum）${sg_note}${sg_extra:+。配布元に無い hooks のファイル（消していません）: $sg_extra}"
}

. "$DIR/deploy/nas/update-skills-instructions.sh"
