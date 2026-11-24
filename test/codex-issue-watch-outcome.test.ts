import { test } from 'node:test';
import assert from 'node:assert/strict';
import { classifyWatchOutcome } from '../scripts/codex-issue-watch-outcome.ts';

const target = { desk: 6513, project: 'owner/repo', number: 10 };
const detail = () => ({
  ...target,
  state: 'closed',
  status: 'completed',
  body: '依頼',
  comments: [],
  pr: { number: 42, url: 'https://example.test/owner/repo/pulls/42' },
  result: { status: 'merged' },
});
const verified = { project: 'owner/repo', number: 42, merged: true };

test('対象PRの確認結果または受付の照会結果でマージを判定する', () => {
  assert.equal(classifyWatchOutcome(target, detail(), verified), 'merged');
  assert.equal(
    classifyWatchOutcome(target, {
      ...detail(),
      pr: { ...detail().pr, merged: true },
    }),
    'merged',
  );
  assert.equal(classifyWatchOutcome(target, detail()), 'unconfirmed');
  assert.equal(
    classifyWatchOutcome(target, detail(), { ...verified, number: 43 }),
    'unconfirmed',
  );
  assert.equal(
    classifyWatchOutcome(target, detail(), {
      ...verified,
      project: 'other/repo',
    }),
    'unconfirmed',
  );
  assert.equal(
    classifyWatchOutcome(target, { ...detail(), pr: undefined }, verified),
    'unconfirmed',
  );
  assert.equal(
    classifyWatchOutcome(
      target,
      { ...detail(), pr: { ...detail().pr, merged: false } },
      verified,
    ),
    'unconfirmed',
  );
});

test('全文と対象の一致を要求し、読取失敗や一部応答を終了にしない', () => {
  for (const value of [
    null,
    { ...detail(), desk: 1 },
    { ...detail(), project: 'other/repo' },
    { ...detail(), number: 11 },
    { ...detail(), body: undefined },
    { ...detail(), comments: undefined },
    { ...detail(), view: 'summary' },
    { ...detail(), partial: true },
  ]) {
    assert.equal(classifyWatchOutcome(target, value, verified), 'unconfirmed');
  }
  assert.equal(
    classifyWatchOutcome(target, { ...detail(), state: 'open' }),
    'continue',
  );
  assert.equal(
    classifyWatchOutcome(target, { ...detail(), state: 'unknown' }),
    'unconfirmed',
  );
});

test('手動またはトークンによる明示取下げと未確認のcloseを区別する', () => {
  for (const source of ['manual', 'token']) {
    const closed = {
      ...detail(),
      closeReason: { reason: 'voluntary', source },
    };
    assert.equal(classifyWatchOutcome(target, closed), 'unconfirmed');
    assert.equal(
      classifyWatchOutcome(target, closed, {
        ...verified,
        number: 43,
        merged: false,
      }),
      'unconfirmed',
    );
    assert.equal(classifyWatchOutcome(target, closed, verified), 'merged');
    assert.equal(
      classifyWatchOutcome(target, closed, { ...verified, merged: false }),
      'withdrawn',
    );
    assert.equal(
      classifyWatchOutcome(target, {
        ...closed,
        pr: { ...closed.pr, merged: false },
      }),
      'withdrawn',
    );
  }
  for (const closeReason of [
    undefined,
    { reason: 'other', source: 'manual' },
    { reason: 'voluntary', source: 'ai' },
    { reason: 'voluntary' },
  ]) {
    assert.equal(
      classifyWatchOutcome(
        target,
        { ...detail(), closeReason },
        { ...verified, merged: false },
      ),
      'unconfirmed',
    );
  }
  assert.equal(
    classifyWatchOutcome(target, {
      ...detail(),
      pr: { ...detail().pr, merged: false },
    }),
    'unconfirmed',
  );
  assert.equal(
    classifyWatchOutcome(target, {
      ...detail(),
      pr: undefined,
      closeReason: { reason: 'voluntary', source: 'manual' },
    }),
    'unconfirmed',
  );
});

test('直接PRのルートがある場合はその対象の照会結果を要求する', () => {
  const routed = {
    ...detail(),
    closeReason: {
      reason: 'voluntary',
      source: 'token',
      route: { repo: 'owner/repo', pr: 42 },
    },
  };
  assert.equal(classifyWatchOutcome(target, routed), 'unconfirmed');
  assert.equal(classifyWatchOutcome(target, routed, verified), 'merged');
  assert.equal(
    classifyWatchOutcome(target, routed, { ...verified, merged: false }),
    'withdrawn',
  );
  assert.equal(
    classifyWatchOutcome(
      target,
      {
        ...routed,
        closeReason: { ...routed.closeReason, route: { repo: 'owner/repo' } },
      },
      verified,
    ),
    'unconfirmed',
  );
  assert.equal(
    classifyWatchOutcome(
      target,
      {
        ...routed,
        closeReason: {
          ...routed.closeReason,
          route: { repo: 'owner/repo', pr: 43 },
        },
      },
      verified,
    ),
    'unconfirmed',
  );
});
