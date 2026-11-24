#!/bin/sh
set -eu

if [ "$#" -ne 5 ]; then
  echo 'usage: build-public-intake-verify-image.sh REPO COMMIT IMAGE GO_IMAGE NODE_IMAGE' >&2
  echo 'optional: PUBLIC_INTAKE_CA_BUNDLE=/path/to/public-ca-bundle.pem (requires openssl and BuildKit)' >&2
  exit 2
fi
repo=$1
commit=$2
image=$3
go_image=$4
node_image=$5
case "$commit" in *[!0-9a-f]*|'') echo 'invalid commit' >&2; exit 2;; esac
[ "${#commit}" -eq 40 ] || { echo 'invalid commit' >&2; exit 2; }
for base in "$go_image" "$node_image"; do
  digest=${base##*@sha256:}
  case "$base" in *@sha256:*) ;; *) echo 'base must be pinned by digest' >&2; exit 2;; esac
  case "$digest" in *[!0-9a-f]*|'') echo 'invalid digest' >&2; exit 2;; esac
  [ "${#digest}" -eq 64 ] || { echo 'invalid digest' >&2; exit 2; }
done
[ "$(git -C "$repo" cat-file -t "$commit" 2>/dev/null)" = commit ] || { echo 'commit not found' >&2; exit 2; }
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
context=$temporary/context
mkdir "$context"
if [ "${PUBLIC_INTAKE_CA_BUNDLE+x}" = x ]; then
  [ -n "$PUBLIC_INTAKE_CA_BUNDLE" ] && [ -f "$PUBLIC_INTAKE_CA_BUNDLE" ] && [ -r "$PUBLIC_INTAKE_CA_BUNDLE" ] || { echo 'invalid CA bundle' >&2; exit 2; }
  cp "$PUBLIC_INTAKE_CA_BUNDLE" "$temporary/ca.pem" || { echo 'invalid CA bundle' >&2; exit 2; }
  # Reject anything except complete PEM certificates, then parse each certificate.
  awk -v dir="$temporary" '
    /^-----BEGIN CERTIFICATE-----\r?$/ { if (inside) exit 1; inside=1; count++; file=dir "/cert-" count; print > file; next }
    /^-----END CERTIFICATE-----\r?$/ { if (!inside) exit 1; print >> file; close(file); inside=0; next }
    { if (inside) print >> file; else if ($0 !~ /^[[:space:]]*$/) exit 1 }
    END { if (inside || !count) exit 1 }
  ' "$temporary/ca.pem" || { echo 'invalid CA bundle' >&2; exit 2; }
  for cert in "$temporary"/cert-*; do
    openssl x509 -in "$cert" -noout >/dev/null 2>&1 || { echo 'invalid CA bundle' >&2; exit 2; }
    rm -f "$cert"
  done
  set -- --secret "id=intake_ca,src=$temporary/ca.pem"
else
  set --
fi
for file in server/go.mod server/go.sum client/package.json client/pnpm-lock.yaml client/pnpm-workspace.yaml deploy/versions.json; do
  entry=$(git -C "$repo" ls-tree "$commit" -- "$file")
  case "$entry" in "100644 blob "*|"100755 blob "*) ;; *) echo "missing regular file: $file" >&2; exit 1;; esac
  mkdir -p "$context/$(dirname "$file")"
  git -C "$repo" show "$commit:$file" > "$context/$file"
done
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cp "$script_dir/../deploy/public/intake-verify.Dockerfile" "$context/Dockerfile"
DOCKER_BUILDKIT=1 docker build "$@" --build-arg "GO_IMAGE=$go_image" --build-arg "NODE_IMAGE=$node_image" -t "$image" "$context"
