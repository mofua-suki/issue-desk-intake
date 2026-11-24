# 常設の指示（Issue #1332。docs/chat-instructions.md『Distribution of the Standing Instructions』）: 正本 chat-instructions.md を
# ~/.claude/CLAUDE.md・~/.codex/AGENTS.md の目印で囲んだ範囲だけへ配る。規則は client/src/chat-instructions.mjs と同じ
# （範囲の外は1文字も変えない、改行（CRLF/LF）と BOM を保つ、目印の不正・読めない・書けないは書かずに1行出す）。
# 組み込みと cmp・cp・mv だけで行う。作成・更新・失敗のときだけ1行出し、変更が無ければ何も出さない。
CI_SOURCE=chat-instructions.md
CI_BEGIN='<!-- issue-desk:chat-instructions:begin -->'
CI_END='<!-- issue-desk:chat-instructions:end -->'
CI_NOTE='この範囲は issue-desk の監視が chat-instructions.md から書き換える（編集は正本で）'
CI_CR=$(printf '\r')
CI_BOM=$(printf '\357\273\277')

# ファイル $1 の中身をそのまま CI_READ に入れる（末尾の改行の有無も保つ。NUL は落ちるので呼び出し元が cmp で確かめる）。
ci_read() {
  CI_READ=''
  while ci_nl=$NL && IFS= read -r ci_l || {
    ci_nl=''
    [ -n "$ci_l" ]
  }; do
    CI_READ=$CI_READ$ci_l$ci_nl
  done <"$1"
}

# 正本の本文 $1 を、BOM と行末の CR・末尾の空行を除いた行の並び（NL 区切り）にして CI_BODY に入れる。
ci_body_from() {
  CI_BODY=''
  ci_r=${1#"$CI_BOM"}
  while [ -n "$ci_r" ]; do
    case "$ci_r" in
    *"$NL"*)
      ci_l=${ci_r%%"$NL"*}
      ci_r=${ci_r#*"$NL"}
      ;;
    *)
      ci_l=$ci_r
      ci_r=''
      ;;
    esac
    CI_BODY=$CI_BODY${ci_l%"$CI_CR"}$NL
  done
  while [ "${CI_BODY%"$NL"}" != "$CI_BODY" ]; do
    CI_BODY=${CI_BODY%"$NL"}
  done
}

# 文字列 $1 の中の $2 の数を CI_N に入れる。
ci_count() {
  CI_N=0
  ci_r=$1
  while :; do
    case "$ci_r" in
    *"$2"*)
      CI_N=$((CI_N + 1))
      ci_r=${ci_r#*"$2"}
      ;;
    *) return 0 ;;
    esac
  done
}

# 改行 $1 でつないだ範囲（開始の目印・説明の1行・空行・正本の本文・終了の目印）を CI_BLOCK に入れる。
ci_block() {
  CI_BLOCK=$CI_BEGIN$1$CI_NOTE$1$1
  ci_r=$CI_BODY
  while :; do
    case "$ci_r" in
    *"$NL"*)
      CI_BLOCK=$CI_BLOCK${ci_r%%"$NL"*}$1
      ci_r=${ci_r#*"$NL"}
      ;;
    *)
      CI_BLOCK=$CI_BLOCK$ci_r$1$CI_END
      return 0
      ;;
    esac
  done
}

