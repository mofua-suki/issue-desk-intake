import { createHash } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { verifyGoResults } from './verify-review-notes.ts';

// Fixed archives are extracted only after verifying their digest. No npm lifecycle scripts run.
const archives = {
  'linux-x64': [
    'x64',
    'I4csxVyW7ck+iwkt7xHeI+0NmHFV2yYEhu/mAKyeJU5TUvf36OH3Z8oUsosQokPMQw0kQbQDcUuajXi90Wsulg==',
  ],
  'linux-arm64': [
    'arm64',
    'ct0AZx/knfNZBc2BZjCS0iPAKPJZzvHMPDiq6e2UrMvQoLH/+d5fjqYS5fYovqSq8MQSyJalqybTP5cLizturQ==',
  ],
};
const entry = archives[`${process.platform}-${process.arch}`];
if (!entry) throw new Error('Cコンパイラーを用意し、CCに指定してください。');
const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'issue-desk-race-'));
const controller = new AbortController();
const forward = () => controller.abort();
process.on('SIGINT', forward);
process.on('SIGTERM', forward);
try {
  const url = `https://registry.npmjs.org/@ryoppippi/zig-linux-${entry[0]}/-/zig-linux-${entry[0]}-0.13.0.tgz`;
  const response = await fetch(url, {
    redirect: 'error',
    signal: controller.signal,
  });
  if (!response.ok)
    throw new Error(
      `Cコンパイラーの取得に失敗しました: HTTP ${response.status}`,
    );
  const chunks = [];
  let size = 0;
  for await (const chunk of response.body) {
    size += chunk.length;
    if (size > 128 * 1024 * 1024)
      throw new Error('Cコンパイラーの配布物が大きすぎます。');
    chunks.push(chunk);
  }
  const bytes = Buffer.concat(chunks);
  if (createHash('sha512').update(bytes).digest('base64') !== entry[1])
    throw new Error('Cコンパイラーの整合性が一致しません。');
  const archive = path.join(directory, 'zig.tgz');
  fs.writeFileSync(archive, bytes, { mode: 0o600 });
  const extracted = spawnSync(
    'tar',
    ['-xzf', archive, '-C', directory, '--no-same-owner'],
    { stdio: 'inherit' },
  );
  if (extracted.error || extracted.status !== 0)
    throw new Error('Cコンパイラーの展開に失敗しました。');
  controller.signal.throwIfAborted();
  const wrapper = path.join(directory, 'cc');
  const zig = path.join(directory, 'package', 'zig');
  fs.writeFileSync(
    wrapper,
    `#!/bin/sh\nexec '${zig.replaceAll("'", "'\\''")}' cc "$@"\n`,
    { mode: 0o700 },
  );
  const env = {
    ...process.env,
    CGO_ENABLED: '1',
    CC: `"${wrapper}"`,
    ZIG_GLOBAL_CACHE_DIR: path.join(directory, 'cache'),
    ZIG_LOCAL_CACHE_DIR: path.join(directory, 'local-cache'),
    CGO_LDFLAGS: `${process.env.CGO_LDFLAGS || ''} -l:libgcc_s.so.1`,
  };
  process.exitCode = await verifyGoResults(['./internal/consultations'], {
    race: true,
    env,
    signal: controller.signal,
  });
} finally {
  fs.rmSync(directory, { recursive: true, force: true });
}
