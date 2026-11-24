import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { classifyWatchEvidence } from '../scripts/codex-issue-watch-evidence.ts';
import {
  saveWatchState,
  resumeWatchState,
} from '../scripts/codex-issue-watch-state.ts';

const identity = {
  sendId: 'delivery-1',
  conversationId: 'conversation-1',
  targetId: 'desk:42',
  notificationId: 'notice-1',
};
const detail = {
  targetId: 'desk:42',
  project: 'org/repo',
  number: 42,
  revision: 'rev-2',
  evidenceIds: ['cause-1'],
  decisionIds: ['ask-1', 'approval-1'],
};
const base = { identity, detail };
const operation = {
  replyId: 'reply-1',
  comment: {
    id: 'reply-1',
    project: 'org/repo',
    number: 42,
    status: 'done',
    result: { text: 'private body' },
  },
};

test('current investigation, completed reply and concrete decisions are recognized', () => {
  assert.deepEqual(
    classifyWatchEvidence(identity, {
      ...base,
      investigation: { evidenceId: 'cause-2', revision: 'rev-2' },
    }),
    { kind: 'investigated', evidenceId: 'cause-2' },
  );
  assert.deepEqual(
    classifyWatchEvidence(identity, {
      ...base,
      investigation: { evidenceId: 'cause-1' },
    }),
    { kind: 'investigated', evidenceId: 'cause-1' },
  );
  assert.deepEqual(classifyWatchEvidence(identity, { ...base, operation }), {
    kind: 'operation',
    evidenceId: 'reply-1',
  });
  for (const decisionId of ['ask-1', 'approval-1'])
    assert.deepEqual(classifyWatchEvidence(identity, { ...base, decisionId }), {
      kind: 'waiting',
      decision: decisionId,
    });
});

test('unmatched, incomplete or stale evidence remains pending and can be saved', () => {
  const candidates = [
    null,
    { identity },
    ...(
      ['sendId', 'conversationId', 'targetId', 'notificationId'] as const
    ).map((key) => ({
      ...base,
      identity: { ...identity, [key]: 'other' },
      operation,
    })),
    { ...base, detail: null, operation },
    { ...base, detail: { ...detail, targetId: 'desk:43' }, operation },
    ...[
      { id: 'other' },
      { project: 'other' },
      { number: 43 },
      { status: 'queued' },
      { status: 'failed' },
      { result: null },
      { result: undefined },
    ].map((change) => ({
      ...base,
      operation: { ...operation, comment: { ...operation.comment, ...change } },
    })),
    { ...base, operation: { ...operation, comment: null } },
    { ...base, investigation: { evidenceId: 'other', revision: 'rev-1' } },
    { ...base, decisionId: 'waiting' },
    { ...base, decisionId: '利用者が確認' },
  ];
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'watch-evidence-'));
  try {
    const file = path.join(dir, 'state.json');
    for (const candidate of candidates) {
      const response = classifyWatchEvidence(identity, candidate);
      assert.deepEqual(response, { kind: 'pending' });
      saveWatchState(file, {
        ...identity,
        notification: 'delivered',
        response,
      });
      assert.deepEqual(resumeWatchState(file, identity)?.response, response);
    }
    for (const response of [
      classifyWatchEvidence(identity, { ...base, operation }),
      classifyWatchEvidence(identity, {
        ...base,
        investigation: { evidenceId: 'cause-1' },
      }),
      classifyWatchEvidence(identity, { ...base, decisionId: 'ask-1' }),
    ]) {
      saveWatchState(file, {
        ...identity,
        notification: 'delivered',
        response,
      });
      assert.deepEqual(resumeWatchState(file, identity)?.response, response);
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