# 既存の本文 $1 へ範囲を入れた本文を CI_NEW に入れる（mergeInstructionBlock と同じ）。目印が不正なら CI_REASON を入れて 1。
ci_merge() {
  ci_bom=''
  ci_text=$1
  case "$ci_text" in
  "$CI_BOM"*)
    ci_bom=$CI_BOM
    ci_text=${ci_text#"$CI_BOM"}
    ;;
  esac
  ci_eol=$NL
  case "$ci_text" in
  *"$CI_CR$NL"*) ci_eol=$CI_CR$NL ;;
  esac
  ci_block "$ci_eol"
  ci_count "$ci_text" "$CI_BEGIN"
  ci_b=$CI_N
  ci_count "$ci_text" "$CI_END"
  ci_e=$CI_N
  if [ -z "$ci_text" ]; then
    CI_NEW=$CI_BLOCK$ci_eol
  elif [ "$ci_b" = 0 ] && [ "$ci_e" = 0 ]; then
    case "$ci_text" in
    *"$NL") CI_NEW=$ci_text ;;
    *) CI_NEW=$ci_text$ci_eol ;;
    esac
    CI_NEW=$CI_NEW$ci_eol$CI_BLOCK$ci_eol
  elif [ "$ci_b" = 0 ]; then
    CI_REASON='終了の目印だけがあり、開始の目印がありません'
    return 1
  elif [ "$ci_e" = 0 ]; then
    CI_REASON='開始の目印だけがあり、終了の目印がありません'
    return 1
  elif [ "$ci_b" -gt 1 ] || [ "$ci_e" -gt 1 ]; then
    CI_REASON="目印が2組以上あります（開始 $ci_b・終了 $ci_e）"
    return 1
  else
    case "${ci_text#*"$CI_BEGIN"}" in
    *"$CI_END"*) ;;
    *)
      CI_REASON='終了の目印が開始の目印より前にあります'
      return 1
      ;;
    esac
    CI_NEW=${ci_text%%"$CI_BEGIN"*}$CI_BLOCK${ci_text#*"$CI_END"}
  fi
  CI_NEW=$ci_bom$CI_NEW
}

ci_fail() {
  say "[update:skills-failed] $1 に常設の指示を書きません: $2"
}

# 書き込み先 $1 へ範囲を書く。既存のファイルは、読んだ中身がファイルと同じ（cmp）ときだけ書き換え、一時ファイルは元の
# ファイルの写し（権限を保つ）に書いてから置き換える。リンクは置き換えると壊れるので書かない。
ci_put() {
  ci_file=$1
  ci_have=''
  ci_kind=作成
  if [ -L "$ci_file" ]; then
    ci_fail "$ci_file" 'リンクのため書きません'
    return 0
  fi
  if [ -e "$ci_file" ]; then
    if [ ! -f "$ci_file" ]; then
      ci_fail "$ci_file" 'ファイルではありません'
      return 0
    fi
    if [ ! -r "$ci_file" ] || ! ci_read "$ci_file" 2>/dev/null; then
      ci_fail "$ci_file" '読めません'
      return 0
    fi
    ci_have=$CI_READ
    ci_kind=更新
  fi
  if ! ci_merge "$ci_have"; then
    ci_fail "$ci_file" "$CI_REASON"
    return 0
  fi
  [ "$ci_kind" = 作成 ] || [ "$CI_NEW" != "$ci_have" ] || return 0
  if ! skills_mkdir "${ci_file%/*}"; then
    ci_fail "$ci_file" 'フォルダーを作れません'
    return 0
  fi
  ci_tmp=$ci_file.$$.tmp
  if [ "$ci_kind" = 更新 ]; then
    if ! cp "$ci_file" "$ci_tmp" 2>/dev/null || ! { printf '%s' "$ci_have" >"$ci_tmp"; } 2>/dev/null ||
      ! cmp -s "$ci_tmp" "$ci_file"; then
      rm -f "$ci_tmp" 2>/dev/null || :
      ci_fail "$ci_file" 'そのままの形で読めない（NUL を含むなど）か、一時ファイルを書けません'
      return 0
    fi
  fi
  if { printf '%s' "$CI_NEW" >"$ci_tmp"; } 2>/dev/null && mv -f "$ci_tmp" "$ci_file" 2>/dev/null; then
    say "[update:skills] $ci_file に常設の指示を${ci_kind}しました"
    return 0
  fi
  rm -f "$ci_tmp" 2>/dev/null || :
  ci_fail "$ci_file" '書けません'
}

# 正本の無い配置先（NAS更新のテストの一時の配置先など）では何もしない。
skills_instructions() {
  ci_src=$DIR/$CI_SOURCE
  [ -e "$ci_src" ] || return 0
  if [ ! -f "$ci_src" ] || [ ! -r "$ci_src" ] || ! ci_read "$ci_src" 2>/dev/null; then
    say "[update:skills-failed] $CI_SOURCE を読めないため、常設の指示を配りません"
    return 0
  fi
  ci_body_from "$CI_READ"
  ci_put "$HOME/.claude/CLAUDE.md"
  ci_put "$HOME/.codex/AGENTS.md"
}
