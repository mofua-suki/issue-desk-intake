#!/bin/sh
# 他マシンへのSSHは、利用者の明示の許可（受付画面の承認、または利用者の発言を引用した host-ssh の許可付与）がある
# 申請だけが発券される。ここは申請して待ち、承認されたときだけ短命の証明書で決まった操作を実行する。
# 呼び出し元専用。CA・署名役の資格情報は読み取らない。
set +x
set -eu
umask 077
if [ -r /var/lib/issue-desk-signer/ca ]; then
  printf '%s\n' 'CA秘密鍵を読める利用者はホスト作業用SSHを実行できません。' >&2
  exit 1
fi
PATH=/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH
work=
public_work=
ticket=
report=0
connection=failed
exit_code=null
summary='ホスト作業用SSHの発券または接続に失敗しました。'
api_limit=30
api=https://issue-desk.example.ts.net/api/ssh-tickets

api_call() {
  method=$1
  endpoint=$2
  expected=$3
  shift 3
  code=$(curl --disable --silent --proto '=https' --connect-timeout 10 --max-time "$api_limit" \
    --request "$method" --header "@$work/header" --header 'Content-Type: application/json' \
    --output "$work/response" --write-out '%{http_code}' "$@" "$api$endpoint" 2>/dev/null) || return 1
  [ "$code" = "$expected" ]
}
. "$(dirname -- "$0")/host-ssh-connect.sh"
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

host=
operation=
reason=
evidence=
emergency=false
input=
repository=
direct_pr_proof=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --host|--operation|--reason|--evidence|--input|--repo|--direct-pr-proof)
      [ "$#" -ge 2 ] || fail 'ホスト作業用SSHの引数が不正です。'
      case "$1" in
        --host) host=$2 ;;
        --operation) operation=$2 ;;
        --reason) reason=$2 ;;
        --evidence) evidence=$2 ;;
        --input) input=$2 ;;
        --repo) repository=$2 ;;
        --direct-pr-proof) direct_pr_proof=$2 ;;
      esac
      shift 2 ;;
    --emergency) emergency=true; shift ;;
    *) fail 'ホスト作業用SSHの引数が不正です。' ;;
  esac
done
[ -n "$host" ] && [ -n "$operation" ] && [ -n "$reason" ] || fail 'ホスト作業用SSHの引数が不正です。'
[ -n "$evidence" ] || evidence=$reason
# --input は、操作の標準入力へ渡すファイル（例: git archive の tar.gz。接続先に認証情報を置かずに作業ツリーを送る）。
[ -z "$input" ] || [ -f "$input" ] || fail 'ホスト作業用SSHの入力ファイルがありません。'
repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
# 送り先の選択（ssh-host-select.ts）と出力の整理（host-ssh-output.ts）は TypeScript なので、固定した PATH の node（NAS本体では v18）ではなく、
# 実行できる node を決まった場所から選んで明示して使う。申請の前に確かめ、無ければ探した場所を示して止める。
. "$repo/deploy/nas-host/host-node.sh"
ts_node=$(typescript_node "$HOME/.local/bin/node" /usr/local/bin/node /usr/bin/node /opt/homebrew/bin/node) || fail 'ホスト作業用SSHの送り先の選択と出力の整理に使う node がありません。'
for tool in node curl ssh-keygen sudo ssh timeout; do
  command -v "$tool" >/dev/null 2>&1 || fail 'ホスト作業用SSHに必要なコマンドがありません。'
done
work=$(mktemp -d "${TMPDIR:-/tmp}/issue-desk-ssh.XXXXXXXX") || fail 'ホスト作業用SSHを準備できません。'
chmod 700 "$work"
# --host auto は、--repo の実装担当のうち担当表の順位が最も高いPCを選ぶ（scripts/ssh-host-select.ts）。
if [ "$host" = auto ]; then
  [ -n "$repository" ] || fail "--host auto には --repo <owner/repo> が必要です。"
  host=$("$ts_node" "$repo/scripts/ssh-host-select.ts" --repo "$repository") || fail "担当表から送り先のPCを選べません。"
  printf "送り先: %s（%s の担当表の順位で選択）\n" "$host" "$repository"
elif [ -n "$repository" ]; then
  fail "--repo は --host auto と一緒に使います。"
fi
# JSONはシェルで展開・評価せず、接続先も方針の一覧から選ぶ。
if ! node -e '
  const fs = require("node:fs");
  const [dir, repo, host, operation, reason, evidence, emergency, tokenFile, directPRProof] = process.argv.slice(1);
  const config = JSON.parse(fs.readFileSync(repo + "/host-ssh.json", "utf8"));
  if (!Object.hasOwn(config.hosts, host) || !Object.hasOwn(config.operations, operation)) process.exit(1);
  const entry = config.hosts[host];
  if (!/^[a-zA-Z0-9][a-zA-Z0-9._-]*$/.test(entry.user) ||
      !/^[a-z0-9][a-z0-9.-]*$/.test(entry.machine)) process.exit(1);
  const token = fs.readFileSync(tokenFile, "utf8").trim();
  if (!/^idk_[A-Za-z0-9_-]{43}$/.test(token)) process.exit(1);
  fs.writeFileSync(dir + "/header", "Authorization: Bearer " + token + "\n");
  fs.writeFileSync(dir + "/destination", entry.user + "@" + entry.machine);
  let directPR;
  if (directPRProof) {
    if (operation !== "verify" || emergency === "true" ||
        fs.statSync(directPRProof).size > 300000) process.exit(1);
    directPR = JSON.parse(fs.readFileSync(directPRProof, "utf8"));
  }
  fs.writeFileSync(dir + "/request", JSON.stringify({host, operation, reason, evidence, emergency: emergency === "true", directPR}));
