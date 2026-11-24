import test from 'node:test';
import assert from 'node:assert/strict';
import { requestRows } from '../src/ui/request-classification.ts';

test('明示終了した子の記録を維持し、親相談は一覧に表示しない', () => {
  const rows = requestRows(
    [],
    [
      { id: 'parent', status: 'closed', consultationChildren: ['child'] },
      {
        id: 'child',
        status: 'closed',
        cleanup: { reason: '本人指示' },
        parentConsultation: 'parent',
        issue: { number: 5 },
      },
    ],
  );
  assert.equal(rows.length, 1);
  assert.equal(rows[0].id, 'child');
  assert.equal(rows[0].status, 'closed');
  assert.equal(rows[0].kind, 'draft');
});
