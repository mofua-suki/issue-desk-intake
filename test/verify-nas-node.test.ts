import { test } from 'node:test';
import assert from 'node:assert/strict';
import { verifyNASNode } from '../scripts/verify-nas-node.ts';

const name =
  'TestNASBoundedHealthVerifierPreservesPipeAndStrictMeaning/sh/node';
test('NAS Node verification requires a real passing subtest', () => {
  const run = (events, status = 0) =>
    verifyNASNode({
      execute: () => ({
        status,
        stdout: events.map((e) => JSON.stringify(e)).join('\n'),
      }),
      output: () => {},
    });
  assert.equal(run([{ Test: name, Action: 'pass' }]), 0);
  assert.equal(run([{ Test: name, Action: 'skip' }]), 1);
  assert.equal(run([]), 1);
  assert.equal(run([{ Test: name, Action: 'pass' }], 1), 1);
  assert.equal(run([{ Test: name, Action: 'pass' }, { Action: 'fail' }]), 1);
});

test('NAS Node verification rejects a minimal execution error', () => {
  assert.equal(
    verifyNASNode({
      execute: () => ({ error: new Error('x') }),
      output: () => {},
    }),
    1,
  );
});

test('NAS Node verification rejects an execution error even with a passing subtest', () => {
  const stdout = JSON.stringify({ Test: name, Action: 'pass' });
  assert.equal(
    verifyNASNode({
      execute: () => ({ status: 0, stdout, error: new Error('x') }),
      output: () => {},
    }),
    1,
  );
});

test('NAS Node verification rejects invalid JSON even with a passing subtest', () => {
  const stdout = `${JSON.stringify({ Test: name, Action: 'pass' })}\nnot json`;
  assert.equal(
    verifyNASNode({ execute: () => ({ status: 0, stdout }), output: () => {} }),
    1,
  );
});
