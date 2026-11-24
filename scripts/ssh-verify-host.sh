#!/bin/sh
# 固定SSH操作 verify の接続先側。標準入力の1行目に固定テストの指定（base64のJSON）、続けて作業ツリーの tar.gz を受け取り、
# 稼働Workerと同じイメージの使い捨て容器で ssh-verify-runner.ts を動かす。作業場所だけを /work に渡し、
# 秘匿の行を除いた出力の末尾64KiBと verify=<終了コード> を返す。正常終了・失敗・中断・切断のどれでも容器と作業場所を消す。
set -u
runner_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
name=host-ssh-verify-$$
dir=
out=
objects=
pid=
cleanup() {
  i=0
  while [ -n "$pid" ] && [ "$i" -lt 30 ] && kill -0 "$pid" 2>/dev/null; do
    kill "$pid" 2>/dev/null
    docker rm -f "$name" >/dev/null 2>&1
    sleep 1
    i=$((i + 1))
  done
  docker rm -f "$name" >/dev/null 2>&1
  rm -rf "$dir" "$out" "$objects"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM PIPE
command -v docker >/dev/null 2>&1 || { echo 'Dockerがありません'; exit 1; }
# タグの新旧順では選ばず、現在動いているWorkerと同じイメージを使う。
container=$(docker ps --filter label=com.docker.compose.service=issue-desk-client --format '{{.ID}}')
[ -n "$container" ] && [ "$(printf '%s\n' "$container" | wc -l)" -eq 1 ] || { echo '稼働Workerのコンテナを一意に確認できません'; exit 1; }
img=$(docker inspect --format '{{.Image}}' "$container") && [ -n "$img" ] || { echo '稼働Workerのイメージを確認できません'; exit 1; }
IFS= read -r spec || { echo '固定テストの指定を読めません'; exit 1; }
case "$spec" in ''|*[!A-Za-z0-9+/=]*) echo '固定テストの指定が不正です'; exit 1 ;; esac
dir=$(mktemp -d "${TMPDIR:-/tmp}/host-ssh-verify.XXXXXXXX") || exit 1
out=$(mktemp "${TMPDIR:-/tmp}/host-ssh-verify-out.XXXXXXXX") || exit 1
direct_head=${ISSUE_DESK_DIRECT_PR_HEAD:-}
if [ -n "$direct_head" ]; then
  case "$direct_head" in *[!0-9a-f]*) exit 1 ;; esac
  [ "${#direct_head}" -eq 40 ] || exit 1
  objects=$(mktemp -d "${TMPDIR:-/tmp}/host-ssh-verify-objects.XXXXXXXX") || exit 1
  export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
  if ! (ulimit -f 262144 && git init --bare --template= -q "$objects" &&
      git --git-dir="$objects" unpack-objects -q &&
      [ "$(git --git-dir="$objects" cat-file -t "$direct_head")" = commit ] &&
      git --git-dir="$objects" read-tree "$direct_head" &&
      git --git-dir="$objects" --work-tree="$dir" checkout-index --all --prefix="$dir/"); then
    echo '証明書で指定されたコミットのGitオブジェクトを検証できません'
    exit 1
  fi
else
  tar -xzf - -C "$dir" || { echo '標準入力のtar.gzを展開できません'; exit 1; }
fi
# --group-add 1001 は、イメージのFlutter SDKの bin/cache が gid 1001 にだけ書き込めるため。
timeout -k 10 5400 docker run --rm --init --cap-drop ALL --security-opt no-new-privileges:true --name "$name" \
  --pull never --network host --user "$(id -u):$(id -g)" --group-add 1001 -e "HOST_SSH_VERIFY_SPEC=$spec" -e "HOST_SSH_DIRECT_PR_HEAD=$direct_head" \
  -v "$dir:/work" -v "$runner_dir:/runner:ro" -w /work --entrypoint node "$img" /runner/ssh-verify-runner.ts \
  >"$out" 2>&1 &
pid=$!
# 1秒ごとに書き、接続が切れていれば書き込みの失敗（PIPE）で気付いて片付ける。
while kill -0 "$pid" 2>/dev/null; do
  sleep 1
  printf '.\n'
done
wait "$pid"
code=$?
pid=
{
  awk '{ line = tolower($0); if (line ~ /-----begin .*private key-----/) { key = 1; next } if (key) { if (line ~ /-----end .*private key-----/) key = 0; next } if (line ~ /idk_|idh_|private key|password|secret|token/) next; print }' "$out" | tail -c 65536
  echo "verify=$code"
} || :
exit "$code"
