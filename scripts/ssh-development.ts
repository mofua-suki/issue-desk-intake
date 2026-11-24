// 呼出元で依頼を固定し、承認付きhost-sshで実行して構造化結果を保存する。
import fs from 'node:fs';
import path from 'node:path';
import { randomUUID, createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { marker, validateRequest } from './ssh-development-runner.ts';

export function readResult(output, request) {
  const lines = output.split(/\r?\n/).filter((line) => line.startsWith(marker));
  if (lines.length !== 1)
    throw Error('別PCの実装結果が未取得または重複しています。');
  const result = JSON.parse(
    Buffer.from(lines[0].slice(marker.length), 'base64').toString(),
  );
  if (
    result.version !== 1 ||
    result.id !== request.id ||
    result.base !== request.base ||
    result.repository !== request.repository ||
    !['verified', 'failed'].includes(result.status) ||
    typeof result.patch !== 'string' ||
    !Array.isArray(result.stages)
  )
    throw Error('実装結果の対象が一致しません。');
  const expected = request.repository.endsWith('/issue-desk-worker')
    ? ['implementation', 'go-test', 'go-vet', 'gofmt']
    : ['implementation', 'verify'];
  if (
    result.status === 'verified' &&
    (result.stages.length !== expected.length ||
      expected.some(
        (name, i) =>
          result.stages[i].name !== name || result.stages[i].exitCode !== 0,
      ))
  )
    throw Error('実装結果の固定検証が不足しています。');
  return result;
}

export function main(argv) {
  const flags: Record<string, string> = {};
  for (let i = 0; i < argv.length; i += 2) {
    if (
      !['--host', '--cwd', '--repo', '--file', '--out'].includes(argv[i]) ||
      !argv[i + 1] ||
      flags[argv[i]]
    )
      throw Error(
        '使い方: node scripts/ssh-development.ts --host <PC> --cwd <clone> --repo <owner/repo> --file <依頼文> --out <結果フォルダー>',
      );
    flags[argv[i]] = argv[i + 1];
  }
  if (
    Object.keys(flags).length !== 5 ||
    !/^[a-z0-9][a-z0-9-]*$/.test(flags['--host'])
  )
    throw Error('対象PCと引数を指定してください。');
  const cwd = path.resolve(flags['--cwd']);
  const out = path.resolve(flags['--out']);
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
  const policy = JSON.parse(
    fs.readFileSync(path.join(root, 'host-ssh.json'), 'utf8'),
  );
  if (!Object.hasOwn(policy.hosts, flags['--host']))
    throw Error('登録されていないPCです。');
  const git = (args) => {
    const result = spawnSync('git', args, {
      cwd,
      encoding: 'utf8',
      maxBuffer: 50 * 1024 * 1024,
    });
    if (result.error || result.status !== 0)
      throw Error(
        result.stderr || result.error?.message || 'Gitの確認に失敗しました。',
      );
    return result.stdout.trim();
  };
  if (git(['status', '--porcelain']))
    throw Error('送信元のcloneに未コミットの変更があります。');
  const base = git(['rev-parse', 'HEAD']);
  // originの対応を確認し、別リポジトリを指定した依頼の取り違えを拒否する。
  const remote = git(['remote', 'get-url', 'origin']);
  if (
    !remote.endsWith(':' + flags['--repo'] + '.git') &&
    !remote.endsWith('/' + flags['--repo'] + '.git')
  )
    throw Error('送信元cloneと対象リポジトリが一致しません。');
  const prompt = fs.readFileSync(flags['--file'], 'utf8');
  fs.mkdirSync(out, { recursive: true, mode: 0o700 });
  if (fs.readdirSync(out).length)
    throw Error('結果フォルダーは空の場所を指定してください。');
  const bundleFile = path.join(out, 'source.bundle');
  const requestFile = path.join(out, 'request.json');
  try {
    git(['bundle', 'create', bundleFile, 'HEAD']);
    const request = validateRequest({
      version: 1,
      id: randomUUID(),
      repository: flags['--repo'],
      base,
      prompt,
      bundle: fs.readFileSync(bundleFile).toString('base64'),
    });
    const digest = createHash('sha256')
      .update(JSON.stringify(request))
      .digest('hex');
    const helper = fs
      .readFileSync(path.join(root, 'scripts/ssh-development-host.sh'))
      .toString('base64');
    const runner = fs
      .readFileSync(path.join(root, 'scripts/ssh-development-runner.ts'))
      .toString('base64');
    fs.writeFileSync(
      requestFile,
      helper + '\n' + runner + '\n' + JSON.stringify(request),
      { mode: 0o600 },
    );
    fs.writeFileSync(
      path.join(out, 'request-info.json'),
      JSON.stringify(
        {
          version: 1,
          id: request.id,
          repository: request.repository,
          base,
          host: flags['--host'],
          sha256: digest,
        },
        null,
        2,
      ) + '\n',
      { mode: 0o600 },
    );
    // 依頼と基点を受付の承認記録に結び付ける。未承認なら既存経路が承認を待つ。
    const execution = spawnSync(
      'sh',
      [
        path.join(root, 'deploy/nas-host/host-ssh.sh'),
        '--host',
        flags['--host'],
        '--operation',
        'develop',
        '--reason',
        `${request.repository}の直接PR実装・固定検証`,
        '--evidence',
        `request=${request.id} base=${base} sha256=${digest}`,
        '--input',
        requestFile,
      ],
      {
        encoding: 'utf8',
        maxBuffer: 4 * 1024 * 1024,
        stdio: ['ignore', 'pipe', 'inherit'],
      },
    );
    let result;
    try {
      result = readResult(execution.stdout ?? '', request);
    } catch (error) {
      throw Error(
        `${error.message} 依頼ID=${request.id}。request-info.jsonで照合し、結果不明のまま再実行しないでください。`,
      );
    }
    fs.writeFileSync(
      path.join(out, 'result.json'),
      JSON.stringify(result, null, 2) + '\n',
      { mode: 0o600 },
    );
    fs.writeFileSync(
      path.join(out, 'change.patch'),
      Buffer.from(result.patch, 'base64'),
      { mode: 0o600 },
    );
    console.log(
      `対象PC: ${flags['--host']} 基点: ${base} 結果: ${result.status}\n結果: ${path.join(out, 'result.json')}\n差分: ${path.join(out, 'change.patch')}`,
    );
    return execution.status === 0 && result.status === 'verified' ? 0 : 1;
  } finally {
    fs.rmSync(bundleFile, { force: true });
    fs.rmSync(requestFile, { force: true });
  }
}
if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  try {
    process.exitCode = main(process.argv.slice(2));
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
