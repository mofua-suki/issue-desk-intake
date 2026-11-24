/* global require, process, console */
/* eslint-disable @typescript-eslint/no-require-imports */
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const { createHash } = require('node:crypto');
const { Buffer } = require('node:buffer');
const { spawnSync } = require('node:child_process');
const IMAGE =
  'ghcr.io/gitleaks/gitleaks@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f';
const digest = (data) => createHash('sha256').update(data).digest('hex');
const sha = (s, n = 64) =>
  typeof s === 'string' && new RegExp(`^[a-f0-9]{${n}}$`).test(s);
function safeFile(file) {
  if (!path.isAbsolute(file) || fs.realpathSync(file) !== file) throw Error();
  const st = fs.lstatSync(file);
  if (
    !st.isFile() ||
    st.isSymbolicLink() ||
    st.uid !== process.getuid() ||
    st.mode & 0o077 ||
    st.nlink !== 1
  )
    throw Error();
  return fs.readFileSync(file);
}
function relative(s) {
  return (
    typeof s === 'string' &&
    s.length > 0 &&
    !s.includes('\\') &&
    !s.includes('\0') &&
    !s.startsWith('/') &&
    path.posix.normalize(s) === s &&
    s.split('/').every((x) => x && x !== '.' && x !== '..')
  );
}
function treeId(root) {
  const h = createHash('sha256');
  function visit(dir, prefix) {
    for (const name of fs.readdirSync(dir).sort()) {
      const rel = prefix ? prefix + '/' + name : name;
      if (!relative(rel)) throw Error();
      const file = path.join(dir, name),
        st = fs.lstatSync(file);
      if (!st.isFile() && !st.isDirectory()) throw Error();
      const data = st.isFile() ? fs.readFileSync(file) : Buffer.alloc(0);
      h.update(
        JSON.stringify([
          rel,
          st.mode & 0o777,
          st.isDirectory() ? 'd' : 'f',
          data.length,
        ]),
      );
      h.update('\0');
      h.update(data);
      if (st.isDirectory()) visit(file, rel);
    }
  }
  const st = fs.lstatSync(root);
  if (!st.isDirectory() || st.isSymbolicLink()) throw Error();
  visit(root, '');
  return h.digest('hex');
}
function main(configFile, jobsFile, output) {
  const configBytes = safeFile(configFile);
  const config = JSON.parse(configBytes);
  if (
    !config ||
    !Array.isArray(config.exceptions) ||
    Object.keys(config).join(',') !== 'exceptions'
  )
    throw Error();
  for (const x of config.exceptions) {
    if (
      !x ||
      Object.keys(x).sort().join(',') !== 'path,rule,sha256,target' ||
      !/^[a-z0-9][a-z0-9-]{0,63}$/.test(x.target) ||
      !relative(x.path) ||
      typeof x.rule !== 'string' ||
      !/^[a-z0-9][a-z0-9-]*$/.test(x.rule) ||
      !sha(x.sha256)
    )
      throw Error();
  }
  const jobsBytes = fs.readFileSync(jobsFile),
    jobs = JSON.parse(jobsBytes);
  const targets = Object.keys(jobs);
  if (
    !targets.length ||
    targets.some((t) => !/^[a-z0-9][a-z0-9-]{0,63}$/.test(t))
  )
    throw Error();
  if (config.exceptions.some((x) => !Object.hasOwn(jobs, x.target)))
    throw Error();
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'public-scan-'));
  try {
    if (temp.includes(',')) throw Error();
    const toml = path.join(temp, 'scan.toml'),
      ignore = path.join(temp, 'ignore'),
      dockerConfig = path.join(temp, 'docker');
    fs.writeFileSync(toml, 'title = "fixed"\n[extend]\nuseDefault = true\n', {
      mode: 0o644,
    });
    fs.chmodSync(toml, 0o644);
    fs.mkdirSync(ignore, { mode: 0o755 });
    fs.chmodSync(ignore, 0o755);
    fs.mkdirSync(dockerConfig, { mode: 0o700 });
    const snapshots = [];
    for (const target of targets) {
      const record = path.join(output, target + '.json'),
        tree = path.join(output, target + '.tree');
      const evidenceBytes = safeFile(record),
        e = JSON.parse(evidenceBytes);
      if (
        e.target !== target ||
        !['prepared', 'no-diff', 'skipped'].includes(e.status) ||
        !sha(e.treeSha256) ||
        ![40, 64].some((n) => sha(e.inputSha, n)) ||
        !['configSha256', 'generatorSha256', 'ciSha256'].every((k) => sha(e[k]))
      )
        throw Error();
      if (e.status === 'skipped') {
        if (
          fs.existsSync(tree) ||
          Object.hasOwn(e, 'commit') ||
          Object.hasOwn(e, 'publicBase')
        )
          throw Error();
        snapshots.push({ record, evidenceBytes, absentTree: tree });
        continue;
      }
      if (
        e.status === 'no-diff' &&
        (e.generatedTree !== e.publicTree ||
          ![40, 64].some((n) => sha(e.publicTree, n)) ||
          Object.hasOwn(e, 'commit') ||
          Object.hasOwn(e, 'publicBase'))
      )
        throw Error();
      if (
        e.status === 'prepared' &&
        (![40, 64].some((n) => sha(e.commit, n)) ||
          ![40, 64].some((n) => sha(e.publicBase, n)) ||
          ![40, 64].some((n) => sha(e.generatedTree, n)) ||
          ![40, 64].some((n) => sha(e.publicTree, n)) ||
          e.generatedTree === e.publicTree)
      )
        throw Error();
      if (tree.includes(',') || treeId(tree) !== e.treeSha256) throw Error();
      const uid = process.getuid() || 1000,
        gid = process.getgid() || 1000;
      const result = spawnSync(
        'docker',
        [
          '--config',
          dockerConfig,
          'run',
          '--rm',
          '--pull',
          'never',
          '--network',
          'none',
          '--read-only',
          '--cap-drop',
          'ALL',
          '--security-opt',
          'no-new-privileges',
          '--user',
          `${uid}:${gid}`,
          '--pids-limit',
          '256',
          '--log-driver',
          'none',
          '--mount',
          `type=bind,source=${tree},destination=/source,readonly`,
          '--mount',
          `type=bind,source=${toml},destination=/scan.toml,readonly`,
          '--mount',
          `type=bind,source=${ignore},destination=/empty-ignore,readonly`,
          IMAGE,
          'dir',
          '/source',
          '--config',
          '/scan.toml',
          '--gitleaks-ignore-path',
          '/empty-ignore',
          '--ignore-gitleaks-allow',
          '--redact=100',
          '--exit-code',
          '14',
          '--report-format',
          'json',
          '--report-path',
          '-',
        ],
        {
          encoding: 'utf8',
          env: {
            PATH: process.env.PATH || '/usr/bin:/bin',
            HOME: dockerConfig,
          },
          maxBuffer: 16 * 1024 * 1024,
        },
      );
      if (result.error || ![0, 14].includes(result.status)) throw Error();
      const report = JSON.parse(result.stdout);
      if (
        !Array.isArray(report) ||
        (result.status === 0) !== (report.length === 0)
      )
        throw Error();
      for (const finding of report) {
        if (
          !finding ||
          typeof finding.File !== 'string' ||
          !finding.File.startsWith('/source/') ||
          !relative(finding.File.slice(8)) ||
          typeof finding.RuleID !== 'string' ||
          !/^[a-z0-9][a-z0-9-]*$/.test(finding.RuleID) ||
          !Number.isSafeInteger(finding.StartLine) ||
          !Number.isSafeInteger(finding.EndLine) ||
          finding.StartLine < 1 ||
          finding.EndLine < finding.StartLine
        )
          throw Error();
        const rel = finding.File.slice(8),
          file = path.join(tree, rel),
          st = fs.lstatSync(file);
        if (!st.isFile() || fs.realpathSync(file) !== file) throw Error();
        const data = fs.readFileSync(file),
          lines = [];
        let start = 0;
        for (let i = 0; i < data.length; i++)
          if (data[i] === 10) {
            lines.push(data.subarray(start, i));
            start = i + 1;
          }
        if (start < data.length) lines.push(data.subarray(start));
        if (finding.EndLine > lines.length) throw Error();
        const selected = Buffer.concat(
          lines
            .slice(finding.StartLine - 1, finding.EndLine)
            .flatMap((line, i) => (i ? [Buffer.from('\n'), line] : [line])),
        );
        if (
          !config.exceptions.some(
            (x) =>
              x.target === target &&
              x.path === rel &&
              x.rule === finding.RuleID &&
              x.sha256 === digest(selected),
          )
        )
          throw Error();
      }
      if (
        treeId(tree) !== e.treeSha256 ||
        !safeFile(record).equals(evidenceBytes)
      )
        throw Error();
      snapshots.push({ record, evidenceBytes, tree, treeSha256: e.treeSha256 });
    }
    for (const s of snapshots)
      if (
        !safeFile(s.record).equals(s.evidenceBytes) ||
        (s.absentTree && fs.existsSync(s.absentTree)) ||
        (s.tree && treeId(s.tree) !== s.treeSha256)
      )
        throw Error();
    if (
      !safeFile(configFile).equals(configBytes) ||
      !fs.readFileSync(jobsFile).equals(jobsBytes)
    )
      throw Error();
  } finally {
    fs.rmSync(temp, { recursive: true, force: true });
  }
}
try {
  if (
    process.argv.length !== 5 ||
    !path.isAbsolute(process.argv[2]) ||
    !path.isAbsolute(process.argv[3]) ||
    !path.isAbsolute(process.argv[4])
  )
    throw Error();
  main(process.argv[2], process.argv[3], process.argv[4]);
} catch {
  console.error('公開候補の機密検査に失敗しました');
  process.exitCode = 1;
}
