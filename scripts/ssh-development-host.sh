#!/bin/sh
# 固定SSH操作 develop。稼働Workerと同じイメージで専用コンテナを使う。
set -eu
PATH="$HOME/.local/bin:$PATH"
export PATH
runner_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
child=
container_name=
cleanup() {
  trap - EXIT HUP INT TERM
  if [ -n "$child" ]; then kill -TERM "$child" 2>/dev/null || :; fi
  if [ -n "$container_name" ]; then docker rm -f "$container_name" >/dev/null 2>&1 || :; fi
  if [ -n "$child" ]; then wait "$child" 2>/dev/null || :; fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
exec 3<&0
command -v docker >/dev/null 2>&1 || { echo '実装用のNode・Codex・GitまたはDockerがありません。' >&2; exit 1; }
# タグの新旧順では選ばず、現在動いているWorkerと同じイメージを使う。
container=$(docker ps --filter label=com.docker.compose.service=issue-desk-client --format '{{.ID}}')
[ -n "$container" ] && [ "$(printf '%s\n' "$container" | wc -l)" -eq 1 ] || {
  echo '稼働Workerのコンテナを一意に確認できません。' >&2; exit 1;
}
image=$(docker inspect --format '{{.Image}}' "$container")
docker volume inspect issue-desk-worker-codex >/dev/null || { echo '既存Codexの認証volumeがありません。' >&2; exit 1; }
# ホストのソケット・Workerの状態・権限設定は渡さない。接続先の既存Codex認証だけを使う。
container_name="issue-desk-ssh-development-$$"
timeout -k 10 3300 docker run --name "$container_name" --rm --pull never --init -i --network host --user 1000:1000 \
  --log-driver none --cap-drop ALL --security-opt no-new-privileges:true \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined -e HOME=/home/node -e CODEX_HOME=/data/codex \
  -v issue-desk-worker-codex:/data/codex \
  -v "$runner_dir:/runner:ro" --entrypoint node "$image" /runner/ssh-development-runner.ts <&3 &
child=$!
wait "$child"
