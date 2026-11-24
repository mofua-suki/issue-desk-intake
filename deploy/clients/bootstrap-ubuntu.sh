#!/usr/bin/env bash
# Ubuntu を入れた直後に本人の Bash で叩く導入の入口。段①〜⑦の後に段⑧（bootstrap-ubuntu-hostd.sh）を冪等に進める。
# 実行: bash bootstrap-ubuntu.sh（root では動かさない。sudo は必要な処理だけで使う）。
# 結果は 1 行ずつ表示する: [済み]=今回実行した / [飛ばした]=実際の状態で既に満たしていた / [止めた]=直して再実行が要る。
set -Eeuo pipefail
# bootstrap-pinned-v1: 入口はこの印を検査し、古い版を実行しない。

nas=${BOOTSTRAP_NAS:-user@192.168.0.10}
nas_dir=${BOOTSTRAP_NAS_DIR:-Workspace/issue-desk-intake}
work=${BOOTSTRAP_DIR:-$HOME/issue-desk-client}
# 次の 2 つは検証用の差し替え口。通常は既定のまま使う（install-node.sh は /tmp/image-versions.env を読む）。
versions_env=${BOOTSTRAP_VERSIONS_ENV:-/tmp/image-versions.env}
os_release=${BOOTSTRAP_OS_RELEASE:-/etc/os-release}
userns_key=kernel.apparmor_restrict_unprivileged_userns
userns_conf=/etc/sysctl.d/60-issue-desk-userns.conf
ctl=''
master=0

