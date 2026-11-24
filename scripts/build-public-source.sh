#!/bin/sh
set -eu
if [ "$#" -eq 1 ] && [ "$1" = --help ]; then
  cat <<'HELP'
Usage: sh scripts/build-public-source.sh --target TARGET --repo REPOSITORY --ref REF --output DIRECTORY [--policy FILE]
       sh scripts/build-public-source.sh --target TARGET --registry FILE --output DIRECTORY
  --target TARGET     対象ID (従来形式では intake または worker)
  --repo REPOSITORY   入力 Git リポジトリ
  --ref REF           入力コミットの ref
  --output DIRECTORY  存在しない出力先
  --policy FILE       ローカル定義 (省略時 scripts/public-source-policy.json)
  --registry FILE     審査済み登録定義
HELP
  exit 0
fi
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
node - "$SCRIPT_DIR/public-source-policy.json" "$@" <<'NODE'
const fs = require('node:fs');
const path = require('node:path');
const {createHash} = require('node:crypto');
const {execFileSync} = require('node:child_process');
const defaultPolicy = process.argv[2];
let temp;
let registered = process.argv.slice(3).includes('--registry');
try {
  const args = process.argv.slice(3), options = {};
  for (let i = 0; i < args.length; i += 2) {
    const key = args[i];
    if (!['--target', '--repo', '--ref', '--output', '--policy', '--registry'].includes(key) || !args[i + 1] || options[key]) throw Error('不正な引数: ' + key);
    options[key] = args[i + 1];
  }
  for (const key of ['--target', '--output']) if (!options[key]) throw Error('必須引数がありません: ' + key);
  registered = !!options['--registry'];
  let definition, ci, verifier, image, configId;
  if (registered) {
    if (options['--repo'] || options['--ref'] || options['--policy']) throw Error('登録方式の引数が不正です');
    const data = fs.readFileSync(options['--registry']);
    const registry = JSON.parse(data);
    const target = options['--target'];
    if (!/^[a-z0-9][a-z0-9-]*$/.test(target) || !registry || !Object.hasOwn(registry, target)) throw Error('未登録の対象です');
    definition = registry[target];
    const keys = ['repo', 'sha', 'paths', 'replacements', 'reject', 'ci', 'verify', 'image'];
    const allowedKeys = [...keys, 'outputs'];
    if (!definition || typeof definition !== 'object' || Array.isArray(definition) || Object.keys(definition).some(k => !allowedKeys.includes(k)) || keys.some(k => !Object.hasOwn(definition, k))) throw Error('登録定義が不正です');
    if (typeof definition.repo !== 'string' || !path.isAbsolute(definition.repo) || typeof definition.sha !== 'string' || !/^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(definition.sha)) throw Error('入力定義が不正です');
    if (!['intake', 'worker'].includes(definition.ci) || !['intake', 'worker'].includes(definition.verify) || typeof definition.image !== 'string' || !/^(?:127\.0\.0\.1:5000\/)?[a-z0-9][a-z0-9._/-]*@sha256:[a-f0-9]{64}$/.test(definition.image)) throw Error('検査方式が不正です');
    options['--repo'] = definition.repo;
    options['--ref'] = definition.sha;
    ci = definition.ci;
    verifier = definition.verify;
    image = definition.image;
    configId = createHash('sha256').update(data).digest('hex');
  } else {
    for (const key of ['--repo', '--ref']) if (!options[key]) throw Error('必須引数がありません: ' + key);
    if (!['intake', 'worker'].includes(options['--target'])) throw Error('不正な対象');
    const policy = JSON.parse(fs.readFileSync(options['--policy'] || defaultPolicy, 'utf8'));
    definition = policy[options['--target']];
    ci = options['--target'];
  }
  if (!definition || !Array.isArray(definition.paths) || !definition.paths.length || !Array.isArray(definition.replacements) || !Array.isArray(definition.reject)) throw Error('対象定義が不正か収録パスが未承認です');
  const safePath = p => typeof p === 'string' && p.length > 0 && !p.startsWith('/') && !p.includes('\\') && p.split('/').every(s => s && s !== '.' && s !== '..' && s.toLowerCase() !== '.git');
  const blocked = p => /(^|\/)(\.env(?:\.|$)(?!example$)|\.ssh|\.aws|\.gnupg|node_modules|runtime|credentials?|secrets?)(\/|$)/i.test(p) || /(^|\/)(id_rsa|id_ed25519|.*\.(?:pem|key|p12|pfx))$/i.test(p);
  const workerRuntime = p => ci === 'worker' && (!registered || verifier === 'worker') &&
    /^internal\/runtime\/(?:[A-Za-z0-9_-]+\.go|testdata\/[A-Za-z0-9_-]+\.json)$/.test(p) &&
    !blocked(p.slice('internal/runtime/'.length)) && !/^(?:credentials?|secrets?)(?:[._-]|$)/i.test(path.posix.basename(p));
  const paths = definition.paths;
  if (new Set(paths).size !== paths.length || paths.some(p => !safePath(p) || (blocked(p) && !workerRuntime(p)))) throw Error('収録パスが不正または禁止されています');
  const aliases = definition.outputs === undefined ? {} : definition.outputs;
  if (!aliases || typeof aliases !== 'object' || Array.isArray(aliases) || Object.entries(aliases).some(([src, dest]) => !paths.includes(src) || !safePath(dest) || (blocked(dest) && !workerRuntime(dest)))) throw Error('出力対応が不正です');
  const outputName = name => Object.hasOwn(aliases, name) ? aliases[name] : name;
  for (const r of definition.replacements) if (!r || !safePath(r.path) || !paths.includes(r.path) || typeof r.from !== 'string' || !r.from || typeof r.to !== 'string' || (registered && Object.keys(r).some(k => !['path', 'from', 'to'].includes(k)))) throw Error('置換定義が不正です');
  if (definition.reject.some(x => typeof x !== 'string' || !x)) throw Error('検査定義が不正です');
  const repo = path.resolve(options['--repo']);
  const git = (...argv) => execFileSync('git', ['-C', repo, ...argv], {maxBuffer: 128 * 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe']});
  const commit = git('rev-parse', '--verify', '--end-of-options', options['--ref'] + '^{commit}').toString().trim();
  if (registered && commit !== options['--ref']) throw Error('固定SHAが一致しません');
  const entries = git('ls-tree', '-rz', '--full-tree', commit).toString('utf8').split('\0').filter(Boolean);
  const tree = new Map();
  for (const e of entries) { const m = /^(\d+) (\w+) ([0-9a-f]+)\t([\s\S]+)$/.exec(e); if (!m) throw Error('Git ツリーを解析できません'); tree.set(m[4], {mode: m[1], type: m[2], id: m[3]}); }
  const output = path.resolve(options['--output']);
  if (fs.existsSync(output) || fs.existsSync(output + '/.')) throw Error('出力先が既に存在します');
  let mvVersion;
  try { mvVersion = execFileSync('mv', ['--version'], {stdio: ['ignore', 'pipe', 'pipe']}).toString(); }
  catch { throw Error('出力の確定には GNU coreutils の mv が必要です'); }
  if (!/^mv \(GNU coreutils\) /m.test(mvVersion)) throw Error('出力の確定には GNU coreutils の mv が必要です');
  temp = fs.mkdtempSync(path.join(path.dirname(output), '.public-source-'));
  const workflow = '.github/workflows/verify.yml';
  const items = paths.map(name => ({name, dest: outputName(name)}));
  if (!paths.includes('LICENSE') || outputName('LICENSE') !== 'LICENSE') items.push({name: 'LICENSE', dest: 'LICENSE', bundled: true});
  items.push({name: workflow, dest: workflow, bundled: true});
  const outputPaths = items.map(item => item.dest);
  const seen = new Set();
  for (const p of outputPaths) {
    if (seen.has(p) || [...seen].some(other => p.startsWith(other + '/') || other.startsWith(p + '/'))) throw Error('出力先が衝突します');
    seen.add(p);
  }
  for (const {name, dest: outputPath, bundled} of items) {
    const entry = tree.get(name);
    const useBundledLicense = bundled && name === 'LICENSE';
    const useBundledWorkflow = bundled && name === workflow;
    if (!useBundledLicense && !useBundledWorkflow && (!entry || entry.type !== 'blob' || !['100644', '100755'].includes(entry.mode))) throw Error('指定パスが通常ファイルではありません: ' + name);
    const mode = !useBundledLicense && !useBundledWorkflow && entry.mode === '100755' ? 0o755 : 0o644;
    let data = useBundledLicense
      ? fs.readFileSync(path.join(path.dirname(defaultPolicy), '../deploy/public/LICENSE'))
      : useBundledWorkflow
        ? fs.readFileSync(path.join(path.dirname(defaultPolicy), '../deploy/public', ci + '-verify.yml'))
        : git('cat-file', 'blob', entry.id);
    const replacements = bundled ? [] : definition.replacements.filter(r => r.path === name);
    if (replacements.length) {
      let content = new TextDecoder('utf-8', {fatal: true}).decode(data);
      for (const r of replacements) { if (!content.includes(r.from)) throw Error('置換元がありません: ' + name); content = content.split(r.from).join(r.to); }
      data = Buffer.from(content);
    }
    const content = data.toString('utf8');
    const knownSecrets = [
      /-----BEGIN (?:[A-Z ]*PRIVATE KEY|OPENSSH PRIVATE KEY)-----/,
      /\b(?:AKIA|ASIA)[A-Z0-9]{16}\b/,
      /\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b/,
      /\bgithub_pat_[A-Za-z0-9_]{20,}\b/,
      /\bxox[baprs]-[A-Za-z0-9-]{10,}\b/,
    ];
    if (knownSecrets.some(pattern => pattern.test(content)) || definition.reject.some(s => data.includes(Buffer.from(s)))) throw Error('公開前検査に失敗: ' + name);
    const dest = path.join(temp, outputPath);
    fs.mkdirSync(path.dirname(dest), {recursive: true});
    fs.writeFileSync(dest, data, {mode, flag: 'wx'});
    fs.chmodSync(dest, mode);
  }
  const operationFile = 'host-ssh.json';
  const runnerFile = 'deploy/nas-host/install-github-actions-runner.sh';
  const operationName = 'install-github-actions-runner';
  if (paths.includes(operationFile) && paths.includes(runnerFile)) {
    let operation;
    try { operation = JSON.parse(fs.readFileSync(path.join(temp, outputName(operationFile)), 'utf8')).operations?.[operationName]; }
    catch { /* Report the same actionable error for malformed definitions. */ }
    const hashes = typeof operation === 'string' ? [...operation.matchAll(/(?<![0-9a-fA-F])[0-9a-fA-F]{64}(?![0-9a-fA-F])/g)].map(m => m[0]) : [];
    const actual = createHash('sha256').update(fs.readFileSync(path.join(temp, outputName(runnerFile)))).digest('hex');
    if (hashes.length !== 1 || hashes[0] !== actual) throw Error(operationName + ': ' + operationFile + ' と ' + runnerFile + ' の固定 SHA-256 が一意でないか一致しません');
  }
  fs.chmodSync(temp, 0o755);
  let treeHash;
  if (registered) {
    const verifierScript = path.join(path.dirname(defaultPolicy), 'public-source-verifier.mts');
    treeHash = execFileSync(process.execPath, [verifierScript, temp, verifier, image], {encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore']}).trim();
  }
  // GNU mv -T -n avoids replacing an existing destination, including an empty directory.
  execFileSync('mv', ['-T', '-n', '--', temp, output], {stdio: 'pipe'});
  if (fs.existsSync(temp)) throw Error('出力先が既に存在します');
  temp = undefined;
  if (registered) {
    console.log('input-sha: ' + commit);
    console.log('config-sha256: ' + configId);
    console.log('tree-sha256: ' + treeHash);
    console.log('公開前検査: 成功');
  } else {
    for (const name of outputPaths) console.log(name);
    console.log('公開前検査: 成功 (' + outputPaths.length + ' files, ' + commit + ')');
  }
} catch (error) {
  console.error('公開ソース生成失敗: ' + (registered ? '登録・生成・検査を完了できません' : error.message));
  process.exitCode = 1;
} finally {
  if (temp) fs.rmSync(temp, {recursive: true, force: true});
}
NODE
