#!/bin/sh
set -eu
if [ "$#" -eq 1 ] && [ "$1" = --help ]; then
  echo 'Usage: sh scripts/publish-public-source.sh --target ID --registry FILE --output DIRECTORY [--previous FILE --previous-tree DIRECTORY]'
  echo '       sh scripts/publish-public-source.sh --send --target ID --registry FILE --candidate FILE --tree DIRECTORY [--confirm VALUE]'
  exit 0
fi
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ "${1-}" = --send ]; then
  shift
  exec node "$SCRIPT_DIR/public-source-send.cjs" "$@"
fi
node - "$SCRIPT_DIR" "$@" <<'NODE'
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const {createHash} = require('node:crypto');
const {execFileSync} = require('node:child_process');
const script = process.argv[2];
let temp;
try {
  const args = process.argv.slice(3), opts = {};
  for (let i = 0; i < args.length; i += 2) {
    const k = args[i];
    if (!['--target','--registry','--output','--previous','--previous-tree','--public-base','--revalidate'].includes(k) || !args[i+1] || opts[k]) throw Error('不正な引数');
    opts[k] = args[i+1];
  }
  for (const k of ['--target','--registry','--output']) if (!opts[k]) throw Error('必須引数がありません');
  if (!!opts['--previous'] !== !!opts['--previous-tree']) throw Error('前回記録と候補木は両方指定してください');
  if (opts['--revalidate'] && (opts['--revalidate'] !== 'true' || !opts['--public-base'])) throw Error('再照合の引数が不正です');
  const target = opts['--target'];
  const registry = JSON.parse(fs.readFileSync(opts['--registry']));
  if (!/^[a-z0-9][a-z0-9-]*$/.test(target) || !Object.hasOwn(registry, target)) throw Error('未登録の対象');
  const entry = registry[target];
  if (!entry || !entry.source || !path.isAbsolute(entry.source.repo) || !path.isAbsolute(entry.publicRepo) || typeof entry.ref !== 'string' || !entry.ref || typeof entry.publicRef !== 'string' || !entry.publicRef) throw Error('登録が不正です');
  const author = entry.author === undefined ? {name:'Public Source', email:'public-source@example.invalid'} : entry.author;
  if (entry.author !== undefined && (!author || typeof author !== 'object' || Array.isArray(author) || typeof author.name !== 'string' || !author.name.trim() || /[\x00-\x1f\x7f-\x9f\u2028\u2029<>]/.test(author.name) || author.name !== author.name.trim() || typeof author.email !== 'string' || !/^(?:[0-9]+\+)?(?![A-Za-z0-9-]*--)(?=[A-Za-z0-9-]{1,39}@)[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?@users\.noreply\.github\.com$(?![\s\S])/.test(author.email))) throw Error('著者が不正です');
  const git = (repo, ...args) => execFileSync('git', ['-C', repo, ...args], {encoding:'utf8', stdio:['ignore','pipe','pipe']}).trim();
  const sha = git(entry.source.repo, 'rev-parse', '--verify', '--end-of-options', entry.ref + '^{commit}');
  if (!/^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(sha)) throw Error('入力 SHA が不正です');
  const digest = data => createHash('sha256').update(data).digest('hex');
  const hashFiles = names => digest(Buffer.concat(names.map(n => fs.readFileSync(path.join(script, n)))));
  const config = digest(JSON.stringify({target, ...entry, source: {...entry.source, sha: undefined}}));
  const generator = hashFiles(['build-public-source.sh','public-source-verifier.mts','../deploy/public/LICENSE']);
  const ci = hashFiles(['../deploy/public/' + (['intake','worker'].includes(entry.source.ci) ? entry.source.ci : 'invalid') + '-verify.yml']);
  const treeId = root => {
    const h = createHash('sha256');
    function visit(dir, prefix) {
      for (const name of fs.readdirSync(dir).sort()) {
        const rel = prefix ? prefix + '/' + name : name, full = path.join(dir, name), st = fs.lstatSync(full);
        if (!st.isFile() && !st.isDirectory()) throw Error('候補木が不正です');
        const data = st.isFile() ? fs.readFileSync(full) : Buffer.alloc(0);
        h.update(JSON.stringify([rel, st.mode & 0o777, st.isDirectory() ? 'd' : 'f', data.length])); h.update('\0'); h.update(data);
        if (st.isDirectory()) visit(full, rel);
      }
    }
    visit(root, ''); return h.digest('hex');
  };
  const base = {target, inputSha:sha, configSha256:config, generatorSha256:generator, ciSha256:ci};
  if (!opts['--revalidate'] && !opts['--previous'] && entry.send && entry.send.mergeMethod === 'merge') {
    const send = entry.send, dir = send.stateDir;
    if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(send.repository || '') || typeof dir !== 'string' || !path.isAbsolute(dir) || path.normalize(dir) !== dir) throw Error('送信設定が不正です');
    let component = path.parse(dir).root;
    for (const part of dir.slice(component.length).split('/').filter(Boolean)) {
      component = path.join(component, part);
      const st = fs.lstatSync(component);
      if (!st.isDirectory() || st.isSymbolicLink()) throw Error('状態ディレクトリが不正です');
    }
    const file = path.join(dir, target + '.merged.json');
    let fd;
    try {
      try { fd = fs.openSync(file, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK); }
      catch (error) { if (error.code !== 'ENOENT') throw error; }
      if (fd !== undefined) {
        const st = fs.fstatSync(fd);
        if (!st.isFile() || st.uid !== process.getuid() || (st.mode & 0o7777) !== 0o600 || st.nlink !== 1) throw Error('マージ記録が不正です');
        const previous = JSON.parse(fs.readFileSync(fd, 'utf8'));
        const oid = value => typeof value === 'string' && /^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(value);
        if (previous && previous.status === 'merged' && previous.repository === send.repository && Object.keys(base).every(k => previous[k] === base[k]) && /^[a-f0-9]{64}$/.test(previous.treeSha256 || '') && [previous.mergeCommit, previous.generatedTree, previous.publicBase, previous.commit].every(oid)) {
          let matches = false;
          try {
            if (opts['--public-base'] && !oid(opts['--public-base'])) throw Error('公開基点が不正です');
            const publicBase = git(entry.publicRepo, 'rev-parse', '--verify', '--end-of-options', (opts['--public-base'] || entry.publicRef) + '^{commit}');
            const actual = git(entry.publicRepo, 'rev-list', '--parents', '-n', '1', previous.mergeCommit).split(' ');
            const tree = git(entry.publicRepo, 'rev-parse', '--verify', previous.mergeCommit + '^{tree}');
            git(entry.publicRepo, 'merge-base', '--is-ancestor', previous.mergeCommit, publicBase);
            matches = tree === previous.generatedTree && actual.length === 3 && actual[0] === previous.mergeCommit && actual[1] === previous.publicBase && actual[2] === previous.commit;
          } catch (_) { /* Missing or unreachable evidence is not a skip. */ }
          if (matches) { console.log(JSON.stringify({...base, treeSha256:previous.treeSha256, status:'skipped'})); process.exit(0); }
        }
      }
    } finally { if (fd !== undefined) fs.closeSync(fd); }
  }
  if (opts['--previous']) {
    const previous = JSON.parse(fs.readFileSync(opts['--previous']));
    if (previous.status === 'merged' && Object.keys(base).every(k => previous[k] === base[k]) && /^[a-f0-9]{64}$/.test(previous.treeSha256) && treeId(opts['--previous-tree']) === previous.treeSha256) {
      console.log(JSON.stringify({...base, treeSha256:previous.treeSha256, status:'skipped'})); process.exit(0);
    }
  }
  const output = path.resolve(opts['--output']);
  if (fs.existsSync(output)) throw Error('出力先が既に存在します');
  temp = fs.mkdtempSync(path.join(os.tmpdir(), 'public-prepare-'));
  const reg = path.join(temp, 'registry.json');
  fs.writeFileSync(reg, JSON.stringify({[target]: {...entry.source, sha}}), {mode:0o600});
  let result;
  try {
    result = execFileSync('sh', [path.join(script,'build-public-source.sh'), '--target',target,'--registry',reg,'--output',output], {encoding:'utf8', stdio:['ignore','pipe','pipe'], env:{PATH:process.env.PATH || '/usr/bin:/bin', HOME:temp}});
  } catch (error) {
    const lines = typeof error.stderr === 'string' ? error.stderr.split('\n') : [];
    const pattern = /^公開ソース生成失敗: (公開前検査に失敗|置換元がありません|指定パスが通常ファイルではありません|runnerの固定SHA-256が一致しません): ([A-Za-z0-9._/-]{1,200})$/;
    const matches = lines.map(line => pattern.exec(line)).filter(Boolean).filter(m => !m[2].startsWith('/') && !m[2].includes('..') && m[2].split('/').every(part => part && part !== '.' && part.toLowerCase() !== '.git'));
    throw Error(matches.length === 1 ? matches[0][1] + ': ' + matches[0][2] : '登録・生成・検査を完了できません');
  }
  const match = /^tree-sha256: ([a-f0-9]{64})$/m.exec(result);
  if (!match || treeId(output) !== match[1]) throw Error('生成木の照合に失敗');
  const index = path.join(temp,'index');
  const env = {...process.env, GIT_INDEX_FILE:index, GIT_WORK_TREE:output};
  const run = (...args) => execFileSync('git',['-C',entry.publicRepo,...args],{env,encoding:'utf8',stdio:['ignore','pipe','pipe']}).trim();
  run('read-tree','--empty'); run('add','-f','--all');
  const files = new Map();
  function collect(dir, prefix) {
    for (const name of fs.readdirSync(dir)) {
      const rel = prefix ? prefix + '/' + name : name, full = path.join(dir, name), st = fs.lstatSync(full);
      if (st.isDirectory()) collect(full, rel);
      else if (st.isFile()) files.set(rel, {full, mode: st.mode & 0o111 ? '100755' : '100644'});
      else throw Error('候補木が不正です');
    }
  }
  collect(output, '');
  const entries = run('ls-files','--stage','-z').split('\0').filter(Boolean);
  for (const item of entries) {
    const m = /^(100644|100755) ([a-f0-9]+) 0\t([\s\S]+)$/.exec(item);
    const file = m && files.get(m[3]);
    if (!file || file.mode !== m[1]) throw Error('候補木とインデックスが不一致です');
    const blob = execFileSync('git',['-C',entry.publicRepo,'cat-file','blob',m[2]],{env,stdio:['ignore','pipe','pipe']});
    if (!blob.equals(fs.readFileSync(file.full))) throw Error('候補木とインデックスが不一致です');
    files.delete(m[3]);
  }
  if (files.size || treeId(output) !== match[1]) throw Error('候補木とインデックスが不一致です');
  const generatedTree = run('write-tree');
  if (opts['--public-base'] && !/^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(opts['--public-base'])) throw Error('公開基点が不正です');
  const publicBase = git(entry.publicRepo,'rev-parse','--verify','--end-of-options',(opts['--public-base'] || entry.publicRef) + '^{commit}');
  const publicTree = git(entry.publicRepo,'rev-parse','--verify',publicBase + '^{tree}');
  const resultData = {...base,treeSha256:match[1],publicTree,generatedTree};
  if (generatedTree === publicTree) {
    resultData.status = 'no-diff';
    if (entry.send && entry.send.mergeMethod === 'merge') {
      const send = entry.send;
      if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(send.repository || '') || !path.isAbsolute(send.stateDir || '') || !fs.statSync(send.stateDir).isDirectory()) throw Error('送信設定が不正です');
      const lock = path.join(send.stateDir, digest(send.repository.toLowerCase()) + '.lock');
      fs.mkdirSync(lock, {mode:0o700});
      let file, fd;
      try {
        if (git(entry.publicRepo,'rev-parse','--verify','--end-of-options',entry.publicRef + '^{commit}') !== publicBase) throw Error('公開基点が変わりました');
        file = path.join(send.stateDir, '.' + target + '.' + require('node:crypto').randomBytes(16).toString('hex') + '.tmp');
        fd = fs.openSync(file,'wx',0o600); fs.writeFileSync(fd,JSON.stringify(resultData)+'\n'); fs.fsyncSync(fd); fs.closeSync(fd); fd = undefined;
        fs.renameSync(file,path.join(send.stateDir,target + '.no-diff.json')); file = undefined;
        const dir = fs.openSync(send.stateDir,'r'); try { fs.fsyncSync(dir); } finally { fs.closeSync(dir); }
      } finally { if (fd !== undefined) fs.closeSync(fd); if (file && fs.existsSync(file)) fs.unlinkSync(file); fs.rmdirSync(lock); }
    }
  }
  else {
    const commitEnv = {...env, GIT_AUTHOR_NAME:author.name, GIT_AUTHOR_EMAIL:author.email, GIT_COMMITTER_NAME:'Public Source', GIT_COMMITTER_EMAIL:'public-source@example.invalid'};
    const commit = execFileSync('git',['-C',entry.publicRepo,'-c','commit.gpgsign=false','commit-tree',generatedTree,'-p',publicBase],{env:commitEnv,input:'Prepare public source\n',encoding:'utf8',stdio:['pipe','pipe','pipe']}).trim();
    Object.assign(resultData,{status:'prepared',publicBase,commit});
  }
  console.log(JSON.stringify(resultData));
} catch (error) { console.error('候補準備失敗: ' + error.message); process.exitCode = 1; }
finally { if (temp) fs.rmSync(temp,{recursive:true,force:true}); }
NODE
