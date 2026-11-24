import { test } from 'node:test';
import assert from 'node:assert/strict';
import { evaluatePublicSourceCI } from '../scripts/public-source-ci.cjs';

const base = 'a'.repeat(40),
  head = 'b'.repeat(40),
  merge = 'c'.repeat(40),
  other = 'd'.repeat(40);
function input() {
  return {
    expected: {
      repository: 'owner/repo',
      baseRef: 'refs/heads/main',
      baseSha: base,
      headSha: head,
      pr: 42,
    },
    pr: {
      repository: 'owner/repo',
      baseRef: 'refs/heads/main',
      baseSha: base,
      headSha: head,
      number: 42,
      state: 'open',
    },
    target: { sha: merge, parents: [base, head] },
    protection: {
      strict: true,
      checks: [
        { name: 'verify', appId: 15368 },
        { name: 'lint', appId: 42 },
      ],
    },
    results: [
      {
        name: 'verify',
        appId: 15368,
        sha: merge,
        status: 'completed',
        conclusion: 'success',
      },
      {
        name: 'lint',
        appId: 42,
        sha: merge,
        status: 'completed',
        conclusion: 'success',
      },
    ],
  };
}

test('CI対象と現在のPRの識別を照合する', () => {
  const valid = input();
  assert.equal(evaluatePublicSourceCI(valid).allowed, true);
  const direct = input();
  direct.target = { sha: head, parents: [] };
  direct.results.forEach((result) => {
    result.sha = head;
  });
  assert.equal(evaluatePublicSourceCI(direct).allowed, true);
  const changes: ((v: ReturnType<typeof input>) => void)[] = [
    (v) => {
      v.pr.repository = 'owner/other';
    },
    (v) => {
      v.pr.baseRef = 'refs/heads/other';
    },
    (v) => {
      v.pr.baseSha = other;
    },
    (v) => {
      v.pr.headSha = other;
    },
    (v) => {
      v.pr.number++;
    },
    (v) => {
      v.pr.state = 'closed';
    },
    (v) => {
      v.target.parents = [other, head];
    },
    (v) => {
      v.target.parents = [base, other];
    },
    (v) => {
      v.target.parents = [head, base];
    },
    (v) => {
      v.target.sha = other;
    },
    (v) => {
      v.results[0].sha = head;
    },
  ];
  for (const change of changes) {
    const value = input();
    change(value);
    assert.equal(evaluatePublicSourceCI(value).allowed, false);
  }
});

test('現在の必須チェックと発行元の成功だけを許可する', () => {
  const changes: ((v: ReturnType<typeof input>) => void)[] = [
    (v) => {
      v.results[0].conclusion = 'failure';
    },
    (v) => {
      v.results[0].status = 'in_progress';
    },
    (v) => {
      v.results.pop();
    },
    (v) => {
      v.results[0].appId++;
    },
    (v) => {
      v.results[0].conclusion = '';
    },
    (v) => {
      v.results.push({ ...v.results[0] });
    },
    (v) => {
      v.results.push({ ...v.results[0], appId: 99 });
    },
    (v) => {
      v.protection.checks = [];
    },
    (v) => {
      v.protection.checks[0].appId = 0;
    },
    (v) => {
      v.protection.checks.push({ ...v.protection.checks[0] });
    },
    (v) => {
      v.protection.strict = false;
    },
  ];
  for (const change of changes) {
    const value = input();
    change(value);
    assert.equal(evaluatePublicSourceCI(value).allowed, false);
  }
  assert.equal(
    evaluatePublicSourceCI({ ...input(), protection: undefined }).allowed,
    false,
  );
  assert.equal(
    evaluatePublicSourceCI({ ...input(), results: undefined }).allowed,
    false,
  );
  assert.equal(
    evaluatePublicSourceCI({ ...input(), results: [] }).allowed,
    false,
  );
});
