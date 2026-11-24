// SSHの固定操作 develop が起動する。接続元の資格情報や任意コマンドは受け取らない。
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import type { SpawnSyncOptionsWithStringEncoding } from 'node:child_process';
import { pathToFileURL } from 'node:url';

export const marker = 'ISSUE_DESK_DEVELOPMENT_RESULT=';
const repositories = [
  'mofua-suki/issue-desk-worker',
  'mofua-suki/issue-desk-intake',
];
type DevelopmentRequest = {
  version: number;
  id: string;
  repository: string;
  base: string;
  prompt: string;
  bundle: string;
};
type Stage = { name: string; exitCode: number };
type DevelopmentResult = {
  version: 1;
  id: string;
  repository: string;
  base: string;
  host: string;
  status: 'failed' | 'verified';
  stages: Stage[];
  patch: string;
  error: string;
};
// 呼び出しの実行部（spawnSync、テストでは差し替え）。cwd と input はいつも文字列で渡す。
type ExecOptions = SpawnSyncOptionsWithStringEncoding & {
  cwd: string;
  input?: string;
};
type Exec = (
  command: string,
  args: string[],
  options: ExecOptions,
) => { status: number | null; stdout?: string; stderr?: string; error?: Error };
type RunOptions = { cwd?: string; input?: string; timeout?: number };

export function validateRequest(value: any): DevelopmentRequest {
  if (
    !value ||
    value.version !== 1 ||
    !repositories.includes(value.repository) ||
    !/^[a-f0-9]{40}$/.test(value.base ?? '') ||
    !/^[a-f0-9-]{36}$/.test(value.id ?? '') ||
    typeof value.prompt !== 'string' ||
    !value.prompt.trim() ||
    value.prompt.length > 8000 ||
    typeof value.bundle !== 'string' ||
    !/^[A-Za-z0-9+/]+=*$/.test(value.bundle) ||
    value.bundle.length > 64 * 1024 * 1024
  )
    throw Error('実装依頼の形式・対象・大きさが不正です。');
  return value;
}

