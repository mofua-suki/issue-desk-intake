/* global require, module, process, __dirname, console */
/* eslint-disable @typescript-eslint/no-require-imports */
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { createHash } = require('node:crypto');
const { Buffer } = require('node:buffer');
const { performance } = require('node:perf_hooks');
const os = require('node:os');
const { checkPolicy } = require('./public-source-release-policy.cjs');

function snapshot(file, tree) {
  const hash = createHash('sha256');
  hash.update(fs.readFileSync(file));
  function visit(dir, prefix) {
    for (const name of fs.readdirSync(dir).sort()) {
      const rel = prefix ? prefix + '/' + name : name;
      const full = path.join(dir, name),
        st = fs.lstatSync(full);
      if (!st.isFile() && !st.isDirectory()) throw Error('tree');
      const data = st.isFile() ? fs.readFileSync(full) : Buffer.alloc(0);
      hash.update(
        JSON.stringify([rel, st.mode, st.isDirectory(), data.length]),
      );
      hash.update(data);
      if (st.isDirectory()) visit(full, rel);
    }
  }
  visit(tree, '');
  return hash.digest('hex');
}

function validateSkip(evidence, tree) {
  const sha = (value, lengths) =>
    typeof value === 'string' &&
    lengths.some((n) => new RegExp(`^[a-f0-9]{${n}}$`).test(value));
  if (
    !sha(evidence.inputSha, [40, 64]) ||
    !['configSha256', 'generatorSha256', 'ciSha256', 'treeSha256'].every((k) =>
      sha(evidence[k], [64]),
    ) ||
    Object.hasOwn(evidence, 'commit') ||
    Object.hasOwn(evidence, 'publicBase')
  )
    throw Error('evidence');
  if (evidence.status === 'skipped') return;
  if (
    !sha(evidence.publicTree, [40, 64]) ||
    evidence.generatedTree !== evidence.publicTree
  )
    throw Error('trees');
  safe(tree, true);
  const hash = createHash('sha256');
  function visit(dir, prefix) {
    for (const name of fs.readdirSync(dir).sort()) {
      const rel = prefix ? prefix + '/' + name : name;
      const full = path.join(dir, name),
        st = fs.lstatSync(full);
      if (!st.isFile() && !st.isDirectory()) throw Error('tree');
      const data = st.isFile() ? fs.readFileSync(full) : Buffer.alloc(0);
      hash.update(
        JSON.stringify([
          rel,
          st.mode & 0o777,
          st.isDirectory() ? 'd' : 'f',
          data.length,
        ]),
      );
      hash.update('\0');
      hash.update(data);
      if (st.isDirectory()) visit(full, rel);
    }
  }
  visit(tree, '');
  if (hash.digest('hex') !== evidence.treeSha256) throw Error('tree digest');
}

