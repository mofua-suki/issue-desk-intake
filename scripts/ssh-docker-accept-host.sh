#!/bin/sh
# Fixed GT12 checks for the intake and scraper candidates.
set -eu
umask 077
work=$(mktemp -d /tmp/desk-gt12-accept.XXXXXXXX)
key=$(printf "%s" "$work" | sha256sum | cut -c 1-20)
name="desk-accept-$key"
docker_ready=
cleanup() {
  code=$?
  trap - EXIT
  if [ -n "$docker_ready" ] && ! clean_docker "$code"; then
    printf '%s\n' '検証用Docker資源の削除を確認できません。' >&2
    code=1
  fi
  rm -rf -- "$work"
  exit "$code"
}
clean_docker() {
  previous_empty=false
  for attempt in 1 2 3; do
    cleanup_docker rm -fv "$name-intake" "$name-db" "$name-test" "$name-scraper" "$name-probe" >/dev/null 2>&1 || :
    cleanup_docker network rm "$name" "$name-dependencies" >/dev/null 2>&1 || :
    cleanup_docker image rm "$name-test-env" "$name-candidate" "$name-intake-tools" "$name-codex-tools" >/dev/null 2>&1 || :
    if containers=$(cleanup_docker ps -aq --filter "name=$name") &&
      networks=$(cleanup_docker network ls -q --filter "name=$name") &&
      images=$(cleanup_docker image ls -q --filter "reference=$name-*") &&
      [ -z "$containers$networks$images" ]; then
      [ "$1" -eq 0 ] || [ "$previous_empty" = true ] && return 0
      previous_empty=true
    else
      previous_empty=false
    fi
    sleep 2
  done
  return 1
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM PIPE
fail() { printf '%s\n' "$1" >&2; exit 1; }
unset DOCKER_CONTEXT DOCKER_HOST DOCKER_TLS_VERIFY DOCKER_CERT_PATH
d() { /usr/bin/docker --config "$work/docker-config" --host unix:///var/run/docker.sock "$@"; }
cleanup_docker() { timeout -k 1 2 /usr/bin/docker --config "$work/docker-config" --host unix:///var/run/docker.sock "$@"; }
receive() {
  archive=$1
  head -c "$2" > "$work/$archive.tar.gz"
  printf '%s  %s\n' "$3" "$work/$archive.tar.gz" | sha256sum -c --status ||
    fail "固定アーカイブの指紋が一致しません: $archive"
}

job=$(head -c 4)
[ "$(head -c 1 | od -An -tx1 | tr -d '[:space:]')" = 0a ] || fail '検証番号の区切りが不正です。'
mkdir "$work/docker-config" "$work/source"
case "$job" in
  6293)
    commit=4f7e290931af98a4f299e0f5b55c356ad67a760c
    size=3660874
    digest=a220b5ec3f6d8c125b76b2037d1a28718b83692ef58f8c4d7d0a3626ec8085ab
    ;;
  6358)
    commit=fe2871c63ff7b3f70c870c6d82e2b5d39440e311
    size=1016325
    digest=91aee0eea32a403a4a9794941e93a73b3a2567b8ec8c3802a98232125b8b37bd
    ;;
  *) fail '許可された検証番号は6293・6358だけです。' ;;
esac
printf 'desk=%s head=%s\n' "$job" "$commit"
receive source "$size" "$digest"
if [ "$job" = 6293 ]; then
  receive intake-tools 124112 7181375118038e70a5512a2e2692bf713bbeb5c4559e3833c83401bca481e360
  receive codex-tools 801066 fc5471eccd48f01de3c2073229cb98e3484d9e3057347f2fc589a7431ef58287
fi
test -z "$(head -c 1 | od -An -tx1 | tr -d '[:space:]')" || fail '余分な入力があります。'
tar --same-permissions -xzf "$work/source.tar.gz" -C "$work/source"

d info --format '{{.ServerVersion}}' >/dev/null || fail 'GT12のDockerを確認できません。'
docker_ready=1
cd "$work/source"

step() {
  label=$1
  shift
  printf 'step=%s command=' "$label"
  printf ' %s' "$@"
  printf '\n'
  code=0
  "$@" || code=$?
  printf 'step=%s exit=%s\n' "$label" "$code"
  return "$code"
}

