/* global require, process, __dirname, console, module */
/* eslint-disable @typescript-eslint/no-require-imports */
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

// Each reviewed pair must retain the preparation verifier and the publish gate.
const ENTRIES = Object.freeze({
  'public-source': Object.freeze({
    prepare: 'scripts/publish-public-source.sh',
    publish: 'scripts/publish-public-source.sh',
  }),
});
function select(job) {
  if (
    !job ||
    typeof job !== 'object' ||
    Array.isArray(job) ||
    !['entry,registry', 'entry,policy,registry'].includes(
      Object.keys(job).sort().join(','),
    ) ||
    (Object.hasOwn(job, 'policy') &&
      (typeof job.policy !== 'string' || !path.isAbsolute(job.policy))) ||
    typeof job.entry !== 'string' ||
    !Object.hasOwn(ENTRIES, job.entry) ||
    typeof job.registry !== 'string' ||
    !path.isAbsolute(job.registry)
  )
    throw Error('invalid job');
  return ENTRIES[job.entry];
}
function run(command, args) {
  const result = spawnSync(command, args, {
    stdio: 'ignore',
    env: {
      PATH: process.env.PATH || '/usr/bin:/bin',
      HOME: '/nonexistent',
      GIT_CONFIG_NOSYSTEM: '1',
      GIT_CONFIG_GLOBAL: '/dev/null',
      GIT_TERMINAL_PROMPT: '0',
    },
  });
  return !result.error && result.status === 0;
}
function check(target, job, root) {
  const pair = select(job);
  for (const script of [pair.prepare, pair.publish])
    fs.accessSync(path.join(root, script), fs.constants.R_OK);
  const registry = JSON.parse(fs.readFileSync(job.registry, 'utf8'));
  if (!registry || !Object.hasOwn(registry, target))
    throw Error('missing target');
  const entry = registry[target],
    source = entry && entry.source;
  if (
    !source ||
    typeof source !== 'object' ||
    !path.isAbsolute(source.repo || '') ||
    !path.isAbsolute(entry.publicRepo || '') ||
    typeof entry.ref !== 'string' ||
    !entry.ref ||
    typeof entry.publicRef !== 'string' ||
    !entry.publicRef ||
    !Array.isArray(source.paths) ||
    !source.paths.length ||
    !Array.isArray(source.replacements) ||
    !Array.isArray(source.reject) ||
    !['intake', 'worker'].includes(source.ci) ||
    !['intake', 'worker'].includes(source.verify) ||
    typeof source.image !== 'string' ||
    !/^(?:127\.0\.0\.1:5000\/)?[a-z0-9][a-z0-9._/-]*@sha256:[a-f0-9]{64}$/.test(
      source.image,
    )
  )
    throw Error('invalid registration');
  for (const [repo, ref] of [
    [source.repo, entry.ref],
    [entry.publicRepo, entry.publicRef],
  ]) {
    if (
      !run('git', [
        '-C',
        repo,
        'rev-parse',
        '--verify',
        '--end-of-options',
        ref + '^{commit}',
      ])
    )
      throw Error('missing base');
  }
  if (!run('docker', ['image', 'inspect', source.image]))
    throw Error('missing image');
  if (!run('mv', ['--version'])) throw Error('missing mv');
  return pair;
}
function safePrevious(dir, target) {
  if (!dir) return [];
  const record = path.join(dir, target + '.merged.json');
  const tree = path.join(dir, target + '.tree');
  function safe(file, directory) {
    const st = fs.lstatSync(file);
    if (
      st.isSymbolicLink() ||
      st.uid !== process.getuid() ||
      st.mode & 0o077 ||
      (directory ? !st.isDirectory() : !st.isFile())
    )
      throw Error('unsafe previous');
  }
  try {
    safe(dir, true);
    safe(record, false);
    safe(tree, true);
    return ['--previous', record, '--previous-tree', tree];
  } catch {
    return [];
  }
}
function prepare(target, job, root, output, previous) {
  const pair = check(target, job, root);
  const entry = JSON.parse(fs.readFileSync(job.registry, 'utf8'))[target];
  const base = spawnSync(
    'git',
    [
      '-C',
      entry.publicRepo,
      'rev-parse',
      '--verify',
      '--end-of-options',
      entry.publicRef + '^{commit}',
    ],
    {
      encoding: 'utf8',
      env: {
        PATH: process.env.PATH || '/usr/bin:/bin',
        HOME: '/nonexistent',
        GIT_CONFIG_NOSYSTEM: '1',
        GIT_CONFIG_GLOBAL: '/dev/null',
        GIT_TERMINAL_PROMPT: '0',
      },
    },
  );
  const sha = (base.stdout || '').trim();
  if (base.status !== 0 || !/^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(sha))
    throw Error('base');
  const result = spawnSync(
    'sh',
    [
      path.join(root, pair.prepare),
      '--target',
      target,
      '--registry',
      job.registry,
      '--output',
      path.join(output, target + '.tree'),
      '--public-base',
      sha,
      ...safePrevious(previous, target),
    ],
    {
      encoding: 'utf8',
      env: {
        PATH: process.env.PATH || '/usr/bin:/bin',
        HOME: '/nonexistent',
        GIT_CONFIG_NOSYSTEM: '1',
        GIT_CONFIG_GLOBAL: '/dev/null',
        GIT_TERMINAL_PROMPT: '0',
      },
      maxBuffer: 1024 * 1024,
    },
  );
  if (result.error || result.status !== 0) throw Error('prepare failed');
  const evidence = JSON.parse(result.stdout);
  if (
    evidence.target !== target ||
    !['prepared', 'no-diff', 'skipped'].includes(evidence.status)
  )
    throw Error('invalid result');
  const tree = path.join(output, target + '.tree');
  const record = path.join(output, target + '.json');
  let fd;
  let created = false;
  try {
    if (evidence.status !== 'skipped') {
      fs.chmodSync(tree, 0o700);
      if ((fs.lstatSync(tree).mode & 0o7777) !== 0o700)
        throw Error('tree mode');
    }
    fd = fs.openSync(record, 'wx', 0o600);
    created = true;
    fs.fchmodSync(fd, 0o600);
    if ((fs.fstatSync(fd).mode & 0o7777) !== 0o600) throw Error('record mode');
    fs.writeFileSync(fd, JSON.stringify(evidence) + '\n');
  } catch (error) {
    if (created) fs.unlinkSync(record);
    if (evidence.status !== 'skipped')
      fs.rmSync(tree, { recursive: true, force: true });
    throw error;
  } finally {
    if (fd !== undefined) fs.closeSync(fd);
  }
  return evidence.status;
}
function main(args) {
  if (args[0] === '--send')
    return require('./run-public-source-jobs-send.cjs').send(
      args,
      select,
      path.resolve(__dirname, '..'),
    );
  const dry =
    args.length === 3 &&
    args[0] === '--dry-run' &&
    args[1] === '--registry' &&
    path.isAbsolute(args[2]);
  const preparing =
    (args.length === 5 || args.length === 7) &&
    args[0] === '--prepare' &&
    args[1] === '--registry' &&
    path.isAbsolute(args[2]) &&
    args[3] === '--output' &&
    path.isAbsolute(args[4]) &&
    (args.length === 5 ||
      (args[5] === '--previous-dir' && path.isAbsolute(args[6])));
  if (!dry && !preparing) {
    console.error(
      '使用法: --dry-run --registry /absolute/jobs.json または --prepare --registry /absolute/jobs.json --output /private/new-directory [--previous-dir /private/previous]',
    );
    return 1;
  }
  if (preparing) {
    try {
      const parent = fs.lstatSync(path.dirname(args[4]));
      if (
        !parent.isDirectory() ||
        parent.isSymbolicLink() ||
        parent.uid !== process.getuid() ||
        parent.mode & 0o077
      )
        throw Error('unsafe parent');
      fs.mkdirSync(args[4], { mode: 0o700 });
      try {
        fs.chmodSync(args[4], 0o700);
        if ((fs.lstatSync(args[4]).mode & 0o7777) !== 0o700)
          throw Error('output mode');
      } catch (error) {
        fs.rmdirSync(args[4]);
        throw error;
      }
    } catch {
      console.error('出力先を作成できません');
      return 1;
    }
  }
  let jobs;
  try {
    jobs = JSON.parse(fs.readFileSync(args[2], 'utf8'));
    if (
      !jobs ||
      typeof jobs !== 'object' ||
      Array.isArray(jobs) ||
      !Object.keys(jobs).length
    )
      throw Error();
  } catch {
    console.error('ジョブ登録を読み込めません');
    return 1;
  }
  let failed = false;
  for (const target of Object.keys(jobs)) {
    // Never echo untrusted registration values, including malformed target names.
    if (!/^[a-z0-9][a-z0-9-]{0,63}$/.test(target)) {
      console.log('不正な対象: 前提不足');
      failed = true;
      continue;
    }
    try {
      const root = path.resolve(__dirname, '..');
      if (dry) {
        check(target, jobs[target], root);
        console.log(target + ': 前提確認済み');
      } else {
        try {
          check(target, jobs[target], root);
        } catch {
          console.log(target + ': 前提不足');
          failed = true;
          continue;
        }
        try {
          console.log(
            target +
              ': ' +
              prepare(target, jobs[target], root, args[4], args[6]),
          );
        } catch {
          console.log(target + ': 失敗');
          failed = true;
        }
      }
    } catch {
      console.log(target + ': 前提不足');
      failed = true;
    }
  }
  return failed ? 1 : 0;
}
if (require.main === module) process.exitCode = main(process.argv.slice(2));
