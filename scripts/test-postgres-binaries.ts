import fs from 'node:fs';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';

export const binaries = JSON.parse(
  fs.readFileSync(
    new URL('./test-postgres-binaries.json', import.meta.url),
    'utf8',
  ),
);

export function checkArchive(bytes, integrity) {
  if (
    `sha512-${createHash('sha512').update(bytes).digest('base64')}` !==
    integrity
  ) {
    throw new Error('検証用PostgreSQLの配布物の整合性が一致しません。');
  }
}

// Download data only: never run npm lifecycle scripts. All native files stay in
// the invocation's scratch directory; the application and host are untouched.
export async function preparePostgres(
  directory,
  env,
  signal,
  config = binaries,
) {
  if (env.ISSUE_DESK_TEST_POSTGRES_BIN)
    return path.resolve(env.ISSUE_DESK_TEST_POSTGRES_BIN);
  const platform = `${process.platform}-${process.arch}`;
  const entry = config.platforms[platform];
  const client = config.clients[platform];
  if (!entry || !client)
    throw new Error(
      'この環境用の検証PostgreSQLがありません。専用DSNかISSUE_DESK_TEST_POSTGRES_BINを指定してください。',
    );
  async function download(entry) {
    const response = await fetch(entry.url, {
      redirect: 'error',
      signal: AbortSignal.any([signal, AbortSignal.timeout(120_000)]),
    });
    if (!response.ok)
      throw new Error(
        `検証用PostgreSQLの取得に失敗しました（HTTP ${response.status}）。`,
      );
    const chunks = [];
    let size = 0;
    for await (const chunk of response.body) {
      size += chunk.length;
      if (size > 64 * 1024 * 1024)
        throw new Error('検証用PostgreSQLの配布物が上限を超えています。');
      chunks.push(chunk);
    }
    const bytes = Buffer.concat(chunks);
    checkArchive(bytes, entry.integrity);
    return bytes;
  }
  const bytes = await download(entry);
  const clientBytes = await download(client);
  const archive = path.join(directory, 'postgres.tgz');
  fs.writeFileSync(archive, bytes, { mode: 0o600 });
  const extracted = spawnSync(
    'tar',
    ['-xzf', archive, '-C', directory, '--no-same-owner'],
    { encoding: 'utf8' },
  );
  if (extracted.error || extracted.status !== 0)
    throw new Error('検証用PostgreSQLの展開に失敗しました。');
  const root = path.join(directory, 'package');
  // npm cannot ship these native library links. Recreate only links whose
  // source and destination remain inside this verified package, without scripts.
  const links = JSON.parse(
    fs.readFileSync(path.join(root, 'native', 'pg-symlinks.json'), 'utf8'),
  );
  for (const { source, target } of links) {
    const from = path.resolve(root, source),
      to = path.resolve(root, target);
    if (![from, to].every((file) => file.startsWith(`${root}${path.sep}`)))
      throw new Error('検証用PostgreSQLに範囲外のリンクがあります。');
    fs.symlinkSync(path.relative(path.dirname(to), from), to);
  }
  const clientDir = path.join(directory, 'client');
  fs.mkdirSync(clientDir);
  const clientArchive = path.join(directory, 'client.tgz');
  fs.writeFileSync(clientArchive, clientBytes, { mode: 0o600 });
  const clientExtract = spawnSync(
    'tar',
    ['-xzf', clientArchive, '-C', clientDir, '--no-same-owner'],
    { encoding: 'utf8' },
  );
  if (clientExtract.error || clientExtract.status !== 0)
    throw new Error('検証用PostgreSQLクライアントの展開に失敗しました。');
  const clientRoot = path.join(clientDir, 'package', 'pg');
  fs.symlinkSync('libpq.so.5.18', path.join(clientRoot, 'lib', 'libpq.so.5'));
  const bin = path.join(root, 'native', 'bin');
  for (const name of ['pg_dump', 'pg_restore', 'psql']) {
    // Resolve the client relative to the wrapper so temporary paths need no shell quoting.
    fs.writeFileSync(
      path.join(bin, name),
      `#!/bin/sh\nbase=$(CDPATH= cd -- "$(dirname -- "$0")/../../../client/package/pg" && pwd)\nLD_LIBRARY_PATH="$base/lib${'${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}'}" exec "$base/bin/${name}" "$@"\n`,
      { mode: 0o700 },
    );
  }
  return bin;
}
