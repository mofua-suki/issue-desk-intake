import test from 'node:test';
import assert from 'node:assert/strict';
import { checkCompletion } from '../skills/task-completion/scripts/check-completion.ts';

const done = () => ({
  version: 1,
  objective: 'リファクタリングを直接PRでマージする',
  originalRequirements: ['refactor'],
  deliverables: [
    {
      id: 'refactor',
      description: '分割とマージ',
      status: 'verified',
      evidence: ['PR 568 merged'],
    },
  ],
  agents: [],
});

test('種類の指定が無い台帳は従来どおり完了と判定する', () => {
  assert.equal(checkCompletion(done()).decision, 'complete');
});

test('完了の基準が無い依頼は、最後の見直しの記録が無ければ続ける', () => {
  const result = checkCompletion({ ...done(), kind: 'open-ended' });
  assert.equal(result.decision, 'continue');
  assert.match(
    result.reasons.join('\n'),
    /最後の独立した見直しの記録がありません/,
  );
});

test('最後の見直しに直すべき指摘が残っていれば続ける', () => {
  const result = checkCompletion({
    ...done(),
    kind: 'open-ended',
    finalReview: {
      evidence: ['見直し3回目'],
      findings: ['長い関数が残っている'],
    },
  });
  assert.equal(result.decision, 'continue');
  assert.match(
    result.reasons.join('\n'),
    /指摘が残っています: 長い関数が残っている/,
  );
});

test('最後の見直しで指摘が無く、残す指摘に理由があれば完了と判定する', () => {
  const result = checkCompletion({
    ...done(),
    kind: 'open-ended',
    finalReview: {
      evidence: ['見直し3回目'],
      findings: [],
      kept: [{ finding: 'http を断らない', reason: '本番が http で止まる' }],
    },
  });
  assert.equal(result.decision, 'complete');
});

test('残すと決めた指摘に理由が無ければ続ける', () => {
  const result = checkCompletion({
    ...done(),
    kind: 'open-ended',
    finalReview: {
      evidence: ['見直し3回目'],
      findings: [],
      kept: [{ finding: 'http を断らない', reason: '' }],
    },
  });
  assert.equal(result.decision, 'continue');
});

test('知らない種類の台帳は不正として続ける', () => {
  const result = checkCompletion({ ...done(), kind: 'other' });
  assert.equal(result.decision, 'continue');
  assert.match(result.reasons.join('\n'), /依頼の種類が不正です/);
});

test('見直しの記録だけあって種類が無い台帳は不正として続ける', () => {
  const result = checkCompletion({
    ...done(),
    finalReview: { evidence: ['見直し'], findings: [] },
  });
  assert.equal(result.decision, 'continue');
  assert.match(result.reasons.join('\n'), /kind: "open-ended"/);
});

for (const [name, finalReview] of [
  ['null', null],
  ['空の証跡', { evidence: [], findings: [] }],
  ['配列でない指摘', { evidence: ['見直し'], findings: 'なし' }],
  ['配列でない kept', { evidence: ['見直し'], findings: [], kept: 'なし' }],
  [
    '指摘の無い kept',
    { evidence: ['見直し'], findings: [], kept: [{ reason: '理由' }] },
  ],
] as const) {
  test(`形の崩れた見直しの記録（${name}）は完了にしない`, () => {
    assert.equal(
      checkCompletion({ ...done(), kind: 'open-ended', finalReview }).decision,
      'continue',
    );
  });
}

test('見直しで指摘が無く残す指摘も無ければ、kept を省いても完了と判定する', () => {
  const result = checkCompletion({
    ...done(),
    kind: 'open-ended',
    finalReview: { evidence: ['見直し'], findings: [] },
  });
  assert.equal(result.decision, 'complete');
});

test('見直しで指摘が無くても、作業項目が終わっていなければ続ける', () => {
  const ledger = {
    ...done(),
    kind: 'open-ended',
    finalReview: { evidence: ['見直し'], findings: [] },
  };
  ledger.deliverables = [
    {
      id: 'refactor',
      description: '分割とマージ',
      status: 'running',
      evidence: [],
    },
  ];
  assert.equal(checkCompletion(ledger).decision, 'continue');
});

for (const [name, finalReview] of [
  ['文字列の kept', { evidence: ['見直し'], findings: [], kept: ['理由なし'] }],
  [
    '空白だけの理由',
    {
      evidence: ['見直し'],
      findings: [],
      kept: [{ finding: '指摘', reason: '  ' }],
    },
  ],
  ['文字列以外の指摘', { evidence: ['見直し'], findings: [1] }],
] as const) {
  test(`形の崩れた見直しの記録（${name}）は完了にしない`, () => {
    assert.equal(
      checkCompletion({ ...done(), kind: 'open-ended', finalReview }).decision,
      'continue',
    );
  });
}
