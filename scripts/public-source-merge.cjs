/* global require, module */
/* eslint-disable @typescript-eslint/no-require-imports */
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const { evaluatePublicSourceCI } = require('./public-source-ci.cjs');

// Called under the send lock, after the candidate and credentials are checked.
function mergePublicSource({
  send,
  prepared,
  target,
  branch,
  number,
  api,
  readRef,
  fail,
}) {
  const repo = `repos/${send.repository}`;
  const baseName = send.base.slice('refs/heads/'.length);
  const get = (endpoint) => JSON.parse(api('GET', endpoint));
  const readPR = () => {
    const pr = get(`${repo}/pulls/${number}`);
    if (
      pr.number !== number ||
      pr.head?.sha !== prepared.commit ||
      pr.head?.ref !== branch ||
      pr.head?.repo?.full_name?.toLowerCase() !==
        send.repository.toLowerCase() ||
      pr.base?.ref !== baseName ||
      pr.base?.repo?.full_name?.toLowerCase() !== send.repository.toLowerCase()
    )
      fail('PRが不一致です');
    return pr;
  };
  const verifyMerged = (pr) => {
    if (
      pr.state !== 'closed' ||
      pr.merged !== true ||
      !/^[a-f0-9]{40}$/.test(pr.merge_commit_sha || '')
    )
      fail('マージ結果が未確定です');
    const commit = get(`${repo}/git/commits/${pr.merge_commit_sha}`);
    if (
      commit.sha !== pr.merge_commit_sha ||
      commit.tree?.sha !== prepared.generatedTree ||
      !Array.isArray(commit.parents) ||
      commit.parents.length !== 2 ||
      commit.parents[0].sha !== prepared.publicBase ||
      commit.parents[1].sha !== prepared.commit ||
      readRef(send.base) !== pr.merge_commit_sha
    )
      fail('公開先のマージ結果が不一致です');
    const record = {
      ...prepared,
      status: 'merged',
      repository: send.repository,
      mergeCommit: pr.merge_commit_sha,
    };
    const final = path.join(send.stateDir, `${target}.merged.json`);
    const temporary = path.join(
      send.stateDir,
      `.${target}.${crypto.randomBytes(16).toString('hex')}.tmp`,
    );
    let fd;
    try {
      fd = fs.openSync(temporary, 'wx', 0o600);
      fs.writeFileSync(fd, JSON.stringify(record) + '\n');
      fs.fsyncSync(fd);
      fs.closeSync(fd);
      fd = undefined;
      fs.renameSync(temporary, final);
      const dir = fs.openSync(send.stateDir, 'r');
      try {
        fs.fsyncSync(dir);
      } finally {
        fs.closeSync(dir);
      }
    } finally {
      if (fd !== undefined) fs.closeSync(fd);
      if (fs.existsSync(temporary)) fs.unlinkSync(temporary);
    }
    return {
      status: 'merged',
      target,
      repository: send.repository,
      base: prepared.publicBase,
      head: prepared.commit,
      tree: prepared.generatedTree,
      branch,
      pr: number,
    };
  };
  let pr = readPR();
  if (pr.merged === true) return verifyMerged(pr);
  if (
    send.mergeMethod !== 'merge' ||
    readRef(send.base) !== prepared.publicBase
  )
    fail('マージ設定または基点が不正です');
  const rules = get(
    `${repo}/rules/branches/${encodeURIComponent(baseName)}?per_page=100`,
  );
  if (!Array.isArray(rules) || rules.length >= 100)
    fail('保護規則を照合できません');
  const protectionResponse = api(
    'GET',
    `${repo}/branches/${encodeURIComponent(baseName)}/protection/required_status_checks`,
    [],
    true,
  );
  const protection =
    protectionResponse === null ? null : JSON.parse(protectionResponse);
  if (
    protectionResponse !== null &&
    (!protection || typeof protection !== 'object')
  )
    fail('保護情報を照合できません');
  const required = [];
  if (
    protection &&
    (protection.strict !== true ||
      !Array.isArray(protection.checks) ||
      !Array.isArray(protection.contexts) ||
      protection.contexts.length !== protection.checks.length ||
      protection.contexts.some(
        (name) => !protection.checks.some((c) => c.context === name),
      ))
  )
    fail('厳格な必須チェックが必要です');
  for (const c of protection?.checks || [])
    required.push({ name: c.context, appId: c.app_id });
  for (const rule of rules) {
    if (rule.type !== 'required_status_checks') continue;
    if (
      rule.parameters?.strict_required_status_checks_policy !== true ||
      !Array.isArray(rule.parameters.required_status_checks)
    )
      fail('厳格な規則が必要です');
    for (const c of rule.parameters.required_status_checks)
      required.push({ name: c.context, appId: c.integration_id });
  }
  const unique = new Map();
  for (const c of required) {
    if (unique.has(c.name) && unique.get(c.name) !== c.appId)
      fail('必須チェックが曖昧です');
    unique.set(c.name, c.appId);
  }
  const checks = [...unique].map(([name, appId]) => ({ name, appId }));
  const commit = get(`${repo}/git/commits/${prepared.commit}`);
  if (
    commit.sha !== prepared.commit ||
    commit.tree?.sha !== prepared.generatedTree ||
    !Array.isArray(commit.parents) ||
    commit.parents.length !== 1 ||
    commit.parents[0].sha !== prepared.publicBase
  )
    fail('候補コミットを照合できません');
  const runs = get(
    `${repo}/commits/${prepared.commit}/check-runs?per_page=100`,
  );
  if (
    !Array.isArray(runs.check_runs) ||
    runs.check_runs.length >= 100 ||
    runs.total_count !== runs.check_runs.length
  )
    fail('チェック結果を全件取得できません');
  const results = runs.check_runs.map((c) => ({
    name: c.name,
    appId: c.app?.id,
    sha: c.head_sha,
    status: c.status,
    conclusion: c.conclusion,
  }));
  const statuses = get(
    `${repo}/commits/${prepared.commit}/status?per_page=100`,
  );
  if (
    !Array.isArray(statuses.statuses) ||
    statuses.statuses.length >= 100 ||
    statuses.total_count !== statuses.statuses.length ||
    statuses.sha !== prepared.commit ||
    statuses.statuses.some((s) => unique.has(s.context))
  )
    fail('状態チェックを照合できません');
  const expected = {
    repository: send.repository,
    baseRef: send.base,
    baseSha: prepared.publicBase,
    headSha: prepared.commit,
    pr: number,
  };
  const check = () => {
    pr = readPR();
    if (readRef(send.base) !== prepared.publicBase)
      fail('必須CIまたはPRを確認できません');
    const decision = evaluatePublicSourceCI({
      expected,
      pr: {
        number: pr.number,
        repository: pr.base.repo.full_name,
        baseRef: send.base,
        baseSha: pr.base.sha,
        headSha: pr.head.sha,
        state: pr.state,
      },
      target: { sha: commit.sha, parents: commit.parents?.map((p) => p.sha) },
      protection: { strict: true, checks },
      results,
    });
    if (decision.waiting)
      return {
        status: 'waiting',
        target,
        repository: send.repository,
        base: prepared.publicBase,
        head: prepared.commit,
        tree: prepared.generatedTree,
        branch,
        pr: number,
      };
    if (!decision.allowed) fail('必須CIまたはPRを確認できません');
  };
  const waiting = check();
  if (waiting) return waiting;
  api('PUT', `${repo}/pulls/${number}/merge`, [
    '-f',
    `sha=${prepared.commit}`,
    '-f',
    'merge_method=merge',
  ]);
  return verifyMerged(readPR());
}
module.exports = { mergePublicSource };
