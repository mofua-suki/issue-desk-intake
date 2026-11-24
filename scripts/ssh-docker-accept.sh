#!/bin/sh
# Send only the approved acceptance case and, when needed, its immutable archive.
set -eu
usage() {
  printf '%s\n' "使い方: $0 6293 <受付リポジトリ> <Workerリポジトリ>" "          $0 6358 <収集リポジトリ>" >&2
  exit 2
}
case "$#" in 2|3) ;; *) usage ;; esac
job=$1
case "$job" in
  6293)
    [ "$#" -eq 3 ] || usage
    head=4f7e290931af98a4f299e0f5b55c356ad67a760c
    digest=a220b5ec3f6d8c125b76b2037d1a28718b83692ef58f8c4d7d0a3626ec8085ab
    ;;
  6358)
    [ "$#" -eq 2 ] || usage
    head=fe2871c63ff7b3f70c870c6d82e2b5d39440e311
    digest=91aee0eea32a403a4a9794941e93a73b3a2567b8ec8c3802a98232125b8b37bd
    ;;
  *) usage ;;
esac
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
umask 077
work=$(mktemp -d /tmp/desk-docker-send.XXXXXXXX)
trap 'rm -rf -- "$work"' EXIT
trap 'exit 1' HUP INT TERM
base64 -w 0 "$root/scripts/ssh-docker-accept-host.sh" > "$work/input"
printf '\n%s\n' "$job" >> "$work/input"
append() {
  git -C "$1" archive --format=tar --output="$work/source.tar" "$2"
  gzip -n -c "$work/source.tar" > "$work/source.tar.gz"
  printf '%s  %s\n' "$3" "$work/source.tar.gz" | sha256sum -c --status || {
    printf '固定アーカイブの指紋が一致しません: %s\n' "$2" >&2
    exit 1
  }
  cat "$work/source.tar.gz" >> "$work/input"
}
append "$2" "$head" "$digest"
if [ "$job" = 6293 ]; then
  append "$3" bc0356ff9e2e638ae694461a983c97154074ce10 7181375118038e70a5512a2e2692bf713bbeb5c4559e3833c83401bca481e360
  append "$3" ca73ecf2fc7f4197dfed3489cde071814aaf922a fc5471eccd48f01de3c2073229cb98e3484d9e3057347f2fc589a7431ef58287
fi
sh "$root/deploy/nas-host/host-ssh.sh" --host gt12-pro --operation verify-docker-6293-6358 \
  --reason "共通番号$jobの固定版を使い捨てDocker資源で検証" --input "$work/input"
