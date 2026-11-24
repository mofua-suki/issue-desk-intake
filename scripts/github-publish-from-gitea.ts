import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const usage =
  '使い方: node scripts/github-publish-from-gitea.ts --source <Gitea参照元> --branch <統合枝> --source-sha <SHA> --repository <owner/repo> --base-branch <既定枝> --base-sha <SHA>';
try {
  const options: Record<string, string> = {};
  const keys = [
    '--source',
    '--branch',
    '--source-sha',
    '--repository',
    '--base-branch',
    '--base-sha',
    '--confirm',
  ];
  const args = process.argv.slice(2);
  for (let i = 0; i < args.length; i += 2) {
    const key = args[i],
      value = args[i + 1];
    if (!keys.includes(key) || !value || value.startsWith('-') || options[key])
      throw new Error(usage);
    options[key] = value;
  }
  if (
    keys.filter((key) => key !== '--confirm').some((key) => !options[key]) ||
    (options['--confirm'] && !/^[a-f0-9]{64}$/.test(options['--confirm'])) ||
    !/^[a-zA-Z0-9_.-]+\/[a-zA-Z0-9_.-]+$/.test(options['--repository']) ||
    ![options['--source-sha'], options['--base-sha']].every((s) =>
      /^[a-f0-9]{40}$/.test(s),
    )
  )
    throw new Error(usage);
  const env = {
    ...process.env,
    GIT_TERMINAL_PROMPT: '0',
    GCM_INTERACTIVE: 'Never',
  };
  const api = spawnSync(
    'gh',
    ['api', '--hostname', 'github.com', `repos/${options['--repository']}`],
    {
      encoding: 'utf8',
      env,
    },
  );
  if (api.status !== 0) throw new Error('公開先を読み取れません');
  let repo: { full_name?: string; default_branch?: string; clone_url?: string };
  try {
    repo = JSON.parse(api.stdout);
  } catch {
    throw new Error('公開先を照合できません');
  }
  if (
    repo.full_name?.toLowerCase() !== options['--repository'].toLowerCase() ||
    repo.default_branch !== options['--base-branch'] ||
    typeof repo.clone_url !== 'string' ||
    repo.clone_url.toLowerCase() !==
      `https://github.com/${options['--repository']}.git`.toLowerCase()
  )
    throw new Error('公開先の参照が一致しません');
  const dir = mkdtempSync(join(tmpdir(), 'publish-preview-'));
  try {
    const run = (argv: string[]) =>
      spawnSync('git', argv, {
        cwd: dir,
        encoding: 'utf8',
        env,
        maxBuffer: 16 * 1024 * 1024,
      });
    const must = (argv: string[], reason: string) => {
      const r = run(argv);
      if (r.status !== 0) throw new Error(reason);
      return r.stdout.trim();
    };
    const sourceRef = `refs/heads/${options['--branch']}`,
      baseRef = `refs/heads/${options['--base-branch']}`;
    for (const ref of [sourceRef, baseRef])
      must(['check-ref-format', ref], '枝の参照が不正です');
    must(['init', '--quiet'], '隔離領域を作成できません');
    const read = (remote: string, ref: string) => {
      const value = must(
        ['ls-remote', '--heads', '--', remote, ref],
        '参照を読み取れません',
      );
      const match = value.match(/^([a-f0-9]{40})\s+(refs\/heads\/[^\s]+)$/);
      if (!match || match[2] !== ref) throw new Error('参照を照合できません');
      return match[1];
    };
    const source = options['--source'],
      destination = repo.clone_url;
    const sourceHead = read(source, sourceRef),
      baseHead = read(destination, baseRef);
    if (baseHead !== options['--base-sha'])
      throw new Error('公開先の基点SHAが一致しません');
    for (const [remote, ref, local, expected] of [
      [source, sourceRef, 'refs/preview/source', sourceHead],
      [destination, baseRef, 'refs/preview/base', baseHead],
    ]) {
      must(
        ['fetch', '--quiet', '--no-tags', '--', remote, `+${ref}:${local}`],
        '参照を取得できません',
      );
      if (
        must(['rev-parse', local], '取得した参照を照合できません') !== expected
      )
        throw new Error('取得した参照のSHAが一致しません');
    }
    const sha = options['--source-sha'];
    if (run(['merge-base', '--is-ancestor', sha, sourceHead]).status !== 0)
      throw new Error('統合元の指定SHAが参照の履歴と一致しません');
    const common = run(['merge-base', baseHead, sha]);
    if (common.status === 1) throw new Error('無関係な履歴です');
    if (common.status !== 0 || !common.stdout.trim())
      throw new Error('共通履歴を照合できません');
    const merged = run(['merge-tree', '--write-tree', baseHead, sha]);
    if (merged.status === 1) throw new Error('競合があります');
    if (merged.status !== 0 || !/^[a-f0-9]{40}$/.test(merged.stdout.trim()))
      throw new Error('候補を計算できません');
    const tree = merged.stdout.trim();
    const candidate = spawnSync(
      'git',
      [
        '-c',
        'user.name=Preview',
        '-c',
        'user.email=preview@invalid',
        'commit-tree',
        tree,
        '-p',
        baseHead,
        '-m',
        'Preview',
      ],
      {
        cwd: dir,
        encoding: 'utf8',
        env: {
          ...env,
          GIT_AUTHOR_NAME: 'Preview',
          GIT_AUTHOR_EMAIL: 'preview@invalid',
          GIT_COMMITTER_NAME: 'Preview',
          GIT_COMMITTER_EMAIL: 'preview@invalid',
          GIT_AUTHOR_DATE: '2000-01-01T00:00:00Z',
          GIT_COMMITTER_DATE: '2000-01-01T00:00:00Z',
        },
      },
    );
    if (
      candidate.status !== 0 ||
      !/^[a-f0-9]{40}$/.test(candidate.stdout.trim())
    )
      throw new Error('候補を確定できません');
    const pathsResult = run([
      'diff',
      '--name-only',
      '-z',
      baseHead,
      tree,
      '--',
    ]);
    if (pathsResult.status !== 0) throw new Error('変更パスを読み取れません');
    const paths = pathsResult.stdout.split('\0').filter(Boolean);
    if (!paths.length) throw new Error('差分がありません');
    if (
      read(source, sourceRef) !== sourceHead ||
      read(destination, baseRef) !== baseHead
    )
      throw new Error('参照のSHAが照合中に変わりました');
    const proof = createHash('sha256')
      .update(
        JSON.stringify({
          repository: repo.full_name!.toLowerCase(),
          baseBranch: options['--base-branch'],
          baseHead,
          source: options['--source'],
          sourceBranch: options['--branch'],
          sha,
          tree,
          paths,
        }),
      )
      .digest('hex');
    const branch = `intake-publish/${proof}`;
    process.stdout.write(
      `対象: ${repo.full_name}\n基点: ${options['--base-branch']} ${baseHead}\n統合元SHA: ${sha}\n候補ツリー指紋: ${tree}\n変更パス: ${JSON.stringify(paths)}\n確認値: ${proof}\n`,
    );
    if (options['--confirm']) {
      if (options['--confirm'] !== proof)
        throw new Error('確認値が一致しません');
      const branchRef = `refs/heads/${branch}`;
      if (branchRef === baseRef) throw new Error('公開先の枝が不正です');
      const existingResult = run([
        'ls-remote',
        '--heads',
        '--',
        destination,
        branchRef,
      ]);
      if (existingResult.status !== 0)
        throw new Error('公開枝を読み取れません');
      const value = existingResult.stdout.trim();
      let match = value
        ? value.match(/^([a-f0-9]{40})\s+(refs\/heads\/[^\s]+)$/)
        : null;
      if (value && (!match || match[2] !== branchRef))
        throw new Error('公開枝を照合できません');
      let head = candidate.stdout.trim();
      if (match) {
        must(
          [
            'fetch',
            '--quiet',
            '--no-tags',
            '--',
            destination,
            `+${branchRef}:refs/preview/published`,
          ],
          '公開枝を取得できません',
        );
        head = must(
          ['rev-parse', 'refs/preview/published'],
          '公開枝を照合できません',
        );
        if (
          head !== match[1] ||
          must(['rev-parse', `${head}^{tree}`], '公開枝を照合できません') !==
            tree ||
          must(
            ['rev-list', '--parents', '-n', '1', head],
            '公開枝を照合できません',
          ) !== `${head} ${baseHead}`
        )
          throw new Error('既存の公開枝が候補と一致しません');
      }
      type Pull = {
        html_url?: string;
        state?: string;
        merged_at?: string | null;
        head?: { ref?: string; sha?: string; repo?: { full_name?: string } };
        base?: { ref?: string; sha?: string; repo?: { full_name?: string } };
      };
      const pulls = (): Pull[] => {
        const response = spawnSync(
          'gh',
          [
            'api',
            '--hostname',
            'github.com',
            `repos/${options['--repository']}/pulls?state=all&head=${options['--repository'].split('/')[0]}:${branch}&per_page=100`,
          ],
          { encoding: 'utf8', env },
        );
        if (response.status !== 0) throw new Error('PRを照合できません');
        let result: unknown;
        try {
          result = JSON.parse(response.stdout);
        } catch {
          throw new Error('PRを照合できません');
        }
        if (!Array.isArray(result) || result.length >= 100)
          throw new Error('PRを照合できません');
        return result;
      };
      const verifyPulls = (list: Pull[]) => {
        if (list.length > 1) throw new Error('複数のPRが存在します');
        if (!list.length) return '';
        const pr = list[0];
        if (pr.state === 'closed')
          throw new Error(
            pr.merged_at
              ? '既存のPRはマージ済みです。GitHubで状態を確認してください'
              : '既存のPRは閉じられています。GitHubで状態を確認してください',
          );
        if (
          !match ||
          pr.state !== 'open' ||
          pr.head?.ref !== branch ||
          pr.head.sha !== head ||
          pr.head.repo?.full_name?.toLowerCase() !==
            repo.full_name!.toLowerCase() ||
          pr.base?.ref !== options['--base-branch'] ||
          pr.base.sha !== baseHead ||
          pr.base.repo?.full_name?.toLowerCase() !==
            repo.full_name!.toLowerCase() ||
          typeof pr.html_url !== 'string' ||
          !/^https:\/\/github\.com\/[^/]+\/[^/]+\/pull\/[0-9]+$/i.test(
            pr.html_url,
          ) ||
          !pr.html_url
            .toLowerCase()
            .startsWith(
              `https://github.com/${options['--repository'].toLowerCase()}/pull/`,
            )
        )
          throw new Error('既存のPRが候補と一致しません');
        return pr.html_url;
      };
      let url = verifyPulls(pulls());
      if (!match) {
        if (
          read(source, sourceRef) !== sourceHead ||
          read(destination, baseRef) !== baseHead
        )
          throw new Error('参照のSHAが送信前に変わりました');
        if (
          run([
            'push',
            '--no-follow-tags',
            '--recurse-submodules=no',
            '--',
            destination,
            `${head}:${branchRef}`,
          ]).status !== 0
        )
          throw new Error(
            '公開枝の送信結果を確認できません。再実行して照合してください',
          );
        if (read(destination, branchRef) !== head)
          throw new Error('公開枝の先頭が一致しません');
        match = [head, head, branchRef] as RegExpMatchArray;
      }
      let createdSuccessfully = false;
      if (!url) {
        if (
          read(destination, branchRef) !== head ||
          read(destination, baseRef) !== baseHead ||
          read(source, sourceRef) !== sourceHead
        )
          throw new Error('参照のSHAがPR作成前に変わりました');
        const created = spawnSync(
          'gh',
          [
            'pr',
            'create',
            '--repo',
            `github.com/${options['--repository']}`,
            '--base',
            options['--base-branch'],
            '--head',
            branch,
            '--title',
            'Gitea integration changes',
            '--body',
            `Source SHA: ${sha}\nCandidate tree: ${tree}`,
          ],
          { encoding: 'utf8', env },
        );
        if (created.status !== 0)
          throw new Error(
            'PR作成結果を確認できません。再実行して照合してください',
          );
        createdSuccessfully = true;
      }
      try {
        if (createdSuccessfully) {
          url = verifyPulls(pulls());
          if (!url) throw new Error('作成したPRが見つかりません');
        }
        if (
          read(destination, branchRef) !== head ||
          read(destination, baseRef) !== baseHead
        )
          throw new Error('公開先の参照が変わりました');
      } catch (error) {
        if (createdSuccessfully)
          throw new Error(
            `PR作成は成功しましたが、作成後の照合に失敗しました。GitHubで状態を確認し、同じ引数で再照合してください: ${(error as Error).message}`,
          );
        throw error;
      }
      process.stdout.write(`PR: ${url}\n確認対象SHA: ${sha}\n`);
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
} catch (error) {
  process.stderr.write(`${(error as Error).message}\n`);
  process.exitCode = 1;
}
