# 共通接続と結果報告。呼び出し元が設定・承認を済ませた後に使用する。
cleanup() {
  outcome=$?
  trap '' HUP INT TERM
  trap - EXIT
  if [ "$report" = 1 ]; then
    if node -e '
      const fs = require("node:fs");
      const [file, connectionResult, code, summary] = process.argv.slice(1);
      fs.writeFileSync(file, JSON.stringify({connectionResult, exitCode: JSON.parse(code), summary}));
    ' "$work/result" "$connection" "$exit_code" "$summary" 2>/dev/null &&
      api_call POST "/$ticket/result" 200 --data-binary "@$work/result"; then
      printf '%s\n' 'ホスト作業用SSHの結果を報告しました。'
    else
      printf '%s\n' 'ホスト作業用SSHの結果を報告できません。' >&2
      outcome=1
    fi
  fi
  [ -z "$public_work" ] || rm -rf -- "$public_work"
  [ -z "$work" ] || rm -rf -- "$work"
  exit "$outcome"
}

run_connection() {
# 個人のSSH設定・agent・他のknown_hostsに依存しない。初回だけTOFUで記録。
ssh_output=$work/ssh-output
set +e
timeout -k 10 "$ssh_timeout" ssh -F /dev/null -T -i "$work/key" -o "CertificateFile=$work/key-cert.pub" \
  -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes -o PasswordAuthentication=no \
  -o KbdInteractiveAuthentication=no -o PreferredAuthentications=publickey \
  -o StrictHostKeyChecking=accept-new -o "UserKnownHostsFile=$known_dir/known_hosts" \
  -o GlobalKnownHostsFile=/dev/null -o UpdateHostKeys=no -o ForwardAgent=no -o ClearAllForwardings=yes \
  -o ConnectTimeout=10 -o ConnectionAttempts=1 -o ServerAliveInterval=15 -o ServerAliveCountMax=2 \
  "$destination" /bin/false < "${input:-/dev/null}" > "$ssh_output" 2>&1
exit_code=$?
set -e
case "$exit_code" in
  0) connection=connected; summary='ホスト作業用SSHの操作が完了しました。' ;;
  124|137|255)
    # 接続先が最後まで動いて同じ終了コードを verify=<終了コード> で返したなら、接続は成功で操作の失敗（段の時間切れなど）。
    if "$ts_node" "$repo/deploy/nas-host/host-ssh-output.ts" --finished "$ssh_output" "$exit_code" 2>/dev/null; then
      connection=connected; summary='ホスト作業用SSHの操作が非0で終了しました。'
    else
      summary='ホスト作業用SSHの接続に失敗または時間切れになりました。'
    fi ;;
  *) connection=connected; summary='ホスト作業用SSHの操作が非0で終了しました。' ;;
esac
# 出力は秘密らしい行を除いた全文を呼び出し元へ表示し、受付への結果には末尾1500文字だけを残す。
# 表示しないと、呼び出したAIや利用者は操作の結果（ログなど）を読めない。
if ! summary=$("$ts_node" "$repo/deploy/nas-host/host-ssh-output.ts" "$ssh_output" "$summary" "$work/clean" 2>/dev/null); then
  summary='ホスト作業用SSHの出力を要約できません。'
  exit 1
fi
if [ "$connection" = failed ]; then
  cat "$work/clean" >&2
else
  cat "$work/clean"
fi
printf '%s\n' "$(printf '%s\n' "$summary" | head -n 1)" >&2
return "$exit_code"
}
