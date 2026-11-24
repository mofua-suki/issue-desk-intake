import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, copyFile, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';

test('生成コマンドは出荷される両モジュールを再現する', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'consultation-generation-'));
  try {
    await mkdir(join(dir, 'scripts/lib'), { recursive: true });
    await mkdir(join(dir, 'shared/src'), { recursive: true });
    await copyFile(
      'scripts/generate-consultation-links.ts',
      join(dir, 'scripts/generate-consultation-links.ts'),
    );
    execFileSync(process.execPath, [
      join(dir, 'scripts/generate-consultation-links.ts'),
    ]);
    for (const path of [
      'shared/src/attention-split.generated.ts',
      'scripts/lib/consultation-link-output.generated.ts',
    ]) {
      assert.deepEqual(await readFile(join(dir, path)), await readFile(path));
    }
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

// desk-alertsが使う生成helperの振る舞いは、読取専用CLIの廃止後も検証する。
test('受付の記録の担当・指示・根拠と受付画面リンクを優先する', async () => {
  const { nextStepForRow } = await import(
    '../scripts/lib/consultation-link-output.generated.ts'
  );
  const input = {
    kind: 'issue',
    project: 'owner/repo',
    number: 42,
    desk: 7,
    status: 'queued',
    nextActor: 'user',
    nextAction: ' 記録の 指示 ',
    evidence: { id: '記録', at: '日時' },
  };
  assert.deepEqual(nextStepForRow(input, 'https://desk.example/'), {
    nextActor: 'user',
    nextAction: '記録の 指示',
    evidence: { id: '記録', at: '日時' },
    url: 'https://desk.example/#/issues/7',
  });
});
