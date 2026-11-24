import { test } from 'node:test';
import assert from 'node:assert/strict';
import { canPublish } from '../scripts/public-source-credentials.cjs';
const now = Date.parse('2030-01-01T00:00:00Z');
const inputs = () => ({
  registration: {
    repository: 'example/project',
    credentialRef: 'issuer:example-1',
  },
  candidate: {
    repository: 'example/project',
    credentialRef: 'issuer:example-1',
    changesWorkflow: false,
  },
  credential: {
    repository: 'example/project',
    credentialRef: 'issuer:example-1',
    permissions: {
      contents: 'write',
      pull_requests: 'write',
      workflows: 'write',
    },
    expiresAt: '2030-01-02T00:00:00Z',
  },
  now,
});

test('信頼済み登録と発行結果が一致し必要な権限があれば許可する', () => {
  const input = inputs();
  assert.equal(canPublish(input), true);
  input.candidate.changesWorkflow = true;
  assert.equal(canPublish(input), true);
  input.credential.permissions.workflows = 'read';
  assert.equal(canPublish(input), false);
  input.candidate.changesWorkflow = false;
  assert.equal(canPublish(input), true);
  assert.equal(
    canPublish({
      ...input,
      credential: { ...input.credential, expiresAt: undefined },
    }),
    true,
  );
});

test('登録・対象・参照・権限・期限が不一致または欠測なら拒否する', () => {
  const input = inputs();
  const reject = (patch: object) =>
    assert.equal(canPublish({ ...input, ...patch }), false);
  reject({ registration: undefined });
  for (const repository of [
    'other/project',
    'example/other',
    'project',
    '',
    'example/..',
  ]) {
    reject({ candidate: { ...input.candidate, repository } });
    reject({ credential: { ...input.credential, repository } });
  }
  reject({ registration: { ...input.registration, repository: 'project' } });
  reject({ candidate: { ...input.candidate, credentialRef: 'issuer:other' } });
  reject({
    credential: { ...input.credential, credentialRef: 'issuer:other' },
  });
  reject({ registration: { ...input.registration, credentialRef: 'bad ref' } });
  reject({ candidate: { ...input.candidate, changesWorkflow: undefined } });
  reject({ credential: { ...input.credential, repository: undefined } });
  reject({ credential: { ...input.credential, credentialRef: undefined } });
  reject({ credential: { ...input.credential, permissions: undefined } });
  for (const permission of ['contents', 'pull_requests']) {
    for (const value of [undefined, 'read']) {
      reject({
        credential: {
          ...input.credential,
          permissions: { ...input.credential.permissions, [permission]: value },
        },
      });
    }
  }
  for (const expiresAt of [
    '2029-12-31T23:59:59Z',
    '2030-01-01T00:00:00Z',
    '',
    'invalid',
    '2030-02-30T00:00:00Z',
  ]) {
    reject({ credential: { ...input.credential, expiresAt } });
  }
});

test('自己申告で発行結果の欠測を補わない', () => {
  const input = inputs();
  const result = canPublish({
    ...input,
    candidate: {
      ...input.candidate,
      permissions: input.credential.permissions,
    },
    credential: { ...input.credential, permissions: undefined },
  });
  assert.equal(result, false);
});