function safe(file, directory) {
  if (!path.isAbsolute(file) || fs.realpathSync(file) !== path.normalize(file))
    throw Error('path');
  const st = fs.lstatSync(file);
  if (
    st.isSymbolicLink() ||
    st.uid !== process.getuid() ||
    st.mode & 0o077 ||
    (directory ? !st.isDirectory() : !st.isFile() || st.nlink !== 1)
  )
    throw Error('unsafe');
}
function run(args, root) {
  const flags =
    '--registry --prepared-dir --confirmations-dir --issuer-registry'.split(
      ' ',
    );
  if (
    ![8, 10].includes(args.length) ||
    flags.some((flag, i) => args[i * 2] !== flag)
  )
    return 1;
  if (
    args.length === 10 &&
    (args[8] !== '--wait-seconds' ||
      !/^(?:[1-9][0-9]*)$/.test(args[9]) ||
      Number(args[9]) > 3600)
  )
    return 1;
  const waitSeconds = args.length === 10 ? Number(args[9]) : 0;
  const [, jobsFile, , dir, , confirmations, , issuer] = args;
  let jobs;
  try {
    for (const file of [jobsFile, issuer]) safe(file, false);
    for (const folder of [dir, confirmations]) safe(folder, true);
    jobs = JSON.parse(fs.readFileSync(jobsFile, 'utf8'));
    if (
      !jobs ||
      typeof jobs !== 'object' ||
      Array.isArray(jobs) ||
      !Object.keys(jobs).length
    )
      throw Error();
  } catch {
    return 1;
  }
  const statuses = new Map();
  const pinned = new Map();
  const env = {
    PATH: process.env.PATH || '/usr/bin:/bin',
    HOME: '/nonexistent',
    GIT_CONFIG_NOSYSTEM: '1',
    GIT_CONFIG_GLOBAL: '/dev/null',
    GIT_TERMINAL_PROMPT: '0',
    GIT_NO_LAZY_FETCH: '1',
  };
  const child = (script, argv, options) =>
    spawnSync(process.execPath, [path.join(root, 'scripts', script), ...argv], {
      env,
      maxBuffer: 1024 * 1024,
      ...options,
    });
  const localObjectsOnly = (entry) => {
    if (!entry || !entry.source || !entry.source.repo || !entry.publicRepo)
      return;
    for (const repo of [entry.source.repo, entry.publicRepo]) {
      const result = spawnSync(
        'git',
        [
          '-C',
          repo,
          'config',
          '--includes',
          '--get-regexp',
          '^(extensions\\.partialclone|remote\\..*\\.promisor)$',
        ],
        { env, stdio: 'ignore' },
      );
      if (result.error || result.status !== 1) throw Error('partial clone');
    }
  };
  const digest = (file) => {
    safe(file, false);
    return createHash('sha256').update(fs.readFileSync(file)).digest('hex');
  };
  const processTarget = (target) => {
    if (!/^[a-z0-9][a-z0-9-]{0,63}$/.test(target)) {
      console.log('invalid: failed');
      statuses.set(target, 'failed');
      return;
    }
    let status = 'failed';
    let temporary;
    try {
      const job = jobs[target];
      if (
        !job ||
        !['entry,registry', 'entry,policy,registry'].includes(
          Object.keys(job).sort().join(','),
        ) ||
        job.entry !== 'public-source'
      )
        throw Error();
      safe(job.registry, false);
      const registry = JSON.parse(fs.readFileSync(job.registry, 'utf8'));
      if (!registry || !Object.hasOwn(registry, target)) throw Error();
      const candidate = path.join(dir, target + '.json');
      let confirmation = path.join(confirmations, target);
      safe(candidate, false);
      const evidence = JSON.parse(fs.readFileSync(candidate, 'utf8'));
      if (evidence.target !== target) throw Error('target');
      if (['no-diff', 'skipped'].includes(evidence.status)) {
        if (pinned.has(target)) throw Error('candidate changed');
        validateSkip(evidence, path.join(dir, target + '.tree'));
        console.log(target + ': ' + evidence.status);
        statuses.set(target, evidence.status);
        return;
      }
      const tree = path.join(dir, target + '.tree');
      safe(tree, true);
      localObjectsOnly(registry[target]);
      const automatic = Object.hasOwn(job, 'policy');
      if (automatic)
        checkPolicy(
          job.policy,
          target,
          registry[target],
          evidence,
          dir,
          safe,
          env,
        );
      else safe(confirmation, false);
      if (
        evidence.target !== target ||
        evidence.status !== 'prepared' ||
        (!automatic &&
          !/^[a-f0-9]{64}$/.test(fs.readFileSync(confirmation, 'utf8').trim()))
      )
        throw Error();
      const before = snapshot(candidate, tree);
      const identity = JSON.stringify({
        jobs: digest(jobsFile),
        registry: digest(job.registry),
        issuer: digest(issuer),
        approval: digest(automatic ? job.policy : confirmation),
        candidate: before,
      });
      const previous = pinned.get(target);
      if (previous && previous.identity !== identity) throw Error('changed');
      const preview = spawnSync(
        'sh',
        [
          path.join(root, 'scripts/publish-public-source.sh'),
          '--send',
          '--target',
          target,
          '--registry',
          job.registry,
          '--candidate',
          candidate,
          '--tree',
          tree,
        ],
        {
          env,
          encoding: 'utf8',
          stdio: ['ignore', 'pipe', 'ignore'],
          maxBuffer: 1024 * 1024,
        },
      );
      if (preview.error || preview.status !== 0) throw Error('preview');
      const result = JSON.parse(preview.stdout);
      if (
        result.status !== 'preview' ||
        result.target !== target ||
        !/^[a-f0-9]{64}$/.test(result.confirmation || '') ||
        (!automatic &&
          result.confirmation !==
            fs.readFileSync(confirmation, 'utf8').trim()) ||
        snapshot(candidate, tree) !== before
      )
        throw Error('confirmation');
      if (previous && previous.preview !== JSON.stringify(result))
        throw Error('preview changed');
      if (automatic) {
        if (
          result.head !== evidence.commit ||
          result.base !== evidence.publicBase ||
          result.tree !== evidence.generatedTree ||
          result.repository !== registry[target].send.repository
        )
          throw Error('preview mismatch');
        checkPolicy(
          job.policy,
          target,
          JSON.parse(fs.readFileSync(job.registry, 'utf8'))[target],
          evidence,
          dir,
          safe,
          env,
        );
        temporary = fs.mkdtempSync(
          path.join(os.tmpdir(), 'public-confirmation-'),
        );
        confirmation = path.join(temporary, 'confirmation');
        fs.writeFileSync(confirmation, result.confirmation + '\n', {
          mode: 0o600,
          flag: 'wx',
        });
      }
      safe(candidate, false);
      safe(tree, true);
      if (snapshot(candidate, tree) !== before)
        throw Error('candidate changed');
      if (
        identity !==
        JSON.stringify({
          jobs: digest(jobsFile),
          registry: digest(job.registry),
          issuer: digest(issuer),
          approval: digest(
            automatic ? job.policy : path.join(confirmations, target),
          ),
          candidate: snapshot(candidate, tree),
        })
      )
        throw Error('changed');
      const issued = child(
        'issue-public-source-github-app.cjs',
        [issuer, target],
        { encoding: 'buffer', stdio: ['ignore', 'pipe', 'ignore'] },
      );
      if (issued.error || issued.status !== 0 || !issued.stdout.length)
        throw Error();
      const argv = [
        '--send',
        '--registry',
        jobsFile,
        '--target',
        target,
        '--prepared-dir',
        dir,
        '--confirmation',
        confirmation,
      ];
      const sent = child('run-public-source-jobs.cjs', argv, {
        input: issued.stdout,
        encoding: 'utf8',
        stdio: ['pipe', 'pipe', 'ignore'],
      });
      issued.stdout.fill(0);
      const output = (sent.stdout || '').trim();
      if (sent.status === 0 && ['published', 'merged'].includes(output))
        status = output;
      else if (sent.status === 2 && output === 'waiting') {
        status = 'waiting';
        if (!previous)
          pinned.set(target, { identity, preview: JSON.stringify(result) });
      }
    } catch {
      /* Only report the target and status. */
    } finally {
      if (temporary) fs.rmSync(temporary, { recursive: true, force: true });
    }
    console.log(target + ': ' + status);
    statuses.set(target, status);
  };
  for (const target of Object.keys(jobs)) processTarget(target);
  const deadline = performance.now() + waitSeconds * 1000;
  while (waitSeconds && [...statuses.values()].includes('waiting')) {
    const remaining = deadline - performance.now();
    if (remaining <= 0) break;
    Atomics.wait(
      new Int32Array(new SharedArrayBuffer(4)),
      0,
      0,
      Math.min(1000, remaining),
    );
    for (const [target, status] of statuses) {
      if (performance.now() >= deadline) break;
      if (status === 'waiting') processTarget(target);
    }
  }
  return [...statuses.values()].includes('failed')
    ? 1
    : [...statuses.values()].includes('waiting')
      ? 2
      : 0;
}
module.exports = { run };
if (require.main === module)
  process.exitCode = run(process.argv.slice(2), path.resolve(__dirname, '..'));
