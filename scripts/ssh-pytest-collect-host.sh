#!/bin/sh
# A9 MAX only. The signer verifies this file before invoking it.
set -eu
umask 077
work=$(mktemp -d /tmp/desk-pytest-collect.XXXXXXXX)
name=${work##*/}
pid=
active_container=
cleanup() {
  if [ -n "$pid" ]; then
    kill "$pid" 2>/dev/null || :
  fi
  if [ -n "$active_container" ]; then
    # A create request might reach Docker just before the client exits.
    for attempt in 1 2 3 4 5; do
      if timeout -k 1 1 docker rm -f "$active_container" >/dev/null 2>&1; then
        break
      fi
      sleep 1
    done
  fi
  if [ -n "$pid" ]; then
    wait "$pid" 2>/dev/null || :
  fi
  rm -rf -- "$work"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM PIPE
fail() { printf '%s\n' "$1" >&2; exit 1; }
unpack() {
  target=$1
  archive="$work/$target.tar.gz"
  head -c "$2" > "$archive"
  printf '%s  %s\n' "$3" "$archive" | sha256sum -c --status ||
    fail "固定アーカイブの指紋が一致しません: $target"
}
unpack before 1044030 131bfef59e8a53433bf805ff92bf7bf627963baa7d83548c71bf224eaa172982
unpack after 1044383 0ed0cfdaf0c93601b6b7b18775fec0d19f33d071260c55681d484ffa0f9239d8
test -z "$(head -c 1 | od -An -tx1 | tr -d '[:space:]')" || fail '余分な入力があります。'
for target in before after; do
  mkdir "$work/$target"
  tar -xzf "$work/$target.tar.gz" -C "$work/$target"
done
container=$(docker ps --filter label=com.docker.compose.service=issue-desk-client --format '{{.ID}}')
test -n "$container" && test "$(printf '%s\n' "$container" | wc -l)" -eq 1 ||
  fail '稼働Workerのツールイメージを一意に確認できません。'
image=$(docker inspect --format '{{.Image}}' "$container")
test -n "$image" || fail '稼働Workerのイメージを確認できません。'
cat > "$work/collect.py" <<'PYTHON'
import hashlib
import json
import os
from pathlib import Path
import subprocess
from statistics import median
import time

root = Path("/verify")
python = root / "before/.venv/bin/python"
modes = {"default": [], "slow": ["-m", "slow"], "all": ["-m", "slow or not slow"]}
hashes = {
    "before": "ce08f887147a1f8595d962be4f237664c0f2ed68",
    "after": "f7328b399aa6ccc2b1befdc8c724e1e09f5ef812",
}
quality = {
    "test_code_quality.py",
    "test_deadcode.py",
    "test_typescript_sources.py",
    "test_update_reload.py",
}


def collect(target, extra_args):
    cmd = [str(python), "-m", "pytest", "--collect-only", "-q",
           "-n", "0", "-p", "no:cacheprovider", *extra_args]
    start = time.monotonic()
    print(json.dumps({"head": hashes[target], "command": cmd[1:]}), flush=True)
    result = subprocess.run(cmd, cwd=root / target, capture_output=True,
                            text=True, timeout=600, env={
                                **os.environ, "PYTHONDONTWRITEBYTECODE": "1",
                            })
    seconds = round(time.monotonic() - start, 3)
    if result.returncode:
        print(json.dumps({"head": hashes[target], "exit": result.returncode,
                          "error": (result.stdout + result.stderr)[-2400:]}))
        raise SystemExit(result.returncode)
    ids = [line for line in result.stdout.splitlines() if line.startswith("tests/") and "::" in line]
    if not ids or len(ids) != len(set(ids)):
        raise SystemExit("missing or duplicate pytest collection IDs")
    return set(ids), seconds


def summary(ids, seconds):
    return {"count": len(ids), "seconds": round(seconds, 3),
            "sha256": hashlib.sha256("\n".join(sorted(ids)).encode()).hexdigest()}


sets = {}
for mode, args in modes.items():
    samples = {"before": [], "after": []}
    collected = {}
    for order in (("before", "after"), ("after", "before")):
        for target in order:
            ids, seconds = collect(target, args)
            if target in collected and collected[target] != ids:
                raise SystemExit("pytest IDs changed between measurements")
            collected[target] = ids
            samples[target].append(seconds)
    before, after = collected["before"], collected["after"]
    before_time = median(samples["before"])
    after_time = median(samples["after"])
    sets[mode] = (before, after)
    added = sorted(after - before)
    missing = sorted(before - after)
    known_additions = {
        f"tests/test_pytest_execution.py::test_slow_file_import_and_selection[{case}]"
        for case in ("arguments0-False-False", "arguments1-True-True",
                     "arguments2-True-True", "arguments3-True-False")
    }
    expected_additions = set() if mode == "slow" else known_additions
    unexpected = sorted(set(added) ^ expected_additions)
    quality_after = sorted({Path(i.split("::", 1)[0]).name for i in after} & quality)
    print(json.dumps({"mode": mode, "before": summary(before, before_time),
                      "after": summary(after, after_time),
                      "differenceSeconds": round(before_time - after_time, 3),
                      "samplesSeconds": samples, "order": ["before/after", "after/before"],
                      "addedCount": len(added), "added": added[:40],
                      "missingCount": len(missing), "missing": missing[:40],
                      "unexpectedCount": len(unexpected), "unexpected": unexpected[:40],
                      "qualityFiles": quality_after}), flush=True)
    if missing or unexpected or (mode == "default" and set(quality_after) != quality):
        raise SystemExit("unexpected pytest selection change")

for index in range(2):
    normal, slow, all_ids = (sets[mode][index] for mode in modes)
    if normal & slow or normal | slow != all_ids:
        raise SystemExit("default and slow selections do not partition all IDs")
PYTHON
run() {
  suffix=$1
  shift
  active_container="$name-$suffix"
  docker run --rm --init --pull never --cap-drop ALL --security-opt no-new-privileges:true \
    --name "$active_container" --user "$(id -u):$(id -g)" \
    --mount "type=bind,source=$work,target=/verify" \
    --env HOME=/tmp --entrypoint /usr/local/bin/python3 "$@" > "$work/$suffix.log" 2>&1 &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    sleep 1
    printf '.\n'
  done
  code=0
  wait "$pid" || code=$?
  pid=
  if [ "$code" -eq 0 ]; then active_container=; fi
  awk 'tolower($0) !~ /idk_|idh_|private key|password|secret|token/' "$work/$suffix.log" | tail -c 32000
  return "$code"
}
run setup --network bridge -w /verify/before "$image" scripts/setup_test_env.py
run collect --network none "$image" /verify/collect.py
