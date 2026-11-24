#!/bin/bash
# Claude Code の PreToolUse フック（各PCの ~/.claude/hooks/guard-gitea.sh の正本。deploy/nas/update-skills.sh が NAS へ配る）。
# Gitea の Issue API とトークンを直接扱う操作を止める。issue-desk への依頼は受付を通す:
#   ~/.issue-desk/desk create-issue.ts --file ~/.issue-desk/requests/<名前>.md
# 依頼ファイル（~/.issue-desk/requests/*.md）への Write は issue-desk のフォルダーの例外で通る。
# 受付の窓口（desk）の1コマンドだけは、本文に Gitea の語を含んでも通す（受付の API だけを使い、Gitea を直接呼ばない）。
# bash と grep だけで動く（jq・node は使わない）。入力の JSON 全体を読み、grep -E で判定する。
# 第1引数が verify のときはフックとして動かず、下の「必須検証の固定入口」を実行する。
# 第1引数が verify-store のときは「必須検証の記録・ロックの操作基盤」の点検を実行する。
# verify と verify-store は、記録の操作に基本の道具（env・id・ls・mkdir・mv・rm・rmdir・sleep）を使う。

# ---- 必須検証の固定入口（段1/6） ----
# 呼び方: リポジトリの作業場所で bash ~/.claude/hooks/guard-gitea.sh verify （引数・標準入力は受け取らない）
# origin から特定したリポジトリの対応表どおりに固定の検証だけを実行し、合否と安全な失敗情報を標準出力へ出す。
# 終了コード: 0=合格 1=不合格 2=拒否（引数あり・対応表に無いリポジトリ） 3=実行不能。
# 合格とみなすのは、終了コード0かつ「必須検証（段1）: 合格」の行が出たときだけ（SHELLOPTS=noexec では何も実行せず終了コード0で終わるため）。
# 開始時に旧い成功を無効にし、最新に始めた検証だけが結果を記録する（下の「検証の開始・終了と記録の接続」）。
# 検証の前後で対象（同一性）が同じと確かめられたときだけ成功を記録する（下の「同一性の記録と検証後の照合」）。
# 記録はまだ送信の許可に使わない（送信の候補の検査と送信直前の照合は後続の段）。入力・検証の出力をコードとして評価しない。
# 検証と Git には呼び出し元の環境値を引き継がず、HOME・PATH・LANG・TMPDIR だけを渡す。

