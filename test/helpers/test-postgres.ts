import fs from 'node:fs';
import path from 'node:path';

// Stand-ins only for testing setup/cleanup control flow. The acceptance tests
// still require the real PostgreSQL engine and the shipped migrations.
export function writePostgresFixture(directory) {
  const bin = path.join(directory, 'bin');
  fs.mkdirSync(bin);
  const write = (name, source) =>
    fs.writeFileSync(path.join(bin, name), `#!/bin/sh\nset -eu\n${source}\n`, {
      mode: 0o700,
    });
  write(
    'postgres',
    `if [ "$1" = --version ]; then echo 'postgres (PostgreSQL) 17.11'; else cat >/dev/null; fi`,
  );
  write('initdb', `mkdir -p "$2"`);
  write(
    'pg_ctl',
    `data="$2"
for action in "$@"; do :; done
printf '%s\\n' "$action" >> "$VERIFY_PG_CALLS"
if [ "$action" = start ]; then
  printf '%s\\n' "$*" >> "$VERIFY_PG_CALLS"
  if [ -n "\${VERIFY_PG_DIRECTORY_MARKER:-}" ]; then dirname "$data" > "$VERIFY_PG_DIRECTORY_MARKER"; fi
  touch "$data/postmaster.pid"
  exit "\${VERIFY_PG_START_EXIT:-0}"
fi
if [ "\${VERIFY_PG_STOP_EXIT:-0}" != 0 ]; then exit "$VERIFY_PG_STOP_EXIT"; fi
rm -f "$data/postmaster.pid"`,
  );
  return bin;
}

// Each recorded call element is one line. Compare whole lines so that a
// temporary path containing "start" or "stop" is not mistaken for an action.
export function readPostgresCalls(file) {
  return fs.readFileSync(file, 'utf8').split('\n');
}
