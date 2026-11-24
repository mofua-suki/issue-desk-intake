# Workerのtrunkの先頭を、NASのレジストリへ発行し、PCごとの「望む版」として登録する。docs/client-release.md。
# update.shからsourceし、DIR・DRY_RUN・TOOLS_IMAGE・say・planを受け取る。10分周期の更新の最後に呼ばれる。
# 失敗しても更新の他の段を止めない（常に0を返す）。失敗は [update:worker-release-failed] で見分ける。
#
# - 状態は $DIR/runtime/worker-release/ に置く（repo.git: trunkのミラー、registered: 登録済みの版、failed: 失敗した版と時刻、lock）。
# - 登録済みの版と同じなら何もしない。同じ版の失敗は1時間おく。ツール定義が変わっていたらツールイメージを作り直す。
# - PCごとのレジストリ名は deploy/nas/worker-release.json の hosts に宣言する（NASは127.0.0.1:5000、他はtailnetの名前）。
# - hostsのキーにドットは使えない（cfgのパス区切りと衝突する）。
worker_release() (
 if [ "${DRY_RUN:-0}" = 1 ]; then plan '予定: Workerのtrunkの先頭を発行し、PCごとの望む版として登録する（変わっていなければ何もしない）'; return 0; fi
 conf=$DIR/deploy/nas/worker-release.json
 [ -f "$conf" ] || return 0
 state=$DIR/runtime/worker-release
 fail() { say "[update:worker-release-failed] $*"; return 0; }
 command -v python3 >/dev/null 2>&1 || { fail 'python3がありません'; return 0; }
 command -v curl >/dev/null 2>&1 || { fail 'curlがありません'; return 0; }
 mkdir -p "$state" || { fail "$state を作れません"; return 0; }
 # 同時に1つだけ。残った鍵は、持ち主のプロセスが居なければ捨てる。
 lock=$state/lock
 if ! mkdir "$lock" 2>/dev/null; then
  owner=$(cat "$lock/pid" 2>/dev/null || true)
  if [ -n "$owner" ] && kill -0 "$owner" 2>/dev/null; then return 0; fi
  rm -rf "$lock"; mkdir "$lock" 2>/dev/null || return 0
 fi
 echo $$ >"$lock/pid"
 work=$(mktemp -d) || { rmdir "$lock" 2>/dev/null || :; return 0; }
 trap 'rm -rf "$work" "$lock"' EXIT
 cfg() { python3 -c 'import json,sys;d=json.load(open(sys.argv[1], encoding="utf-8-sig"));v=d
for k in sys.argv[2].split("."):
    v=v[k]
if len(sys.argv) > 3 and sys.argv[3] == "keys":
    if not isinstance(v, dict):
        sys.exit("keys requires an object")
    print("\n".join(v))
else:
    print(v)' "$conf" "$1" "${2:-}"; }
 remote=${ISSUE_DESK_WORKER_REMOTE:-$(cfg remote)} || { fail '設定の remote を読めません'; return 0; }
 branch=$(cfg branch) && flavor=$(cfg flavor) && push_registry=$(cfg pushRegistry) || { fail "設定 $conf を読めません"; return 0; }
 repo=$state/repo.git
 if [ ! -d "$repo" ]; then
  git clone --quiet --bare "$remote" "$repo" 2>/dev/null || { fail "Workerのリポジトリを取得できません（$remote）"; return 0; }
 fi
 git --git-dir="$repo" fetch --quiet origin "+refs/heads/$branch:refs/heads/$branch" 2>/dev/null || { fail 'Workerのtrunkを取得できません'; return 0; }
 sha=$(git --git-dir="$repo" rev-parse --verify "$branch^{commit}") || { fail "Workerの $branch を読めません"; return 0; }
 [ "$(cat "$state/registered" 2>/dev/null || true)" != "$sha" ] || return 0
 # 時刻や差分を確認できない場合は待機せず、従来どおり登録を試みる。
 waiting_until=$(python3 - "$repo" "$state/registered" "$sha" <<'PY'
import datetime, os, subprocess, sys, time
repo, registered, sha = sys.argv[1:]
try:
    with open(registered, encoding="ascii") as f:
        base = f.read().strip()
    now = time.time()
    registered_at = os.stat(registered).st_mtime
    if not base or registered_at > now or now - registered_at >= 3600:
        sys.exit(0)
    subprocess.run(["git", "--git-dir", repo, "cat-file", "-e", base + "^{commit}"], check=True, stderr=subprocess.DEVNULL)
    files = subprocess.run(["git", "--git-dir", repo, "diff", "--no-renames", "--name-only", "-z", base, sha], check=True, capture_output=True).stdout.split(b"\0")
    align_files = (b"deploy/versions.json", b"deploy/antigravity-release.json", b"internal/execution/config.go")
    align_prefixes = (b"internal/intake/",)
    ignore_exact = (b"README.md",)
    ignore_prefixes = (b"docs/",)
    ignore_suffixes = (b"/README.md", b"_test.go")
    for file in files:
        if not file or file.startswith(ignore_prefixes) or file in ignore_exact or file.endswith(ignore_suffixes):
            continue
        if file in align_files or file.startswith(align_prefixes):
            sys.exit(0)
    jst = datetime.timezone(datetime.timedelta(hours=9))
    print(datetime.datetime.fromtimestamp(registered_at + 3600, jst).strftime("%Y-%m-%d %H:%M:%S %z"))
except (OSError, ValueError, UnicodeError, subprocess.SubprocessError, OverflowError):
    pass
PY
 ) || waiting_until=''
 if [ -n "$waiting_until" ]; then
  say "[update:worker-release-waiting] 前回の登録から60分未満のため待ちます。次に登録できる時刻: $waiting_until"
  return 0
 fi
 failed=$(cat "$state/failed" 2>/dev/null || true)
 if [ "${failed%% *}" = "$sha" ]; then
  failed_at=${failed#* }
  now=$(date +%s)
  case "$failed_at" in ''|*[!0-9]*) failed_at=0 ;; esac
  [ $((now - failed_at)) -ge 3600 ] || return 0
 fi
 note_failure() { printf '%s %s\n' "$sha" "$(date +%s)" >"$state/failed"; fail "$*"; return 0; }
 short=$(printf '%s' "$sha" | cut -c1-12)
 tag=$short
 repository=issue-desk-client
 target=''
 if [ "$flavor" = -python ]; then tag=$short-client-python; repository=issue-desk-client-python; target=client-python; fi
 say "[update:worker-release] Worker $short を発行して登録します"

 # 保存されたイメージが正しく、ローカルに存在する場合だけ優先する。
 saved_tools=$(python3 - "$state/tools-image" <<'PYIMAGE'
import re, sys
try:
    with open(sys.argv[1], encoding="ascii") as f:
        value = f.read()
    if re.fullmatch(r"issue-desk-tools:[0-9a-f]{12}\n?", value):
        print(value.strip())
except (OSError, UnicodeError):
    pass
PYIMAGE
 ) || saved_tools=''
 if [ -n "$saved_tools" ] && docker image inspect "$saved_tools" >/dev/null 2>&1; then TOOLS_IMAGE=$saved_tools; fi
 docker image inspect "$TOOLS_IMAGE" >/dev/null 2>&1 || { note_failure "ツールイメージ $TOOLS_IMAGE がありません"; return 0; }
 tools_base=${TOOLS_IMAGE##*:}
 rebuild=0
 if ! git --git-dir="$repo" cat-file -e "$tools_base^{commit}" 2>/dev/null; then
  rebuild=1
 else
  # Worker の旧版 (.mjs) と TypeScript 版 (.mts) の両方を配布対象にできる。
  changes=$(git --git-dir="$repo" log --oneline "$tools_base..$sha" -- \
   Dockerfile.tools deploy/versions.json deploy/build-tools.sh \
   'deploy/install-agent-tools.m[jt]s' 'deploy/install-antigravity.m[jt]s' deploy/antigravity-release.json \
   deploy/install-caddy.sh deploy/install-node.sh 'deploy/check-image-tools.m[jt]s' \
   'deploy/image-versions.m[jt]s' 'deploy/tools-policy.test.m[jt]s' deploy/agy.sh \
   'deploy/install-sdks.m[jt]s' deploy/sdk-release.json) || { note_failure 'ツール定義の変更を確認できません'; return 0; }
  [ -z "$changes" ] || rebuild=1
 fi
 if [ "$rebuild" = 1 ]; then
  new_tools=issue-desk-tools:$short
  if ! docker image inspect "$new_tools" >/dev/null 2>&1; then
   if ! git --git-dir="$repo" archive --format=tar -o "$work/tools-context.tar" "$sha" 2>"$state/tools-build.log" ||
      ! docker build -f Dockerfile.tools --build-arg "ISSUE_DESK_TOOLS_REVISION=$sha" -t "$new_tools" - <"$work/tools-context.tar" >>"$state/tools-build.log" 2>&1; then
    note_failure "ツールイメージの構築に失敗しました（$state/tools-build.log を見てください）"; return 0
   fi
  fi
  printf '%s\n' "$new_tools" >"$state/tools-image.tmp" && mv -f "$state/tools-image.tmp" "$state/tools-image" || { note_failure 'ツールイメージの状態を保存できません'; return 0; }
  TOOLS_IMAGE=$new_tools
 fi

 accept='application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'
 code=$(curl -sS -o /dev/null -w '%{http_code}' -I -H "Accept: $accept" "http://$push_registry/v2/$repository/manifests/$tag" 2>/dev/null) || { note_failure 'レジストリのタグの有無を確認できません'; return 0; }
 case "$code" in
  200) : ;;
  404)
   git --git-dir="$repo" archive --format=tar -o "$work/context.tar" "$sha" || { note_failure 'git archive に失敗しました'; return 0; }
   if [ -n "$target" ]; then
    docker build --target "$target" --build-arg "TOOLS_IMAGE=$TOOLS_IMAGE" --build-arg "ISSUE_DESK_REVISION=$sha" -t "$push_registry/$repository:$tag" - <"$work/context.tar" >"$state/build.log" 2>&1 || { note_failure "ビルドに失敗しました（$state/build.log を見てください）"; return 0; }
   else
    docker build --build-arg "TOOLS_IMAGE=$TOOLS_IMAGE" --build-arg "ISSUE_DESK_REVISION=$sha" -t "$push_registry/$repository:$tag" - <"$work/context.tar" >"$state/build.log" 2>&1 || { note_failure "ビルドに失敗しました（$state/build.log を見てください）"; return 0; }
   fi
   docker push "$push_registry/$repository:$tag" >"$state/push.log" 2>&1 || { note_failure "pushに失敗しました（$state/push.log を見てください）"; return 0; }
   ;;
  *) note_failure "レジストリのタグの有無を確認できません（HTTP $code）"; return 0 ;;
 esac
 digest=$(curl -fsS -o /dev/null -D - -I -H "Accept: $accept" "http://$push_registry/v2/$repository/manifests/$tag" 2>/dev/null |
  sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *\(sha256:[0-9a-f]\{64\}\).*$/\1/p' | tr -d '\r')
 [ -n "$digest" ] || { note_failure '発行したイメージのdigestを読めません'; return 0; }

 admin() { sh "$DIR/deploy/nas/intake.sh" exec -T /usr/local/bin/intake admin --config /state/config.local.json --application-dir /app "$@"; }
 # 変更の基点は、前回この段が登録した版（全PCにまとめて登録するので、PC専用の行の現在の版と一致する）。
 # 一致しないと受付は種類を align（すぐ引き取りを止めて更新）にしてしまう。初回だけ、受付の共通の行の版で代える。
 base=$(cat "$state/registered" 2>/dev/null || true)
 if [ -z "$base" ]; then
  if [ -z "$flavor" ]; then base=$(admin client-release previous </dev/null 2>/dev/null | tr -d '\r\n') || base=''
  else base=$(admin client-release previous "$flavor" </dev/null 2>/dev/null | tr -d '\r\n') || base=''; fi
 fi
 hosts=$(cfg hosts keys) || { note_failure '設定の hosts を読めません'; return 0; }
 # 一部のPCだけ登録してから失敗しないよう、全キーを先に確認する。
 set -f # ホスト名をパス名展開しない
 for host in $hosts; do
  case "$host" in *.*) note_failure "設定の hosts のキーにドットは使えません: $host"; return 0 ;; esac
 done
 registered=0
 for host in $hosts; do
  registry=$(cfg "hosts.$host") || { note_failure "$host のレジストリ名を読めません"; return 0; }
  python3 - "$repo" "$sha" "$base" "$registry" "$tag" "$digest" "$host" "$flavor" >"$work/body.json" <<'PY' || { note_failure "$host の登録JSONを作れません"; return 0; }
