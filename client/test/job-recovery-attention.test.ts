import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { compileSource } from '../scripts/source.ts';
import {
  attentionRows,
  rowGroup,
  requestRows,
  selectAttention,
} from '../src/ui/request-classification.ts';

test('attention classification existing boundaries', () => {
  const rows = attentionRows([
    {
      key: 'job-recovery:owner/repo#12',
      kind: 'job_recovery',
      stateLabel: '利用者の判断が必要',
      project: 'owner/repo',
      number: 12,
      desk: 4703,
      title: '自動認証',
      reason: '承認時の前提が変わりました。',
      requiresUser: true,
      nextAction: '変更された対象と条件を承認するか判断する',
    } as any,
  ]);
  assert.equal(rows.length, 1);
  assert.equal(rows[0].caseKind, 'job_recovery');
  assert.equal(rows[0].reason, '承認時の前提が変わりました。');
  assert.equal(rows[0].desk, 4703);
  assert.equal(rowGroup(rows[0]), 'attention');
  const pending = {
    id: 'deploy',
    headSha: 'a'.repeat(40),
    reason: '対象と条件の承認待ち',
    fingerprint: 'fp',
  };
  const requests = requestRows(
    [
      { project: 'o/r', number: 9, status: 'failed', state: 'closed' },
      {
        project: 'o/r',
        number: 10,
        status: 'review_held',
        pendingFinalApprovals: [pending],
      },
      { project: 'o/r', number: 11, status: 'stopped', followup: true },
    ],
    [],
    [
      {
        id: 'confirm',
        project: 'o/r',
        number: 9,
        status: 'confirm',
        state: 'closed',
      },
    ],
  );
  assert.deepEqual(requests.map(rowGroup), [
    'closed',
    'attention',
    'followups',
    'attention',
  ]);
  const selected = selectAttention(
    requests.filter((r) => rowGroup(r) === 'attention'),
  );
  assert.deepEqual(selected.attention[0].pendingFinalApprovals, [pending]);
  assert.equal(selected.attention.length, 2);
  const missingJudgmentRows = attentionRows([
    {
      key: 'old',
      kind: 'job_recovery',
      stateLabel: '通知',
      state: 'notify',
      reason: '旧記録',
    } as any,
  ]);
  assert.equal(selectAttention(missingJudgmentRows).attention.length, 0);
  assert.equal(
    selectAttention(missingJudgmentRows).monitoring[0].reason,
    '旧記録',
  );
});

test('判断理由と具体的な質問を表示し、無関係な調査Issueや架空の回数を案内しない', () => {
  const source = fs.readFileSync(
    new URL('../src/ui/app-attention-alerts.ts', import.meta.url),
    'utf8',
  );
  const lines = vm.runInNewContext(
    compileSource(
      source +
        '\nattentionAlertLines({caseKind:"job_recovery",issueTitle:"自動認証",stateLabel:"利用者の判断が必要",reason:"前提が変わりました",gist:"新しい対象への変更を承認するか判断してください"});',
      'attention.ts',
    ),
  );
  assert.ok(lines.includes('前提が変わりました'));
  assert.ok(lines.includes('新しい対象への変更を承認するか判断してください'));
  assert.ok(!lines.some((line: string) => /調査Issue|回数/.test(line)));
});
