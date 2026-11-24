# NAS の Remote Control で相談するための作業場所を用意する。
consultation_file() (
  cf_target=$1
  [ ! -L "$cf_target" ] || return 1
  if [ -e "$cf_target" ]; then
    [ -f "$cf_target" ] || return 1
    IFS= read -r cf_marker <"$cf_target" || return 1
    [ "$cf_marker" = '<!-- issue-desk:managed-consultation -->' ] || return 1
  fi
  cf_source=$(mktemp "${cf_target%/*}/.consultation.XXXXXX") || return 1
  trap 'rm -f "$cf_source"' EXIT
  printf '%s\n' '<!-- issue-desk:managed-consultation -->' >"$cf_source" || return 1
  cat >>"$cf_source" || return 1
  remote_control_put "$cf_source" "$cf_target"
)

consultation_directory() {
  [ ! -L "$1" ] && mkdir -p "$1" && [ ! -e "$1/.git" ] && [ ! -L "$1/.git" ]
}

consultation_instructions() {
  printf '# %s の相談\n\n' "$1"
  printf 'ここは NAS の相談用フォルダーです。相談はチャットで始められます。\n'
  printf '対象は `mofua-suki/%s` です。\n' "$1"
  if [ "$1" = image-review-client ]; then
    cat <<'ROUTING'
画像生成の相談は `mofua-suki/demo-image-generator`、好みの判定・学習の相談は
`mofua-suki/demo-image-rating`、レビュー画面・中継の相談は `mofua-suki/image-review-client` を対象にする。
ROUTING
  fi
  cat <<'INSTRUCTIONS'
対象のリポジトリの AGENTS.md を先に読む。NAS の既存チェックアウトは
`~/Workspace/<名前>` と `~/rc/<名前>` を確認し、使える写しが無ければ対象の Git リポジトリから
AGENTS.md を読む。コードの作業は利用者が依頼した場合に対象リポジトリの規則に従って行う。
相談の送信を依頼された場合は issue-desk-operations の手順に従い、
対象リポジトリを `create-issue.ts --project mofua-suki/<名前>` で明示する。
INSTRUCTIONS
}

# Claude でも画像系の相談先を選べるよう、起動前にプロジェクトの指示を置く。
claude_consultations() (
  cc_dir=$HOME/rc/image-review-client
  [ ! -L "$HOME/rc" ] && [ ! -L "$cc_dir" ] && mkdir -p "$cc_dir" || return 1
  consultation_instructions image-review-client | consultation_file "$cc_dir/CLAUDE.md"
)

# Codex のデーモンはホスト単位。各相談では Git 管理外のフォルダーをローカルで開く。
codex_consultations() (
  cx_root=$HOME/Consultations
  [ -x "$HOME/.local/bin/codex" ] || return 1
  cx_status=0
  if consultation_directory "$cx_root"; then
    for cx_name in "$RC_PRIMARY" $RC_KEEP; do
      cx_dir=$cx_root/$cx_name
      if ! consultation_directory "$cx_dir" ||
        ! consultation_instructions "$cx_name" | consultation_file "$cx_dir/AGENTS.md"; then
        say "[update:remote-control-failed] Codex の相談用フォルダーを準備できません: $cx_dir"
        cx_status=1
      fi
    done
  else
    cx_status=1
  fi
  # 更新サービスが終了しても、起動したデーモンを systemd が止めないようにする。
  systemd-run --user --scope --quiet timeout 30 \
    "$HOME/.local/bin/codex" remote-control start --json >/dev/null || return 1
  return "$cx_status"
)
