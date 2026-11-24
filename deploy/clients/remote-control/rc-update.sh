#!/bin/sh
# Ubuntu の PC の claude remote-control を、正本の写し（~/.issue-desk/remote-control/source）から配る。
# issue-desk-rc-update.service（ユーザータイマー、10分ごと）が、写しを trunk へ fast-forward してから呼ぶ。手で変えない。
# 資料: docs/chat-deploy.md の「他のPC（Ubuntu）」。root・sudo は使わない。
#
# - deploy/nas/update-remote-control.sh と update-remote-control-guard.sh を . で読み込み、say・plan・DIR・DRY_RUN を
#   定めて remote_control を呼ぶ。ユニット・控え・各 ~/rc/*/.claude/settings.json の置き換え、変わった回だけの再起動、
#   起動確認と戻し、未完了記録、失敗の組は NAS と同じ挙動になる。許可一覧は deploy/nas/remote-control-settings.json だけ。
# - その前に、Stop フック（~/.claude/hooks/chat-stop-hook.ts）と起動用ファイル（~/.issue-desk/node）を配る。
# - 出力は変えた回と失敗した回の1行だけ（[rc-update] …・[rc-update-failed] …）。終了コードは常に 0。
# - --dry-run では何も書かず、予定を出す。
# - Remote Control は NAS にシステムごとに1つだけ置く（docs/chat-operations.md）。この PC の claude-rc@*.service は
#   disable --now で止める（止めた回と失敗した回だけ1行）。配布物（Skill・hooks・窓口）は配り続ける。

set -u

case "${HOME:-}" in
/*) ;;
*)
  echo '[rc-update-failed] HOME が絶対パスではないため、配りません'
  exit 0
  ;;
esac

DRY_RUN=0
[ "${1:-}" != --dry-run ] || DRY_RUN=1
DIR=$(cd "$(dirname "$0")/../../.." 2>/dev/null && pwd) || DIR=''

say() { printf '%s\n' "$*"; }
plan() { say "$*"; }

for ru_need in deploy/nas/update-remote-control.sh deploy/nas/update-remote-control-guard.sh deploy/nas/update-remote-control-single.sh \
  deploy/nas/remote-control-settings.json scripts/chat-stop-hook.ts scripts/chat-turn-checks.ts scripts/chat-shell-parser.ts scripts/chat-usage-report.ts scripts/stop-check-record.ts scripts/guard-gitea.sh; do
  if [ ! -f "$DIR/$ru_need" ]; then
    say "[rc-update-failed] $DIR/$ru_need を読めないため、配りません"
    exit 0
  fi
done
. "$DIR/deploy/nas/update-remote-control-guard.sh"
. "$DIR/deploy/nas/update-remote-control.sh"
. "$DIR/deploy/nas/update-remote-control-single.sh"
NL="
"
SKILLS_HOST_NODE=1
# PCのNode入口は既存のru_launcherが一度だけ配る。
SKILLS_NODE_MANAGED_EXTERNALLY=1
. "$DIR/deploy/nas/update-skills.sh"

RU_MARK=issue-desk-rc-node

# Stop フックを配る（cmp で違うときだけ、一時ファイルから mv）。
ru_hook() {
  ru_dest=$HOME/.claude/hooks/chat-stop-hook.ts
  # フックが読み込む検査の部品（同じ場所に置く）を先に配る。
  if ! mkdir -p "$HOME/.claude/hooks" 2>/dev/null || ! remote_control_put "$DIR/scripts/chat-shell-parser.ts" "$HOME/.claude/hooks/chat-shell-parser.ts"; then
    say "[rc-update-failed] $HOME/.claude/hooks/chat-shell-parser.ts を書けません"
  elif ! remote_control_put "$DIR/scripts/chat-usage-report.ts" "$HOME/.claude/hooks/chat-usage-report.ts"; then
    say "[rc-update-failed] $HOME/.claude/hooks/chat-usage-report.ts を書けません"
  elif ! remote_control_put "$DIR/scripts/chat-turn-checks.ts" "$HOME/.claude/hooks/chat-turn-checks.ts"; then
    say "[rc-update-failed] $HOME/.claude/hooks/chat-turn-checks.ts を書けません"
  elif ! remote_control_put "$DIR/scripts/stop-check-record.ts" "$HOME/.claude/hooks/stop-check-record.ts"; then
    say "[rc-update-failed] $HOME/.claude/hooks/stop-check-record.ts を書けません"
  elif ! remote_control_put "$DIR/scripts/chat-stop-hook.ts" "$ru_dest"; then
    say "[rc-update-failed] $ru_dest を書けません"
  elif [ "$RC_WROTE" = 1 ]; then
    say "[rc-update] $ru_dest を配りました"
  fi
}

# PreToolUse は読むだけ。未登録・読取不能を区別し、配布元にないフックも消さない。
ru_gitea_hook() {
  ru_dest=$HOME/.claude/hooks/guard-gitea.sh
  if [ -f "$ru_dest" ] && [ -x "$ru_dest" ] && cmp -s "$DIR/scripts/guard-gitea.sh" "$ru_dest"; then
    return 0
  fi
  ru_action=作成
  [ ! -e "$ru_dest" ] || ru_action=更新
  ru_sum=$(cksum <"$DIR/scripts/guard-gitea.sh") || ru_sum=''
  ru_tmp=$ru_dest.$$.tmp
  if [ -z "$ru_sum" ] || [ -L "$ru_dest" ] || [ -d "$ru_dest" ] ||
    ! { mkdir -p "$HOME/.claude/hooks" 2>/dev/null &&
      cp "$DIR/scripts/guard-gitea.sh" "$ru_tmp" 2>/dev/null &&
      chmod 755 "$ru_tmp" 2>/dev/null && mv -f "$ru_tmp" "$ru_dest" 2>/dev/null; }; then
    rm -f "$ru_tmp" 2>/dev/null || :
    say "[rc-update-failed] $ru_dest を書けません"
    return 0
  fi
  ru_registration=$(node - "$HOME/.claude/settings.json" <<'CHECK' 2>/dev/null
const fs = require('node:fs');
try {
  const file = process.argv[2];
  const settings = fs.existsSync(file) ? JSON.parse(fs.readFileSync(file, 'utf8')) : {};
  const entries = settings.hooks?.PreToolUse ?? [];
  if (!Array.isArray(entries)) throw new Error('PreToolUse');
  const registered = entries.some(entry => Array.isArray(entry?.hooks) &&
    entry.hooks.some(hook => hook?.type === 'command' && typeof hook.command === 'string' &&
      /(?:^|[\/\s])guard-gitea\.sh(?:[\s"\x27]|$)/.test(hook.command)));
  process.stdout.write(registered ? '登録あり' : '登録なし（登録はしません）');
} catch {
  process.stdout.write('確認不能（settings.json を読めません）');
}
CHECK
  ) || ru_registration=''
  [ -n "$ru_registration" ] || ru_registration='確認不能（node が使えません）'
  ru_extra=''
  for ru_file in "$HOME/.claude/hooks/"* "$HOME/.claude/hooks/".[!.]* "$HOME/.claude/hooks/"..?*; do
    [ -f "$ru_file" ] || [ -L "$ru_file" ] || continue
    ru_name=${ru_file##*/}
    case "$ru_name" in chat-stop-hook.ts | chat-turn-checks.ts | chat-shell-parser.ts | chat-usage-report.ts | stop-check-record.ts | guard-gitea.sh) continue ;; esac
    ru_extra="$ru_extra${ru_extra:+, }$ru_name"
  done
  say "[rc-update] $ru_dest を$ru_actionしました (cksum: $ru_sum); PreToolUse: $ru_registration; 配布元にないフック（消しません）: ${ru_extra:-該当ファイルなし}"
}

