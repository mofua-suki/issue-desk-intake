#!/bin/sh
# Send the two immutable snapshots to the A9 MAX acceptance operation.
set -eu
[ "$#" -eq 1 ] || { echo "使い方: $0 <demo-image-ratingの作業ツリー>" >&2; exit 2; }
repo=$1
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
umask 077
work=$(mktemp -d /tmp/desk-pytest-send.XXXXXXXX)
trap 'rm -rf -- "$work"' EXIT
runner="$root/scripts/ssh-pytest-collect-host.sh"
base64 -w 0 "$runner" > "$work/input"
printf '\n' >> "$work/input"
while read -r sha digest; do
  git -C "$repo" archive --format=tar --output="$work/tree.tar" "$sha"
  gzip -n -c "$work/tree.tar" > "$work/tree.tar.gz"
  printf '%s  %s\n' "$digest" "$work/tree.tar.gz" | sha256sum -c --status || {
    echo "固定アーカイブの指紋を確認できません: $sha" >&2
    exit 1
  }
  cat "$work/tree.tar.gz" >> "$work/input"
done <<'SNAPSHOTS'
ce08f887147a1f8595d962be4f237664c0f2ed68 131bfef59e8a53433bf805ff92bf7bf627963baa7d83548c71bf224eaa172982
f7328b399aa6ccc2b1befdc8c724e1e09f5ef812 0ed0cfdaf0c93601b6b7b18775fec0d19f33d071260c55681d484ffa0f9239d8
SNAPSHOTS
sh "$root/deploy/nas-host/host-ssh.sh" --host a9-max --operation verify-pytest-6357 \
  --reason '共通番号6357の指定した2版のpytest収集比較' --input "$work/input"