ok() { printf '[済み] %s\n' "$*"; }
skip() { printf '[飛ばした] %s\n' "$*"; }
stop() { printf '[止めた] %s\n' "$*" >&2; exit 1; }
has() { command -v "$1" >/dev/null 2>&1; }
trap 'printf "[止めた] 失敗しました（%s 行目: %s）。表示された原因を直し、同じ1行をもう一度叩いてください。\n" "$LINENO" "$BASH_COMMAND" >&2' ERR
cleanup() {
  local status=$?
  if [[ $master -eq 1 ]]; then ssh "${ssh_opts[@]}" -O exit "$nas" >/dev/null 2>&1 || true; fi
  if [[ -n $ctl ]]; then rm -rf -- "$ctl"; fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

[[ $(id -u) -ne 0 ]] || stop 'root では実行しません。本人のアカウントの Bash で bash bootstrap-ubuntu.sh と叩いてください（sudo は必要な処理だけで使います）。'
has sudo || stop 'sudo がありません。本人が sudo を使えるようにしてから叩き直してください。'
[[ $nas =~ ^[A-Za-z0-9][A-Za-z0-9._@:-]*$ ]] || stop "BOOTSTRAP_NAS の形が不正です: $nas"
[[ $nas_dir =~ ^[A-Za-z0-9._][A-Za-z0-9._/-]*$ ]] || stop "BOOTSTRAP_NAS_DIR の形が不正です: $nas_dir"
entry='ssh "${BOOTSTRAP_NAS:-user@192.168.0.10}" "sh ~/Workspace/issue-desk-intake/deploy/clients/bootstrap-entry.sh" > ~/bootstrap-ubuntu.sh && bash ~/bootstrap-ubuntu.sh'
revision=${BOOTSTRAP_REVISION:-}
[[ $revision =~ ^[0-9a-f]{40}$ ]] || stop "取得版の40桁SHAを確定できません。入口から再試行してください: $entry"
printf '取得版: %s（入口で選んだ稼働受付版。配置先HEADとは別です）\n' "$revision"

# --- ファイルの取得: 0700 の一時フォルダーの制御用ソケットで認証を 1 回にする。常設の鍵・SSH 設定は足さない ---
ctl=$(mktemp -d)
ssh_opts=(-F /dev/null -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o "ControlPath=$ctl/ctl")
master=1
ssh "${ssh_opts[@]}" -o ControlMaster=yes -fN "$nas" || stop "NAS ($nas) へ SSH できません。上の SSH の診断を確認してください。NAS のホスト鍵を確認できない場合は、入口取得の1行で本人が先に鍵を確認・記録してください。宛先は BOOTSTRAP_NAS で変えられます。"
# 完全な転送・展開が済むまで既存の作業フォルダーに触れず、段①以降も始めない。
ssh "${ssh_opts[@]}" "$nas" "git -C '$nas_dir' cat-file -e '$revision^{commit}' && git -C '$nas_dir' archive --format=tar '$revision' deploy/clients deploy/install-node.sh deploy/versions.json assignments.json fleet.json" >"$ctl/bundle.tar" ||
  stop "確定SHA $revision の一式を取得できません。上の SSH の診断とGitのコミットを確認してください。NAS のホスト鍵を確認できない場合は、入口取得の1行で本人が先に鍵を確認・記録してください。入口から再試行してください: $entry"
mkdir -- "$ctl/bundle"
tar -xf "$ctl/bundle.tar" -C "$ctl/bundle" || stop "確定SHA $revision の転送一式が不完全です。入口から再試行してください: $entry"
mkdir -p -- "$work"
cp -R -- "$ctl/bundle/." "$work/"
ok "ファイルの取得: $work（deploy/clients・install-node.sh・versions.json・assignments.json・fleet.json）"

code=$(. "$os_release" && printf '%s' "${UBUNTU_CODENAME:-${VERSION_CODENAME:-}}")
[[ -n $code ]] || stop "$os_release から Ubuntu のコードネームを読めません。"
user=$(id -un)

# --- 段①: OS の前提 ---
if has git && has curl; then
  skip '段① git・curl'
else
  sudo apt-get update
  sudo apt-get install -y ca-certificates git curl
  ok '段① git・curl を入れた'
fi

if has docker && docker compose version >/dev/null 2>&1; then
  skip '段① Docker Engine と Compose plugin'
else
  arch=$(dpkg --print-architecture)
  sudo install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo tee /etc/apt/keyrings/docker.asc >/dev/null
  sudo chmod a+r /etc/apt/keyrings/docker.asc
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu %s stable\n' "$arch" "$code" |
    sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
  sudo apt-get update
  sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  { has docker && docker compose version >/dev/null 2>&1; } || stop 'Docker または Compose plugin を入れても使えません。apt の出力を確認してください。'
  ok '段① Docker Engine と Compose plugin を入れた'
fi

if systemctl is-enabled docker >/dev/null 2>&1 && systemctl is-active docker >/dev/null 2>&1; then
  skip '段① docker の systemd'
else
  sudo systemctl enable --now docker
  ok '段① docker を systemd で有効化して起動した'
fi

if has tailscale; then
  skip '段① Tailscale'
else
  curl -fsSL "https://pkgs.tailscale.com/stable/ubuntu/$code.noarmor.gpg" | sudo tee /usr/share/keyrings/tailscale-archive-keyring.gpg >/dev/null
  curl -fsSL "https://pkgs.tailscale.com/stable/ubuntu/$code.tailscale-keyring.list" | sudo tee /etc/apt/sources.list.d/tailscale.list >/dev/null
  sudo apt-get update
  sudo apt-get install -y tailscale
  has tailscale || stop 'Tailscale を入れても tailscale コマンドがありません。apt の出力を確認してください。'
  ok '段① Tailscale を入れた'
fi

if has sshd && systemctl is-enabled ssh >/dev/null 2>&1; then
  skip '段① openssh-server'
else
  has sshd || sudo apt-get install -y openssh-server
  sudo systemctl enable --now ssh
  ok '段① openssh-server を入れて有効化した'
fi

case " $(id -nG) " in
*" docker "*) skip "段① $user は docker グループ" ;;
*)
  sudo usermod -aG docker "$user"
  ok "段① $user を docker グループへ足した"
  stop "docker グループは再ログインしてから有効になります。いったんログアウトして入り直し、同じ1行をもう一度叩くと続きから進みます: $entry"
  ;;
esac

if [[ $(sysctl -n "$userns_key" 2>/dev/null || true) == 0 ]]; then
  skip "段① $userns_key=0"
else
  printf '%s\n' "$userns_key=0" | sudo tee "$userns_conf" >/dev/null
  sudo sysctl -p "$userns_conf" >/dev/null
  [[ $(sysctl -n "$userns_key" 2>/dev/null || true) == 0 ]] ||
    stop "$userns_key を 0 にできません。キーの有無と /etc/sysctl.d の競合する設定を確認してください。"
  ok "段① $userns_key=0 を $userns_conf に置いて反映した"
fi

want_node=$(sed -n 's/^[[:space:]]*"node":[[:space:]]*"\([0-9][0-9.]*\)".*/\1/p' "$work/deploy/versions.json" | sed -n '1p')
[[ -n $want_node ]] || stop "$work/deploy/versions.json から node の版を読めません。"
if has node && [[ $(node --version) == "v$want_node" ]]; then
  skip "段① Node v$want_node"
