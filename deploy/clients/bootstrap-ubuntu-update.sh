#!/usr/bin/env bash
# bootstrap-ubuntu-client.sh から source する既存PCの更新。接続・NAS関数は呼び出し元と共有する。
# 引き取りの再開は段⑥の検査後の段⑦だけで行う。秘密はSSHから更新stdinへのパイプだけで渡す。
wait_attempts=${BOOTSTRAP_WAIT_ATTEMPTS:-60}
poll=${BOOTSTRAP_POLL_SECONDS:-5}
[[ $wait_attempts =~ ^[1-9][0-9]*$ && $poll =~ ^[0-9]+$ ]] ||
  stop '段③ 更新待ちの回数は正の整数、間隔は0以上の整数で指定してください。'
update_dc() { docker compose -f "$work/compose.yaml" run --rm --no-deps -T --entrypoint /usr/local/bin/desk issue-desk-client "$@"; }
update_read() {
 local out
 update_active=0 update_idle=0 update_indefinite=0
 out=$(update_dc get --config /state/config.local.json --path /api/hosts </dev/null | node -e '
let text="";process.stdin.on("data",b=>text+=b).on("end",()=>{try{const response=JSON.parse(text);const view=response.body;if(!Array.isArray(view.hosts))throw Error();const row=view.hosts.find(h=>h.name===process.argv[1]);if(!row)throw Error();const now=Date.now(),beat=Date.parse(row.updatedAt??"");const active=!!row.drain&&(row.drain.indefinite===true||Date.parse(row.drain.until)>now);const idle=row.restartSafe===true&&row.implementing===0&&row.publishing===0&&beat>Number(process.argv[2])&&beat<=now&&now-beat<900000;console.log([active,idle,row.drain?.indefinite===true].map(Number).join(" "));}catch{process.exitCode=1;}})' "$host" "$1") || return 1
 [[ $out =~ ^[01]\ [01]\ [01]$ ]] || return 1
 read -r update_active update_idle update_indefinite <<<"$out"
}
# 接続とworkerIdを停止要求より先に確認する。
update_read 0 || stop '段③ 更新前の設定・受付接続を確認できません。既存配置先とトークンを直して入口を叩き直してください。'
if ((!update_indefinite)); then
  printf '{"reason":"実装PCの導入更新（bootstrap-ubuntu）"}\n' | update_dc operation --config /state/config.local.json --path "/api/hosts/$host/drain" --method POST --body-file - >/dev/null || stop '段③ 引き取り停止に失敗しました。'
fi
update_read 0 && ((update_active)) || stop '段③ 有効な停止印を確認できません。受付の状態を直して入口を叩き直してください。'
update_since=$(node -e 'process.stdout.write(String(Date.now()))')
for ((update_i = 0; update_i < wait_attempts; update_i++)); do
  update_read "$update_since" || stop '段③ 停止後の受付の確認に失敗しました。引き取りは停止したままです。原因を直して入口を叩き直してください。'
  ((update_active)) || stop '段③ 停止印が消失または期限切れです。更新せず止めます。入口を叩き直してください。'
  if ((update_idle)); then break; fi
  sleep "$poll"
done
((update_idle)) || stop '段③ 新しいheartbeat・restartSafe=true・実装中0・公開中0の確認が時間切れです。引き取りは停止したままです。原因を直して入口を叩き直してください。'
if ! nas_ssh 'cat ~/.config/issue-desk/registry.password' </dev/null |
  bash "$src/update-linux.sh" "$work" "$tag" >/dev/null; then
  stop "段③ 更新に失敗しました。SSH・レジストリ接続・更新ロックを確認してください。引き取りは停止したままです。必要なら bash $src/update-linux.sh $work --restore-previous で復帰し、原因を直して入口の同じ1行をもう一度叩いてください。"
fi
