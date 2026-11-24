/* global module */
'use strict';

// All inputs must come from trusted registration, the prepared candidate, or
// complete authenticated reads. A caller must not fill missing reads from a PR.
// protection.checks and results are complete, not paginated fragments. Each
// check has {name, appId}; each result has {name, appId, sha, status, conclusion}.
// pr has {number, repository, baseRef, baseSha, headSha, state}; target has
// {sha, parents} obtained from the commit API (parents in Git order).
function evaluatePublicSourceCI(input) {
  const { expected, pr, target, protection, results } = input || {};
  const reject = (reason) => ({ allowed: false, reason });
  const oid = (s) =>
    typeof s === 'string' && /^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(s);
  const name = (s) => typeof s === 'string' && s.trim() === s && s.length > 0;
  const app = (n) => Number.isSafeInteger(n) && n > 0;
  if (
    !expected ||
    !pr ||
    !name(expected.repository) ||
    !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(expected.repository) ||
    !name(expected.baseRef) ||
    !expected.baseRef.startsWith('refs/heads/') ||
    expected.baseRef.length <= 11 ||
    !oid(expected.baseSha) ||
    !oid(expected.headSha) ||
    !Number.isSafeInteger(expected.pr) ||
    expected.pr <= 0 ||
    pr.repository !== expected.repository ||
    pr.baseRef !== expected.baseRef ||
    pr.baseSha !== expected.baseSha ||
    pr.headSha !== expected.headSha ||
    pr.number !== expected.pr ||
    pr.state !== 'open'
  )
    return reject('PR identity mismatch or unavailable');
  if (!target || !oid(target.sha) || !Array.isArray(target.parents))
    return reject('CI target unavailable');
  if (
    target.sha !== expected.headSha &&
    !(
      target.sha !== expected.baseSha &&
      target.parents.length === 2 &&
      target.parents[0] === expected.baseSha &&
      target.parents[1] === expected.headSha
    )
  )
    return reject('CI target does not match candidate');
  // Only strict branch protection is accepted; an unverified alternative
  // concurrency mechanism is not evidence that the base cannot change.
  if (
    !protection ||
    protection.strict !== true ||
    !Array.isArray(protection.checks) ||
    protection.checks.length === 0
  )
    return reject('required protection unavailable or not strict');
  if (!Array.isArray(results)) return reject('check results unavailable');
  const required = new Set();
  let waiting = false;
  for (const check of protection.checks) {
    if (
      !check ||
      !name(check.name) ||
      !app(check.appId) ||
      required.has(check.name)
    )
      return reject('ambiguous required checks');
    required.add(check.name);
    const matches = results.filter(
      (result) => result && result.name === check.name,
    );
    if (matches.length !== 1)
      return reject('missing or ambiguous check result');
    const result = matches[0];
    if (result.appId !== check.appId || result.sha !== target.sha)
      return reject('required check did not succeed on target');
    if (
      ['queued', 'in_progress', 'waiting', 'pending', 'requested'].includes(
        result.status,
      ) &&
      result.conclusion === null
    )
      waiting = true;
    else if (result.status !== 'completed' || result.conclusion !== 'success')
      return reject('required check did not succeed on target');
  }
  if (waiting) return { allowed: false, waiting: true };
  return { allowed: true };
}

module.exports = { evaluatePublicSourceCI };
