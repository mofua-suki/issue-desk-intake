/* global require, process */
/* eslint-disable @typescript-eslint/no-require-imports */
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');

function privatePath(file, directory) {
  if (!path.isAbsolute(file) || fs.realpathSync(file) !== file) throw Error();
  const st = fs.lstatSync(file);
  if (
    st.uid !== process.getuid() ||
    st.mode & 0o077 ||
    (directory ? !st.isDirectory() : !st.isFile())
  )
    throw Error();
}
function read(file) {
  privatePath(file, false);
  return JSON.parse(fs.readFileSync(file, 'utf8'));
}
function git(args, httpAuth = false, publicFetch = false) {
  const env = {
    PATH: process.env.PATH || '/usr/bin:/bin',
    HOME: publicFetch ? '/nonexistent' : process.env.HOME || '/nonexistent',
    GIT_TERMINAL_PROMPT: '0',
    GIT_ASKPASS: '/bin/false',
    SSH_ASKPASS: '/bin/false',
    GIT_CONFIG_NOSYSTEM: '1',
    GIT_CONFIG_GLOBAL: httpAuth
      ? (process.env.HOME || '/nonexistent') + '/.gitconfig'
      : '/dev/null',
    SSH_AUTH_SOCK: publicFetch ? '' : process.env.SSH_AUTH_SOCK || '',
    GIT_SSH_COMMAND: publicFetch ? '/bin/false' : 'ssh -oBatchMode=yes',
  };
  const result = spawnSync('git', args, {
    env,
    encoding: 'utf8',
    maxBuffer: 1024 * 1024,
  });
  if (result.error || result.status !== 0) throw Error();
  return result.stdout.trim();
}
function object(value) {
  return value && typeof value === 'object' && !Array.isArray(value);
}
try {
  const [registration, jobsFile] = process.argv.slice(2);
  if (process.argv.length !== 4) throw Error();
  const entries = read(registration),
    jobs = read(jobsFile);
  if (!object(entries) || !Object.keys(entries).length || !object(jobs))
    throw Error();
  const targets = [];
  for (const [id, item] of Object.entries(entries)) {
    if (
      !object(item) ||
      !Object.keys(item).every((k) =>
        ['repo', 'remote', 'ref', 'httpAuth', 'public'].includes(k),
      ) ||
      typeof item.repo !== 'string' ||
      typeof item.remote !== 'string' ||
      !/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(item.remote) ||
      typeof item.ref !== 'string' ||
      !item.ref.startsWith('refs/heads/') ||
      (item.httpAuth !== undefined && typeof item.httpAuth !== 'boolean')
    )
      throw Error();
    privatePath(item.repo, true);
    const job = jobs[id];
    if (!object(job) || typeof job.registry !== 'string') throw Error();
    const target = read(job.registry)[id];
    if (
      !object(target) ||
      !object(target.source) ||
      target.source.repo !== item.repo ||
      target.ref !== item.ref
    )
      throw Error();
    if (git(['-C', item.repo, 'rev-parse', '--is-bare-repository']) !== 'true')
      throw Error();
    git(['check-ref-format', item.ref]);
    const url = git(['-C', item.repo, 'remote', 'get-url', item.remote]);
    if (/^(?:https?|ftp):\/\/[^/]*@/i.test(url) || /[\r\n]/.test(url))
      throw Error();
    if (
      !item.httpAuth &&
      git(['-C', item.repo, 'config', '--local', '--name-only', '--list'])
        .split('\n')
        .some((name) =>
          /^(?:http\..*extraheader|credential\..*helper)$/i.test(name),
        )
    )
      throw Error();
    targets.push(item);
    if (item.public !== undefined) {
      const pub = item.public;
      if (
        !object(pub) ||
        Object.keys(pub).sort().join(',') !== 'ref,remote,repo' ||
        typeof pub.repo !== 'string' ||
        typeof pub.remote !== 'string' ||
        !/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(pub.remote) ||
        typeof pub.ref !== 'string' ||
        !pub.ref.startsWith('refs/heads/') ||
        target.publicRepo !== pub.repo ||
        target.publicRef !== pub.ref
      )
        throw Error();
      privatePath(pub.repo, true);
      if (git(['-C', pub.repo, 'rev-parse', '--is-bare-repository']) !== 'true')
        throw Error();
      git(['check-ref-format', pub.ref]);
      const publicUrl = git(['-C', pub.repo, 'remote', 'get-url', pub.remote]);
      if (
        (!publicUrl.startsWith('/') && !/^https:\/\//i.test(publicUrl)) ||
        /[\r\n]/.test(publicUrl) ||
        /^https:\/\/[^/]*@/i.test(publicUrl)
      )
        throw Error();
      if (
        git(['-C', pub.repo, 'config', '--local', '--name-only', '--list'])
          .split('\n')
          .some((name) =>
            /^(?:http\.|credential\.|url\.|include(?:if)?\.|remote\..*\.(?:proxy|uploadpack|vcs)$)/i.test(
              name,
            ),
          )
      )
        throw Error();
      targets.push({ ...pub, publicFetch: true });
    }
  }
  for (const item of targets) {
    const config = item.httpAuth
      ? []
      : ['-c', 'credential.helper=', '-c', 'http.extraHeader='];
    git(
      [
        ...config,
        '-C',
        item.repo,
        'fetch',
        '--no-tags',
        '--no-recurse-submodules',
        '--',
        item.remote,
        '+' + item.ref + ':' + item.ref,
      ],
      item.httpAuth,
      item.publicFetch,
    );
    git([
      '-C',
      item.repo,
      'rev-parse',
      '--verify',
      '--end-of-options',
      item.ref + '^{commit}',
    ]);
  }
} catch {
  process.stderr.write('公開元ミラーの更新に失敗しました\n');
  process.exitCode = 1;
}
