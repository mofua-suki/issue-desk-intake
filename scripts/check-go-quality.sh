#!/bin/sh
set -eu
unset GODEBUG
GO=${GO:-go}
# -test includes test-only callers. deadcode reports findings on stdout but exits
# successfully, so a nonempty report must explicitly fail this check.
status=0
report=$("$GO" run golang.org/x/tools/cmd/deadcode@v0.52.0 -test ./...) || status=$?
if [ -n "$report" ]; then
  printf '%s\n' "$report" >&2
  [ "$status" -ne 0 ] || status=1
fi
[ "$status" -eq 0 ] || exit "$status"
"$GO" run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