import json, re, subprocess, sys
repo, sha, base, registry, tag, digest, host, flavor = sys.argv[1:]
body = {"host": host, "flavor": flavor, "registry": registry, "tag": tag, "digest": digest, "revision": sha}
if re.fullmatch(r"[0-9a-f]{40}", base) and subprocess.run(["git", "--git-dir", repo, "cat-file", "-e", base + "^{commit}"], stderr=subprocess.DEVNULL).returncode == 0:
    out = subprocess.run(["git", "--git-dir", repo, "diff", "--no-renames", "--name-only", "-z", base, sha], check=True, capture_output=True).stdout
    body["baseRevision"] = base
    body["changedFiles"] = [f for f in out.decode().split("\0") if f]
json.dump(body, sys.stdout)
PY
  if admin client-release register <"$work/body.json" >"$work/register-$host.log" 2>&1; then
   registered=$((registered + 1))
   sed -n 's/^警告: /[update:worker-release-warning] /p' "$work/register-$host.log" | while IFS= read -r line; do say "$line"; done
  else
   note_failure "$host への登録に失敗しました"; return 0
  fi
 done
 [ "$registered" -gt 0 ] || { note_failure '登録するPCがありません（設定の hosts が空）'; return 0; }
 printf '%s\n' "$sha" >"$state/registered"
 rm -f "$state/failed"
 say "[update:worker-release-success] Worker $short を発行し、${registered}台に登録しました。各PCのhostdが5分周期で取り込みます"
 return 0
)
