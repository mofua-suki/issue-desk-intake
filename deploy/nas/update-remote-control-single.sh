# deploy/nas/update.sh が . で読み込む: NAS の remote-control をシステムごとに1つ（代表 claude-rc@issue-desk-intake と
# RC_KEEP の各システム）にそろえる（docs/chat-operations.md の「Remote Control は NAS にシステムごとに1つ」）。単独では実行しない。定義だけで、読み込んでも何もしない。
# update.sh の say・plan・DRY_RUN と、update-remote-control.sh の remote_control_env・remote_control_wait を使う。
#
# - ~/.config/systemd/user/claude-rc@.service と ~/rc/issue-desk-intake（リンクでないフォルダー）があるホストだけで働く。
# - 代表のフォルダーを ~/.claude.json の projects[<パス>].hasTrustDialogAccepted に足す（無人起動で信頼の確認に
#   止まらないように。他のキーは保ち、変わるときだけ一時ファイルから mv）。node が無い・JSON が壊れていれば触らない。
# - 代表を有効にして起動を確かめる（RC_WAIT_SECONDS 秒まで）。起動しなければほかのインスタンスは止めない。
# - 残す窓口がすべて動いていれば、ほかの claude-rc@*.service（読み込み済みのものと default.target.wants の有効化）を
#   disable --now で止める。止めたとき・失敗したときだけ1行出す。終了コードは変えない。
# - RC_KEEP の各システム（窓口を置くリポジトリ。利用者の指定）は止めず、~/rc/<名前> があれば代表と同じく
#   信頼済み・有効にして起動する。フォルダーが無い・起動しないときは1行出し、ほかの窓口の停止は見送る。

RC_PRIMARY=issue-desk-intake
RC_KEEP='issue-desk-worker demo-image-generator image-review-client'

# 代表のフォルダーを信頼済みにする。書いた・変わらないは 0、触れなかったは 1。
remote_control_single_trust() {
  rs_cj=$HOME/.claude.json
  rs_tmp=$rs_cj.$$.new
  command -v node >/dev/null 2>&1 || return 1
  rs_result=$(node - "$rs_cj" "$rs_tmp" "$1" <<'CJ' 2>/dev/null
const fs = require('node:fs');
const [file, tmp, dir] = process.argv.slice(2);
let data = {};
if (fs.existsSync(file)) {
  try {
    data = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch {
    process.stdout.write('invalid');
    process.exit(0);
  }
  if (data === null || typeof data !== 'object' || Array.isArray(data)) {
    process.stdout.write('invalid');
    process.exit(0);
  }
}
const projects = data.projects && typeof data.projects === 'object' && !Array.isArray(data.projects) ? data.projects : {};
const entry = projects[dir] && typeof projects[dir] === 'object' && !Array.isArray(projects[dir]) ? projects[dir] : {};
if (entry.hasTrustDialogAccepted === true) process.exit(0);
entry.hasTrustDialogAccepted = true;
projects[dir] = entry;
data.projects = projects;
fs.writeFileSync(tmp, JSON.stringify(data, null, 2) + '\n', { mode: 0o600 });
fs.chmodSync(tmp, 0o600);
process.stdout.write('write');
CJ
  ) || {
    rm -f "$rs_tmp" 2>/dev/null || :
    return 1
  }
  [ "$rs_result" = invalid ] && return 1
  [ "$rs_result" = write ] || return 0
  if mv -f "$rs_tmp" "$rs_cj" 2>/dev/null; then
    return 0
  fi
  rm -f "$rs_tmp" 2>/dev/null || :
  return 1
}

# $1（空白区切りのユニット名。空なら全部）以外の claude-rc@*.service の名前を空白区切りで RS_OTHERS に入れる。systemctl が失敗したら 1。
remote_control_single_others() {
  RS_OTHERS=''
  rs_listed=$(systemctl --user list-units 'claude-rc@*.service' --all --plain --no-legend --no-pager 2>/dev/null) || return 1
  for rs_want in "$HOME"/.config/systemd/user/default.target.wants/claude-rc@*.service; do
    [ -e "$rs_want" ] || [ -L "$rs_want" ] || continue
    rs_listed="$rs_listed
${rs_want##*/}"
  done
  while IFS= read -r rs_row || [ -n "$rs_row" ]; do
    rs_row=${rs_row#● }
    rs_name=${rs_row%% *}
    case "$rs_name" in
    claude-rc@?*.service)
      case " ${1:-} " in
      *" $rs_name "*) continue ;;
      esac
      case " $RS_OTHERS " in
      *" $rs_name "*) ;;
      *) RS_OTHERS=$RS_OTHERS${RS_OTHERS:+ }$rs_name ;;
      esac
      ;;
    esac
  done <<EOF
$rs_listed
EOF
  return 0
}