export function runDevelopment(
  request: DevelopmentRequest,
  {
    home = os.homedir(),
    exec = spawnSync as Exec,
  }: { home?: string; exec?: Exec } = {},
): DevelopmentResult {
  validateRequest(request);
  if (Number(process.versions.node.split('.')[0]) < 24)
    throw Error('Node 24以上が必要です。');
  // 呼出ごとに別clone。既存Worker、配置先、利用者の作業ツリーを使用しない。
  const root = path.join(home, '.issue-desk', 'ssh-development');
  fs.mkdirSync(root, { recursive: true, mode: 0o700 });
  const work = fs.mkdtempSync(path.join(root, 'run-'));
  const cwd = path.join(work, 'repo');
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    PATH: path.join(home, '.local/bin') + ':' + process.env.PATH,
  };
  delete env.GODEBUG;
  // 接続元のAPIキーへの切替や、ログインしていないときのAPI課金への切替をしない。
  delete env.OPENAI_API_KEY;
  delete env.CODEX_API_KEY;
  const result: DevelopmentResult = {
    version: 1,
    id: request.id,
    repository: request.repository,
    base: request.base,
    host: os.hostname(),
    status: 'failed',
    stages: [],
    patch: '',
    error: '',
  };
  const deadline = Date.now() + 3000000;
  const run = (
    command: string,
    args: string[],
    options: RunOptions = {},
  ): string => {
    const { timeout: requestedTimeout = 900000, ...rest } = options;
    const timeout = Math.min(requestedTimeout, deadline - Date.now());
    if (timeout <= 0) throw Error('実装・検証の全体期限50分に達しました。');
    const r = exec(
      'timeout',
      ['-k', '10', String(Math.ceil(timeout / 1000)), command, ...args],
      {
        cwd,
        env,
        encoding: 'utf8',
        maxBuffer: 16 * 1024 * 1024,
        timeout: timeout + 30000,
        killSignal: 'SIGKILL',
        ...rest,
      },
    );
    if (r.error || r.status !== 0)
      throw Error(
        `${command}: ${r.error?.message ?? `exit=${r.status}`}\n${r.stderr ?? ''}\n${r.stdout ?? ''}`,
      );
    return r.stdout ?? '';
  };
  try {
    run('git', ['--version'], { cwd: work });
    run('codex', ['--version'], { cwd: work });
    run('codex', ['login', 'status'], { cwd: work });
    run('go', ['version'], { cwd: work });
    if (request.repository.endsWith('/issue-desk-intake'))
      run('pnpm', ['--version'], { cwd: work });
    fs.writeFileSync(
      path.join(work, 'source.bundle'),
      Buffer.from(request.bundle, 'base64'),
      { mode: 0o600 },
    );
    run(
      'git',
      ['clone', '--no-checkout', '--', path.join(work, 'source.bundle'), cwd],
      { cwd: work },
    );
    run('git', ['checkout', '--detach', request.base]);
    if (run('git', ['rev-parse', 'HEAD']).trim() !== request.base)
      throw Error('基点SHAが一致しません。');
    run('git', ['remote', 'remove', 'origin']);
    fs.mkdirSync(path.join(cwd, '.tmp'));
    fs.appendFileSync(path.join(cwd, '.git', 'info', 'exclude'), '\n.tmp/\n');
    const prompt = `この依頼の実装だけを行ってください。最初にAGENTS.mdを読み従ってください。直接PRの実装は利用者が許可済みです。push・PR・マージ・起票は接続元が担当します。資格情報・権限設定を変更せず、sandboxの制限を迂回しないでください。変更後はコミットせず作業ツリーに残してください。\n\n${request.prompt}`;
    run(
      'codex',
      ['exec', '--sandbox', 'workspace-write', '--color', 'never', '-'],
      { input: prompt, timeout: 2400000 },
    );
    result.stages.push({ name: 'implementation', exitCode: 0 });
    // 新規ファイルも差分に含め、AGENTSの確認やテスト成功をAIの自己申告だけで判定しない。
    run('git', ['add', '-N', '--', '.']);
    const patch = run('git', [
      'diff',
      '--no-ext-diff',
      '--no-textconv',
      '--full-index',
      request.base,
      '--',
      '.',
    ]);
    if (
      Buffer.byteLength(patch) > 200000 ||
      patch.includes('GIT binary patch') ||
      /^Binary files /m.test(patch)
    )
      throw Error('差分は200000 bytes以内のテキストにしてください。');
    result.patch = Buffer.from(patch).toString('base64');
    const steps: [string, string, string[]][] = request.repository.endsWith(
      '/issue-desk-worker',
    )
      ? [
          ['go-test', 'go', ['test', './...']],
          ['go-vet', 'go', ['vet', './...']],
          ['gofmt', 'gofmt', ['-l', '.']],
        ]
      : [['verify', 'sh', ['scripts/verify.sh']]];
    for (const [name, command, args] of steps) {
      const output = run(command, args);
      if (name === 'gofmt' && output.trim())
        throw Error('gofmtが必要です。\n' + output);
      result.stages.push({ name, exitCode: 0 });
    }
    // 検証が生成・変更したものも含めて、検証後の差分を返す。
    run('git', ['add', '-N', '--', '.']);
    const finalPatch = run('git', [
      'diff',
      '--no-ext-diff',
      '--no-textconv',
      '--full-index',
      request.base,
      '--',
      '.',
    ]);
    if (finalPatch !== patch)
      throw Error(
        '検証中に差分が変わりました。差分を確認して再検証してください。',
      );
    result.status = 'verified';
  } catch (error) {
    result.error = String((error as Error).message)
      .replace(/.*(?:idk_|idh_|PRIVATE KEY|password|secret|token).*\n?/gi, '')
      .slice(-6000);
  } finally {
    fs.rmSync(work, { recursive: true, force: true });
  }
  return result;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  try {
    const input = fs.readFileSync(0);
    if (input.length > 65 * 1024 * 1024) throw Error('依頼が大きすぎます。');
    const result = runDevelopment(JSON.parse(input.toString('utf8')));
    console.log(
      marker + Buffer.from(JSON.stringify(result)).toString('base64'),
    );
    process.exitCode = result.status === 'verified' ? 0 : 1;
  } catch (error) {
    console.error((error as Error).message);
    process.exitCode = 1;
  }
}