scraper() {
  step build d build --tag "$name-candidate" -f deploy/nas/Dockerfile .
  version=$(d run --rm --name "$name-probe" --network none --entrypoint node "$name-candidate" --version)
  printf 'node=%s\n' "$version"
  [ "$version" = v26.13.1 ]
  step playwright d run --rm --name "$name-probe" --network none --entrypoint node "$name-candidate" node_modules/playwright/cli.js --version
  step chromium d run --rm --name "$name-probe" --network none --entrypoint sh "$name-candidate" -eu -c '
    binary=$(node -e "console.log(require(\"playwright\").chromium.executablePath())")
    "$binary" --version'
  step typecheck d run --rm --name "$name-probe" --network none --entrypoint pnpm "$name-candidate" exec tsc --noEmit
  step fixed d run --rm --name "$name-probe" --network none --entrypoint node "$name-candidate" --test 'test/**/*.test.ts'
  step launcher d run --rm --name "$name-probe" --network none --entrypoint node "$name-candidate" --test test/launchers.test.ts
  step browser d run --rm --name "$name-scraper" --network none --security-opt "seccomp=$PWD/deploy/nas/seccomp_profile.json" --entrypoint sh "$name-candidate" -eu -c '
    test "$(id -u)" -ne 0
    base=$(mktemp -d)
    mkdir -m 700 "$base/runtime"
    export XDG_RUNTIME_DIR="$base/runtime"
    node -e "const fs=require(\"node:fs\");const p=process.argv[1];const c=JSON.parse(fs.readFileSync(\"config/settings.nas.example.json\"));Object.assign(c,{dataDir:p+\"/data\",imagesDir:p+\"/images\",profileDir:p+\"/profile\",targets:[{id:\"acceptance-fixture\",kind:\"account\",value:\"acceptance_fixture\",enabled:true,initialStartTime:\"oldest\"}]});fs.writeFileSync(p+\"/settings.json\",JSON.stringify(c));" "$base"
    Xvfb "$DISPLAY" -screen 0 1280x900x24 -nolisten tcp &
    display_pid=$!
    trap "kill $display_pid 2>/dev/null || true" EXIT
    for attempt in 1 2 3 4 5 6 7 8 9 10; do
      xdpyinfo -display "$DISPLAY" >/dev/null 2>&1 && break
      sleep 1
    done
    xdpyinfo -display "$DISPLAY" >/dev/null
    node src/cli.ts check-browsers --config "$base/settings.json" --jsonl > "$base/browser.jsonl"
    cat "$base/browser.jsonl"
    node --input-type=module -e "
      import {readFileSync} from \"node:fs\";
      const summary = readFileSync(process.argv[1], \"utf8\").trim().split(\"\\n\").map(line => JSON.parse(line)).at(-1);
      if (summary?.status !== \"ok\" || summary.sites?.length !== 1 ||
          summary.sites[0].site !== \"x\" || summary.sites[0].result !== \"ok\") process.exit(1);
    " "$base/browser.jsonl"'
}

tool_image() {
  snapshot=$1
  revision=$2
  mkdir "$work/$snapshot"
  tar --same-permissions -xzf "$work/$snapshot.tar.gz" -C "$work/$snapshot"
  tool_result="$name-$snapshot"
  step "$snapshot" d build --tag "$tool_result" --build-arg "ISSUE_DESK_TOOLS_REVISION=$revision" \
    -f "$work/$snapshot/Dockerfile.tools" "$work/$snapshot"
}

intake() {
  tool_image intake-tools bc0356ff9e2e638ae694461a983c97154074ce10
  intake_tools=$tool_result
  tool_image codex-tools ca73ecf2fc7f4197dfed3489cde071814aaf922a
  codex_tools=$tool_result
  step build d build --tag "$name-candidate" --build-arg "ISSUE_DESK_REVISION=$commit" \
    --build-arg "TOOLS_IMAGE=$intake_tools" --build-arg "CODEX_TOOLS_IMAGE=$codex_tools" -f deploy/nas/Dockerfile .
  node_version=$(d run --rm --name "$name-probe" --network none --entrypoint node "$name-candidate" --version)
  codex_version=$(d run --rm --name "$name-probe" --network none --entrypoint codex "$name-candidate" --version)
  printf 'node=%s\ncodex=%s\n' "$node_version" "$codex_version"
  [ "$node_version" = v26.13.1 ] || fail '候補のNode.js版が一致しません。'
  [ "$codex_version" = 'codex-cli 0.169.1' ] || fail '候補のCodex CLI版が一致しません。'
  d network create --internal "$name" >/dev/null
  step database d run -d --name "$name-db" --network "$name" \
    -e POSTGRES_HOST_AUTH_METHOD=trust -e POSTGRES_USER=desk_test \
    -e POSTGRES_DB=issue_desk_test postgres:17.11-bookworm
  for attempt in $(seq 1 30); do
    if d exec "$name-db" pg_isready -h 127.0.0.1 -U desk_test -d issue_desk_test >/dev/null; then break; fi
    sleep 2
  done
  d exec "$name-db" pg_isready -h 127.0.0.1 -U desk_test -d issue_desk_test >/dev/null ||
    fail '検証用PostgreSQLの起動を確認できません。'
  d exec -i "$name-db" psql -v ON_ERROR_STOP=1 -U desk_test -d issue_desk_test <<'SQL'
CREATE DATABASE issue_desk_candidate;
\connect issue_desk_candidate
CREATE SCHEMA issue_desk_app_migration;
CREATE TABLE issue_desk_app_migration.app_store_cutover (id INTEGER PRIMARY KEY CHECK (id = 1), completed_at BIGINT NOT NULL);
INSERT INTO issue_desk_app_migration.app_store_cutover VALUES (1, 1);
SQL
  printf '%s\n' '{"workerId":"nas","giteaUrl":"http://gitea.invalid","runtimeDir":"/state/runtime","tokenFile":"/state/private/admin-token","codex":"/opt/agent-tools/bin/codex","repositories":[{"name":"fixture/intake"}],"intake":{"bind":"127.0.0.1","port":8787}}' > "$work/intake-config.json"
  chmod 644 "$work/intake-config.json"
  step entrypoint d run -d --name "$name-intake" --network "$name" \
    --mount "type=bind,source=$work/intake-config.json,target=/verify-config.json,readonly" \
    -e ISSUE_DESK_CONFIG=/verify-config.json \
    -e "ISSUE_DESK_DATABASE_URL=postgres://desk_test@$name-db/issue_desk_candidate?sslmode=disable" "$name-candidate"
  health='fetch("http://127.0.0.1:8787/healthz").then(async r=>{ const v=await r.json(); console.log(JSON.stringify(v)); process.exit(r.status===200&&v.ok===true&&v.ready===true&&v.revision===process.argv[1]?0:1) }).catch(()=>process.exit(1))'
  healthy=0
  for attempt in $(seq 1 30); do
    if d exec "$name-intake" node -e "$health" "$commit"; then healthy=1; break; fi
    sleep 2
  done
  [ "$healthy" -eq 1 ] || { d logs --tail 30 "$name-intake" >&2; fail '候補のhealthzが200とok=trueを返しませんでした。'; }
  cat > "$work/Verify.Dockerfile" <<DOCKERFILE
FROM golang:1.27.3-bookworm AS go
FROM $name-candidate
USER root
COPY --from=go /usr/local/go /usr/local/go
RUN apt-get update && apt-get install -y --no-install-recommends git gcc libc6-dev util-linux && rm -rf /var/lib/apt/lists/*
ENV PATH=/usr/local/go/bin:\$PATH
DOCKERFILE
  step test-image d build --tag "$name-test-env" -f "$work/Verify.Dockerfile" .
  # Only the test client gets outbound access for package downloads.
  d network create "$name-dependencies" >/dev/null
  d create --name "$name-test" --network "$name" \
    --mount "type=bind,source=$work/source,target=/work" -w /work \
    -e "ISSUE_DESK_TEST_POSTGRES_DSN=postgres://desk_test@$name-db/issue_desk_test?sslmode=disable" \
    --entrypoint sh "$name-test-env" -eu -c '
      git init -q
      git add -A
      CI=true sh scripts/verify.sh
    ' >/dev/null
  d network connect "$name-dependencies" "$name-test"
  step fixed d start --attach "$name-test"
  [ "$(d inspect --format '{{.State.ExitCode}}' "$name-test")" -eq 0 ]
}

case "$job" in
  6293) intake ;;
  6358) scraper ;;
esac
