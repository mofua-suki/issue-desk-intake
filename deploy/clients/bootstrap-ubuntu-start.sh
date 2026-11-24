#!/usr/bin/env bash
# 導入の入口（bootstrap-ubuntu.sh）から段③④の後に呼ぶ段⑤CLIのログイン・段⑥起動と確認・段⑦引き取りの再開。単独でも叩ける（何度叩いても済んだ項目は飛ばす）。
# 実行: bash bootstrap-ubuntu-start.sh <作業フォルダー>（root では動かさない）。
# ログインの URL・コードは端末だけに出し、ログ・ファイルへ残さない。受付への問い合わせは監視コンテナの中の既存スクリプト
# （host-drain Skillと、config.local.json の serverUrl・serverTokenFile による PC の受付トークン接続）で行い、トークンの値は読まない。
set +x
set -Eeuo pipefail
umask 077

# 待ちの上限は 回数 × 間隔（秒）。既定は 60 回 × 5 秒。検証用に環境変数で変えられる。
wait_attempts=${BOOTSTRAP_WAIT_ATTEMPTS:-60}
poll=${BOOTSTRAP_POLL_SECONDS:-5}
drain_reason='実装PCの導入（bootstrap-ubuntu）'
# 認証の volume と段の対応（<段>:<volume>）。deploy/client/compose.yaml の /data/ 配下の認証 volume と一致させる（test/bootstrap-ubuntu-start.test.ts が照合する）。
auth_stages=(codex:issue-desk-worker-codex claude:issue-desk-worker-cli antigravity:issue-desk-worker-agy)

ok() { printf '[済み] %s\n' "$*"; }
skip() { printf '[飛ばした] %s\n' "$*"; }
stop() {
  if [[ ${verifying:-0} == 1 && ${drained:-0} == 0 ]]; then
    desk_drain --reason "$drain_reason" || printf '受付での引き取り停止を確認できません。受付の接続と状態を確認してください。\n' >&2
  fi
  printf '[止めた] %s\n' "$*" >&2
  exit 1
}
trap 'printf "[止めた] 失敗しました（%s 行目: %s）。表示された原因を直し、入口の同じ1行をもう一度叩いてください。\n" "$LINENO" "$BASH_COMMAND" >&2' ERR
trap 'exit 130' INT
trap 'exit 143' TERM