else
  # root が読み込むので、他のユーザーが差し替えられないよう既存を消して排他的に作る。
  rm -f -- "$versions_env"
  (set -C && printf 'NODE_VERSION=%s\n' "$want_node" >"$versions_env")
  sudo sh "$work/deploy/install-node.sh"
  [[ $(node --version) == "v$want_node" ]] || stop "Node v$want_node を入れても版が合いません。install-node.sh の出力を確認してください。"
  ok "段① Node v$want_node を入れた"
fi

# --- 段②: tailnet とホスト名 ---
if tailscale status --json 2>/dev/null | grep '"BackendState": *"Running"' >/dev/null; then
  skip '段② tailnet に参加済み'
else
  printf '段② tailnet 未参加です。次に表示される承認URLをブラウザーで開いて承認してください（承認されるまで待ちます）。\n'
  sudo tailscale up
  ok '段② tailnet に参加した'
fi

os_host=$(hostname | tr '[:upper:]' '[:lower:]')
tail_host=$(tailscale status --json | node -e 'let s="";process.stdin.on("data",b=>s+=b).on("end",()=>process.stdout.write(String(JSON.parse(s).Self?.DNSName??"").split(".")[0].toLowerCase()))')
if [[ -z $os_host || $os_host != "$tail_host" ]]; then
  stop "段② OS のホスト名 (${os_host:-空}) と tailnet のホスト名 (${tail_host:-空}) が違います。OS 側は sudo hostnamectl set-hostname <名前>、tailnet 側は sudo tailscale set --hostname=<名前> で担当表の名前に揃え、同じ1行をもう一度叩いてください。"
fi
# 担当表の名前か、担当表の hosts.<名前>.machineName（実機の OS・tailnet の名前）に当たれば、担当表の名前を後の段の PC 名にする。
desk_host=$(node -e 'const a=JSON.parse(require("node:fs").readFileSync(process.argv[1],"utf8"));const m=process.argv[2];const hosts=a.hosts??{};const ok=(h)=>Object.hasOwn(hosts,h)&&Object.hasOwn(a.workerAccounts??{},h);const h=ok(m)?m:Object.keys(hosts).find((k)=>hosts[k]?.machineName===m&&ok(k));if(!h)process.exit(1);process.stdout.write(h)' "$work/assignments.json" "$os_host") ||
  stop "段② ホスト名 $os_host が assignments.json の hosts（名前か machineName）と workerAccounts にありません。担当表の名前と実機の名前が違うなら、担当表の hosts.<担当表の名前>.machineName に $os_host を書く PR を依頼するか、名前を担当表のものへ揃えて（hostnamectl と tailscale set --hostname）から、同じ1行をもう一度叩いてください。"
if [[ $desk_host == "$os_host" ]]; then
  ok "段② ホスト名 $os_host は OS・tailnet・担当表で一致"
else
  ok "段② ホスト名 $os_host は OS・tailnet で一致し、担当表の $desk_host の machineName に当たる"
fi

# --- 段③④の呼び出し口: 取得した一式の bootstrap-ubuntu-client.sh へ作業フォルダーと引数（--tag）を渡す ---
# 子は同じ NAS への制御用ソケットを使うので、宛先と一時フォルダーを渡す。
client="$work/deploy/clients/bootstrap-ubuntu-client.sh"
[[ -f $client ]] || stop "取得した一式に $client がありません。"
# 段③〜⑧は担当表の名前を BOOTSTRAP_HOST で受け取る（実機の名前と違う PC があるため）。
export BOOTSTRAP_NAS="$nas" BOOTSTRAP_NAS_DIR="$nas_dir" BOOTSTRAP_SSH_CTL="$ctl" BOOTSTRAP_HOST="$desk_host"
status=0
bash "$client" "$work" "$@" || status=$?
[[ $status -eq 0 ]] || exit "$status"

# --- 段⑤〜⑦の呼び出し口: 取得した一式の bootstrap-ubuntu-start.sh（CLI のログイン・起動と確認・引き取りの再開） ---
# BOOTSTRAP_SKIP_START=1 は段③④までを確かめる検証用の差し替え口（通常は使わない）。
if [[ ${BOOTSTRAP_SKIP_START:-} == 1 ]]; then
  skip '段⑤〜⑦ BOOTSTRAP_SKIP_START=1 のため呼ばない（検証用）'
  exit 0
fi
start="$work/deploy/clients/bootstrap-ubuntu-start.sh"
[[ -f $start ]] || stop "取得した一式に $start がありません。"
bash "$start" "$work" || status=$?
[[ $status -eq 0 ]] || exit "$status"
hostd="$work/deploy/clients/bootstrap-ubuntu-hostd.sh"
[[ -f $hostd ]] || stop "取得した一式に $hostd がありません。"
bash "$hostd" "$work" || status=$?
exit "$status"