# 起動用ファイル（~/.issue-desk/node）を置く。目印の無い別の内容が既にあれば上書きしない。
ru_launcher() {
  ru_dest=$HOME/.issue-desk/node
  if [ -e "$ru_dest" ] && ! grep -Fq "$RU_MARK" "$ru_dest" 2>/dev/null; then
    say "[rc-update-failed] $ru_dest は issue-desk の起動用ファイルではない（目印 $RU_MARK が無い）ため、上書きしません"
    return 0
  fi
  ru_tmp=$ru_dest.$$.new
  if ! mkdir -p "$HOME/.issue-desk" 2>/dev/null; then
    say "[rc-update-failed] $ru_dest を書けません"
    return 0
  fi
  cat >"$ru_tmp" <<'LAUNCHER'
#!/bin/sh
# issue-desk-rc-node: deploy/clients/remote-control/rc-update.sh が書き直すので、手で変えない。
# Stop フックなどを、ホストの Node.js 24 以上で動かす。見つからなければ標準入力を読み捨てて exit 0 で終える
# （何もせず通す。チャットを止めない）。探す順は PATH、次に ISSUE_DESK_NODE_DIRS（コロン区切り。未設定なら
# ~/.local/node/bin・~/.local/bin・/usr/local/bin・/usr/bin・/opt/node/bin）。
usable() {
  [ -x "$1" ] || return 1
  v=$("$1" --version 2>/dev/null) || return 1
  v=${v#v}
  v=${v%%.*}
  case "$v" in '' | *[!0-9]*) return 1 ;; esac
  [ "$v" -ge 24 ]
}
n=$(command -v node 2>/dev/null) || n=''
if [ -n "$n" ] && usable "$n"; then exec "$n" "$@"; fi
dirs=${ISSUE_DESK_NODE_DIRS-$HOME/.local/node/bin:$HOME/.local/bin:/usr/local/bin:/usr/bin:/opt/node/bin}
set -f
IFS=:
for d in $dirs; do
  [ -n "$d" ] && usable "$d/node" && exec "$d/node" "$@"
done
unset IFS
while IFS= read -r line || [ -n "$line" ]; do :; done
exit 0
LAUNCHER
  chmod 755 "$ru_tmp" 2>/dev/null || :
  if [ -f "$ru_dest" ] && [ -x "$ru_dest" ] && cmp -s "$ru_tmp" "$ru_dest"; then
    rm -f "$ru_tmp" 2>/dev/null || :
  elif mv -f "$ru_tmp" "$ru_dest" 2>/dev/null; then
    say "[rc-update] $ru_dest を置きました"
  else
    rm -f "$ru_tmp" 2>/dev/null || :
    say "[rc-update-failed] $ru_dest を書けません"
  fi
}

