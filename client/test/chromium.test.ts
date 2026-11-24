import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { startChromium } from './chromium.ts';

test('ブラウザが応答を開始しない場合は起動待ちを打ち切る', async (t) => {
  const directory = mkdtempSync(join(tmpdir(), 'chromium-startup-'));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const executable = join(directory, 'chromium');
  writeFileSync(
    executable,
    [
      '#!/bin/sh',
      `echo 'process_singleton_posix.cc: socket() failed: Operation not permitted' >&2`,
      'exec sleep 20',
      '',
    ].join('\n'),
    { mode: 0o700 },
  );

  await assert.rejects(
    startChromium(t, executable, directory, 'about:blank', 500),
    /ブラウザの起動を 500ms 以内に確認できませんでした: process_singleton_posix/,
  );
});
