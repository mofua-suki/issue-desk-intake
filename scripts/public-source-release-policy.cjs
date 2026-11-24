/* global require, module */
/* eslint-disable @typescript-eslint/no-require-imports */
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { TextDecoder } = require('node:util');
const { createHash } = require('node:crypto');

const oid = (value) =>
  typeof value === 'string' && /^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(value);
const initialKeys = ['publicBase', 'inputSha', 'generatedTree'];

// The policy is an operator-owned allowlist, never a candidate-provided claim.
function checkPolicy(file, target, entry, evidence, dir, safe, env) {
  safe(file, false);
  const p = JSON.parse(fs.readFileSync(file, 'utf8'));
  const keys = [
    'target',
    'repository',
    'base',
    'publicRepo',
    'publicRef',
    'sourceRepo',
    'sourceRef',
    'preparedRoot',
    'configSha256',
    'generatorSha256',
    'ciSha256',
    'ci',
    'verify',
    'paths',
  ];
  const hasInitial = p && Object.hasOwn(p, 'initialCandidate');
  if (hasInitial) keys.push('initialCandidate');
  if (
    !p ||
    Array.isArray(p) ||
    Object.keys(p).length !== keys.length ||
    !keys.every((key) => Object.hasOwn(p, key))
  )
    throw Error('policy');
  const initial = p.initialCandidate;
  if (
    hasInitial &&
    (!initial ||
      typeof initial !== 'object' ||
      Array.isArray(initial) ||
      Object.keys(initial).length !== initialKeys.length ||
      !initialKeys.every(
        (key) => Object.hasOwn(initial, key) && oid(initial[key]),
      ))
  )
    throw Error('initial candidate');
  const config =
    entry &&
    entry.source &&
    createHash('sha256')
      .update(
        JSON.stringify({
          target,
          ...entry,
          source: { ...entry.source, sha: undefined },
        }),
      )
      .digest('hex');
  if (
    !entry ||
    !entry.source ||
    !entry.send ||
    p.target !== target ||
    p.repository !== entry.send.repository ||
    p.base !== 'refs/heads/main' ||
    p.base !== entry.send.base ||
    p.publicRef !== p.base ||
    p.publicRef !== entry.publicRef ||
    p.publicRepo !== entry.publicRepo ||
    p.sourceRepo !== entry.source.repo ||
    p.sourceRef !== entry.ref ||
    !path.isAbsolute(p.preparedRoot || '') ||
    path.dirname(dir) !== p.preparedRoot ||
    p.ci !== entry.source.ci ||
    p.verify !== entry.source.verify ||
    config !== evidence.configSha256 ||
    !['intake', 'worker'].includes(p.ci) ||
    !['intake', 'worker'].includes(p.verify) ||
    !['configSha256', 'generatorSha256', 'ciSha256'].every(
      (k) => /^[a-f0-9]{64}$/.test(p[k]) && p[k] === evidence[k],
    ) ||
    !Array.isArray(p.paths) ||
    !p.paths.length ||
    new Set(p.paths).size !== p.paths.length
  )
    throw Error('policy mismatch');
  const allowed = new Set(p.paths);
  for (const name of allowed) {
    if (
      typeof name !== 'string' ||
      !/^[A-Za-z0-9_./-]+$/.test(name) ||
      name.startsWith('/') ||
      name.split('/').some((x) => !x || x === '.' || x === '..') ||
      /(^|\/)(?:\.github|\.gitlab|\.circleci|\.git|\.env(?:\.|$)|workflows?|credentials?|secrets?|approval|approvals)(?:\/|$|[._-])/i.test(
        name,
      ) ||
      /(?:^|\/)(?:AGENTS\.md|[^/]*(?:approval|credential|secret|policy)[^/]*)$/i.test(
        name,
      ) ||
      !/\.(?:md|txt|go|js|cjs|mjs|ts|tsx|jsx|css|html|json|yaml|yml|sh|sql|dart)$/i.test(
        name,
      )
    )
      throw Error('path');
  }
  const git = (repo, ...args) => {
    const r = spawnSync('git', ['-C', repo, ...args], {
      env,
      encoding: 'buffer',
      maxBuffer: 16 * 1024 * 1024,
      stdio: ['ignore', 'pipe', 'ignore'],
    });
    if (r.error || r.status !== 0) throw Error('git');
    return r.stdout;
  };
  const resolve = (repo, ref) =>
    git(repo, 'rev-parse', '--verify', '--end-of-options', ref + '^{commit}')
      .toString('utf8')
      .trim();
  if (
    !oid(evidence.commit) ||
    !oid(evidence.publicBase) ||
    resolve(entry.publicRepo, entry.publicRef) !== evidence.publicBase ||
    resolve(entry.source.repo, entry.ref) !== evidence.inputSha
  )
    throw Error('base');
  if (initial && initialKeys.every((key) => initial[key] === evidence[key])) {
    const commit = git(
      entry.publicRepo,
      '--no-replace-objects',
      'log',
      '-1',
      '--no-show-signature',
      '--format=%H %T %P',
      '--end-of-options',
      evidence.commit,
      '--',
    )
      .toString('utf8')
      .trim();
    if (
      commit !==
      `${evidence.commit} ${initial.generatedTree} ${initial.publicBase}`
    )
      throw Error('initial commit');
    return;
  }
  const changes = git(
    entry.publicRepo,
    'diff-tree',
    '--no-commit-id',
    '-r',
    '--raw',
    '-z',
    '--no-renames',
    evidence.publicBase,
    evidence.commit,
  )
    .toString('utf8')
    .split('\0');
  if (changes.pop() !== '' || !changes.length || changes.length % 2)
    throw Error('diff');
  const decoder = new TextDecoder('utf-8', { fatal: true });
  for (let i = 0; i < changes.length; i += 2) {
    const match = /^:100644 100644 ([a-f0-9]+) ([a-f0-9]+) M$/.exec(changes[i]);
    const name = changes[i + 1];
    if (!match || !allowed.has(name) || !entry.source.paths.includes(name))
      throw Error('change');
    for (const oid of [match[1], match[2]]) {
      const bytes = git(entry.publicRepo, 'cat-file', 'blob', oid);
      if (bytes.includes(0)) throw Error('binary');
      decoder.decode(bytes);
    }
  }
}
module.exports = { checkPolicy };