# このユニットとタイマー自身（導入済みのときだけ）。変わった回だけ daemon-reload する。
ru_units() {
  ru_ud=$HOME/.config/systemd/user
  ru_changed=0
  for ru_n in issue-desk-rc-update.service issue-desk-rc-update.timer; do
    [ -f "$ru_ud/$ru_n" ] || continue
    if remote_control_put "$DIR/deploy/clients/remote-control/$ru_n" "$ru_ud/$ru_n"; then
      [ "$RC_WROTE" = 1 ] && ru_changed=1
    else
      say "[rc-update-failed] $ru_ud/$ru_n を書けません"
    fi
  done
  if [ "$ru_changed" = 1 ]; then
    if user_units_reload; then
      say '[rc-update] issue-desk-rc-update のユニットを更新しました'
    else
      say '[rc-update-failed] ユニットを更新しましたが、systemctl --user daemon-reload に失敗しました'
    fi
  fi
}

# 手で置いた作業フォルダーの設定（Git 未追跡で、内容が正本から hooks だけを外したもの）を、控えが無い初回だけ控えとして
# 引き継ぐ。guard は「以前の控えと同じ既存設定」を更新対象にするので、独自設定として止まらず正本へ置き換わる。
ru_adopt_hand_settings() {
  ru_ctl=$HOME/.issue-desk/remote-control/settings.json
  [ -f "$HOME/.config/systemd/user/claude-rc@.service" ] && [ ! -e "$ru_ctl" ] || return 0
  for ru_dir in "$HOME"/rc/*; do
    ru_conf=$ru_dir/.claude/settings.json
    [ -d "$ru_dir" ] && [ ! -L "$ru_dir" ] && [ ! -L "$ru_dir/.claude" ] && [ ! -L "$ru_conf" ] && [ -f "$ru_conf" ] || continue
    if git -C "$ru_dir" ls-files --error-unmatch -- .claude/settings.json >/dev/null 2>&1; then
      continue
    fi
    node - "$DIR/deploy/nas/remote-control-settings.json" "$ru_conf" <<'HAND' >/dev/null 2>&1 || continue
const fs = require('node:fs');
const { isDeepStrictEqual } = require('node:util');
const source = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
delete source.hooks;
process.exit(isDeepStrictEqual(source, JSON.parse(fs.readFileSync(process.argv[3], 'utf8'))) ? 0 : 1);
HAND
    if [ ! -e "$ru_ctl" ]; then
      if mkdir -p "$HOME/.issue-desk/remote-control" 2>/dev/null && cp "$ru_conf" "$ru_ctl.$$.tmp" 2>/dev/null &&
        mv -f "$ru_ctl.$$.tmp" "$ru_ctl" 2>/dev/null; then
        say "[rc-update] 手で置いた hooks なしの設定 $ru_conf を前の控えとして引き継ぎ、正本へ置き換えます"
        continue
      else
        rm -f "$ru_ctl.$$.tmp" 2>/dev/null || :
        say "[rc-update-failed] $ru_conf の引き継ぎに失敗しました"
        return 0
      fi
    fi

    # 最初の設定を控えにした後は、意味が同じで整形だけ異なる設定を控えの
    # バイト列へ揃える。guard の比較はバイト単位なので、ここで正規化する。
    if [ -f "$ru_ctl" ] && ! cmp -s "$ru_conf" "$ru_ctl"; then
      if cp "$ru_ctl" "$ru_conf.$$.tmp" 2>/dev/null && mv -f "$ru_conf.$$.tmp" "$ru_conf" 2>/dev/null; then
        :
      else
        rm -f "$ru_conf.$$.tmp" 2>/dev/null || :
        say "[rc-update-failed] $ru_conf の引き継ぎに失敗しました"
        return 0
      fi
    fi
  done
}

# ~/.claude.json に remote-control の無人起動に要る項目（hasCompletedOnboarding・remoteDialogSeen と、各 ~/rc/* の
# projects[<パス>].hasTrustDialogAccepted）を足す。他のキーは保ち、変わるときだけ一時ファイルから mv で書く。
# 読めない・JSON でない・node が無いときは触らない（壊さない）。
ru_claude_json() {
  ru_cj=$HOME/.claude.json
  ru_dirs=''
  for ru_dir in "$HOME"/rc/*; do
    [ -d "$ru_dir" ] && [ ! -L "$ru_dir" ] || continue
    ru_dirs="$ru_dirs${ru_dirs:+
}$ru_dir"
  done
  ru_tmp=$ru_cj.$$.new
  ru_result=$(RU_DIRS=$ru_dirs node - "$ru_cj" "$ru_tmp" <<'CJ' 2>/dev/null
const fs = require('node:fs');
const [file, tmp] = process.argv.slice(2);
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
const before = JSON.stringify(data);
if (data.hasCompletedOnboarding !== true) data.hasCompletedOnboarding = true;
if (data.remoteDialogSeen !== true) data.remoteDialogSeen = true;
const projects = data.projects && typeof data.projects === 'object' && !Array.isArray(data.projects) ? data.projects : {};
for (const dir of (process.env.RU_DIRS || '').split('\n').filter(Boolean)) {
  const entry = projects[dir] && typeof projects[dir] === 'object' ? projects[dir] : {};
  if (entry.hasTrustDialogAccepted !== true) entry.hasTrustDialogAccepted = true;
  projects[dir] = entry;
}
data.projects = projects;
if (JSON.stringify(data) !== before) {
  fs.writeFileSync(tmp, JSON.stringify(data, null, 2) + '\n', { mode: 0o600 });
  fs.chmodSync(tmp, 0o600);
  process.stdout.write('write');
}
CJ
  ) || {
    rm -f "$ru_tmp" 2>/dev/null || :
    return 0
  }
  if [ "$ru_result" = invalid ]; then
    say "[rc-update-failed] $ru_cj は JSON が壊れているため変更しません"
    return 0
  fi
  [ "$ru_result" = write ] || return 0
  if mv -f "$ru_tmp" "$ru_cj" 2>/dev/null; then
    say "[rc-update] $ru_cj に remote-control の項目を足しました"
  else
    rm -f "$ru_tmp" 2>/dev/null || :
    say "[rc-update-failed] $ru_cj を書けません"
  fi
}

# この PC の claude-rc@*.service を全部止める（deploy/nas/update-remote-control-single.sh の共通関数）。
ru_stop_remote_control() {
  if ! remote_control_stop_others ''; then
    say '[rc-update-failed] systemctl --user list-units に失敗したため、remote-control を止めていません'
    return 0
  fi
  [ -z "$RS_STOPPED" ] || say "[rc-update] remote-control は NAS だけに置くため、$RS_STOPPED を止めました"
  [ -z "$RS_FAILED" ] || say "[rc-update-failed] 止められなかった remote-control: $RS_FAILED"
}

skills_listed
remote_control_env
if [ "$DRY_RUN" = 1 ]; then
  plan "予定: $DIR/scripts/chat-stop-hook.ts を $HOME/.claude/hooks/chat-stop-hook.ts へ、$HOME/.issue-desk/node を置く"
  plan "予定: $DIR/scripts/guard-gitea.sh を $HOME/.claude/hooks/guard-gitea.sh へ実行可で配る（PreToolUse は登録しない）"
else
  ru_hook
  ru_gitea_hook
  ru_launcher
  ru_units
  if [ "${ISSUE_DESK_RC_EXISTING_ONLY:-0}" != 1 ]; then
    ru_adopt_hand_settings
    ru_claude_json
  fi
fi
# 受付の窓口（~/.issue-desk/desk）。定義は desk-access.sh。失敗でも配布は続ける。
if [ -f "$DIR/deploy/clients/remote-control/desk-access.sh" ]; then
  . "$DIR/deploy/clients/remote-control/desk-access.sh"
  linux_desk_access || :
fi
remote_control
if [ "$DRY_RUN" = 1 ]; then
  plan '予定: この PC の claude-rc@*.service を disable --now で止める（Remote Control は NAS だけ）'
else
  ru_stop_remote_control
fi
if [ -f "$DIR/deploy/clients/remote-control/rc-report.sh" ]; then
  . "$DIR/deploy/clients/remote-control/rc-report.sh"
  rc_update_report || :
fi
exit 0
