#!/usr/bin/env bash
# IntakeのLinux導入からWorkerの局所token-placeを呼ぶ。秘密はstdinだけで対象の state へ渡す。
set +x
set -euo pipefail
umask 077
[[ $# -eq 1 ]] || { echo '使い方: worker-token-linux.sh <client-dir>' >&2; exit 2; }
work=$(cd -- "$1" && pwd)
host=${BOOTSTRAP_HOST:-$(hostname | tr '[:upper:]' '[:lower:]')}
[[ $host =~ ^[a-z0-9][a-z0-9-]{0,30}$ ]] || exit 2
registry=$(sed -n 's/^REGISTRY=//p' "$work/.env" | tail -n 1)
flavor=$(sed -n 's/^FLAVOR=//p' "$work/.env" | tail -n 1)
tag=$(sed -n 's/^ISSUE_DESK_CLIENT_BLUE_TAG=//p' "$work/.env" | tail -n 1)
[[ -n $tag ]] || tag=$(sed -n 's/^ISSUE_DESK_CLIENT_TAG=//p' "$work/.env" | tail -n 1)
image=$registry/issue-desk-client$flavor:$tag
[[ $tag =~ ^[a-f0-9]{12}(-client-python)?$ ]] || { echo 'Workerの版を確認できません。' >&2; exit 1; }
[[ -r $work/compose.yaml ]] || { echo 'compose.yaml を読めません。' >&2; exit 1; }
volumes=(issue-desk-worker-state)
if grep -Fq -- issue-desk-worker-state-green "$work/compose.yaml"; then
 volumes+=(issue-desk-worker-state-green)
fi
place() {
 local volume=$1 phase=$2 kind=$3
 docker run --rm -i --pull never --log-driver none --user 0 -v "$volume:/state" --entrypoint /usr/local/bin/worker "$image" token-place --config /state/config.local.json --phase "$phase" --kind "$kind" --worker-id "$host"
}
# 秘密の入力より先にK2と受付接続を両色で確認する。
for volume in "${volumes[@]}"; do
 docker run --rm -i --pull never --log-driver none --user 0 -v "$volume:/state:ro" --entrypoint /usr/local/bin/desk "$image" doctor --config /state/config.local.json </dev/null
done
account=$(docker run --rm -i --pull never --log-driver none --user 0 -v issue-desk-worker-state:/state:ro --entrypoint /usr/local/bin/desk "$image" get --config /state/config.local.json --path "/api/v1/client-config?host=$host" </dev/null | node -e 'let s="";process.stdin.on("data",b=>s+=b).on("end",()=>{try{const v=JSON.parse(s).body.config.assignments.workerAccounts[process.argv[1]];if(typeof v!=="string"||!/^[A-Za-z0-9_.-]+$/.test(v))throw Error();process.stdout.write(v);}catch{process.exitCode=1;}})' "$host") || { echo '担当表の監視用アカウント名を読めません。' >&2; exit 1; }
printf '監視用アカウント: %s（担当表から取得）\n' "$account" >&2
secret=''
trap 'unset secret' EXIT
existing=1
for volume in "${volumes[@]}"; do
 if ! place "$volume" existing worker-token </dev/null; then existing=0; fi
done
if ((existing)); then
 printf '対象の既存監視用トークンは本人照合済みです。\n'
 exit 0
fi
for kind in worker-token; do
 printf '%s用の既存Giteaトークンを入力してください（このPC専用の監視用アカウント。表示しません）: ' "$kind" >&2
 IFS= read -r -s secret || { echo 'トークンを入力できません。' >&2; exit 1; }
 printf '\n' >&2
 [[ -n $secret ]] || { echo 'トークンが空です。' >&2; exit 1; }
 for phase in check write verify; do
  for volume in "${volumes[@]}"; do
   printf '%s\n' "$secret" | place "$volume" "$phase" "$kind"
  done
 done
 unset secret
done
printf '対象のGitea本人照合とトークン配置が完了しました。\n'
