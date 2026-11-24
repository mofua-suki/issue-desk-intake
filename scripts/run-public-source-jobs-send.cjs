/* global require, process, console, module */
/* eslint-disable @typescript-eslint/no-require-imports */
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

function safe(file, directory) {
  if (!path.isAbsolute(file)) throw Error('path');
  const st = fs.lstatSync(file);
  if (
    st.isSymbolicLink() ||
    st.uid !== process.getuid() ||
    st.mode & 0o077 ||
    (directory ? !st.isDirectory() : !st.isFile())
  )
    throw Error('unsafe');
  if (fs.realpathSync(file) !== path.normalize(file)) throw Error('path');
}
function send(args, select, root) {
  try {
    if (
      args.length !== 9 ||
      args[1] !== '--registry' ||
      args[3] !== '--target' ||
      args[5] !== '--prepared-dir' ||
      args[7] !== '--confirmation' ||
      !/^[a-z0-9][a-z0-9-]{0,63}$/.test(args[4])
    )
      throw Error('arguments');
    const [, , jobsFile, , target, , dir, , confirmationFile] = args;
    safe(jobsFile, false);
    safe(dir, true);
    safe(confirmationFile, false);
    const jobs = JSON.parse(fs.readFileSync(jobsFile, 'utf8'));
    if (!jobs || !Object.hasOwn(jobs, target)) throw Error('target');
    const job = jobs[target],
      pair = select(job);
    safe(job.registry, false);
    const registry = JSON.parse(fs.readFileSync(job.registry, 'utf8'));
    if (!registry || !Object.hasOwn(registry, target))
      throw Error('registration');
    const candidate = path.join(dir, target + '.json'),
      tree = path.join(dir, target + '.tree');
    safe(candidate, false);
    safe(tree, true);
    const evidence = JSON.parse(fs.readFileSync(candidate, 'utf8'));
    if (evidence.target !== target || evidence.status !== 'prepared')
      throw Error('candidate');
    const confirmation = fs.readFileSync(confirmationFile, 'utf8').trim();
    if (!/^[a-f0-9]{64}$/.test(confirmation)) throw Error('confirmation');
    const argv = [
      path.join(root, pair.publish),
      '--send',
      '--target',
      target,
      '--registry',
      job.registry,
      '--candidate',
      candidate,
      '--tree',
      tree,
    ];
    const env = {
      PATH: process.env.PATH || '/usr/bin:/bin',
      HOME: '/nonexistent',
      GIT_CONFIG_NOSYSTEM: '1',
      GIT_CONFIG_GLOBAL: '/dev/null',
      GIT_TERMINAL_PROMPT: '0',
    };
    const preview = spawnSync('sh', argv, {
      encoding: 'utf8',
      env,
      stdio: ['ignore', 'pipe', 'pipe'],
      maxBuffer: 1024 * 1024,
    });
    if (preview.error || preview.status !== 0) throw Error('preview');
    const result = JSON.parse(preview.stdout);
    if (
      result.status !== 'preview' ||
      result.target !== target ||
      result.confirmation !== confirmation
    )
      throw Error('confirmation');
    const issued = fs.readFileSync(0);
    const sent = spawnSync('sh', [...argv, '--confirm', confirmation], {
      input: issued,
      encoding: 'utf8',
      env,
      maxBuffer: 1024 * 1024,
    });
    if (sent.error) throw Error('send');
    const response = JSON.parse(sent.stdout);
    if (sent.status === 1 && response.status === 'waiting') {
      console.log('waiting');
      return 2;
    }
    if (
      sent.status === 0 &&
      ['published', 'merged'].includes(response.status)
    ) {
      console.log(response.status);
      return 0;
    }
    throw Error('send');
  } catch {
    console.error('送信に失敗しました');
    return 1;
  }
}
module.exports = { send };
