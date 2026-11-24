/* global require, process, __dirname, console */
/* eslint-disable @typescript-eslint/no-require-imports */
'use strict';
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const crypto = require('node:crypto');
const { spawnSync } = require('node:child_process');
const { canPublish } = require('./public-source-credentials.cjs');
const { mergePublicSource } = require('./public-source-merge.cjs');

const hash = (value) => crypto.createHash('sha256').update(value).digest('hex');
const oid = (value) =>
  typeof value === 'string' && /^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(value);
class Stop extends Error {}
const fail = (message) => {
  throw new Stop(message);
};
let temp, lock;
try {
  const args = process.argv.slice(2),
    options = {};
  for (let i = 0; i < args.length; i += 2) {
    const key = args[i];
    if (
      ![
        '--target',
        '--registry',
        '--candidate',
        '--tree',
        '--confirm',
      ].includes(key) ||
      !args[i + 1] ||
      options[key]
    )
      fail('不正な引数');
    options[key] = args[i + 1];
  }
  for (const key of ['--target', '--registry', '--candidate', '--tree'])
    if (!options[key]) fail('必須引数がありません');
  const target = options['--target'];
  if (!/^[a-z0-9][a-z0-9-]*$/.test(target)) fail('対象が不正です');
  const registry = JSON.parse(fs.readFileSync(options['--registry'], 'utf8'));
  if (!Object.hasOwn(registry, target)) fail('未登録の対象');
  const entry = registry[target],
    send = entry && entry.send;
  if (
    !send ||
    (send.mergeMethod !== undefined && send.mergeMethod !== 'merge') ||
    !path.isAbsolute(entry.publicRepo || '') ||
    !path.isAbsolute(send.stateDir || '') ||
    typeof send.base !== 'string' ||
    !/^refs\/heads\/[A-Za-z0-9][A-Za-z0-9._/-]*$/.test(send.base)
  )
    fail('送信設定が不正です');
  const prepared = JSON.parse(fs.readFileSync(options['--candidate'], 'utf8'));
  if (
    prepared.status !== 'prepared' ||
    prepared.target !== target ||
    !oid(prepared.commit) ||
    !oid(prepared.publicBase) ||
    !oid(prepared.generatedTree)
  )
    fail('候補が不正です');
  if (!fs.statSync(send.stateDir).isDirectory())
    fail('状態ディレクトリが不正です');
  temp = fs.mkdtempSync(path.join(os.tmpdir(), 'public-send-'));
  const env = {
    PATH: process.env.PATH || '/usr/bin:/bin',
    HOME: temp,
    GIT_CONFIG_NOSYSTEM: '1',
    GIT_CONFIG_GLOBAL: '/dev/null',
    GIT_TERMINAL_PROMPT: '0',
    LC_ALL: 'C',
  };
  const run = (command, argv, options = {}) => {
    const result = spawnSync(command, argv, {
      encoding: 'utf8',
      env,
      stdio: ['ignore', 'pipe', 'pipe'],
      ...options,
    });
    if (result.status !== 0) fail('検査または照会に失敗しました');
    return result.stdout.trim();
  };
  const prep = run('sh', [
    path.join(__dirname, 'publish-public-source.sh'),
    '--target',
    target,
    '--registry',
    options['--registry'],
    '--output',
    path.join(temp, 'tree'),
    '--public-base',
    prepared.publicBase,
    '--revalidate',
    'true',
  ]);
  const fresh = JSON.parse(prep);
  for (const key of [
    'status',
    'target',
    'inputSha',
    'configSha256',
    'generatorSha256',
    'ciSha256',
    'treeSha256',
    'publicTree',
    'generatedTree',
    'publicBase',
  ]) {
    if (fresh[key] !== prepared[key]) fail('候補の再照合に失敗しました');
  }
  function files(root) {
    const result = new Map();
    function visit(dir, prefix) {
      for (const name of fs.readdirSync(dir).sort()) {
        const rel = prefix ? prefix + '/' + name : name,
          full = path.join(dir, name),
          stat = fs.lstatSync(full);
        if (stat.isDirectory()) {
          result.set(rel, { kind: 'directory', mode: stat.mode & 0o777 });
          visit(full, rel);
        } else if (stat.isFile())
          result.set(rel, {
            kind: 'file',
            mode: stat.mode & 0o777,
            bytes: fs.readFileSync(full),
          });
        else fail('候補木が不正です');
      }
    }
    visit(root, '');
    return result;
  }
  const original = files(options['--tree']),
    generated = files(path.join(temp, 'tree'));
  if (original.size !== generated.size) fail('候補木が不一致です');
  for (const [name, file] of original) {
    const other = generated.get(name);
    if (
      !other ||
      file.kind !== other.kind ||
      file.mode !== other.mode ||
      (file.kind === 'file' && !file.bytes.equals(other.bytes))
    )
      fail('候補木が不一致です');
  }
  const git = (...args) => run('git', ['-C', entry.publicRepo, ...args]);
  git('check-ref-format', send.base);
  // Compare the complete raw commit against the regenerated one, allowing only
  // Git's creation timestamps to differ.
  const rawCommit = (commit) => {
    const result = spawnSync(
      'git',
      [
        '-C',
        entry.publicRepo,
        '--no-replace-objects',
        'cat-file',
        'commit',
        commit,
      ],
      {
        env,
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'pipe'],
      },
    );
    if (result.status !== 0) fail('コミットを読み取れません');
    return result.stdout;
  };
  const normalizeCommit = (contents) => {
    const match =
      /^(tree [a-f0-9]+\nparent [a-f0-9]+\nauthor [^\n]+) [0-9]+ [+-][0-9]{4}\n(committer Public Source <public-source@example\.invalid>) [0-9]+ [+-][0-9]{4}\n\nPrepare public source\n$(?![\s\S])/.exec(
        contents,
      );
    return match && match[1] + '\n' + match[2];
  };
  const expected = normalizeCommit(rawCommit(fresh.commit));
  if (!expected || normalizeCommit(rawCommit(prepared.commit)) !== expected)
    fail('コミットが不一致です');
  const changesWorkflow = !!git(
    'diff',
    '--name-only',
    prepared.publicBase,
    prepared.commit,
    '--',
    '.github/workflows',
  );
  const registration = {
    repository: send.repository,
    credentialRef: send.credentialRef,
  };
  const candidate = { ...registration, changesWorkflow };
  // Validate the identifiers even in preview; actual issuer evidence is checked before networking.
  if (
    !canPublish({
      registration,
      candidate,
      credential: {
        ...registration,
        permissions: {
          contents: 'write',
          pull_requests: 'write',
          workflows: 'write',
        },
      },
    })
  )
    fail('登録が不正です');
  const branch =
    'public-source/' +
    hash(
      JSON.stringify([
        send.repository.toLowerCase(),
        send.base,
        prepared.generatedTree,
        prepared.publicBase,
      ]),
    );
  const ref = 'refs/heads/' + branch;
  if (ref === send.base) fail('送信先が不正です');
  const proof = hash(
    JSON.stringify([
      target,
      send.repository,
      send.base,
      prepared.commit,
      prepared.generatedTree,
      prepared.publicBase,
      ref,
      send.credentialRef,
    ]),
  );
  if (!options['--confirm']) {
    console.log(
      JSON.stringify({
        status: 'preview',
        target,
        repository: send.repository,
        base: prepared.publicBase,
        head: prepared.commit,
        tree: prepared.generatedTree,
        branch,
        confirmation: proof,
      }),
    );
  } else {
    if (options['--confirm'] !== proof) fail('確認値が不一致です');
    const issued = JSON.parse(fs.readFileSync(0, 'utf8'));
    if (
      !issued ||
      typeof issued.token !== 'string' ||
      !issued.token ||
      /[\r\n]/.test(issued.token) ||
      !canPublish({ registration, candidate, credential: issued.metadata })
    )
      fail('資格情報を照合できません');
    const stateKey = hash(send.repository.toLowerCase());
    lock = path.join(send.stateDir, stateKey + '.lock');
    try {
      fs.mkdirSync(lock, { mode: 0o700 });
    } catch {
      lock = undefined;
      fail('対象は処理中です');
    }
    const pending = path.join(
      send.stateDir,
      hash(JSON.stringify([send.repository.toLowerCase(), branch, send.base])) +
        '.pending',
    );
    const remote = `https://github.com/${send.repository}.git`;
    // A fresh bare repository isolates remote operations from repository-local Git configuration.
    const bare = path.join(temp, 'transport.git');
    run('git', [
      'init',
      '--bare',
      '-q',
      `--object-format=${prepared.commit.length === 64 ? 'sha256' : 'sha1'}`,
      bare,
    ]);
    const objects = git('rev-parse', '--git-path', 'objects');
    const objectsPath = path.resolve(entry.publicRepo, objects);
    const askpass = path.join(temp, 'askpass');
    fs.writeFileSync(
      askpass,
      '#!/bin/sh\ncase "$1" in *sername*) printf "%s\\n" x-access-token;; *) printf "%s\\n" "$PUBLIC_SEND_TOKEN";; esac\n',
      { mode: 0o700 },
    );
    const authEnv = {
      ...env,
      GIT_ALTERNATE_OBJECT_DIRECTORIES: objectsPath,
      GIT_ASKPASS: askpass,
      PUBLIC_SEND_TOKEN: issued.token,
      GH_TOKEN: issued.token,
      GH_HOST: 'github.com',
    };
    const transport = (...args) =>
      run(
        'git',
        [
          '--git-dir',
          bare,
          '-c',
          'credential.helper=',
          '-c',
          'http.followRedirects=false',
          '-c',
          'core.hooksPath=/dev/null',
          ...args,
        ],
        { env: authEnv },
      );
    const readRef = (name) => {
      const output = transport('ls-remote', '--refs', '--', remote, name);
      if (!output) return '';
      const match =
        /^([a-f0-9]{40}|[a-f0-9]{64})\s+(refs\/heads\/[^\s]+)$/.exec(output);
      if (!match || match[2] !== name) fail('公開参照を照合できません');
      return match[1];
    };
    const baseBranch = send.base.slice('refs/heads/'.length);
    const api = (method, endpoint, fields = [], allowUnprotected = false) => {
      if (allowUnprotected) {
        const result = spawnSync(
          'gh',
          [
            'api',
            '--hostname',
            'github.com',
            '--include',
            '--method',
            method,
            endpoint,
            ...fields,
          ],
          { encoding: 'utf8', env: authEnv, stdio: ['ignore', 'pipe', 'pipe'] },
        );
        const match =
          /^HTTP\/\S+ 404[^\r\n]*\r?\n[\s\S]*?\r?\n\r?\n([\s\S]*)$/.exec(
            result.stdout || '',
          );
        if (result.status !== 0 && match) {
          try {
            if (JSON.parse(match[1]).message === 'Branch not protected')
              return null;
          } catch {
            /* Refuse malformed responses. */
          }
        }
        if (result.status !== 0) fail('検査または照会に失敗しました');
        const body =
          /^HTTP\/\S+ 200[^\r\n]*\r?\n[\s\S]*?\r?\n\r?\n([\s\S]*)$/.exec(
            result.stdout || '',
          );
        if (!body) fail('保護情報を照合できません');
        return body[1];
      }
      return run(
        'gh',
        [
          'api',
          '--hostname',
          'github.com',
          '--method',
          method,
          endpoint,
          ...fields,
        ],
        { env: authEnv },
      );
    };
    const finish = (number) => {
      const result = mergePublicSource({
        send,
        prepared,
        target,
        branch,
        number,
        api,
        readRef,
        fail,
      });
      if (result.status === 'waiting') process.exitCode = 1;
      return result;
    };
    const checkRepository = () => {
      const repository = JSON.parse(
        run(
          'gh',
          ['api', '--hostname', 'github.com', `repos/${send.repository}`],
          { env: authEnv },
        ),
      );
      if (
        typeof repository.full_name !== 'string' ||
        repository.full_name.toLowerCase() !== send.repository.toLowerCase() ||
        repository.default_branch !== baseBranch
      )
        fail('公開先が不一致です');
    };
    const checkRefs = () => {
      checkRepository();
      if (
        readRef(send.base) !== prepared.publicBase ||
        readRef(ref) !== prepared.commit
      )
        fail('公開参照が変わりました');
    };
    checkRepository();
    if (readRef(send.base) !== prepared.publicBase) {
      if (send.mergeMethod !== 'merge') fail('公開基点が変わりました');
      const endpoint = `repos/${send.repository}/pulls?state=all&head=${encodeURIComponent(send.repository.split('/')[0] + ':' + branch)}&base=${encodeURIComponent(baseBranch)}&per_page=100`;
      const list = JSON.parse(api('GET', endpoint));
      if (
        !Array.isArray(list) ||
        list.length !== 1 ||
        !Number.isSafeInteger(list[0].number) ||
        list[0].merged_at == null
      )
        fail('公開基点が変わりました');
      console.log(JSON.stringify(finish(list[0].number)));
    } else {
      let existing = readRef(ref);
      if (existing && existing !== prepared.commit)
        fail('候補枝が競合しました');
      if (!existing) {
        try {
          transport(
            'push',
            '--no-follow-tags',
            '--recurse-submodules=no',
            `--force-with-lease=${ref}:`,
            '--',
            remote,
            `${prepared.commit}:${ref}`,
          );
        } catch {
          if (readRef(ref) !== prepared.commit)
            fail('候補枝の作成結果を確認できません');
        }
      }
      checkRefs();
      const endpoint = `repos/${send.repository}/pulls?state=all&head=${encodeURIComponent(send.repository.split('/')[0] + ':' + branch)}&base=${encodeURIComponent(baseBranch)}&per_page=100`;
      const pulls = () => {
        const list = JSON.parse(
          run('gh', ['api', '--hostname', 'github.com', endpoint], {
            env: authEnv,
          }),
        );
        if (!Array.isArray(list) || list.length >= 100 || list.length > 1)
          fail('PRを照合できません');
        if (!list.length) return null;
        const pr = list[0];
        if (
          pr.state !== 'open' ||
          !Number.isSafeInteger(pr.number) ||
          pr.number < 1 ||
          pr.head?.ref !== branch ||
          pr.head?.sha !== prepared.commit ||
          pr.head?.repo?.full_name?.toLowerCase() !==
            send.repository.toLowerCase() ||
          pr.base?.ref !== baseBranch ||
          pr.base?.sha !== prepared.publicBase ||
          pr.base?.repo?.full_name?.toLowerCase() !==
            send.repository.toLowerCase()
        )
          fail('PRが不一致です');
        return pr;
      };
      let pr = pulls();
      if (!pr) {
        if (fs.existsSync(pending)) fail('PR作成結果が未確定です');
        checkRefs();
        const pendingFd = fs.openSync(pending, 'wx', 0o600);
        try {
          fs.writeFileSync(pendingFd, 'pending\n');
          fs.fsyncSync(pendingFd);
        } finally {
          fs.closeSync(pendingFd);
        }
        const directoryFd = fs.openSync(send.stateDir, 'r');
        try {
          fs.fsyncSync(directoryFd);
        } finally {
          fs.closeSync(directoryFd);
        }
        try {
          run(
            'gh',
            [
              'api',
              '--hostname',
              'github.com',
              '--method',
              'POST',
              `repos/${send.repository}/pulls`,
              '-f',
              'title=Public source update',
              '-f',
              'body=Verified public source update.',
              '-f',
              `head=${branch}`,
              '-f',
              `base=${baseBranch}`,
            ],
            { env: authEnv },
          );
        } catch {
          /* The request may have been accepted. Only query from here. */
        }
        pr = pulls();
        if (!pr) fail('PR作成結果が未確定です');
      }
      checkRefs();
      console.log(
        JSON.stringify(
          send.mergeMethod === 'merge'
            ? finish(pr.number)
            : {
                status: 'published',
                target,
                repository: send.repository,
                base: prepared.publicBase,
                head: prepared.commit,
                tree: prepared.generatedTree,
                branch,
                pr: pr.number,
              },
        ),
      );
    }
  }
} catch (error) {
  // Do not expose child process output, registry contents, or credential data.
  console.error(
    error instanceof Stop
      ? error.message
      : '公開送信を完了できません。入力と状態を確認してください。',
  );
  process.exitCode = 1;
} finally {
  if (lock) {
    try {
      fs.rmdirSync(lock);
    } catch {
      /* Keep unexpected contents for review. */
    }
  }
  if (temp) fs.rmSync(temp, { recursive: true, force: true });
}
