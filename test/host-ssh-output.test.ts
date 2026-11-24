import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import {
  cleanOutput,
  reportSummary,
} from '../deploy/nas-host/host-ssh-output.ts';

const script = path.resolve('deploy/nas-host/host-ssh-output.ts');

test('ホスト作業用SSHは秘密らしい行を除いた出力の全文を呼び出し元へ渡し、受付には末尾だけを報告する', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'host-ssh-output-'));
  try {
    const input = path.join(dir, 'ssh-output');
    const out = path.join(dir, 'clean');
    const log = Array.from(
      { length: 400 },
      (_, i) => `worker log line ${i}`,
    ).join('\n');
    fs.writeFileSync(
      input,
      `${log}\nAuthorization token=abc\n-----BEGIN OPENSSH PRIVATE KEY-----\nlast line\n`,
    );
    const run = spawnSync(
      process.execPath,
      [script, input, 'ホスト作業用SSHの操作が完了しました。', out],
      { encoding: 'utf8' },
    );
    assert.equal(run.status, 0, run.stderr);
    const shown = fs.readFileSync(out, 'utf8');
    assert.match(
      shown,
      /^worker log line 0$/m,
      '先頭の行も呼び出し元に見える必要があります',
    );
    assert.match(shown, /^last line$/m);
    assert.doesNotMatch(shown, /token|PRIVATE KEY/);
    assert.ok(run.stdout.startsWith('ホスト作業用SSHの操作が完了しました。\n'));
    assert.ok(
      Array.from(run.stdout).length <=
        'ホスト作業用SSHの操作が完了しました。\n'.length + 1500,
    );
    assert.doesNotMatch(
      run.stdout,
      /^worker log line 0$/m,
      '受付への報告は末尾だけに絞る必要があります',
    );
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('出力が空なら要約は固定の文だけで、表示する全文も空にする', () => {
  assert.equal(cleanOutput('\n  \n'), '');
  assert.equal(reportSummary('固定', ''), '固定');
});
