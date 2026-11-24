#!/usr/bin/env bash
# 導入の入口（bootstrap-ubuntu.sh）から呼ぶ段③ clientの配置と段④ トークンの配置。単独でも叩ける（何度叩いても済んだ項目は飛ばす）。
# 実行: bash bootstrap-ubuntu-client.sh <作業フォルダー> [--tag <sha12>]（root では動かさない）。
# 秘密（レジストリのパスワード・受付のトークン）は NAS からの SSH の標準出力を標準入力へ直接つなぐだけで、引数・ログ・表示・PC のファイルには出さない。
set +x
set -Eeuo pipefail
umask 077

nas=${BOOTSTRAP_NAS:-user@192.168.0.10}
nas_dir=${BOOTSTRAP_NAS_DIR:-Workspace/issue-desk-intake}
server_url=${BOOTSTRAP_SERVER_URL:-https://issue-desk.example.ts.net/}
# 入口が作った 0700 の一時フォルダー（制御用ソケット）。単独で叩いたときは空で、通常の ssh を使う。
ctl=${BOOTSTRAP_SSH_CTL:-}
token_file=/state/private/intake-client-token
state_volume=issue-desk-worker-state

ok() { printf '[済み] %s\n' "$*"; }
skip() { printf '[飛ばした] %s\n' "$*"; }
stop() { printf '[止めた] %s\n' "$*" >&2; exit 1; }
trap 'printf "[止めた] 失敗しました（%s 行目: %s）。表示された原因を直し、入口の同じ1行をもう一度叩いてください。\n" "$LINENO" "$BASH_COMMAND" >&2' ERR
trap 'exit 130' INT
trap 'exit 143' TERM

[[ $(id -u) -ne 0 ]] || stop 'root では実行しません。'
[[ $# -ge 1 ]] || stop '使い方: bootstrap-ubuntu-client.sh <作業フォルダー> [--tag <sha12>]'
work=$(cd -- "$1" 2>/dev/null && pwd) || stop "作業フォルダーがありません: $1"
shift
want_tag=''
while [[ $# -gt 0 ]]; do
  case $1 in
  --tag)
    [[ $# -ge 2 && $2 =~ ^[0-9a-f]{12}$ ]] || stop '--tag には 12 桁の小文字 16 進（sha12）を渡してください。'
    want_tag=$2
    shift 2
    ;;
  *) stop "不明な引数です: $1" ;;
  esac
done
[[ $nas =~ ^[A-Za-z0-9][A-Za-z0-9._@:-]*$ ]] || stop "BOOTSTRAP_NAS の形が不正です: $nas"
[[ $nas_dir =~ ^[A-Za-z0-9._][A-Za-z0-9._/-]*$ ]] || stop "BOOTSTRAP_NAS_DIR の形が不正です: $nas_dir"
[[ $server_url =~ ^https?://[A-Za-z0-9._:/-]+$ ]] || stop "BOOTSTRAP_SERVER_URL の形が不正です: $server_url"
# 担当表の名前。入口が段②で決めて渡す（単独で叩いたときは OS のホスト名）。
host=${BOOTSTRAP_HOST:-$(hostname | tr '[:upper:]' '[:lower:]')}
[[ $host =~ ^[a-z0-9][a-z0-9-]{0,30}$ ]] || stop "ホスト名の形が不正です: ${host:-空}"

nas_ssh() {
  if [[ -n $ctl ]]; then
    local status=0
    ssh -F /dev/null -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o "ControlPath=$ctl/ctl" "$nas" "$@" || status=$?
    if [[ $status -ne 0 ]]; then
      printf '%s\n' 'SSH の診断を確認してください。NAS のホスト鍵を確認できない場合は、入口取得の1行で本人が先に鍵を確認・記録してください。' >&2
    fi
    return "$status"
  else
    ssh "$nas" "$@"
  fi
}
# NAS の受付コンテナ内のスクリプトを実行する遠隔コマンド（引数は固定の語と検証済みの値だけ）。
intake_exec() { printf 'sh ~/%s/deploy/nas/intake.sh exec -T /usr/local/bin/intake admin --config /state/config.local.json --application-dir /app %s' "$nas_dir" "$1"; }

flavor=$(node - "$work/fleet.json" "$host" <<'JS'
const fs = require('node:fs');
const [file, host] = process.argv.slice(2);
let fleet;
try { fleet = JSON.parse(fs.readFileSync(file, 'utf8')); } catch {
  console.error('段③ fleet.json を読めません。NAS の fleet.json を確認して入口から取り直してください。');
  process.exit(1);
}
if (!Object.hasOwn(fleet.hosts ?? {}, host)) {
  console.error('段③ ホスト名 ' + host + ' が fleet.json の hosts にありません。OS・tailnet の名前を登録名に揃えるか、fleet.json への追加を PR で依頼して入口から取り直してください。');
  process.exit(1);
}
process.stdout.write(fleet.hosts[host]?.python ? '-python' : '');
JS
) || stop '段③ flavor を決められません。上の理由を直して同じ1行をもう一度叩いてください。'
suffix=''
if [[ $flavor == -python ]]; then
  suffix=-client-python
fi

# --- 段③: タグ ---
if [[ -z $want_tag ]]; then
  revision=$(nas_ssh "$(intake_exec "client-release previous '$flavor'")" </dev/null) ||
    stop '段③ 受付に登録された望む版を NAS から読めません。SSH と受付の稼働を確認するか、--tag <sha12> で指定してください。'
  revision=${revision//$'\r'/}
  [[ $revision =~ ^[0-9a-f]{40}$ ]] ||
    stop "段③ 受付が返した望む版の形が違います（sha40 を期待）。まだ登録が無い可能性があります。--tag <sha12> で指定してください。"
  want_tag=${revision:0:12}
fi
tag=$want_tag$suffix

# --- 段③: 正本の compose.yaml と .env ---
bootstrap_src=$work/deploy/clients
worker_dir=${BOOTSTRAP_WORKER_NAS_DIR:-.local/share/issue-desk/worker.git}
[[ $worker_dir =~ ^[A-Za-z0-9._][A-Za-z0-9._/-]*$ ]] || stop 'Worker配置先の形が不正です。'
worker_bundle=$(mktemp -d)
trap 'rm -rf -- "$worker_bundle"' EXIT
nas_ssh "git -C '$worker_dir' fetch --quiet origin trunk && git -C '$worker_dir' archive --format=tar '$want_tag' deploy" >"$worker_bundle/worker.tar" || stop '望むWorker版の配置資産を取得できません。'
mkdir -p "$work/worker"
tar -xf "$worker_bundle/worker.tar" -C "$work/worker" || stop 'Workerの配置資産を展開できません。'
rm -rf -- "$worker_bundle"
trap - EXIT
src=$work/worker/deploy
if [[ -f $work/compose.yaml ]] && cmp -s "$src/compose.yaml" "$work/compose.yaml"; then
  skip '段③ compose.yaml'
else
  cp -- "$src/compose.yaml" "$work/compose.yaml"
  ok "段③ 正本の compose.yaml を置いた: $work/compose.yaml"
fi
env_get() { sed -n "s/^$1=//p" "$work/.env" | sed -n '$p'; }
[[ ! -e $work/.linux-update.pending ]] ||
  stop "段③ 前回の更新が未完了です。退避を上書きしません。bash $src/update-linux.sh $work --restore-previous で復帰し、原因を直して入口の同じ1行をもう一度叩いてください。"
if [[ -f $work/.env ]]; then
  current_flavor=$(env_get FLAVOR)
  [[ $current_flavor == "$flavor" ]] ||
    stop "段③ 既存の .env の FLAVOR (${current_flavor:-空}) が fleet.json の指定 (${flavor:-空}) と違います。.env は上書きしません。切り替えるには $work/.env を別名で退避し、FLAVOR=$flavor に編集し、ISSUE_DESK_CLIENT_TAG と ISSUE_DESK_CLIENT_BLUE_TAG を $tag に揃えてください。次で取得してから更新します（それぞれレジストリのパスワードを聞かれます）: bash $work/worker/deploy/update-linux.sh $work $tag --pull-only 、続いて bash $work/worker/deploy/update-linux.sh $work $tag"
  current=$(env_get ISSUE_DESK_CLIENT_BLUE_TAG)
  [[ -n $current ]] || current=$(env_get ISSUE_DESK_CLIENT_TAG)
  [[ $current =~ ^[0-9a-f]{12}${suffix}$ ]] || stop '段③ 既存の .env のタグが空または不正です。設定を確認して入口を叩き直してください。'
  if [[ $current != "$tag" ]]; then
    source "$bootstrap_src/bootstrap-ubuntu-update.sh"
    ok "段③ 望む版 $tag へ更新した"
  else
    skip "段③ .env（タグ $tag）"
  fi
else
  sed -e "s/^FLAVOR=.*/FLAVOR=$flavor/" -e "s/^ISSUE_DESK_CLIENT_TAG=.*/ISSUE_DESK_CLIENT_TAG=$tag/" "$bootstrap_src/.env.example" >"$work/.env.new"
  mv -n -- "$work/.env.new" "$work/.env"
  ok "段③ .env を .env.example から作った（FLAVOR=${flavor:-空} タグ $tag）"
fi
registry=$(env_get REGISTRY)
[[ -n $registry ]] || stop "段③ $work/.env に REGISTRY がありません。"
image="$registry/issue-desk-client$(env_get FLAVOR):$tag"

# --- 段③: 初回取得（パスワードは SSH の出力を標準入力へ直接渡す） ---
if docker image inspect "$image" >/dev/null 2>&1; then
  skip "段③ イメージ $image は取得済み"
else
  if ! nas_ssh 'cat ~/.config/issue-desk/registry.password' </dev/null |
    bash "$src/update-linux.sh" "$work" "$tag" --pull-only >/dev/null; then
    stop "段③ イメージを取得できません。NAS の ~/.config/issue-desk/registry.password を SSH で読めるか、レジストリへ届くかを確認してください。"
  fi
  ok "段③ イメージ $image を取得した"
fi

run_c() { docker run --rm --pull never --log-driver none --read-only --user 0 "$@"; }

# --- 段③: volume と持ち主 ---
volumes=(issue-desk-worker-state issue-desk-worker-codex issue-desk-worker-cli issue-desk-worker-agy)
mounts=(-v issue-desk-worker-state:/state -v issue-desk-worker-codex:/data/codex -v issue-desk-worker-cli:/data/claude -v issue-desk-worker-agy:/data/agy)
dirs=(/state /data/codex /data/claude /data/agy)
green=0
for optional in state-green colors; do
  volume=issue-desk-worker-$optional
  referenced=0
  if grep -Fq -- "$volume" "$work/compose.yaml"; then referenced=1; fi
  if [[ $optional == colors ]] && grep -Fq -- "$volume" "$src/update-linux.sh"; then referenced=1; fi
  if ((referenced)); then
    volumes+=("$volume")
    mounts+=(-v "$volume:/$([[ $optional == colors ]] && printf worker-colors || printf state-green)")
    dirs+=("/$([[ $optional == colors ]] && printf worker-colors || printf state-green)")
    [[ $optional != state-green ]] || green=1
  fi
done
for volume in "${volumes[@]}"; do
  if docker volume inspect "$volume" >/dev/null 2>&1; then
    skip "段③ volume $volume"
  else
    docker volume create "$volume" >/dev/null
    ok "段③ volume $volume を作った"
  fi
done
if run_c "${mounts[@]}" --entrypoint sh "$image" -c 'for d in "$@"; do [ "$(stat -c %u:%g "$d")" = 1000:1000 ] || exit 1; done' sh "${dirs[@]}" </dev/null >/dev/null 2>&1; then
  skip '段③ 対象 volume の持ち主は node:node'
else
  run_c "${mounts[@]}" --entrypoint chown "$image" -R node:node "${dirs[@]}" </dev/null
  ok '段③ 対象 volume の持ち主を node:node にした（消さずに再利用）'
fi

# --- 段③: 設定3項目 ---
read -r -d '' config_program <<'JS' || true
const fs = require('node:fs');
const [workerId, serverUrl, serverTokenFile] = process.argv.slice(1);
const file = '/state/config.local.json';
let existing;
try { existing = JSON.parse(fs.readFileSync(file, 'utf8')); } catch (e) {
  if (e.code !== 'ENOENT') { console.error('state の config.local.json を読めません'); process.exit(1); }
}
if (existing) {
  if (existing.workerId !== workerId) {
    console.error('state の config.local.json の workerId (' + existing.workerId + ') がこの PC (' + workerId + ') と違います');
    process.exit(4);
  }
  console.log('exists');
} else {
  const base = JSON.parse(fs.readFileSync(0, 'utf8'));
  const text = JSON.stringify({ ...base, workerId, serverUrl, serverTokenFile }, null, 2) + '\n';
  fs.writeFileSync(file + '.new', text, { mode: 0o600 });
  fs.chownSync(file + '.new', 1000, 1000);
  fs.renameSync(file + '.new', file);
  console.log('created');
}
JS
config_state=$(run_c -i -v "$state_volume:/state" --entrypoint node "$image" -e "$config_program" "$host" "$server_url" "$token_file" \
  <<<'{}') ||
  stop '段③ blue の /state/config.local.json を作れません。上の理由を直してください（workerId が違うときは state の設定が別の PC のものです）。'
if [[ $config_state == created ]]; then
  ok "段③ /state/config.local.json を作った（workerId=$host serverUrl=$server_url）"
else
  skip "段③ /state/config.local.json（workerId=$host）"
fi

if ((green)); then
  run_c -v issue-desk-worker-state:/blue:ro -v issue-desk-worker-state-green:/green --entrypoint sh "$image" -c 'if [ ! -f /green/config.local.json ]; then cp /blue/config.local.json /green/config.local.json; chmod 600 /green/config.local.json; chown 1000:1000 /green/config.local.json; fi' </dev/null
fi

# --- 段④: 受付のPC別トークン（NAS で発行した出力を SSH の標準出力から使い捨てコンテナの標準入力へ直接渡す） ---
placed() { run_c -v "$state_volume:/state:ro" --entrypoint test "$image" -s "$1" </dev/null >/dev/null 2>&1; }
read -r -d '' intake_token_program <<'JS' || true
const fs = require('node:fs');
const path = require('node:path');
const secret = fs.readFileSync(0, 'utf8').trim();
for (const file of process.argv.slice(1)) {
if (!/^idk_[A-Za-z0-9_-]{43}$/.test(secret)) { console.error('受付の出力がトークンの形ではありません'); process.exit(1); }
const dir = path.dirname(file);
fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
fs.chmodSync(dir, 0o700);
fs.chownSync(dir, 1000, 1000);
const temporary = file + '.new';
fs.rmSync(temporary, { force: true });
fs.writeFileSync(temporary, secret, { mode: 0o600 });
fs.chownSync(temporary, 1000, 1000);
fs.chmodSync(temporary, 0o600);
fs.renameSync(temporary, file);
}
JS
if placed "$token_file"; then
  skip "段④ 受付のトークン（$token_file）"
else
  listing=$(nas_ssh "$(intake_exec 'pc-token list')" </dev/null) ||
    stop '段④ 受付のトークンの一覧を NAS から読めません。SSH と受付の稼働を確認してください。'
  if printf '%s\n' "$listing" | grep -q "^$host"$'\t'; then
    printf '段④ 受付には %s の有効なトークンが既にありますが、この PC に置かれていません。失効させて発行し直します（古いトークンを使う他の接続は止まります）。よければ y を入力してください: ' "$host" >&2
    answer=''
    read -r answer || true
    [[ ${answer//$'\r'/} == y ]] || stop "段④ 確認が y ではないため、$host のトークンの失効・発行は行わずに止めました。"
    nas_ssh "$(intake_exec "pc-token revoke $host")" </dev/null >/dev/null ||
      stop "段④ $host のトークンを失効できません。NAS の受付の稼働を確認してください。"
    ok "段④ $host の既存トークンを失効させた"
  fi
  token_mounts=(-v "$state_volume:/state")
  token_paths=(/state/private/intake-client-token)
  if ((green)); then
    token_mounts+=(-v issue-desk-worker-state-green:/state-green)
    token_paths+=(/state-green/private/intake-client-token)
  fi
  if ! nas_ssh "$(intake_exec "pc-token issue $host")" </dev/null |
    run_c -i "${token_mounts[@]}" --entrypoint node "$image" -e "$intake_token_program" "${token_paths[@]}" >/dev/null; then
    stop "段④ 受付のトークンを発行して置けません。原因を直して同じ1行をもう一度叩くと、発行済みの $host のトークンを失効して発行し直します。"
  fi
  ok "段④ 受付のトークンを $token_file に置いた"
fi

# --- 段④: 監視用トークン（worker-token-linux.sh。秘密は同スクリプトが標準入力で使い捨てコンテナへ渡す） ---
bash "$bootstrap_src/worker-token-linux.sh" "$work" || stop '段④ 監視用トークンの本人照合・配置が完了しません。表示された理由を確認してください。'
ok '段④ 対象の監視用トークンを本人照合した'