' "$work" "$repo" "$host" "$operation" "$reason" "$evidence" "$emergency" \
  "${ISSUE_DESK_TOKEN_FILE:-$HOME/.issue-desk/token}" "$direct_pr_proof" 2>/dev/null; then
  fail 'ホスト作業用SSHの設定または入力を確認できません。'
fi
api_call POST '' 201 --data-binary "@$work/request" || fail 'ホスト作業用SSHを申請できません。'
ticket=$(node -e '
  const value = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
  if (!/^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/.test(value.id)) process.exit(1);
  process.stdout.write(value.id);
' "$work/response" 2>/dev/null) || fail 'ホスト作業用SSHの申請応答が不正です。'
printf '%s\n' 'ホスト作業用SSHの承認を待っています（最大30分）。'
deadline=$(($(date +%s) + 1800))
while :; do
  remaining=$((deadline - $(date +%s)))
  [ "$remaining" -gt 0 ] || fail 'ホスト作業用SSHの承認待ちを打ち切りました。'
  api_limit=30
  [ "$remaining" -ge 30 ] || api_limit=$remaining
  api_call GET "/$ticket" 200 || fail 'ホスト作業用SSHの承認状態を確認できません。'
  [ "$(date +%s)" -lt "$deadline" ] || fail 'ホスト作業用SSHの承認待ちを打ち切りました。'
  state=$(node -e '
    const value = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
    if (value.id !== process.argv[2] || !["requested", "approved", "denied", "expired"].includes(value.status)) process.exit(1);
    process.stdout.write(value.status);
  ' "$work/response" "$ticket" 2>/dev/null) || fail 'ホスト作業用SSHの承認応答が不正です。'
  case "$state" in
    approved) break ;;
    denied) fail 'ホスト作業用SSHの申請は却下されました。' ;;
    expired) fail 'ホスト作業用SSHの申請は期限切れです。' ;;
  esac
  remaining=$((deadline - $(date +%s)))
  [ "$remaining" -gt 0 ] || fail 'ホスト作業用SSHの承認待ちを打ち切りました。'
  pause=5
  [ "$remaining" -ge 5 ] || pause=$remaining
  sleep "$pause"
done
api_limit=30
# 公開鍵だけを別の受け渡し場所へ写す。秘密鍵ディレクトリは0700を保つ。
ssh-keygen -q -t ed25519 -N '' -C host-ssh -f "$work/key" >/dev/null 2>&1 || fail 'ホスト作業用SSHの鍵を作成できません。'
# TMPDIRは利用者専用で他ユーザーが通れないことがあるため、公開鍵は/tmpで渡す。
public_work=$(mktemp -d /tmp/issue-desk-ssh-public.XXXXXXXX) || fail 'ホスト作業用SSHの公開鍵を準備できません。'
cp "$work/key.pub" "$public_work/key.pub"
chmod 644 "$public_work/key.pub"
chmod 755 "$public_work"
report=1
sudo -n -u issue-desk-signer /usr/local/libexec/issue-desk-ssh-sign "$ticket" "$public_work/key.pub" \
  >"$work/key-cert.pub" 2>/dev/null || fail 'ホスト作業用SSHの証明書を取得できません。'
[ -s "$work/key-cert.pub" ] || fail 'ホスト作業用SSHの証明書を取得できません。'
known_dir=$HOME/.issue-desk/host-ssh
mkdir -p "$known_dir" || fail 'ホスト作業用SSHの接続記録を準備できません。'
[ ! -L "$known_dir/known_hosts" ] || fail 'ホスト作業用SSHの接続記録を準備できません。'
chmod 700 "$known_dir"
touch "$known_dir/known_hosts"
chmod 600 "$known_dir/known_hosts"
destination=$(cat "$work/destination")
ssh_timeout=300
[ "$operation" != verify-intake ] || ssh_timeout=1800
# 精算アプリの固定テストは容器内の1800秒の打ち切りより後に切る
case "$operation" in verify-sample-expense-server|verify-sample-expense-client) ssh_timeout=1900 ;; esac
[ "$operation" != develop ] || ssh_timeout=3500
# 汎用の固定テスト（scripts/ssh-verify.ts）は接続先の容器全体の5400秒の打ち切りより後に切る
[ "$operation" != verify ] || ssh_timeout=5600
[ "$operation" != verify-pytest-6357 ] || ssh_timeout=2750
[ "$operation" != verify-docker-6293-6358 ] || ssh_timeout=5500
# MacのiOSシミュレーターのテストはビルドと端末テストを含み、初回はiOSの実行環境・依存の取得とビルドで長くかかる
[ "$operation" != verify-sample-expense-client-ios ] || ssh_timeout=3500
[ "$operation" != probe-worker-apple-ios ] || ssh_timeout=3500
# Macのrunnerの入れ替えはWorkerのビルド（初回は依存の取得を含む）と検査・再起動
[ "$operation" != deploy-worker-apple-runner ] || ssh_timeout=1800
run_connection