# $1（空なら全部）以外の claude-rc@*.service を disable --now で止める。止めたものを RS_STOPPED、止められなかった
# ものを RS_FAILED（空白区切り）に入れる。一覧を読めなければ 1。Ubuntu の rc-update.sh も $1 を空にして使う。
remote_control_stop_others() {
  RS_STOPPED=''
  RS_FAILED=''
  remote_control_single_others "${1:-}" || return 1
  for rs_u in $RS_OTHERS; do
    # 止めた後も読み込み済みで残るものは、動いておらず有効でもなければ済んでいる（毎回は出さない）。
    if ! systemctl --user is-enabled --quiet "$rs_u" 2>/dev/null && ! systemctl --user is-active --quiet "$rs_u" 2>/dev/null &&
      ! systemctl --user is-failed --quiet "$rs_u" 2>/dev/null; then
      continue
    fi
    if systemctl --user disable --now "$rs_u" >/dev/null 2>&1; then
      systemctl --user reset-failed "$rs_u" >/dev/null 2>&1 || :
      RS_STOPPED=$RS_STOPPED${RS_STOPPED:+ }$rs_u
    else
      RS_FAILED=$RS_FAILED${RS_FAILED:+ }$rs_u
    fi
  done
  return 0
}

# RC_KEEP の1つ（$1）を信頼済み・有効にして起動を確かめる。止めることはしない。問題があれば1行出す。
remote_control_single_keep() {
  rk_dir=$HOME/rc/$1
  rk_unit=claude-rc@$1.service
  if [ ! -d "$rk_dir" ] || [ -L "$rk_dir" ]; then
    say "[update:remote-control-failed] システムの窓口 $rk_unit の作業フォルダー $rk_dir が無いため、起動していません"
    return 1
  fi
  if ! remote_control_single_trust "$rk_dir"; then
    say "[update:remote-control-failed] ~/.claude.json に $rk_dir の信頼を書けないため、$rk_unit を起動していません"
    return 1
  fi
  systemctl --user is-enabled --quiet "$rk_unit" 2>/dev/null && systemctl --user is-active --quiet "$rk_unit" 2>/dev/null && return 0
  systemctl --user reset-failed "$rk_unit" >/dev/null 2>&1 || :
  systemctl --user enable --now "$rk_unit" >/dev/null 2>&1 || :
  RC_UNITS=$rk_unit
  remote_control_wait
  if [ -n "$RC_BAD" ]; then
    say "[update:remote-control-failed] システムの窓口 $rk_unit が起動しません"
    return 1
  fi
  say "[update:remote-control] システムの窓口 $rk_unit を有効にして起動しました"
  return 0
}

# NAS 更新から呼ぶ。$1 が 0 なら画像レビュー窓口の準備を次回へ回す。
remote_control_single() {
  case "${HOME:-}" in
  /*) ;;
  *) return 0 ;;
  esac
  [ -f "$HOME/.config/systemd/user/claude-rc@.service" ] || return 0
  rs_dir=$HOME/rc/$RC_PRIMARY
  rs_unit=claude-rc@$RC_PRIMARY.service
  if [ ! -d "$rs_dir" ] || [ -L "$rs_dir" ]; then
    say "[update:remote-control-failed] 代表の作業フォルダー $rs_dir が無いため、remote-control をシステムごとに1つにそろえていません"
    return 0
  fi
  if [ "$DRY_RUN" = 1 ]; then
    plan "予定（最後に）: $rs_unit とシステムの窓口 ${RC_KEEP:-（なし）} を有効にして起動を確かめ、ほかの claude-rc@*.service を disable --now で止める"
    return 0
  fi
  remote_control_env
  if ! remote_control_single_trust "$rs_dir"; then
    say "[update:remote-control-failed] ~/.claude.json に $rs_dir の信頼を書けないため、remote-control をシステムごとに1つにそろえていません"
    return 0
  fi
  if ! systemctl --user is-enabled --quiet "$rs_unit" 2>/dev/null || ! systemctl --user is-active --quiet "$rs_unit" 2>/dev/null; then
    systemctl --user reset-failed "$rs_unit" >/dev/null 2>&1 || :
    systemctl --user enable --now "$rs_unit" >/dev/null 2>&1 || :
  fi
  RC_UNITS=$rs_unit
  remote_control_wait
  if [ -n "$RC_BAD" ]; then
    say "[update:remote-control-failed] 代表の $rs_unit が起動しないため、ほかの remote-control は止めていません"
    return 0
  fi
  rs_keep_units=$rs_unit
  rs_ready=1
  for rs_k in $RC_KEEP; do
    rs_keep_units="$rs_keep_units claude-rc@$rs_k.service"
    if [ "$rs_k" = image-review-client ] && [ "${1:-1}" = 0 ]; then
      rs_ready=0
      continue
    fi
    remote_control_single_keep "$rs_k" || rs_ready=0
  done
  [ "$rs_ready" = 1 ] || return 0
  if ! remote_control_stop_others "$rs_keep_units"; then
    say '[update:remote-control-failed] systemctl --user list-units に失敗したため、ほかの remote-control は止めていません'
    return 0
  fi
  [ -z "$RS_STOPPED" ] || say "[update:remote-control] システムごとに1つの窓口だけにするため、$RS_STOPPED を止めました"
  [ -z "$RS_FAILED" ] || say "[update:remote-control-failed] 止められなかった remote-control: $RS_FAILED"
  return 0
}