# ガード自身の bash の起動時に効く設定を検出する。見つかれば 1 を返す。
# 環境から渡された BASH_ENV・SHELLOPTS・BASHOPTS と、環境から取り込まれた関数（BASH_FUNC_名前%%）が対象。
# このガードの関数は verify_main より前に定義した verify_ で始まるものだけなので、ほかの関数があれば取り込まれたものとみなす。
# 一覧を読めないとき（declare などを差し替えられて何も返らないとき）も不合格にする。判定は [[ ]] と case だけで行う。
verify_startup_clean() {
  local name flags funcs line nl=$'\n'
  [[ -z ${BASH_ENV+x} ]] || return 1
  for name in SHELLOPTS BASHOPTS; do
    flags=$(builtin declare -p "$name" 2>/dev/null) || return 1
    [[ $flags == 'declare -'*" $name="* ]] || return 1
    flags=${flags#declare -}
    flags=${flags%% *}
    [[ $flags != *x* ]] || return 1
  done
  funcs=$(builtin declare -F) || return 1
  [[ -n $funcs ]] || return 1
  while [[ -n $funcs ]]; do
    line=${funcs%%"$nl"*}
    [[ $line == 'declare -f verify_'* ]] || return 1
    [[ $funcs == *"$nl"* ]] || break
    funcs=${funcs#*"$nl"}
  done
  return 0
}

# 検証と Git を実行するときの環境を作る。呼び出し元の環境値は引き継がず、許可した名前（HOME・PATH・LANG・TMPDIR）だけを
# env -i で渡す。env は絶対パスで呼ぶ。VERIFY_CLEAN に置く。env が無ければ失敗する。
verify_clean_env() {
  local bin name
  VERIFY_CLEAN=()
  for bin in /usr/bin/env /bin/env; do
    [ -x "$bin" ] && break
    bin=
  done
  [ -n "$bin" ] || return 1
  VERIFY_CLEAN=("$bin" -i)
  for name in HOME PATH LANG TMPDIR; do
    [ -n "${!name+x}" ] && VERIFY_CLEAN+=("$name=${!name}")
  done
  return 0
}

# 許可した環境値だけで PATH から道具を探す。見つからなければ失敗する。
verify_has() {
  "${VERIFY_CLEAN[@]}" sh -c 'command -v -- "$1"' sh "$1" >/dev/null 2>&1 </dev/null
}

# origin の URL から owner/repo を取り出す。取り出せなければ失敗する。
verify_repo_key() {
  local LC_ALL=C # 呼び出し元のロケールに依らず、ASCII の文字だけに一致させる。
  local url owner repo
  local name_re='^[A-Za-z0-9._-]+$'
  local url_re='^[A-Za-z0-9._:/@~+%-]+/[A-Za-z0-9._-]+/?$'
  url=$("${VERIFY_CLEAN[@]}" git remote get-url origin 2>/dev/null </dev/null) || return 1
  [[ $url =~ $url_re ]] || return 1
  url=${url%/}
  url=${url%.git}
  repo=${url##*/}
  owner=${url%/*}
  owner=${owner##*[/:]}
  [[ $owner =~ $name_re && $repo =~ $name_re ]] || return 1
  printf '%s/%s' "$owner" "$repo"
}

# リポジトリ別の対応表: 必須の検証の名前を1行ずつ返す。対応表に無ければ失敗する。
verify_checks() {
  case "$1" in
    mofua-suki/issue-desk-intake) printf '%s\n' intake-verify ;;
    mofua-suki/issue-desk-worker) printf '%s\n' go-test go-vet gofmt ;;
    *) return 1 ;;
  esac
}

# 検証の名前 → 表示用の固定コマンド。
verify_label() {
  case "$1" in
    intake-verify) printf '%s' 'sh scripts/verify.sh' ;;
    go-test) printf '%s' 'go test ./...' ;;
    go-vet) printf '%s' 'go vet ./...' ;;
    gofmt) printf '%s' 'gofmt -l .（出力が空であること）' ;;
  esac
}

# scripts/verify.sh と同じく、利用者が入れた ~/.local/bin の道具を優先する。
verify_tool() {
  if [ -x "$HOME/.local/bin/$1" ]; then printf '%s' "$HOME/.local/bin/$1"; else printf '%s' "$1"; fi
}

# 検証のスクリプト（scripts/verify.sh）を使えるか。リンクでない通常のファイルのときだけ使う。
# 実行するかどうかと、同一性の検証定義に内容を含めるかどうかを、この1つの判定で揃える
# （リンクを実行すると、リンク先の中身が検証定義にも内容にも入らないまま差し替えられる）。
verify_script_usable() {
  [ -f scripts/verify.sh ] && [ ! -L scripts/verify.sh ]
}

# 検証を1つ実行する。VERIFY_CODE と VERIFY_OUT に結果を置く。実行できなければ 1 を返す。
verify_run() {
  local tool
  VERIFY_CODE=0
  VERIFY_OUT=
  case "$1" in
    intake-verify)
      verify_script_usable && verify_has sh || return 1
      VERIFY_OUT=$("${VERIFY_CLEAN[@]}" sh scripts/verify.sh 2>&1 </dev/null) || VERIFY_CODE=$?
      ;;
    go-test | go-vet)
      tool=$(verify_tool go)
      verify_has "$tool" || return 1
      VERIFY_OUT=$("${VERIFY_CLEAN[@]}" "$tool" "${1#go-}" ./... 2>&1 </dev/null) || VERIFY_CODE=$?
      ;;
    gofmt)
      tool=$(verify_tool gofmt)
      verify_has "$tool" || return 1
      VERIFY_OUT=$("${VERIFY_CLEAN[@]}" "$tool" -l . 2>&1 </dev/null) || VERIFY_CODE=$?
      # 終了コードが0でも、未整形のファイル名などの出力があれば不合格にする。
      if [ "$VERIFY_CODE" -eq 0 ] && [ -n "$VERIFY_OUT" ]; then VERIFY_CODE=1; fi
      ;;
    *) return 1 ;;
  esac
  return 0
}

# 失敗テスト名として表示してよいか。長さ・文字の種類を限り、秘密らしい長い英数字の並びを含むものは通さない。
verify_name_safe() {
  local name=$1 ascii rest run
  local bad='*[!A-Za-z0-9 _./#:,+()-]*'
  local run_re='[A-Za-z0-9+_-]{32,}'
  [ -n "$name" ] && [ "${#name}" -le 200 ] || return 1
  ascii=${name//[![:ascii:]]/}
  case "$ascii" in $bad) return 1 ;; esac
  rest=$ascii
  while [[ $rest =~ $run_re ]]; do
    run=${BASH_REMATCH[0]}
    [[ $run =~ [0-9] ]] && return 1
    rest=${rest#*"$run"}
  done
  return 0
}

# VERIFY_OUT から失敗テスト名を取り出す（go test の「--- FAIL:」、node --test の「not ok」「✖」）。
# VERIFY_NAMES（安全な名前・重複なし・最大10件）、VERIFY_NAMES_MORE（11件目以降の数）、VERIFY_NAMES_UNSAFE（表示しない名前の数）に置く。
verify_failed_names() {
  local line name seen
  local go_re='^[[:space:]]*---[[:space:]]FAIL:[[:space:]]+([^[:space:]]+)'
  local tap_re='^[[:space:]]*not[[:space:]]ok[[:space:]]+[0-9]+[[:space:]]+-[[:space:]]+(.*)$'
  local spec_re='^[[:space:]]*✖[[:space:]]+(.*)$'
  local time_re='^(.*)[[:space:]]\([0-9.]+m?s\)$'
  local directive_re='[[:space:]]#[[:space:]]*(TODO|SKIP|todo|skip)'
  VERIFY_NAMES=()
  VERIFY_NAMES_MORE=0
  VERIFY_NAMES_UNSAFE=0
  while IFS= read -r line; do
    if [[ $line =~ $go_re ]]; then
      name=${BASH_REMATCH[1]}
    elif [[ $line =~ $tap_re ]]; then
      name=${BASH_REMATCH[1]}
      [[ $name =~ $directive_re ]] && continue
    elif [[ $line =~ $spec_re ]]; then
      name=${BASH_REMATCH[1]}
      # node --test の失敗一覧の見出し行はテスト名ではない。
      [ "$name" = 'failing tests:' ] && continue
    else
      continue
    fi
    if [[ $name =~ $time_re ]]; then name=${BASH_REMATCH[1]}; fi
    if ! verify_name_safe "$name"; then
      VERIFY_NAMES_UNSAFE=$((VERIFY_NAMES_UNSAFE + 1))
      continue
    fi
    for seen in "${VERIFY_NAMES[@]}"; do
      [ "$seen" = "$name" ] && continue 2
    done
    if [ "${#VERIFY_NAMES[@]}" -ge 10 ]; then
      VERIFY_NAMES_MORE=$((VERIFY_NAMES_MORE + 1))
      continue
    fi
    VERIFY_NAMES+=("$name")
  done <<<"$VERIFY_OUT"
}

# 失敗した検証1つ分の安全な失敗情報を出す。検証の出力そのものは出さない。
verify_report_failure() {
  local name
  printf '失敗した検証: %s（終了コード %s）\n' "$(verify_label "$1")" "$VERIFY_CODE"
  verify_failed_names
  if [ "${#VERIFY_NAMES[@]}" -eq 0 ]; then
    printf '%s\n' '失敗テスト名: 取得できない（検証の出力から安全に表示できる失敗テスト名を取り出せなかった。上の検証を実行して確かめる）'
  else
    printf '%s\n' '失敗テスト名（最大10件）:'
    for name in "${VERIFY_NAMES[@]}"; do printf -- '- %s\n' "$name"; done
    if [ "$VERIFY_NAMES_MORE" -gt 0 ]; then printf 'ほか %s 件は表示しない\n' "$VERIFY_NAMES_MORE"; fi
  fi
  if [ "$VERIFY_NAMES_UNSAFE" -gt 0 ]; then printf '安全に表示できない名前 %s 件は表示しない\n' "$VERIFY_NAMES_UNSAFE"; fi
}

verify_main() {
  local key checks check root reason failed=0 unrunnable=0
  if ! verify_startup_clean; then
    builtin printf '%s\n' '必須検証（段1）: 実行不能。ガードの起動環境に BASH_ENV・SHELLOPTS・BASHOPTS または取り込まれたシェル関数があり、検証を差し替え得るため実行しない。これらを外して実行する。'
    return 3
  fi
  shift
  if [ "$#" -ne 0 ]; then
    printf '%s\n' '必須検証（段1）: 拒否。引数は受け付けない（検証コマンドの指定・結果の申告・フラグでは合格にならない）。作業場所で引数なしの verify を実行する。'
    return 2
  fi
  # 中断（INT・TERM・HUP）は verify_on_signal が受け、記録を無効にして終わる（下の「検証の開始・終了と記録の接続」）。
  trap verify_on_signal INT TERM HUP
  if ! verify_clean_env; then
    printf '%s\n' '必須検証（段1）: 実行不能。環境値を外して実行するための env が無い。'
    return 3
  fi
  # このガード自身の場所（同一性の「フック」に使う）。作業場所の最上位へ移る前に、絶対パスにして覚える。
  VERIFY_SELF=${BASH_SOURCE[0]}
  [[ $VERIFY_SELF == /* ]] || VERIFY_SELF=$PWD/$VERIFY_SELF
  if ! root=$("${VERIFY_CLEAN[@]}" git rev-parse --show-toplevel 2>/dev/null </dev/null) || [ -z "$root" ] || ! cd -- "$root" 2>/dev/null; then
    printf '%s\n' '必須検証（段1）: 実行不能。Gitの作業場所を確認できない。'
    return 3
  fi
  VERIFY_RUN_ROOT=$root
  if ! key=$(verify_repo_key); then
    printf '%s\n' '必須検証（段1）: 実行不能。origin からリポジトリを特定できない。'
    verify_invalidate_all "$root" || printf '記録: この作業場所の旧い記録を無効にできない（%s）。\n' "$VERIFY_STORE_ERR"
    return 3
  fi
  if ! checks=$(verify_checks "$key"); then
    printf '%s\n' '必須検証（段1）: 拒否。必須検証の対応表に無いリポジトリのため、検証済みにできない。'
    verify_invalidate_all "$root" || printf '記録: この作業場所の旧い記録を無効にできない（%s）。\n' "$VERIFY_STORE_ERR"
    return 2
  fi
  # 開始: 旧い成功を無効にし、この検証の世代を記録する。できなければ検証を実行しない。
  if ! verify_begin "$root" "$key"; then
    printf '必須検証（段1）: 実行不能 repo=%s。検証の記録を開始できない（%s）。旧い成功を無効にして世代を記録できるまで、検証を実行しない。\n' "$key" "$VERIFY_STORE_ERR"
    return 3
  fi
  # 同一性: 何を検証するかを、検証を実行する前に読んで覚える。読めなければ検証を実行しない。
  if ! verify_identity_begin; then
    reason=$VERIFY_STORE_ERR
    verify_finish invalid || printf '記録: この検証の結果を保存できない（%s）。\n' "$VERIFY_STORE_ERR"
    printf '必須検証（段1）: 実行不能 repo=%s。検証の対象の同一性を記録できない（%s）。何を検証したかを記録できるまで、検証を実行しない。\n' "$key" "$reason"
    return 3
  fi
  for check in $checks; do
    if ! verify_run "$check"; then
      unrunnable=1
      VERIFY_RUN_RESULTS+=${VERIFY_RUN_RESULTS:+,}$check:x
      printf '実行できない検証: %s（必要なコマンドまたはファイルが無い）\n' "$(verify_label "$check")"
    else
      VERIFY_RUN_RESULTS+=${VERIFY_RUN_RESULTS:+,}$check:$VERIFY_CODE
      if [ "$VERIFY_CODE" -ne 0 ]; then
        failed=1
        verify_report_failure "$check"
      fi
    fi
    VERIFY_OUT=
  done
  # 終了: 最新の世代のときだけ結果を保存する。成功は、保存できたときだけ合格にする。
  if [ "$failed" -ne 0 ]; then
    verify_finish failed || printf '記録: この検証の結果を保存できない（%s）。\n' "$VERIFY_STORE_ERR"
    printf '必須検証（段1）: 不合格 repo=%s。変更と無関係な失敗・修正前からの失敗も免除しない。\n' "$key"
    return 1
  fi
  if [ "$unrunnable" -ne 0 ]; then
    verify_finish invalid || printf '記録: この検証の結果を保存できない（%s）。\n' "$VERIFY_STORE_ERR"
    printf '必須検証（段1）: 実行不能 repo=%s。必須の検証をすべて実行できるまで合格にしない。\n' "$key"
    return 3
  fi
  # 検証後の照合: 始めたときと同じ対象のままと確かめられたときだけ、成功を保存する。
  if ! verify_identity_same; then
    reason=$VERIFY_STORE_ERR
    verify_finish invalid || printf '記録: この検証の結果を保存できない（%s）。\n' "$VERIFY_STORE_ERR"
    printf '必須検証（段1）: 実行不能 repo=%s。検証は成功したが、%s。検証した対象と今の対象が同じと確かめられない成功は合格にしない。\n' "$key" "$reason"
    return 3
  fi
  if ! verify_finish passed; then
    printf '必須検証（段1）: 実行不能 repo=%s。検証は成功したが結果を記録できない（%s）。記録できない成功は合格にしない。\n' "$key" "$VERIFY_STORE_ERR"
    return 3
  fi
  printf '必須検証（段1）: 合格 repo=%s。結果を世代 %s として記録し、検証の前後で対象が同じことを確かめた。この記録だけでは送信の許可にならない（送信直前の照合は未完成）。\n' "$key" "$VERIFY_RUN_GEN"
  return 0
}

# ---- 必須検証の記録・ロックの操作基盤（段2/6 前半） ----
# 呼び方: リポジトリの作業場所で bash ~/.claude/hooks/guard-gitea.sh verify-store （引数・標準入力は受け取らない）
# 保存先・ロック・記録を安全に操作できるかを点検するだけで、検証は実行しない。verify はこの基盤で結果を記録する
# （下の「検証の開始・終了と記録の接続」）。フックはまだ記録を読まず、記録は送信の許可にならない。
# 終了コード: 0=点検に成功 2=拒否（引数あり・対応表に無いリポジトリ） 3=実行不能（保存先・ロック・記録を安全に扱えない）。
# 保存先: <基点>/guard-gitea/verify/<作業場所の最上位のパスを % と / だけ置き換えた名前>/ （作業場所ごとに1つ）。
#   基点は /run/user/<実行者の番号>（リンクでないディレクトリ・実行者の所有・権限700のとき。再起動で消える）、
#   そうでなければ $HOME/.local/state。環境値からは選ばない。
#   guard-gitea 以下のディレクトリは実行者の所有・権限700・リンクでないこと、記録は実行者の所有・権限600・通常のファイル・
#   ハードリンクなしであることを、使うたびに確かめる。1つでも外れれば失敗を返し、成功として扱わない。
# 記録の形（7行固定）: 「guard-gitea-verify 3」「state=語」「generation=数」「repo=owner/repo」「run=書いた実行の識別子」
#   「identity=同一性の要約か -」「results=各検証の結果（名前:終了コード をカンマ区切り。実行できなければ x）か -」。
#   read と正規表現だけで読み、中身をコードとして評価しない。形が違えば失敗を返す。成功（passed）は同一性の要約が
#   あり結果がすべて 0 のときだけ正しい形とする。
#   旧い版（版1は4行、版2は run= までの5行）は読めるが、どの実行の記録とも一致せず、書くときは必ず版3にする。
# ロック: 保存先の lock ディレクトリ（mkdir の成否で排他）。中の owner に持ち主のプロセス番号を書く。約2秒待っても取れなければ失敗を返す。
#   残ったロックを自動では奪わない（持ち主が生きているかを安全に判定できないため）。
# 各関数は失敗すると 0 以外を返し、VERIFY_STORE_ERR に固定の理由を置く。

VERIFY_STORE=
VERIFY_LOCK=
VERIFY_STORE_ERR=

# 固定の理由を置いて失敗を返す。「条件 || verify_store_fail 理由 || return 1」の形で使う。
verify_store_fail() {
  VERIFY_STORE_ERR=$1
  return 1
}

# この実行の識別子（32桁の16進数）を1回だけ作って VERIFY_RUN_ID に置く。記録に書き、終了時に自分の記録かを照合する
# （記録が消えて世代が同じ番号に戻っても、別の実行の記録を自分のものと取り違えない）。
# 環境値からは読まず、bash の乱数（SRANDOM。bash 5.1 以降）から作る。作れなければ失敗を返す。
VERIFY_RUN_ID=
verify_run_id() {
  local LC_ALL=C
  local a b c d num_re='^[0-9]{1,10}$'
  [ -z "$VERIFY_RUN_ID" ] || return 0
  # 5.1 より前の bash では SRANDOM がただの変数で、環境値がそのまま入り得る。
  [[ ${BASH_VERSINFO[0]} =~ $num_re && ${BASH_VERSINFO[1]} =~ $num_re ]] &&
    { [ "${BASH_VERSINFO[0]}" -gt 5 ] || { [ "${BASH_VERSINFO[0]}" -eq 5 ] && [ "${BASH_VERSINFO[1]}" -ge 1 ]; }; } ||
    verify_store_fail '実行ごとの識別子を作れない（bash 5.1 以降が要る）' || return 1
  a=$SRANDOM b=$SRANDOM c=$SRANDOM d=$SRANDOM
  [[ $a =~ $num_re && $b =~ $num_re && $c =~ $num_re && $d =~ $num_re ]] && ! { [ "$a" = "$b" ] && [ "$b" = "$c" ] && [ "$c" = "$d" ]; } ||
    verify_store_fail '実行ごとの識別子を作れない（乱数を得られない）' || return 1
  printf -v VERIFY_RUN_ID '%08x%08x%08x%08x' "$a" "$b" "$c" "$d"
}

# 許可した環境値だけで基本の道具（id・ls・mkdir・mv・rm・rmdir・sleep）を呼ぶ。
# 道具は中断（INT・TERM・HUP）を無視して動かす。端末の Ctrl+C はプロセスの組の全員に届くので、無視しないと
# ロックの解放や記録の置き換えの途中で道具だけが止まり、ロックや一時ファイルが残る。中断はガード自身が受けて後始末する。
verify_fs() {
  (
    trap '' INT TERM HUP
    exec "${VERIFY_CLEAN[@]}" "$@"
  ) 2>/dev/null </dev/null
}

# ls -ldn の先頭から、種類と権限（VERIFY_MODE、10文字）・リンク数（VERIFY_LINKS）・所有者の番号（VERIFY_UID）・
# 大きさ（VERIFY_SIZE、バイト数）を読む。
verify_stat() {
  local LC_ALL=C # 数字の照合を ASCII の 0-9 だけにする。
  local out mode links uid gid size rest
  local num_re='^[0-9]+$'
  out=$(verify_fs ls -ldn -- "$1") || return 1
  read -r mode links uid gid size rest <<<"$out"
  VERIFY_MODE=${mode:0:10}
  VERIFY_LINKS=$links
  VERIFY_UID=$uid
  VERIFY_SIZE=$size
  [[ $links =~ $num_re && $uid =~ $num_re && $size =~ $num_re ]]
}

# 保存先のディレクトリとして安全か（リンクでない・実行者の所有・読み書きできる・権限700）。
verify_dir_safe() {
  [ ! -L "$1" ] || verify_store_fail '保存先がシンボリックリンクになっている' || return 1
  [ -d "$1" ] || verify_store_fail '保存先がディレクトリでない' || return 1
  verify_stat "$1" || verify_store_fail '保存先の所有者と権限を読めない' || return 1
  [ -O "$1" ] && [ "$VERIFY_UID" = "$EUID" ] || verify_store_fail '保存先の所有者が実行者と違う' || return 1
  [ -r "$1" ] && [ -w "$1" ] && [ -x "$1" ] || verify_store_fail '保存先を読み書きできない' || return 1
  [ "$VERIFY_MODE" = 'drwx------' ] || verify_store_fail '保存先の権限が所有者だけ（700）になっていない' || return 1
}

# 記録のファイルとして安全か（リンクでない・通常のファイル・実行者の所有・読める・権限600・ハードリンクなし）。
verify_file_safe() {
  [ ! -L "$1" ] || verify_store_fail '記録がシンボリックリンクになっている' || return 1
  [ -f "$1" ] || verify_store_fail '記録が通常のファイルでない' || return 1
  verify_stat "$1" || verify_store_fail '記録の所有者と権限を読めない' || return 1
  [ -O "$1" ] && [ "$VERIFY_UID" = "$EUID" ] || verify_store_fail '記録の所有者が実行者と違う' || return 1
  [ -r "$1" ] || verify_store_fail '記録を読めない' || return 1
  [ "$VERIFY_MODE" = '-rw-------' ] || verify_store_fail '記録の権限が所有者だけ（600）になっていない' || return 1
  [ "$VERIFY_LINKS" = 1 ] || verify_store_fail '記録にハードリンクがある' || return 1
}

# 基点の候補（$1）を使えるか（途中も含めてリンクでないディレクトリ・実行者の所有・読み書きできる・権限ちょうど700）。
# 使えないときは失敗を返すだけで、理由は置かない（呼び出し側が $HOME/.local/state を基点にする）。
verify_store_base() {
  local phys
  [ ! -L "$1" ] && [ -d "$1" ] && verify_stat "$1" && [ -O "$1" ] && [ "$VERIFY_UID" = "$EUID" ] &&
    [ "$VERIFY_MODE" = 'drwx------' ] && [ -r "$1" ] && [ -w "$1" ] && [ -x "$1" ] &&
    phys=$(cd -P -- "$1" 2>/dev/null && pwd -P) && [ "$phys" = "$1" ]
}

# 作業場所の最上位（$1）の保存先を用意して確かめ、VERIFY_STORE に置く。無ければ権限700で作る。
# 基点は /run/user/<id -u の番号> が使えればそこ、使えなければ $HOME/.local/state。どちらでも guard-gitea 以下の検査は同じ。
verify_store_open() {
  verify_store_key "$1" && verify_base_pick && verify_store_at "$VERIFY_BASE" 1 || return 1
}

# 作業場所の最上位（$1）から保存先の名前を作り、VERIFY_KEY に置く。
verify_store_key() {
  local LC_ALL=C # 名前の長さをバイト数で数える。
  local root=$1 key
  VERIFY_KEY=
  VERIFY_STORE=
  [[ $root == /* && $root != *[[:cntrl:]]* ]] || verify_store_fail '作業場所のパスを保存先の名前にできない' || return 1
  key=${root//%/%25}
  key=${key//\//%2F}
  [ "${#key}" -le 200 ] || verify_store_fail '作業場所のパスが長すぎて保存先の名前にできない' || return 1
  VERIFY_KEY=$key
}

# HOME 側の基点（$HOME/.local/state）を確かめて VERIFY_BASE に置く。$1 が 1 なら無い階層を作る。
# 1 でなければ何も作らず、HOME 側の基点が無いときは 2 を返す。
verify_base_home() {
  local part home phys
  VERIFY_BASE=
  if ! { [[ ${HOME-} == /* ]] && [ -d "$HOME" ]; }; then
    [ "$1" = 1 ] || return 2
    verify_store_fail 'HOME が絶対パスのディレクトリでない'
    return 1
  fi
  # $HOME より下の途中（.local・state）を1段ずつ、リンクでないディレクトリと確かめながら作る。リンクなら何も作らない。
  for part in "$HOME/.local" "$HOME/.local/state"; do
    [ ! -L "$part" ] || verify_store_fail '保存先の途中にリンクがある' || return 1
    if [ ! -e "$part" ]; then
      [ "$1" = 1 ] || return 2
      verify_fs mkdir -- "$part" || [ -d "$part" ] || verify_store_fail '保存先を作れない' || return 1
    fi
    [ ! -L "$part" ] && [ -d "$part" ] || verify_store_fail '保存先を作れない' || return 1
  done
  home=$(cd -P -- "$HOME" 2>/dev/null && pwd -P) && phys=$(cd -P -- "$HOME/.local/state" 2>/dev/null && pwd -P) &&
    [ "$phys" = "${home%/}/.local/state" ] || verify_store_fail '保存先の途中にリンクがある' || return 1
  # 基点は実行者の所有であること（権限は確かめない。ACL付きの /home では700にならないため）。
  verify_stat "$phys" && [ -O "$phys" ] && [ "$VERIFY_UID" = "$EUID" ] || verify_store_fail '保存先の基点の所有者が実行者と違う' || return 1
  VERIFY_BASE=$phys
}

# 今の基点を選んで VERIFY_BASE に、種類（run か home）を VERIFY_BASE_KIND に置く。
# 基点の候補は環境値（XDG_RUNTIME_DIR など）から読まず、実行者の番号（id -u と bash の EUID が同じとき）だけで決める。
verify_base_pick() {
  local LC_ALL=C
  local uid num_re='^[0-9]+$'
  uid=$(verify_fs id -u) || uid=
  if [[ $uid =~ $num_re ]] && [ "$uid" = "$EUID" ] && verify_store_base "/run/user/$uid"; then
    VERIFY_BASE=/run/user/$uid
    VERIFY_BASE_KIND=run
    return 0
  fi
  VERIFY_BASE_KIND=home
  verify_base_home 1
}

# 基点（$1）の下の、VERIFY_KEY の保存先を確かめて VERIFY_STORE に置く。$2 が 1 なら無い階層を権限700で作る。
# 1 でなければ何も作らず、保存先が無いときは 2 を返す。
verify_store_at() {
  local part
  VERIFY_STORE=
  [ -n "$VERIFY_KEY" ] || verify_store_fail '保存先の名前を決めていない' || return 1
  for part in guard-gitea guard-gitea/verify "guard-gitea/verify/$VERIFY_KEY"; do
    if [ ! -e "$1/$part" ] && [ ! -L "$1/$part" ]; then
      [ "$2" = 1 ] || return 2
      # 同時に作られた場合は mkdir が失敗するので、下の検査で確かめる。
      verify_fs mkdir -m 700 -- "$1/$part" || [ -d "$1/$part" ] || verify_store_fail '保存先を作れない' || return 1
    fi
    verify_dir_safe "$1/$part" || return 1
  done
  VERIFY_STORE=$1/guard-gitea/verify/$VERIFY_KEY
}

# ロックを取る。取れたら VERIFY_LOCK にロックのパスを置く。
verify_lock_acquire() {
  local lock=$VERIFY_STORE/lock try=0
  [ -n "$VERIFY_STORE" ] && [ -z "$VERIFY_LOCK" ] || verify_store_fail 'ロックを取る前提（保存先の確認済み・未取得）を満たしていない' || return 1
  while :; do
    [ ! -L "$lock" ] || verify_store_fail 'ロックがシンボリックリンクになっている' || return 1
    verify_fs mkdir -m 700 -- "$lock" && break
    # mkdir の失敗後に別の実行がロックを解放する場合がある。消えた場合も回数を制限して再試行する。
    [ ! -L "$lock" ] || verify_store_fail 'ロックがシンボリックリンクになっている' || return 1
    if [ -e "$lock" ] && [ ! -d "$lock" ]; then
      verify_store_fail 'ロックを作れない' || return 1
    fi
    try=$((try + 1))
    if [ "$try" -ge 20 ]; then
      verify_store_fail 'ロックを取得できない（ほかの検証が記録を操作しているか、中断で残ったロックがある）' || return 1
    fi
    verify_fs sleep 0.1 || verify_store_fail 'ロックを取得できない（待機に失敗）' || return 1
  done
  if ! (set -C && umask 077 && printf '%s\n' "$$" >"$lock/owner") 2>/dev/null; then
    # このロックは上の mkdir で自分が作ったものなので、書きかけの owner ごと消してから戻る。
    verify_fs rm -f -- "$lock/owner"
    verify_fs rmdir -- "$lock"
    verify_store_fail 'ロックの持ち主を書き込めない'
    return 1
  fi
  VERIFY_LOCK=$lock
}

# 自分が取ったロックを解放する。持ち主が自分でなくなっていれば消さずに失敗を返す。
verify_lock_release() {
  local lock=$VERIFY_LOCK owner=
  [ -n "$lock" ] || verify_store_fail 'ロックを持っていない' || return 1
  VERIFY_LOCK=
  [ ! -L "$lock" ] && [ -d "$lock" ] && [ ! -L "$lock/owner" ] && [ -f "$lock/owner" ] &&
    { IFS= read -r owner <"$lock/owner"; } 2>/dev/null && [ "$owner" = "$$" ] ||
    verify_store_fail 'ロックが自分のものでなくなっている' || return 1
  verify_fs rm -f -- "$lock/owner" && verify_fs rmdir -- "$lock" || verify_store_fail 'ロックを解放できない' || return 1
}

# 記録の値が決まった形か。$1=state $2=generation $3=repo $4=identity（同一性の要約か -） $5=results（各検証の結果か -）。
# 成功（passed）は、同一性の要約があり、各検証の結果がすべて 0 のときだけ決まった形とする。
verify_record_valid() {
  local LC_ALL=C # 呼び出し元のロケールに依らず、ASCII の文字だけに一致させる（全角の数字などを通さない）。
  local state_re='^(passed|failed|invalid|running|probe)$'
  local gen_re='^(0|[1-9][0-9]{0,17})$'
  local repo_re='^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$'
  local id_re='^(-|[0-9a-f]{40}|[0-9a-f]{64})$'
  local results_re='^(-|[a-z][a-z0-9-]*:([0-9]{1,3}|x)(,[a-z][a-z0-9-]*:([0-9]{1,3}|x))*)$'
  local zero_re='^[a-z][a-z0-9-]*:0(,[a-z][a-z0-9-]*:0)*$'
  [[ $1 =~ $state_re && $2 =~ $gen_re && $3 =~ $repo_re && $4 =~ $id_re && $5 =~ $results_re ]] &&
    [ "${#3}" -le 200 ] && [ "${#5}" -le 100 ] || return 1
  [ "$1" != passed ] || [[ $4 != - && $5 =~ $zero_re ]]
}

# 保存先の記録（$1 は record か probe）を読み、VERIFY_REC_STATE・VERIFY_REC_GEN・VERIFY_REC_REPO に置く。
# 戻り値: 0=読めた 1=記録が無い 2=安全に読めない（リンク・所有・権限の不備、読取不能、形の不正）。0 以外を成功として扱わない。
verify_record_read() {
  local LC_ALL=C # 長さをバイト数で数える。
  local file=$VERIFY_STORE/$1 head= state= gen= repo= run= identity= results= extra= body= size= run_re='^run=[0-9a-f]{32}$'
  VERIFY_REC_VERSION=
  VERIFY_REC_STATE=
  VERIFY_REC_GEN=
  VERIFY_REC_REPO=
  VERIFY_REC_RUN=
  VERIFY_REC_IDENTITY=
  VERIFY_REC_RESULTS=
  [ -n "$VERIFY_STORE" ] || verify_store_fail '保存先を確かめていない' || return 2
  if [ ! -e "$file" ] && [ ! -L "$file" ]; then
    verify_store_fail '記録が無い'
    return 1
  fi
  verify_file_safe "$file" || return 2
  # 正規の7行が取り得る大きさ（最大491バイト）を超える記録は読まない。
  size=$VERIFY_SIZE
  [ "${#size}" -le 3 ] && [ "$size" -le 491 ] || verify_store_fail '記録が大きすぎる' || return 2
  # 版3は7行（run=識別子、identity=同一性の要約、results=各検証の結果）。旧い版（版1は4行、版2は run= までの5行）は読めるが、
  # 同一性を持たないので VERIFY_REC_VERSION で区別し、この実行の記録としても合格の記録としても扱わない。
  {
    IFS= read -r head && IFS= read -r state && IFS= read -r gen && IFS= read -r repo &&
      { { [ "$head" != 'guard-gitea-verify 2' ] && [ "$head" != 'guard-gitea-verify 3' ]; } || IFS= read -r run; } &&
      { [ "$head" != 'guard-gitea-verify 3' ] || { IFS= read -r identity && IFS= read -r results; }; } &&
      ! IFS= read -r extra && [ -z "$extra" ]
  } 2>/dev/null <"$file" || verify_store_fail '記録を読めないか、行の数が決まった形でない' || return 2
  case "$head" in
    'guard-gitea-verify 1') identity='identity=-' results='results=-' ;;
    'guard-gitea-verify 2') [[ $run =~ $run_re ]] && identity='identity=-' results='results=-' || head= ;;
    'guard-gitea-verify 3') [[ $run =~ $run_re ]] && [ "${identity%%=*}" = identity ] && [ "${results%%=*}" = results ] || head= ;;
    *) head= ;;
  esac
  # 旧い版は同一性を持たないので、成功（passed）でも形の検査は同一性なしで通す（合格として扱うかは版で決める）。
  [ -n "$head" ] && [ "${state%%=*}" = state ] && [ "${gen%%=*}" = generation ] && [ "${repo%%=*}" = repo ] &&
    if [ "$head" = 'guard-gitea-verify 3' ]; then
      verify_record_valid "${state#*=}" "${gen#*=}" "${repo#*=}" "${identity#*=}" "${results#*=}"
    else
      verify_record_valid "${state#*=}" "${gen#*=}" "${repo#*=}" 0000000000000000000000000000000000000000 a:0
    fi || verify_store_fail '記録の中身が決まった形でない' || return 2
  # read は NUL を落とすので、読んだ行を組み直したバイト数がファイルの大きさと同じことを確かめる。
  printf -v body '%s\n%s\n%s\n%s\n' "$head" "$state" "$gen" "$repo"
  [ -z "$run" ] || body+=$run$'\n'
  [ "$head" != 'guard-gitea-verify 3' ] || body+=$identity$'\n'$results$'\n'
  [ "${#body}" -eq "$size" ] || verify_store_fail '記録に読み取れないバイトがある' || return 2
  VERIFY_REC_VERSION=${head##* }
  VERIFY_REC_STATE=${state#*=}
  VERIFY_REC_GEN=${gen#*=}
  VERIFY_REC_REPO=${repo#*=}
  VERIFY_REC_RUN=${run#*=}
  VERIFY_REC_IDENTITY=${identity#*=}
  VERIFY_REC_RESULTS=${results#*=}
}

# 記録（$1 は record か probe）を書く。$2=state $3=generation $4=repo $5=identity（省けば -） $6=results（省けば -）。
# ロックを持っているときだけ書ける。同じ保存先の一時ファイルへ書いてから置き換え、読み戻して一致を確かめる。
verify_record_write() {
  local file=$VERIFY_STORE/$1 tmp=$VERIFY_STORE/$1.tmp.$$ identity=${5:--} results=${6:--}
  [ -n "$VERIFY_STORE" ] && [ -n "$VERIFY_LOCK" ] || verify_store_fail 'ロックを持たずに記録を書こうとした' || return 1
  verify_record_valid "$2" "$3" "$4" "$identity" "$results" || verify_store_fail '書こうとした記録の中身が決まった形でない' || return 1
  verify_run_id || return 1
  verify_dir_safe "$VERIFY_STORE" || return 1
  if [ -e "$file" ] || [ -L "$file" ]; then verify_file_safe "$file" || return 1; fi
  # 同じ名前の一時ファイルが前からあれば自分の物ではないので、触らずに失敗を返す。
  [ ! -e "$tmp" ] && [ ! -L "$tmp" ] || verify_store_fail '記録を書き込めない' || return 1
  if ! (set -C && umask 077 && printf 'guard-gitea-verify 3\nstate=%s\ngeneration=%s\nrepo=%s\nrun=%s\nidentity=%s\nresults=%s\n' "$2" "$3" "$4" "$VERIFY_RUN_ID" "$identity" "$results" >"$tmp") 2>/dev/null; then
    # 書きかけの一時ファイルは自分が作ったものなので消す。
    verify_fs rm -f -- "$tmp"
    verify_store_fail '記録を書き込めない'
    return 1
  fi
  if ! verify_fs mv -f -- "$tmp" "$file"; then
    verify_fs rm -f -- "$tmp"
    verify_store_fail '記録を置き換えられない'
    return 1
  fi
  verify_record_read "$1" && [ "$VERIFY_REC_STATE" = "$2" ] && [ "$VERIFY_REC_GEN" = "$3" ] && [ "$VERIFY_REC_REPO" = "$4" ] &&
    [ "$VERIFY_REC_RUN" = "$VERIFY_RUN_ID" ] && [ "$VERIFY_REC_VERSION" = 3 ] &&
    [ "$VERIFY_REC_IDENTITY" = "$identity" ] && [ "$VERIFY_REC_RESULTS" = "$results" ] ||
    verify_store_fail '保存した記録を読み戻せない' || return 1
}

# 記録（$1 は record か probe）を消す。ロックを持っているときだけ消せる。
verify_record_remove() {
  local file=$VERIFY_STORE/$1
  [ -n "$VERIFY_STORE" ] && [ -n "$VERIFY_LOCK" ] || verify_store_fail 'ロックを持たずに記録を消そうとした' || return 1
  verify_file_safe "$file" || return 1
  verify_fs rm -f -- "$file" && [ ! -e "$file" ] || verify_store_fail '記録を消せない' || return 1
}

# ---- 検証の開始・終了と記録の接続（段2/6 後半） ----
# verify は、開始時に記録を「実行中（running）・世代を1つ進めた番号・この実行の識別子」に書き換えて旧い成功を無効にし、
# 終了時に記録がまだこの実行のもの（世代・リポジトリ・識別子が一致）で実行中であるときだけ結果（passed・failed・invalid）を
# 保存する。後から始まった検証があれば世代と識別子が変わっているので、遅れて終わった旧い検証は保存できない（記録が消えて
# 世代が同じ番号に戻っても、識別子が違う）。開始・保存・無効化のどれかに失敗したら合格にしない。
# 基点は実行のたびに選ぶので、今の基点でない側（/run/user/<番号> と $HOME/.local/state のもう一方）に同じ作業場所の
# 記録があれば、開始時にそちらも無効（invalid・世代を1つ進める）にする。無効にできなければ検証を始めない。
# この記録はまだ送信の許可に使わない（同一性の記録と検証後の照合は下の段3、送信直前の照合は段4）。

VERIFY_RUN_ROOT=
VERIFY_RUN_BASE=
VERIFY_RUN_GEN=
VERIFY_RUN_REPO=
VERIFY_RUN_OTHER=
VERIFY_RUN_STARTED=
VERIFY_CRITICAL=
VERIFY_INTERRUPTED=
VERIFY_ABORTING=

# ロックを取って $@ を実行し、ロックを解放する。どれかが失敗すれば失敗を返す（理由は最初の失敗のもの）。
# ロックを持つ間に届いた中断は、解放した後に処理する。
verify_locked() {
  local rc=0 err=
  VERIFY_CRITICAL=1
  if verify_lock_acquire; then
    "$@" || rc=1
    err=$VERIFY_STORE_ERR
    if ! verify_lock_release && [ "$rc" -eq 0 ]; then
      rc=1
      err=$VERIFY_STORE_ERR
    fi
    VERIFY_STORE_ERR=$err
  else
    rc=1
  fi
  if [ -z "$VERIFY_ABORTING" ]; then
    VERIFY_CRITICAL=
    [ -z "$VERIFY_INTERRUPTED" ] || verify_abort
  fi
  return "$rc"
}

# ロックを持った状態で: 記録があれば世代を1つ進めて無効（invalid）にする。記録が無い・既に無効なら何もしない。
# 安全に読めない記録は失敗を返す（読めない記録は合格として扱われないが、無効にできたとは言えないため）。
verify_invalidate_locked() {
  local rc=0
  verify_record_read record || rc=$?
  [ "$rc" -ne 1 ] || return 0
  [ "$rc" -eq 0 ] || return 1
  # 既に無効の記録は書き換えない（世代だけが進むのを避ける）。
  [ "$VERIFY_REC_STATE" != invalid ] || return 0
  verify_record_write record invalid "$((VERIFY_REC_GEN + 1))" "$VERIFY_REC_REPO"
}

# 基点の種類（$1 は run か home）の側にある保存先を、作らずに確かめて VERIFY_STORE に置く。無ければ 2 を返す。
verify_store_side() {
  local rc=0
  if [ "$1" = run ]; then
    VERIFY_BASE=/run/user/$EUID
    [ ! -L "$VERIFY_BASE" ] && [ -d "$VERIFY_BASE" ] || return 2
  else
    verify_base_home 0 || return $?
  fi
  verify_store_at "$VERIFY_BASE" 0
}

# 基点の種類（$1）の側に保存先があれば、その記録を無効にする。無ければ何もしない。
verify_invalidate_side() {
  local rc=0
  verify_store_side "$1" || rc=$?
  [ "$rc" -ne 2 ] || return 0
  [ "$rc" -eq 0 ] || return 1
  verify_locked verify_invalidate_locked
}

# 検証を始められないとき（リポジトリを特定できない・対応表に無い）に、作業場所（$1）の両方の基点の記録を無効にする。
# 保存先は作らない。
verify_invalidate_all() {
  local rc=0 err=
  verify_store_key "$1" || return 1
  verify_invalidate_side run || { rc=1; err=$VERIFY_STORE_ERR; }
  verify_invalidate_side home || { rc=1; err=$VERIFY_STORE_ERR; }
  VERIFY_STORE_ERR=$err
  return "$rc"
}

# ロックを持った状態で: 記録を「実行中・世代を1つ進めた番号」に書き換える。旧い成功はここで無効になる。
verify_begin_locked() {
  local rc=0 gen=0
  verify_record_read record || rc=$?
  [ "$rc" -le 1 ] || return 1
  [ "$rc" -eq 1 ] || gen=$VERIFY_REC_GEN
  gen=$((gen + 1))
  # 中断されたときに無効にできるよう、書く前に自分の世代を覚える。
  VERIFY_RUN_BASE=$1
  VERIFY_RUN_GEN=$gen
  verify_record_write record running "$gen" "$VERIFY_RUN_REPO"
}

# 検証の開始: 作業場所（$1）・リポジトリ（$2）の記録を実行中にする。今の基点でない側の記録も無効にする。
verify_begin() {
  local base other=home
  VERIFY_RUN_BASE=
  VERIFY_RUN_GEN=
  VERIFY_RUN_REPO=$2
  verify_store_key "$1" && verify_base_pick || return 1
  base=$VERIFY_BASE
  [ "$VERIFY_BASE_KIND" = run ] || other=run
  VERIFY_RUN_OTHER=$other
  if ! verify_invalidate_side "$other"; then
    VERIFY_STORE_ERR="今の基点でない側の保存先にある記録を無効にできない（$VERIFY_STORE_ERR）"
    return 1
  fi
  verify_store_at "$base" 1 || return 1
  verify_locked verify_begin_locked "$base" || return 1
  VERIFY_RUN_STARTED=1
}

# ロックを持った状態で: 記録がこの実行のもの（世代・リポジトリ・識別子が一致）で実行中のままなら、結果（$1）を保存する。
# 中断の後始末（VERIFY_ABORTING）では、状態を問わずこの実行の記録を書き換える（保存した直後の成功も無効にする）。
verify_finish_locked() {
  local rc=0
  verify_record_read record || rc=$?
  if [ "$rc" -eq 1 ]; then verify_store_fail '開始時に書いた記録が無くなっている'; fi
  [ "$rc" -eq 0 ] || return 1
  { [ "$VERIFY_REC_STATE" = running ] || [ -n "$VERIFY_ABORTING" ]; } && [ "$VERIFY_REC_GEN" = "$VERIFY_RUN_GEN" ] &&
    [ "$VERIFY_REC_REPO" = "$VERIFY_RUN_REPO" ] && [ -n "$VERIFY_RUN_ID" ] && [ "$VERIFY_REC_RUN" = "$VERIFY_RUN_ID" ] &&
    [ "$VERIFY_REC_VERSION" = 3 ] ||
    verify_store_fail '後から始まった検証があり、この検証は最新でない' || return 1
  # 結果と一緒に、検証を始めたときの同一性の要約と各検証の結果を記録する（成功は、検証後も同じと確かめた後にだけここへ来る）。
  if ! verify_record_write record "$1" "$VERIFY_RUN_GEN" "$VERIFY_RUN_REPO" "$VERIFY_RUN_IDENTITY_ID" "$VERIFY_RUN_RESULTS"; then
    # 成功を書く途中で失敗したときは、成功の記録が残らないよう無効を書き直してみる（結果は失敗のまま）。
    rc=$VERIFY_STORE_ERR
    [ "$1" != passed ] || verify_record_write record invalid "$VERIFY_RUN_GEN" "$VERIFY_RUN_REPO"
    VERIFY_STORE_ERR=$rc
    return 1
  fi
}

# 検証の終了: 結果（$1 は passed・failed・invalid）を保存する。最新の世代でなければ保存せずに失敗を返す。
verify_finish() {
  local rc=0
  [ -n "$VERIFY_RUN_BASE" ] && [ -n "$VERIFY_RUN_GEN" ] || verify_store_fail '検証の記録を開始していない' || return 1
  # 成功を保存する前に、今の基点でない側をもう一度無効にする（検証の間に、別の基点を選んだ検証が成功を書いた場合。
  # 互いに相手側を無効にしてから保存するので、両方の基点に成功が残ることはない）。
  if [ "$1" = passed ] && ! verify_invalidate_side "$VERIFY_RUN_OTHER"; then
    VERIFY_STORE_ERR="今の基点でない側の保存先にある記録を無効にできない（$VERIFY_STORE_ERR）"
    return 1
  fi
  verify_store_at "$VERIFY_RUN_BASE" 0 || rc=$?
  if [ "$rc" -eq 2 ]; then verify_store_fail '開始時に使った保存先が無くなっている'; fi
  [ "$rc" -eq 0 ] || return 1
  verify_locked verify_finish_locked "$1"
}

# ---- 同一性の記録と検証後の照合（段3/6） ----
# verify は、開始の記録の直後に「何を検証するか」（同一性）を読み、検証がすべて成功した後にもう一度読んで比べる。
# 同じと確かめられたときだけ成功を保存し、記録に同一性の要約（identity=）と各検証の結果（results=）を持たせる。
# 読めない・比べられない・違うときは成功にせず、記録を無効（invalid）にして実行不能で終わる。
# 同一性に含めるもの: リポジトリ、作業場所、管理場所（Gitのディレクトリと共有のディレクトリ）、先頭コミット、ブランチ、
#   送信先（origin の取得用・送信用のURL）、検証定義（対応表の検証と scripts/verify.sh の内容）、フック（このガード自身の内容）、
#   内容（先頭コミットに、作業場所の未コミットの変更と無視されていない未追跡ファイルを足した木）。
# 内容の木は、保存先に置く一時のインデックスで作る（作業場所のインデックスは変えない。Gitの保管庫に中身の控えが増えることはある）。
# 送信の候補の検査と送信直前の照合（段4）はまだ無く、フックはこの記録を読まない。

VERIFY_SELF=
VERIFY_IDENTITY=
VERIFY_IDENTITY_ID=
VERIFY_RUN_IDENTITY=
VERIFY_RUN_IDENTITY_ID=
VERIFY_RUN_RESULTS=

# 許可した環境値だけで Git を呼ぶ（標準入力なし・エラー出力は捨てる）。
verify_git() {
  "${VERIFY_CLEAN[@]}" git "$@" 2>/dev/null </dev/null
}

# 同一性の本体。作業場所の最上位（$1。カレントディレクトリでもある）・リポジトリ（$2）・一時のインデックスの置き場所（$3）。
# 内訳（1行1項目）を VERIFY_IDENTITY に、その要約を VERIFY_IDENTITY_ID に置く。1つでも読めなければ失敗を返す。
verify_identity_read() {
  local LC_ALL=C # 値の照合を ASCII の文字だけにする。
  local hex_re='^([0-9a-f]{40}|[0-9a-f]{64})$' nl=$'\n'
  local gitdir common head branch push fetch checks check definition= guard tree text id rc=0
  gitdir=$(verify_git rev-parse --absolute-git-dir) && [[ $gitdir == /* && $gitdir != *[[:cntrl:]]* ]] &&
    common=$(verify_git rev-parse --path-format=absolute --git-common-dir) && [[ $common == /* && $common != *[[:cntrl:]]* ]] ||
    verify_store_fail '管理場所（Gitのディレクトリ）を読めない' || return 1
  head=$(verify_git rev-parse --verify 'HEAD^{commit}') && [[ $head =~ $hex_re ]] ||
    verify_store_fail '先頭コミットを読めない' || return 1
  # ブランチ: 先頭がブランチを指していなければ（終了コード1・出力なし）その旨を値にする。ほかの失敗は読めない扱い。
  branch=$(verify_git symbolic-ref -q HEAD) || rc=$?
  if [ "$rc" -eq 1 ] && [ -z "$branch" ]; then branch='(detached)'; rc=0; fi
  [ "$rc" -eq 0 ] && [[ $branch == '(detached)' || $branch == refs/heads/?* ]] && [[ $branch != *[[:cntrl:]]* ]] ||
    verify_store_fail 'ブランチを読めない' || return 1
  fetch=$(verify_git remote get-url origin) && push=$(verify_git remote get-url --push --all origin) &&
    [ -n "$fetch" ] && [ -n "$push" ] || verify_store_fail '送信先（origin）を読めない' || return 1
  push=${push//"$nl"/ }
  [[ $fetch != *[[:cntrl:]]* && $push != *[[:cntrl:]]* ]] || verify_store_fail '送信先（origin）を読めない' || return 1
  # 検証定義: 対応表の検証の名前と固定コマンド。Intakeの検証は scripts/verify.sh の内容も含める（無ければその旨を値にする）。
  checks=$(verify_checks "$2") || verify_store_fail '検証定義を読めない' || return 1
  for check in $checks; do
    definition+="$check=$(verify_label "$check");"
    [ "$check" = intake-verify ] || continue
    if verify_script_usable; then
      id=$(verify_git hash-object --no-filters -- scripts/verify.sh) && [[ $id =~ $hex_re ]] ||
        verify_store_fail '検証定義（scripts/verify.sh）を読めない' || return 1
      definition+="scripts/verify.sh=$id;"
    else
      definition+='scripts/verify.sh=(none);'
    fi
  done
  [[ $VERIFY_SELF == /* ]] && [ -f "$VERIFY_SELF" ] && guard=$(verify_git hash-object --no-filters -- "$VERIFY_SELF") &&
    [[ $guard =~ $hex_re ]] || verify_store_fail 'フック（このガード自身）の内容を読めない' || return 1
  # 内容: 一時のインデックスへ先頭コミットを読み、作業場所の今の中身（無視されていない未追跡ファイルを含む）を重ねて木にする。
  # 置き場所に前から物が無いことは、呼び出し元（verify_identity）が確かめている。
  "${VERIFY_CLEAN[@]}" "GIT_INDEX_FILE=$3" git read-tree HEAD >/dev/null 2>&1 </dev/null &&
    "${VERIFY_CLEAN[@]}" "GIT_INDEX_FILE=$3" git add -A >/dev/null 2>&1 </dev/null &&
    tree=$("${VERIFY_CLEAN[@]}" "GIT_INDEX_FILE=$3" git write-tree 2>/dev/null </dev/null) && [[ $tree =~ $hex_re ]] ||
    verify_store_fail '作業場所の内容を読めない' || return 1
  printf -v text 'guard-gitea-identity 1\nrepo=%s\nworktree=%s\ngitdir=%s\ncommondir=%s\nhead=%s\nbranch=%s\nfetch=%s\npush=%s\ndefinition=%s\nguard=%s\ncontent=%s\n' \
    "$2" "$1" "$gitdir" "$common" "$head" "$branch" "$fetch" "$push" "$definition" "$guard" "$tree"
  id=$(printf '%s' "$text" | "${VERIFY_CLEAN[@]}" git hash-object --stdin 2>/dev/null) && [[ $id =~ $hex_re ]] ||
    verify_store_fail '同一性の要約を作れない' || return 1
  VERIFY_IDENTITY=$text
  VERIFY_IDENTITY_ID=$id
}

# 同一性を読む。保存先（VERIFY_RUN_BASE の側）に一時のインデックスを置き、読んだ後に必ず消す。
# 読む間に届いた中断は、一時ファイルを消した後に処理する。
verify_identity() {
  local rc=0 err= tmp
  VERIFY_IDENTITY=
  VERIFY_IDENTITY_ID=
  VERIFY_CRITICAL=1
  if verify_store_at "$VERIFY_RUN_BASE" 0; then
    tmp=$VERIFY_STORE/identity.$$
    if [ -e "$tmp" ] || [ -L "$tmp" ] || [ -e "$tmp.lock" ] || [ -L "$tmp.lock" ]; then
      # 前から在る同じ名前の物は自分が置いた物ではないので、使わず、消しもしない。
      verify_store_fail '内容を読むための一時ファイルを置けない'
      rc=1
    else
      verify_identity_read "$VERIFY_RUN_ROOT" "$VERIFY_RUN_REPO" "$tmp" || rc=1
      err=$VERIFY_STORE_ERR
      if [ -e "$tmp" ] || [ -L "$tmp" ] || [ -e "$tmp.lock" ] || [ -L "$tmp.lock" ]; then
        verify_fs rm -f -- "$tmp" "$tmp.lock"
        if [ -e "$tmp" ] || [ -L "$tmp" ] || [ -e "$tmp.lock" ] || [ -L "$tmp.lock" ]; then
          [ "$rc" -ne 0 ] || err='内容を読むための一時ファイルを消せない'
          rc=1
        fi
      fi
      VERIFY_STORE_ERR=$err
    fi
  else
    [ "$?" -ne 2 ] || verify_store_fail '開始時に使った保存先が無くなっている'
    rc=1
  fi
  if [ "$rc" -ne 0 ]; then
    VERIFY_IDENTITY=
    VERIFY_IDENTITY_ID=
  fi
  if [ -z "$VERIFY_ABORTING" ]; then
    VERIFY_CRITICAL=
    [ -z "$VERIFY_INTERRUPTED" ] || verify_abort
  fi
  return "$rc"
}

# 検証を始めるときの同一性を読んで覚える。
verify_identity_begin() {
  VERIFY_RUN_IDENTITY=
  VERIFY_RUN_IDENTITY_ID=
  verify_identity || return 1
  VERIFY_RUN_IDENTITY=$VERIFY_IDENTITY
  VERIFY_RUN_IDENTITY_ID=$VERIFY_IDENTITY_ID
}

# 同一性の内訳の項目名 → 表示用の名前。
verify_identity_label() {
  case "$1" in
    repo) printf '%s' 'リポジトリ' ;;
    worktree) printf '%s' '作業場所' ;;
    gitdir | commondir) printf '%s' '管理場所' ;;
    head) printf '%s' '先頭コミット' ;;
    branch) printf '%s' 'ブランチ' ;;
    fetch | push) printf '%s' '送信先' ;;
    definition) printf '%s' '検証定義' ;;
    guard) printf '%s' 'フック' ;;
    content) printf '%s' '内容' ;;
    *) printf '%s' '同一性の形式' ;;
  esac
}

# 検証の後の同一性を読み、始めたときと同じかを確かめる。読めない・違うときは失敗を返し、VERIFY_STORE_ERR に理由を置く
# （違うときは、変わった項目の名前だけを並べる。値は表示しない）。
verify_identity_same() {
  local before after i name label changed=
  [ -n "$VERIFY_RUN_IDENTITY" ] && [ -n "$VERIFY_RUN_IDENTITY_ID" ] || verify_store_fail '検証を始めたときの同一性を覚えていない' || return 1
  if ! verify_identity; then
    VERIFY_STORE_ERR="検証の後の同一性を確かめられない（$VERIFY_STORE_ERR）"
    return 1
  fi
  [ "$VERIFY_IDENTITY" != "$VERIFY_RUN_IDENTITY" ] || [ "$VERIFY_IDENTITY_ID" != "$VERIFY_RUN_IDENTITY_ID" ] || return 0
  mapfile -t before <<<"$VERIFY_RUN_IDENTITY"
  mapfile -t after <<<"$VERIFY_IDENTITY"
  for ((i = 0; i < ${#before[@]} || i < ${#after[@]}; i++)); do
    [ "${before[i]-}" != "${after[i]-}" ] || continue
    name=${before[i]-}
    [ -n "$name" ] || name=${after[i]-}
    label=$(verify_identity_label "${name%%=*}")
    case "・$changed・" in *"・$label・"*) ;; *) changed+=${changed:+・}$label ;; esac
  done
  VERIFY_STORE_ERR="検証の間に対象が変わった（${changed:-同一性の要約}）"
  return 1
}

# 中断（INT・TERM・HUP）を受けたとき。ロックを持つ間は印だけ付け、解放した後に verify_abort が処理する。
verify_on_signal() {
  VERIFY_INTERRUPTED=1
  [ -n "$VERIFY_CRITICAL" ] || verify_abort
}

# 中断の後始末: 合格にせず終了コード3で終わる。自分が取ったロックが残っていれば解放する（ほかの持ち主のロックは消さない）。
# 開始の記録が済んでいれば、状態を問わずこの実行の記録を無効にする。済んでいなければ（世代・識別子が未確定）、
# 作業場所の両方の基点にある旧い記録の無効化を試みる。無効にできなかったときは、無効にしたとは表示しない。
verify_abort() {
  local head='必須検証（段1）: 実行不能'
  trap '' INT TERM HUP
  VERIFY_ABORTING=1
  VERIFY_CRITICAL=1
  [ -z "$VERIFY_LOCK" ] || verify_lock_release
  [ -z "$VERIFY_RUN_REPO" ] || head+=" repo=$VERIFY_RUN_REPO"
  if [ -z "$VERIFY_RUN_ROOT" ]; then
    printf '%s。中断された。検証を始めていない（記録には触れていない）。\n' "$head"
  elif [ -n "$VERIFY_RUN_STARTED" ]; then
    if verify_finish invalid; then
      printf '%s。中断された。旧い成功は無効のままで、この検証の結果は合格にしない。\n' "$head"
    else
      printf '%s。中断された。この検証の記録を無効にできない（%s）。この検証の結果は合格にしない。\n' "$head" "$VERIFY_STORE_ERR"
    fi
  elif verify_invalidate_all "$VERIFY_RUN_ROOT"; then
    printf '%s。中断された。検証を始める前だったため、この作業場所の旧い記録を無効にした。この検証の結果は合格にしない。\n' "$head"
  else
    printf '%s。中断された。この作業場所の旧い記録を無効にできない（%s）。この検証の結果は合格にしない。\n' "$head" "$VERIFY_STORE_ERR"
  fi
  exit 3
}

# 点検の本体: 保存先を確かめ、ロックを取り、既存の記録が安全に読めることを確かめ、点検用の記録（probe）を書いて読み戻して消す。
# 既存の記録（record）は読むだけで書き換えない。
verify_store_probe() {
  local rc=0
  verify_store_open "$1" || return 1
  verify_lock_acquire || return 1
  verify_record_read record || rc=$?
  [ "$rc" -le 1 ] || return 1
  verify_record_write probe probe 0 "$2" || return 1
  verify_record_remove probe || return 1
}

verify_store_main() {
  local key root reason= failed=0
  if ! verify_startup_clean; then
    builtin printf '%s\n' '必須検証の記録（段2前半）: 実行不能。ガードの起動環境に BASH_ENV・SHELLOPTS・BASHOPTS または取り込まれたシェル関数があり、点検を差し替え得るため実行しない。これらを外して実行する。'
    return 3
  fi
  shift
  if [ "$#" -ne 0 ]; then
    printf '%s\n' '必須検証の記録（段2前半）: 拒否。引数は受け付けない（保存先・記録の中身・結果は指定できない）。作業場所で引数なしの verify-store を実行する。'
    return 2
  fi
  # 中断（INT・TERM・HUP）は印を付けるだけにして、点検用の記録とロックを片付けてから下で実行不能にする。
  VERIFY_CRITICAL=1
  trap verify_on_signal INT TERM HUP
  if ! verify_clean_env; then
    printf '%s\n' '必須検証の記録（段2前半）: 実行不能。環境値を外して実行するための env が無い。'
    return 3
  fi
  if ! root=$("${VERIFY_CLEAN[@]}" git rev-parse --show-toplevel 2>/dev/null </dev/null) || [ -z "$root" ] || ! cd -- "$root" 2>/dev/null; then
    printf '%s\n' '必須検証の記録（段2前半）: 実行不能。Gitの作業場所を確認できない。'
    return 3
  fi
  if ! key=$(verify_repo_key); then
    printf '%s\n' '必須検証の記録（段2前半）: 実行不能。origin からリポジトリを特定できない。'
    return 3
  fi
  if ! verify_checks "$key" >/dev/null; then
    printf '%s\n' '必須検証の記録（段2前半）: 拒否。必須検証の対応表に無いリポジトリのため、記録を扱わない。'
    return 2
  fi
  if ! verify_store_probe "$root" "$key"; then
    failed=1
    reason=$VERIFY_STORE_ERR
  fi
  if [ -n "$VERIFY_LOCK" ] && ! verify_lock_release && [ "$failed" -eq 0 ]; then
    failed=1
    reason=$VERIFY_STORE_ERR
  fi
  if [ -n "$VERIFY_INTERRUPTED" ]; then
    printf '必須検証の記録（段2前半）: 実行不能 repo=%s。中断された。中断された点検は成功として扱わない。\n' "$key"
    return 3
  fi
  if [ "$failed" -ne 0 ]; then
    printf '必須検証の記録（段2前半）: 実行不能 repo=%s。%s。保存先・記録・ロックを安全に扱えないときは成功として扱わない。\n' "$key" "$reason"
    return 3
  fi
  printf '必須検証の記録（段2前半）: 点検に成功 repo=%s。保存先・記録・ロックを安全に操作できる。この点検と保存先の記録は検証結果を有効にせず、送信の許可にもならない。\n' "$key"
  return 0
}

if [ "${1:-}" = verify ]; then
  verify_main "$@"
  exit $?
fi

if [ "${1:-}" = verify-store ]; then
  verify_store_main "$@"
  exit $?
fi

# ---- PreToolUse フック ----
input=$(cat)

deny() {
  printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"%s"}}\n' "$1"
  exit 0
}

# (a) 所有ファイルの例外
if printf '%s' "$input" | grep -Eq '"file_path": *"[^"]*(gitea_pr\.py|test_gitea_pr_script\.py|test_guard_gitea\.py|guard-gitea\.sh|guard-gitea\.test\.ts|skills[\\/]+(pr|issue-desk-request)[\\/]+SKILL\.md)"'; then
  exit 0
fi

# (b) issue-desk のフォルダーでの編集の例外（実行するコマンドは引き続き調べる）
if printf '%s' "$input" | grep -Eq '"tool_name": *"(Write|Edit|MultiEdit|NotebookEdit|apply_patch)"' &&
  printf '%s' "$input" | grep -Eq '"(cwd|file_path)": *"[^"]*issue-desk([\\/"]|-[0-9a-f]|__)'; then
  exit 0
fi

# 窓口の例外: Bash で、desk の1コマンドだけを実行するもの。; & | ` $( < > と改行（JSON の \n。\" 以外のバックスラッシュ全般）があれば当たらない。
desk_re='"command": *"(~|\$HOME|\$\{HOME\}|/root|/(home|Users)/[A-Za-z0-9._-]+)/\.issue-desk/desk +[A-Za-z0-9][A-Za-z0-9._-]*\.ts( +([^"\\;&|`<>$]|\\"|\$[A-Za-z_{])*)?"'
if printf '%s' "$input" | grep -Eq '"tool_name": *"Bash"' && printf '%s' "$input" | grep -Eq "$desk_re"; then
  exit 0
fi

# (c)
if printf '%s' "$input" | grep -Eq "repos/[^[:space:]\\\"']+/issues"; then
  deny "GiteaのIssue APIは直接呼ばない。issue-deskへの依頼は受付を通す（~/.issue-desk/desk create-issue.ts --file ~/.issue-desk/requests/<名前>.md）。Issueの操作が必要なら利用者に依頼する。拒否を別の書き方で回り込まない。"
fi

# (d)
if printf '%s' "$input" | grep -Eq 'gitea-token|gitea_pr\.(call|load_token|DECRYPT)|(import|from) gitea_pr|ConvertTo-SecureString'; then
  deny "Giteaのトークンは各プロジェクトのPR用ヘルパー（create・merge・list）からだけ使う。関数の流用やトークンの直接読み取りはしない。issue-deskへの依頼は受付を通す（~/.issue-desk/desk create-issue.ts --file ~/.issue-desk/requests/<名前>.md）。拒否を別の書き方で回り込まない。"
fi

# (e)
exit 0
