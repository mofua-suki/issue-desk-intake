// Report a fresh fetch and commits present only locally; never infer freshness from a cached tracking ref.
import { spawnSync } from 'node:child_process';
import { pathToFileURL } from 'node:url';

const usage =
  '使い方: node scripts/git-sync-status.ts [--cwd <作業ディレクトリ>] [--remote <origin>] [--branch <ブランチ>]';
type Result = {
  status: number | null;
  stdout: string;
  stderr: string;
  error?: Error;
};
type Run = (args: string[], cwd: string) => Result;

function git(args: string[], cwd: string, spawn = spawnSync): Result {
  return spawn('git', args, {
    cwd,
    encoding: 'utf8',
    timeout: 30_000,
    killSignal: 'SIGKILL',
    maxBuffer: 4 * 1024 * 1024,
    stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, GIT_TERMINAL_PROMPT: '0', GCM_INTERACTIVE: 'Never' },
  });
}

// Omit whole URLs, including their userinfo and query, rather than exposing an embedded credential.
function safe(text: string, failed: boolean): string {
  const redacted = text
    .replace(/[a-z][a-z0-9+.-]*:\/\/\S+/gi, '[URL省略]')
    // eslint-disable-next-line no-control-regex -- 入力や表示から制御文字を除外する安全性検証。
    .replace(/[\x00-\x08\x0b-\x1f\x7f]/g, '');
  return failed
    ? redacted.replace(/最新|追いついている/g, '[表現省略]')
    : redacted;
}

function syncStatus(
  argv: string[],
  run: Run = git,
): { code: number; output: string } {
  let cwd = process.cwd();
  let remote = 'origin';
  let branch = '';
  let ref = '未指定';
  let fetched: Result | undefined;
  const lines: string[] = [];
  const finish = (code: number) => ({
    code,
    output: safe(lines.join('\n') + '\n', code !== 0),
  });
  try {
    for (let i = 0; i < argv.length; i++) {
      const key = argv[i];
      const value = argv[++i];
      if (
        !['--cwd', '--remote', '--branch'].includes(key) ||
        !value ||
        value.startsWith('--')
      )
        throw new Error(usage);
      if (key === '--cwd') cwd = value;
      if (key === '--remote') remote = value;
      if (key === '--branch') branch = value;
    }
    if (!/^[a-zA-Z0-9][a-zA-Z0-9._-]*$/.test(remote))
      throw new Error('remote は登録済みの名前を指定してください');
    if (run(['rev-parse', '--git-dir'], cwd).status !== 0)
      throw new Error('Gitリポジトリを確認できません');
    if (!branch) {
      const head = run(
        ['symbolic-ref', '--quiet', `refs/remotes/${remote}/HEAD`],
        cwd,
      );
      const prefix = `refs/remotes/${remote}/`;
      branch =
        head.status === 0 && head.stdout.trim().startsWith(prefix)
          ? head.stdout.trim().slice(prefix.length)
          : 'main';
    }
    ref = `${remote}/${branch}`;
    if (
      branch.startsWith('-') ||
      run(['check-ref-format', `refs/heads/${branch}`], cwd).status !== 0
    )
      throw new Error('ブランチ名が不正です');
    // An explicit refspec ensures success really refreshes the reference we compare, regardless of remote.fetch.
    fetched = run(
      [
        'fetch',
        '--no-tags',
        '--',
        remote,
        `+refs/heads/${branch}:refs/remotes/${ref}`,
      ],
      cwd,
    );
    if (fetched.status === 0) lines.push('取り込み: 成功');
    else {
      const reason = (
        fetched.error?.message ||
        fetched.stderr ||
        'git fetch に失敗'
      ).split(/\r?\n/)[0];
      lines.push(`取り込み: 未確認（失敗）: ${reason}`);
    }
    lines.push(`比較参照: ${ref}`);
    const range = `refs/remotes/${ref}..HEAD`;
    const count = run(['rev-list', '--count', range, '--'], cwd);
    if (count.status !== 0 || !/^\d+$/.test(count.stdout.trim()))
      throw new Error('比較参照またはHEADがありません');
    const commits = run(['log', '-20', '--format=%h %s', range, '--'], cwd);
    if (commits.status !== 0)
      throw new Error('手元だけのコミットを読み取れません');
    const total = Number(count.stdout.trim());
    lines.push(`手元だけのコミット (${ref}..HEAD): ${total}件`);
    if (commits.stdout.trim()) lines.push(commits.stdout.trimEnd());
    if (total > 20) lines.push(`ほか ${total - 20}件（一覧は20件まで）`);
    return finish(fetched.status === 0 ? 0 : 1);
  } catch (error) {
    if (!fetched) lines.push('取り込み: 未確認（未実施）', `比較参照: ${ref}`);
    lines.push(
      `使い方の誤り: ${(error as Error).message}`,
      '手元だけのコミット: 未確認',
    );
    return finish(2);
  }
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  const result = syncStatus(process.argv.slice(2));
  process.stdout.write(result.output);
  process.exitCode = result.code;
}