[[ $(id -u) -ne 0 ]] || stop 'root では実行しません。'
[[ $# -eq 1 ]] || stop '使い方: bootstrap-ubuntu-start.sh <作業フォルダー>'
work=$(cd -- "$1" 2>/dev/null && pwd) || stop "作業フォルダーがありません: $1"
[[ $wait_attempts =~ ^[0-9]+$ && $poll =~ ^[0-9]+$ ]] || stop 'BOOTSTRAP_WAIT_ATTEMPTS と BOOTSTRAP_POLL_SECONDS は数字で指定してください。'
compose=$work/compose.yaml
[[ -f $compose && -f $work/.env ]] || stop "$work に compose.yaml と .env がありません。段③を先に済ませてください（入口の同じ1行を叩き直す）。"
# 担当表の名前。入口が段②で決めて渡す（単独で叩いたときは OS のホスト名）。
host=${BOOTSTRAP_HOST:-$(hostname | tr '[:upper:]' '[:lower:]')}
[[ $host =~ ^[a-z0-9][a-z0-9-]{0,30}$ ]] || stop "ホスト名の形が不正です: ${host:-空}"

dc() { docker compose -f "$compose" "$@"; }

# 段④は別件（共通番号3744）。ログインなどの副作用より前に配置済みか確かめる。
step_prerequisites() {
 dc run --rm --no-deps -T --entrypoint /usr/local/bin/desk issue-desk-client doctor --config /state/config.local.json </dev/null || stop '段④の受付接続を確認できません。'
 dc run --rm --no-deps -T --entrypoint sh issue-desk-client -c 'test -s /state/private/worker-token' </dev/null || stop '段④の監視用トークンを確認できません。'
  ok '段④のトークン配置を確認した'
}

has_wired_ipv4() {
  local addresses
  addresses=$(ip -4 -o addr show scope global) || stop '段⑤の前 IPv4の状態取得に失敗しました。設定は変更しません。'
  printf '%s\n' "$addresses" | grep -Eq '^[0-9]+: (en[^ ]*|eth[^ ]*)[[:space:]]+inet '
}

ensure_dhcp() {
  local nic candidates=() config line section='' enabled=0 i
  for nic in "${BOOTSTRAP_SYS_CLASS_NET:-/sys/class/net}"/*; do
    [[ -e $nic/device && ! -d $nic/wireless && -r $nic/type ]] || continue
    [[ $(cat "$nic/type") == 1 ]] && candidates+=("${nic##*/}")
  done
  ((${#candidates[@]} == 1)) || stop '段⑤の前 DHCP対象不明または複数候補です。物理有線LANを一意に確認して入口を叩き直してください。'
  nic=${candidates[0]}
  [[ $nic =~ ^(en|eth)[a-zA-Z0-9_-]+$ ]] || stop '段⑤の前 DHCP対象の名前を安全に解釈できません。'
  sudo bash -c '
    set -eu
    # netplan以外の管理設定は自動変更の対象外。読取不能も安全側で止める。
    for suffix in /etc/network /etc/NetworkManager/system-connections /run/NetworkManager/system-connections /etc/systemd/network /run/systemd/network /usr/local/lib/systemd/network /usr/lib/systemd/network /lib/systemd/network; do
      dir=$1$suffix
      [ ! -e "$dir" ] && continue
      [ -d "$dir" ] && [ -r "$dir" ] && [ -x "$dir" ] || exit 1
      for file in "$dir"/*; do
        [ ! -L "$file" ] || exit 1
        [ -e "$file" ] || continue
        case "$file" in
          "$1"/etc/network/interfaces)
            contents=$(cat "$file") || exit 1
            if printf "%s\n" "$contents" | grep -Eq "^[[:space:]]*[^#[:space:]]"; then exit 1; fi ;;
          "$1"/etc/network/interfaces.d)
            [ -d "$file" ] && [ -r "$file" ] && [ -x "$file" ] || exit 1
            entries=$(ls -A "$file") || exit 1
            [ -z "$entries" ] || exit 1 ;;
          "$1/run/systemd/network/10-netplan-$2.network")
            # 生成名だけでは信用せず、対象のDHCPだけを設定する内容を確認する。
            [ -f "$file" ] && [ -r "$file" ] || exit 1
            contents=$(cat "$file") || exit 1
            section= matched=0 dhcp=0
            while IFS= read -r line; do
              case "$line" in
                ""|\#*) ;;
                "[Match]") section=match ;;
                "Name=$2") [ "$section" = match ] || exit 1; matched=1 ;;
                "[Network]") section=network ;;
                "DHCP=ipv4"|"DHCP=yes") [ "$section" = network ] || exit 1; dhcp=1 ;;
                "LinkLocalAddressing=ipv6") [ "$section" = network ] || exit 1 ;;
                "[DHCP]"|"[DHCPv4]") section=dhcp ;;
                # netplanがdhcp4だけでも生成する既定値。別セクションや上書き値は許さない。
                "RouteMetric=100"|"UseMTU=true") [ "$section" = dhcp ] || exit 1 ;;
                *) exit 1 ;;
              esac
            done <<< "$contents"
            [ "$matched" = 1 ] && [ "$dhcp" = 1 ] || exit 1 ;;
          *.network|*.network.d|*/system-connections/*) exit 1 ;;
        esac
      done
    done
    state=$(systemctl show --property=ActiveState --value NetworkManager) || exit 1
    [ "$state" = inactive ] || exit 1
    exit 0
  ' bootstrap-network "${BOOTSTRAP_NETWORK_ROOT:-}" "$nic" || stop '段⑤の前 netplan以外の管理設定があるか読取不能です。設定は変更しません。'
  config=$(sudo netplan get) || stop '段⑤の前 netplanの設定読取に失敗しました。設定は変更しません。'
  # netplan自身が解釈した有効設定の、ごく狭い安全な部分集合だけを許す。
  # 固定IP、match/set-name、別NIC、bridge、管理renderer等は推測で上書きしない。
  while IFS= read -r line; do
    case $line in
    'network:'|'network: {}'|'  version: 2'|'  renderer: networkd'|'  ethernets: {}'|'') ;;
    '  ethernets:') section=ethernets ;;
    "    $nic:") [[ $section == ethernets ]] || stop '段⑤の前 netplan設定競合です。'; section=target ;;
    '      dhcp4: true') [[ $section == target ]] || stop '段⑤の前 netplan設定競合です。'; enabled=1 ;;
    *) stop '段⑤の前 netplan設定競合または解釈できない管理設定です。設定は変更しません。' ;;
    esac
  done <<<"$config"
  [[ -n $config ]] || stop '段⑤の前 netplanの空の応答を解釈できません。'
  if ((enabled)); then
    skip '有線LANのDHCPは有効（再書き込みしない）'
  else
    # 専用ファイルが既にある場合も上書きしない。失敗後は状態を確認して同じ入口で再試行する。
    sudo test ! -e /etc/netplan/90-issue-desk-bootstrap.yaml || stop '段⑤の前 DHCP用設定ファイルが既にあります。設定を確認してください。'
    sudo test ! -L /etc/netplan/90-issue-desk-bootstrap.yaml || stop '段⑤の前 DHCP用設定ファイルがsymlinkです。設定を確認してください。'
    sudo netplan set --origin-hint 90-issue-desk-bootstrap "network.ethernets.$nic.dhcp4=true" || stop '段⑤の前 DHCP設定を保存できません。netplanを確認してください。'
    sudo netplan apply || stop '段⑤の前 DHCP設定の反映に失敗しました。netplanを確認してください。'
    ok '有線LANのDHCP設定を反映した'
  fi
  for ((i = 0; i < wait_attempts; i++)); do
    if ip -4 -o addr show dev "$nic" scope global | grep -q ' inet '; then return; fi
    sleep "$poll"
  done
  stop '段⑤の前 有線LANのIPv4取得に失敗しました。DHCP・配線を確認して入口を叩き直してください。'
}

serve_state() {
  local status
  status=$(tailscale serve status --json) || stop 'Webhook の8790転送の状態取得に失敗しました。'
  printf '%s' "$status" | node -e '
    let s = "";
    process.stdin.on("data", c => s += c).on("end", () => {
      try {
        const v = JSON.parse(s);
        if (!v || typeof v !== "object" || Array.isArray(v)) throw Error();
        if (Object.keys(v).some(k => !["TCP", "Web", "AllowFunnel", "Foreground"].includes(k))) throw Error();
        if (v.Foreground && Object.keys(v.Foreground).length) throw Error();
        if (v.TCP !== undefined && (!v.TCP || typeof v.TCP !== "object" || Array.isArray(v.TCP))) throw Error();
        const e = v.TCP?.["8790"];
        if (e === undefined) console.log("missing");
        else if (e && Object.keys(e).length === 1 && e.TCPForward === "127.0.0.1:8790") console.log("ready");
        else throw Error();
      } catch { process.exitCode = 1; }
    });' || stop 'Webhook の8790転送が競合または状態を解釈できません。上書きせず停止します。'
}

step_network() {
  has_wired_ipv4 || ensure_dhcp
  curl -4 --silent --show-error --connect-timeout 8 --max-time 15 --output /dev/null https://auth.openai.com/ ||
    stop '段⑤の前 IPv4でauth.openai.comへ接続できません。有線LAN・DNS・経路を確認して入口を叩き直してください。'
  ok '段⑤の前 有線LANのIPv4と外への接続を確認した'
  local state
  state=$(serve_state) || stop 'Webhook の8790転送を確認できません。tailscale serveを確認してください。'
  if [[ $state == ready ]]; then
    skip 'Webhook の8790転送は設定済み'
  else
    sudo tailscale serve --bg --tcp 8790 tcp://127.0.0.1:8790 || stop 'Webhook の8790転送を設定できません。tailscale serveを確認してください。'
    state=$(serve_state) && [[ $state == ready ]] || stop 'Webhook の8790転送を確認できません。tailscale serveを確認してください。'
    ok 'Webhook の8790転送を設定した'
  fi
}

# --- 段⑤: CLI のログイン（済みの判定は volume にある認証の有無。値は読まない） ---
logged_in() {
  case $1 in
  codex) dc run --rm --no-deps -T --entrypoint test issue-desk-client -s /data/codex/auth.json ;;
  claude) dc run --rm --no-deps -T --entrypoint test issue-desk-client -s /data/claude/.credentials.json ;;
  antigravity) dc run --rm --no-deps -T --entrypoint /opt/agent-tools/bin/agy issue-desk-client -p /usage ;;
  esac </dev/null >/dev/null 2>&1
}

login_stage() {
  local stage=${1%%:*} volume=${1#*:}
  if logged_in "$stage"; then
    skip "段⑤ $stage のログインは済み（volume $volume）"
    return
  fi
  printf '段⑤ %s にログインします。表示された URL をブラウザーで開いて承認してください（URL・コードは記録しません）。\n' "$stage"
  case $stage in
  codex) dc run --rm -it --entrypoint codex issue-desk-client login --device-auth ;;
  claude) dc run --rm -it --entrypoint claude issue-desk-client auth login ;;
  antigravity) dc run --rm -it --entrypoint sh issue-desk-client /app/deploy/client/login-antigravity.sh ;;
  esac || stop "段⑤ $stage のログインに失敗しました。表示された原因を直し、入口の同じ1行を叩き直してください（済んだログインは飛ばします）。"
  logged_in "$stage" || stop "段⑤ $stage のログイン後も volume $volume に認証が見つかりません。承認が済んだか確認し、入口の同じ1行を叩き直してください。"
  ok "段⑤ $stage にログインした（volume $volume）"
}

step_logins() {
  local entry
  for entry in "${auth_stages[@]}"; do login_stage "$entry"; done
}

# --- 受付の見え方（監視コンテナの中。PC の受付トークンで GET /api/hosts を読む） ---
# 出力は「heartbeat がある・引き取りが止まっている・ready=true・heartbeat が <since>(ms) より新しい」を 0/1 で並べた1行。
seen=0 drained=0 ready=0 fresh=0 indefinite=0
read_view() {
 local out
 seen=0 drained=0 ready=0 fresh=0 indefinite=0
 out=$(dc run --rm --no-deps -T --entrypoint /usr/local/bin/desk issue-desk-client get --config /state/config.local.json --path /api/hosts </dev/null | node -e '
let text="";process.stdin.on("data",b=>text+=b).on("end",()=>{try{const response=JSON.parse(text);const v=response.body;if(!Array.isArray(v.hosts))throw Error();const h=v.hosts.find(h=>h.name===process.argv[1]);const beat=Date.parse(h?.updatedAt??"");const seen=Number.isFinite(beat);const now=Date.now();console.log([seen,!!h?.drain&&(h.drain.indefinite===true||Date.parse(h.drain.until)>now),h?.ready===true,seen&&beat>Number(process.argv[2])&&beat<=now&&now-beat<900000,h?.drain?.indefinite===true].map(Number).join(" "));}catch{process.exitCode=1;}})' "$host" "$1") || return 1
 [[ $out =~ ^[01]\ [01]\ [01]\ [01]\ [01]$ ]] || return 1
 read -r seen drained ready fresh indefinite <<<"$out"
}
desk_drain() {
 if [[ ${1:-} == --resume ]]; then
  dc run --rm --no-deps -T --entrypoint /usr/local/bin/desk issue-desk-client operation --config /state/config.local.json --path "/api/hosts/$host/drain" --method DELETE --body-file - <<<'{}'
 else
  printf '{"reason":"実装PCの導入（bootstrap-ubuntu）"}\n' | dc run --rm --no-deps -T --entrypoint /usr/local/bin/desk issue-desk-client operation --config /state/config.local.json --path "/api/hosts/$host/drain" --method POST --body-file -
 fi
}
now_ms() { node -e 'process.stdout.write(String(Date.now()))'; }

both_running() {
  local services defined
  defined=$(dc config --services </dev/null) || return 1
  services=$(dc ps --status running --services </dev/null 2>/dev/null) || return 1
  printf '%s\n' "$services" | grep -Fx issue-desk-client >/dev/null || return 1
  if printf '%s\n' "$defined" | grep -Fx issue-desk-webhook-entry >/dev/null; then
    printf '%s\n' "$services" | grep -Fx issue-desk-webhook-entry >/dev/null || return 1
  fi
}

# --- 段⑥の前: 受付でこの PC の引き取りが止まっていることを確かめ、止まっていなければ止める ---
was_running=0 first_boot=0
step_drain() {
  read_view 0 || stop '段⑥の前 受付からこの PC の状態を読めません。受付トークン（段④）と受付の稼働を確認してください。上の理由を直して入口の同じ1行を叩き直します。'
  if both_running; then
    was_running=1
    if ((ready || drained)); then
      skip '段⑥の前 監視は既に起動中（受付の停止状態を確認し、確認を続けます）'
      return
    fi
  fi
  if ((drained)); then
    skip '段⑥の前 受付でこの PC の引き取りは止まっている'
    return
  fi
  if ((! seen)); then
    first_boot=1
    skip '段⑥の前 初回のheartbeatがまだありません。起動して行の作成を待ち、確認へ進む前に引き取りを止めます'
    return
  fi
  if ! desk_drain --reason "$drain_reason"; then
    stop '段⑥の前 受付がこの PC の引き取りの停止を断りました。受付の接続と停止理由を確認して入口を叩き直してください。'
  fi
  if ((seen)); then
    read_view 0 && ((drained)) || stop '段⑥の前 引き取りを止めたはずですが、受付に止まった印が見えません。受付画面で確認してください。'
  fi
  drained=1
  ok '段⑥の前 受付でこの PC の引き取りを止めた'
}

# --- 段⑥: 起動と確認 ---
step_up_and_verify() {
  verifying=1
 local start_ms i
 start_ms=$(now_ms)
  if ((was_running && !drained)); then
    dc up -d --no-recreate </dev/null || stop '段⑥ docker compose up -d --no-recreate に失敗しました。表示された原因を直してください（引き取りは止めたままです）。'
  else
    dc up -d </dev/null || stop '段⑥ docker compose up -d に失敗しました。表示された原因を直してください（引き取りは止めたままです）。'
  fi

  # 初回はheartbeatだけがhostsの行を作る。行ができ次第停止し、その確認後に自己検査へ進む。
  if ((first_boot)); then
    for ((i = 0; i < wait_attempts; i++)); do
      if read_view 0 && ((seen)); then break; fi
      sleep "$poll"
    done
    if ((! seen)) || ! desk_drain --reason "$drain_reason" || ! read_view 0 || ((! drained)); then
      # 停止印を確認できない初回の監視を、動いたまま残さない。
      dc stop issue-desk-client </dev/null || printf '初回の監視コンテナも停止できません。docker composeの状態を確認してください。\n' >&2
      verifying=0
      stop '段⑥ 初回のheartbeatと引き取り停止を確認できません。受付の接続と停止理由を直して同じ1行を叩き直してください。'
    fi
    ok '段⑥ 初回のheartbeatを受け取り、受付で引き取り停止を確認した'
  fi

 dc exec -T issue-desk-client /usr/local/bin/worker check --config /state/config.local.json </dev/null || stop '段⑥ Workerの自己検査が不合格です。引き取りは停止したままです。'
 ok '段⑥ Workerの自己検査が合格した'

  # 今回の起動より新しい heartbeat と ready=true を待つ（前回の heartbeat・ready は合格にしない）。
  for ((i = 0; i < wait_attempts; i++)); do
    if read_view "$start_ms" && ((seen && ready && fresh)); then break; fi
    sleep "$poll"
  done
  ((seen)) || stop '段⑥ 受付にこの PC の heartbeat がありません。受付トークンと serverUrl を確認してください（引き取りは止めたままです）。'
  ((fresh)) || stop '段⑥ 今回の起動より新しい heartbeat が受付に届きません（前回の値は合格にしません）。docker compose logs と受付トークンを確認してください（引き取りは止めたままです）。'
  ((ready)) || stop '段⑥ 受付の ready が true になりません（false の PC には仕事が配られません）。host-check.mjs の出力を確認してください（引き取りは止めたままです）。'
  ok '段⑥ 今回の起動より新しい heartbeat と ready=true を受付で確認した'
  verifying=0
}

# --- 段⑦: ⑥に合格したときだけ引き取りを戻す ---
step_resume() {
  if ((drained && indefinite)); then
    skip '段⑦ 無期限の手動停止は維持します'
    ok "導入が完了しました: $host（CLI のログイン・起動・自己検査・readyを確認済み、引き取りは停止中）"
    return
  elif ((drained)); then
    desk_drain --resume || stop "段⑦ 引き取りを戻せません。上の理由を直して入口の同じ1行を叩き直すか、host-drain Skillの取消手順で $host の引き取りを戻してください。"
    ok '段⑦ 引き取りを戻した'
  else
    skip '段⑦ 引き取りは止まっていない'
  fi
  ok "導入が完了しました: $host（CLI のログイン・起動・自己検査・ready・引き取りまで確認済み）"
}

step_prerequisites
step_network
step_logins
step_drain
step_up_and_verify
step_resume
