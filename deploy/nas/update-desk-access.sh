# NAS本体のチャット窓口。稼働Serverと同じ版・ネットワーク・PostgreSQLを使い、要求ごとの一時キーを発行する。
# update.shからsourceし、DIR・DRY_RUN・say・planを受け取る。PCトークンの発行・写しは行わない。
# PCトークンを要する操作は一覧に従い、同じ窓口が ~/.issue-desk/desk-pc（稼働WorkerのK2のPCトークン接続）へ振り分ける。
desk_access() (
 case "${HOME:-}" in /*) ;; *) say '[update:desk-access-failed] HOMEが絶対パスではありません'; return 1;; esac
 if [ "${DRY_RUN:-0}" = 1 ]; then plan "$HOME/.issue-desk/desk（NASの一時キー窓口）を配置する"; return 0; fi
 umask 077
 . "$DIR/deploy/nas/update-desk-scripts.sh"
 desk_scripts_pin || { say '[update:desk-access-failed] スクリプトの写しを更新できません'; return 1; }
 base=$HOME/.issue-desk
 mkdir -p "$base" || return 1
 [ ! -L "$base/desk" ] && [ ! -d "$base/desk" ] || return 1
 temporary=$(mktemp "$base/.desk.XXXXXXXX") || return 1
 trap 'rm -f "$temporary"' EXIT
 cat >"$temporary" <<'WRAPPER'
#!/bin/sh
# issue-desk-desk: NASの一時キー専用。Bearerへの自動切替はしない。
case "${1:-}" in ''|*[!A-Za-z0-9._-]*|*..*) echo '受付スクリプト名を指定してください。' >&2;exit 2;; esac
# 入口は .ts だけを受け付ける。権限保護で .mjs のまま残る scripts/ssh-*-runner.mjs はホスト作業用SSHの固定操作が動かし、この窓口からは呼ばない。
case "$1" in *.ts) ;; *) echo '受付スクリプト名.tsを指定してください。' >&2;exit 2;; esac
script=$1;shift
physical_home=$(CDPATH= cd "$HOME" && pwd -P) || exit 1
physical_cwd=$(pwd -P) || exit 1
case "$physical_cwd/" in "$physical_home/"*) ;; *) echo 'ホームの下から実行してください。' >&2;exit 1;; esac
if [ "$script" = follow-issues.ts ]; then
 state_next=0
 for argument in "$@"; do
  if [ "$state_next" = 1 ]; then
   case "$argument" in /*) ;; *) echo '--state はホームの下の絶対パスを指定してください。' >&2; exit 1;; esac
   state_path=$(realpath -m -- "$argument" 2>/dev/null) || { echo '--state を解決できません。ホームの下を指定してください。' >&2; exit 1; }
   case "$state_path/" in "$physical_home/"*) ;; *) echo '--state はホームの下に置いてください。' >&2; exit 1;; esac
   # 未作成の親を許しつつ、実在する祖先を探索できない場合は拒否する。
   ancestor=$state_path
   while [ ! -e "$ancestor" ] && [ ! -L "$ancestor" ]; do
    ancestor=${ancestor%/*}; [ -n "$ancestor" ] || ancestor=/
   done
   if [ -L "$ancestor" ] && ! realpath -e -- "$ancestor" >/dev/null 2>&1; then
    echo '--state のリンクを解決できません。ホームの下を指定してください。' >&2; exit 1
   fi
   [ -d "$ancestor" ] || ancestor=${ancestor%/*}
   [ -n "$ancestor" ] || ancestor=/
   [ -x "$ancestor" ] && realpath -e -- "$ancestor" >/dev/null 2>&1 || { echo '--state の親を確認できません。ホームの下を指定してください。' >&2; exit 1; }
   state_next=0
  elif [ "$argument" = --state ]; then state_next=1
  fi
 done
fi
for argument in "$@"; do
 case "$argument" in --*=*) candidate=${argument#*=};; *) candidate=$argument;; esac
 [ -e "$candidate" ] || continue
 physical_path=$(readlink -f -- "$candidate") || exit 1
 case "$physical_path/" in "$physical_home/"*) ;; *) echo '参照ファイルはホームの下へ置いてください。' >&2;exit 1;; esac
done
# 振り分け一覧は配置時に埋め込む。引数は値付き形式も判定する。
route_pc=0
while IFS='|' read -r route flags; do
 [ "$script" = "$route" ] || continue
 if [ -z "$flags" ]; then route_pc=1; break; fi
 # サブコマンドは第1引数だけ、オプションは全引数を照合する。
 case "|$flags|" in *"|${1:-}|"*) [ -n "${1:-}" ] && route_pc=1;; esac
 [ "$route_pc" = 0 ] || break
 for argument in "$@"; do
  option=${argument%%=*}
  case "$option" in --*) case "|$flags|" in *"|$option|"*) route_pc=1; break;; esac;; esac
 done
done <<'ROUTES'
WRAPPER
 # JSONの各要素はスクリプト名と任意の条件引数を | で区切った文字列。
 # 許可した文字・構造以外を拒否し、不完全な一覧を配置しない。
 awk '
 BEGIN { state=0; count=0 }
 /^[[:space:]]*\[[[:space:]]*$/ && state==0 { state=1; next }
 /^[[:space:]]*\][[:space:]]*$/ && state==1 && count>0 { state=2; next }
 state==1 && /^[[:space:]]*"[A-Za-z0-9._|-]+",?[[:space:]]*$/ {
   line=$0; sub(/^[[:space:]]*"/, "", line); sub(/",?[[:space:]]*$/, "", line)
   n=split(line, fields, "|"); if (fields[1] !~ /^[A-Za-z0-9._-]+\.ts$/) exit 1
   for (i=2; i<=n; i++) if (fields[i] !~ /^--[a-z0-9-]+$/ && fields[i] !~ /^[a-z][a-z0-9-]*$/) exit 1
   print line; count++; next
 }
 { exit 1 }
 END { if (state!=2) exit 1 }
 ' "$DIR/deploy/nas/desk-pc-routes.json" >>"$temporary" || return 1
 cat >>"$temporary" <<'WRAPPER'
ROUTES
if [ "$route_pc" = 1 ]; then
 [ -x "$HOME/.issue-desk/desk-pc" ] || { echo 'PCトークン窓口（~/.issue-desk/desk-pc）がありません。NASの更新（deploy/nas/update.sh）で配置されます。' >&2;exit 1; }
 exec sh "$HOME/.issue-desk/desk-pc" "$script" "$@"
fi
case "${ISSUE_DESK_NAS_DIR:-$HOME/Workspace/issue-desk-intake}" in /*) nas_dir=${ISSUE_DESK_NAS_DIR:-$HOME/Workspace/issue-desk-intake};; *) echo 'NAS配置先は絶対パスを指定してください。' >&2;exit 1;; esac
# 対象外の呼出しにはホストのNodeを要求しない。
recovery_candidate() (
 thread=${CODEX_THREAD_ID:-}
 [ "${#thread}" -eq 36 ] || return 1
 case "$thread" in *[!a-f0-9-]* ) return 1;; esac
 case "$thread" in ????????-????-????-????-????????????) ;; *) return 1;; esac
 thread_hex=$(printf '%s' "$thread" | tr -d '-') || return 1
 [ "${#thread_hex}" -eq 32 ] || return 1
 project=0
 while [ "$#" -gt 0 ]; do
  case "$1" in
   --spec|--check-only|--dry-run|--submit|--retry|--draft|--check) return 1;;
   --project) [ "$#" -ge 2 ] || return 1; project=1; shift;;
   --consultation-provider) [ "$#" -ge 2 ] && [ "$2" = codex ] || return 1; shift;;
   --hold) ;;
   --provider|--text|--file|--timeout|--priority|--priority-reason|--tier|--implementation-tier|--not-duplicate|--over-spec|--despite-pending|--reason|--replace-issues|--send-id|--expect-revision|--read-evidence-file|--history-read|--url|--token-file)
    [ "$#" -ge 2 ] || return 1; shift;;
   *) return 1;;
  esac
  shift
 done
 [ "$project" = 1 ]
)
# 記録はコンテナの起動より先に確定する。既存IDは置き換えず、同じ会話からの再送だけ許す。
if [ "$script" = create-issue.ts ] && recovery_candidate "$@"; then
 recovery_id=$(node -e '
const fs = require("node:fs"), path = require("node:path"), os = require("node:os"), crypto = require("node:crypto");
const uuid = /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/;
const thread = process.env.CODEX_THREAD_ID || "";
if (!uuid.test(thread)) process.exit(0);
const values = new Set(["project", "provider", "consultation-provider", "draft", "text", "file", "timeout", "priority", "priority-reason", "tier", "implementation-tier", "not-duplicate", "over-spec", "despite-pending", "reason", "replace-issues", "check", "send-id", "expect-revision", "read-evidence-file", "history-read", "url", "token-file"]);
const flags = new Set(["hold", "submit", "retry", "check-only", "dry-run", "spec"]);
const opts = {};
for (let i = 1; i < process.argv.length; i++) {
 const arg = process.argv[i];
 if (!arg.startsWith("--")) process.exit(0);
 const key = arg.slice(2);
 if (opts[key] !== undefined) process.exit(0);
 if (flags.has(key)) opts[key] = true;
 else if (values.has(key) && process.argv[i + 1] && !process.argv[i + 1].startsWith("--")) opts[key] = process.argv[++i];
 else process.exit(0);
}
if (!opts.project || opts.draft || opts.check || opts.submit || opts.retry || opts["check-only"] || opts["dry-run"] || opts.spec || (opts["consultation-provider"] && opts["consultation-provider"] !== "codex")) process.exit(0);
let id = opts["send-id"];
if (id && !uuid.test(id.toLowerCase())) process.exit(0);
id = id ? id.toLowerCase() : crypto.randomUUID();
try {
 const dir = path.join(process.env.HOME, ".issue-desk", "recovery");
 fs.mkdirSync(dir, { mode: 0o700, recursive: true });
 const stat = fs.lstatSync(dir);
 if (!stat.isDirectory() || stat.isSymbolicLink() || stat.uid !== process.getuid() || (stat.mode & 0o077)) throw Error("unsafe directory");
 const parent = fs.openSync(path.dirname(dir), "r"); try { fs.fsyncSync(parent); } finally { fs.closeSync(parent); }
 const file = path.join(dir, id + ".json"), record = { sendId: id, threadId: thread, host: os.hostname(), receiptState: "unreconciled" };
 const temporary = path.join(dir, "." + crypto.randomUUID());
 try {
  const fd = fs.openSync(temporary, "wx", 0o600);
  try { fs.writeFileSync(fd, JSON.stringify(record) + "\n"); fs.fsyncSync(fd); } finally { fs.closeSync(fd); }
  try { fs.linkSync(temporary, file); }
  catch (e) {
   if (e.code !== "EEXIST") throw e;
   const oldStat = fs.lstatSync(file);
   if (!oldStat.isFile() || oldStat.isSymbolicLink() || oldStat.uid !== process.getuid() || (oldStat.mode & 0o077)) throw Error("unsafe record");
   const old = JSON.parse(fs.readFileSync(file, "utf8"));
   if (old.sendId !== id || old.threadId !== thread || old.host !== record.host) throw Error("conflicting record");
  }
 } finally { try { fs.unlinkSync(temporary); } catch (e) { if (e.code !== "ENOENT") throw e; } }
 const fd = fs.openSync(dir, "r"); try { fs.fsyncSync(fd); } finally { fs.closeSync(fd); }
 if (!opts["send-id"]) process.stdout.write(id);
} catch (_) { console.error("回復記録を保存できません。送信しません。"); process.exit(1); }
' -- "$@") || exit 1
 [ -z "$recovery_id" ] || set -- "$@" --send-id "$recovery_id"
fi
wait_writer() {
 deadline=$1
 announced=0
 while :; do
  now=$(date +%s) || return 1
  remaining=$((deadline - now))
  [ "$remaining" -gt 0 ] || { echo '書き手の待機期限を超えました。' >&2; return 1; }
  code=0
  container=$(timeout "$remaining" sh "$nas_dir/deploy/nas/intake.sh" probe) || code=$?
  case "$code" in
   0) [ -n "$container" ] || { echo '書き手の応答が不正です。' >&2; return 1; }; return 0;;
   3|4) [ "$announced" = 1 ] || { echo '書き手の交代を待っています（不在または接続を確認中）。' >&2; announced=1; }; sleep 1;;
   124) echo '書き手の確認が待機期限を超えました。' >&2; return 1;;
   *) return 1;;
  esac
 done
}
wait_writer "$(( $(date +%s) + 180 ))" || exit 1
[ -n "$container" ] || { echo '稼働Serverの書き手を確認できません。' >&2;exit 1; }
unset ISSUE_DESK_URL ISSUE_DESK_TOKEN_FILE
restart_allowed() (
 case "$script" in follow-issues.ts|list-issues.ts|desk-get.ts|create-issue.ts) ;; *) return 1;; esac
 case "$script" in
  desk-get.ts) [ "${1:-}" = GET ] && [ "$#" -ge 2 ] || return 1; case "$2" in */next|*/next\?*|*..*|*//*|*\\*|*'#'*|*' '*) return 1;; /api/*) ;; *) return 1;; esac; shift 2;;
 esac
 checked=0
 seen='|'
 have_desk=0; have_state=0; have_number=0; while [ "$#" -gt 0 ]; do
  option=$1; shift
  case "$seen" in *"|$option|"*) return 1;; esac
  seen="$seen$option|"
  case "$script:$option" in
   list-issues.ts:--duplicates) continue;;
   create-issue.ts:--check) [ "$checked" = 0 ] || return 1; checked=1;;
   follow-issues.ts:--desk|follow-issues.ts:--state|follow-issues.ts:--interval|follow-issues.ts:--stall|follow-issues.ts:--run|list-issues.ts:--project|list-issues.ts:--query|desk-get.ts:--max-bytes|create-issue.ts:--timeout) ;;
   *) return 1;;
  esac
  [ "$#" -gt 0 ] || return 1
  case "$1" in --*) return 1;; esac
  case "$script:$option" in
   follow-issues.ts:--desk) case "$1" in ''|*[!0-9,]*|,*|*,|*,,*) return 1;; esac; have_desk=1;;
   follow-issues.ts:--state) case "$1" in /*) have_state=1;; *) return 1;; esac;;
   create-issue.ts:--check) case "$1" in ''|*[!A-Za-z0-9-]*) return 1;; esac;;
  esac
  case "$script:$option" in create-issue.ts:--timeout|follow-issues.ts:--run|follow-issues.ts:--interval|follow-issues.ts:--stall)
   awk -v value="$1" 'BEGIN { exit !(value ~ /^([0-9]+([.][0-9]*)?|[.][0-9]+)$/ && value+0 > 0) }' || return 1;;
  esac
  shift
 done
 case "$script" in
  create-issue.ts) [ "$checked" = 1 ];;
  follow-issues.ts) [ "$have_desk" = 1 ] && [ "$have_state" = 1 ];;
  list-issues.ts) case "|$seen" in *"|--duplicates|"*) :;; *) return 1;; esac;;
 esac
)
handoff=none
restart_allowed "$@" && handoff=read
[ "$script" != create-issue.ts ] || [ "$handoff" = read ] || handoff=notice
started=$(date +%s) || exit 1
restarts=0
handoff_restarts=0
handoff_deadline=0
# 稼働コンテナの保存先の設定（PostgreSQL）を子へ渡す。渡し忘れると子が保存先を開けず失敗する（SQLiteは開かない）。
# 値は引数・ps に出さず、0600 の一時ファイルの --env-file で渡し、終了時に必ず消す（execしない）。
envfile=$(umask 077; mktemp "$HOME/.issue-desk/.desk-env.XXXXXXXX") || exit 1
trap 'rm -f "$envfile"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
bounded() {
 if [ "$handoff_deadline" -eq 0 ]; then "$@"; return $?; fi
 left=$((handoff_deadline - $(date +%s)))
 [ "$left" -gt 0 ] || { echo '読取の再起動期限を超えました。' >&2; return 1; }
 timeout "$left" "$@"
}
while :; do
image=$(bounded docker inspect --format '{{.Image}}' "$container" 2>/dev/null) || { echo '稼働受付のイメージを期限内に確認できません。' >&2;exit 1; }
# 対象の名前だけを抽出する。改行入りの値はenv-fileで別の変数になり得るため値を出さず拒否する。
# パイプを使わずinspect自体の失敗も検出する。Dockerの診断に環境の値を含めない。
bounded docker inspect --format '{{range .Config.Env}}{{$name := index (split . "=") 0}}{{if or (eq $name "ISSUE_DESK_APP_STORE") (eq $name "ISSUE_DESK_DATABASE_URL") (eq $name "PGHOST") (eq $name "PGUSER") (eq $name "PGDATABASE") (eq $name "PGPASSWORD")}}{{if or (gt (len (split . "\n")) 1) (gt (len (split . "\r")) 1)}}INVALID_STORE_ENV{{println}}{{else}}{{println .}}{{end}}{{end}}{{end}}' "$container" >"$envfile" 2>/dev/null || {
 echo '稼働受付の保存先の環境を読めないため、窓口を起動しません。' >&2;exit 1;
}
app_store=$(awk '
 BEGIN { store="sqlite" }
 # docker inspect --format は出力の最後に空行を1つ付ける。その1つだけ許し、途中の空行・2つ以上は不正にする。
 $0=="" { if (++blank>1) invalid=1; next }
 blank { invalid=1 }
 {
  at=index($0,"="); name=substr($0,1,at-1); value=substr($0,at+1)
  if (!at || name !~ /^(ISSUE_DESK_APP_STORE|ISSUE_DESK_DATABASE_URL|PGHOST|PGUSER|PGDATABASE|PGPASSWORD)$/ || seen[name]++) invalid=1
  if (name=="ISSUE_DESK_APP_STORE") { store=tolower(value); gsub(/^[[:space:]]+|[[:space:]]+$/, "", store); if (store=="") store="sqlite" }
  if (name=="ISSUE_DESK_DATABASE_URL" && value ~ /[^[:space:]]/) dsn=1
 }
 END {
  if (invalid || (store!="sqlite" && store!="postgres")) exit 1
  if (store=="postgres" && !dsn) exit 2
  print store
 }
' "$envfile") || {
 case $? in
  2) echo '受付の保存先はPostgreSQLですが、窓口へ渡すISSUE_DESK_DATABASE_URLが未設定です。' >&2;;
  *) echo '稼働受付の保存先の環境が不正です（保存先の指定・重複・改行を確認してください）。窓口を起動しません。' >&2;;
 esac
 exit 1
}
# adminでも転送後の設定を照合する。未設定のsqliteをPostgreSQLへ自動推定しない。
printf 'ISSUE_DESK_EXPECTED_APP_STORE=%s\n' "$app_store" >>"$envfile" || exit 1
[ "$handoff_deadline" -eq 0 ] || [ "$(date +%s)" -lt "$handoff_deadline" ] || { echo '読取の再起動期限を超えました。' >&2; exit 1; }
child_started=$(date +%s) || exit 1
# execするので子の終了コード（引き継ぎの75を含む）はそのまま返る。
docker run --rm -i --pull never --env-file "$envfile" --network "container:$container" --user 1000:1000 \
 -v issue-desk-intake-state:/state -v "$HOME:$HOME" -e HOME="$HOME" -e ISSUE_DESK_CONFIG=/state/config.local.json -e ISSUE_DESK_NAS_HANDOFF="$handoff" -e ISSUE_DESK_NAS_STARTED="$started" -e ISSUE_DESK_NAS_RESTARTS="$restarts" \
 -w "$PWD" --entrypoint sh "$image" -c 'exec node --import /app/deploy/nas/chat-local.ts "$@"' sh "/app/scripts/$script" "$@"
code=$?
[ "$code" = 75 ] && [ "$handoff" = read ] || exit "$code"
now=$(date +%s) || exit 1
# 180秒以上稼働した子の切断は、新しい引き継ぎとして扱う。
if [ "$((now - child_started))" -ge 180 ]; then
 handoff_deadline=0
 handoff_restarts=0
fi
[ "$handoff_deadline" -ne 0 ] || handoff_deadline=$((now + 180))
[ "$handoff_restarts" -lt 3 ] || { echo '読取の再起動回数の上限に達しました。' >&2; exit 1; }
[ "$now" -lt "$handoff_deadline" ] || { echo '読取の再起動期限を超えました。' >&2; exit 1; }
wait_writer "$handoff_deadline" || exit 1
restarts=$((restarts + 1))
handoff_restarts=$((handoff_restarts + 1))
done
WRAPPER
 chmod 755 "$temporary" || return 1
 # PCトークン窓口は、Ubuntu PCと同じ本体（deploy/clients/remote-control/desk-access.sh）を使う。PCトークンの発行・写しは行わない。
 [ ! -L "$base/desk-pc" ] && [ ! -d "$base/desk-pc" ] || return 1
 temporary_pc=$(mktemp "$base/.desk-pc.XXXXXXXX") || return 1
 trap 'rm -f "$temporary" "$temporary_pc"' EXIT
 (. "$DIR/deploy/clients/remote-control/desk-access.sh" && linux_desk_access_body) >"$temporary_pc" || return 1
 chmod 755 "$temporary_pc" || return 1
 # 先にPCトークン窓口を置く（一時キー窓口が先に振り分けても、行き先が必ずある状態にする）。
 if ! { [ -x "$base/desk-pc" ] && cmp -s "$temporary_pc" "$base/desk-pc"; }; then
  mv -f "$temporary_pc" "$base/desk-pc" || return 1
  say '[update:desk-access] NASのPCトークン窓口（PR作成・マージ用）を更新しました'
 fi
 if [ -x "$base/desk" ] && cmp -s "$temporary" "$base/desk"; then return 0; fi
 mv -f "$temporary" "$base/desk" || return 1
 say '[update:desk-access] NASの一時キー窓口を更新しました'
)
