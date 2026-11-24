import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const usage =
  '使い方: [--apply] --source <Gitea参照元> --branch <枝> --source-sha <公開済みSHA> --destination-sha <現在のSHA> --repository <owner/repo> --base-branch <既定枝> --base-sha <現在のSHA> --pr <番号> --candidate-tree <公開候補木>';
try {
  const keys = [
    '--source',
    '--branch',
    '--source-sha',
    '--destination-sha',
    '--repository',
    '--base-branch',
    '--base-sha',
    '--pr',
    '--candidate-tree',
  ];
  const o: Record<string, string> = {};
  const args = process.argv.slice(2);
  const apply = args[0] === '--apply';
  if (apply) args.shift();
  for (let i = 0; i < args.length; i += 2) {
    if (
      !keys.includes(args[i]) ||
      !args[i + 1] ||
      args[i + 1].startsWith('-') ||
      o[args[i]]
    )
      throw new Error(usage);
    o[args[i]] = args[i + 1];
  }
  if (
    keys.some((k) => !o[k]) ||
    !/^[\w.-]+\/[\w.-]+$/.test(o['--repository']) ||
    !/^[1-9][0-9]*$/.test(o['--pr']) ||
    ![
      '--source-sha',
      '--destination-sha',
      '--base-sha',
      '--candidate-tree',
    ].every((k) => /^[a-f0-9]{40}$/.test(o[k]))
  )
    throw new Error(usage);
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(o['--source'])) {
    let parsed: URL;
    try {
      parsed = new URL(o['--source']);
    } catch {
      throw new Error('参照元URLが不正です');
    }
    if (parsed.username || parsed.password)
      throw new Error('参照元URLに認証情報を含められません');
  }
  const env = {
    ...process.env,
    GIT_TERMINAL_PROMPT: '0',
    GCM_INTERACTIVE: 'Never',
  };
  const api = (path: string) => {
    const r = spawnSync('gh', ['api', '--hostname', 'github.com', path], {
      encoding: 'utf8',
      env,
    });
    if (r.status !== 0) throw new Error('GitHub情報を取得できません');
    try {
      return JSON.parse(r.stdout);
    } catch {
      throw new Error('GitHub情報を照合できません');
    }
  };
  const name = o['--repository'];
  const repo = api(`repos/${name}`);
  const url = `https://github.com/${name}.git`;
  if (
    typeof repo.full_name !== 'string' ||
    repo.full_name.toLowerCase() !== name.toLowerCase() ||
    repo.default_branch !== o['--base-branch'] ||
    typeof repo.clone_url !== 'string' ||
    repo.clone_url.toLowerCase() !== url.toLowerCase()
  )
    throw new Error('公開先が一致しません');
  const pr = api(`repos/${name}/pulls/${o['--pr']}`);
  const sha = (s: unknown) => typeof s === 'string' && /^[a-f0-9]{40}$/.test(s);
  if (
    pr.number !== Number(o['--pr']) ||
    pr.state !== 'closed' ||
    !pr.merged_at ||
    !sha(pr.merge_commit_sha) ||
    !sha(pr.base?.sha) ||
    !sha(pr.head?.sha) ||
    pr.base?.ref !== o['--base-branch'] ||
    pr.base?.repo?.full_name?.toLowerCase() !== name.toLowerCase() ||
    pr.head?.repo?.full_name?.toLowerCase() !== name.toLowerCase() ||
    typeof pr.head?.ref !== 'string' ||
    typeof pr.body !== 'string' ||
    pr.body !==
      `Source SHA: ${o['--source-sha']}\nCandidate tree: ${o['--candidate-tree']}`
  )
    throw new Error('PRが一致しないか未マージです');
  const dir = mkdtempSync(join(tmpdir(), 'reconcile-preview-'));
  try {
    const run = (a: string[]) =>
      spawnSync('git', a, {
        cwd: dir,
        encoding: 'utf8',
        env,
        maxBuffer: 16 * 1024 * 1024,
      });
    const must = (a: string[], reason: string) => {
      const r = run(a);
      if (r.status !== 0) throw new Error(reason);
      return r.stdout.trim();
    };
    const sourceRef = `refs/heads/${o['--branch']}`,
      baseRef = `refs/heads/${o['--base-branch']}`,
      headRef = `refs/pull/${o['--pr']}/head`;
    must(['check-ref-format', `refs/heads/${pr.head.ref}`], '枝が不正です');
    for (const ref of [sourceRef, baseRef, headRef])
      must(['check-ref-format', ref], '枝が不正です');
    must(['init', '--quiet'], '隔離領域を作成できません');
    const read = (remote: string, ref: string) => {
      const value = must(
        ['ls-remote', '--', remote, ref],
        '参照を読み取れません',
      );
      const m = value.match(/^([a-f0-9]{40})\s+(refs\/[^\s]+)$/);
      if (!m || m[2] !== ref) throw new Error('参照を照合できません');
      return m[1];
    };
    const source = o['--source'],
      sourceHead = read(source, sourceRef),
      baseHead = read(url, baseRef),
      publishedHead = read(url, headRef);
    if (
      sourceHead !== o['--destination-sha'] ||
      baseHead !== o['--base-sha'] ||
      publishedHead !== pr.head.sha
    )
      throw new Error('参照のSHAが一致しません');
    for (const [remote, ref, local, expected] of [
      [source, sourceRef, 'refs/reconcile/source', sourceHead],
      [url, baseRef, 'refs/reconcile/base', baseHead],
      [url, headRef, 'refs/reconcile/published', publishedHead],
    ]) {
      must(
        ['fetch', '--quiet', '--no-tags', '--', remote, `+${ref}:${local}`],
        '参照を取得できません',
      );
      if (
        must(['rev-parse', local], '取得した参照を照合できません') !== expected
      )
        throw new Error('取得した参照が一致しません');
    }
    const ancestor = (a: string, b: string) =>
      run(['merge-base', '--is-ancestor', a, b]).status === 0;
    if (
      !ancestor(o['--source-sha'], sourceHead) ||
      !ancestor(pr.merge_commit_sha, baseHead)
    )
      throw new Error('指定SHAまたはマージが履歴にありません');
    if (
      must(
        ['rev-list', '--parents', '-n', '1', publishedHead],
        'PR先頭を取得できません',
      ) !== `${publishedHead} ${pr.base.sha}` ||
      must(
        ['rev-parse', `${publishedHead}^{tree}`],
        'PR先頭を照合できません',
      ) !== o['--candidate-tree'] ||
      !sha(
        must(['rev-parse', `${pr.base.sha}^{tree}`], 'PR基点を取得できません'),
      )
    )
      throw new Error('PRの木または親が一致しません');
    const pathsResult = run([
      'diff',
      '--name-only',
      '-z',
      pr.base.sha,
      o['--candidate-tree'],
      '--',
    ]);
    if (pathsResult.status !== 0) throw new Error('公開差分を取得できません');
    const paths = pathsResult.stdout.split('\0').filter(Boolean);
    if (!paths.length) throw new Error('公開差分がありません');
    const proof = createHash('sha256')
      .update(
        JSON.stringify({
          repository: repo.full_name.toLowerCase(),
          baseBranch: o['--base-branch'],
          baseHead: pr.base.sha,
          source,
          sourceBranch: o['--branch'],
          sha: o['--source-sha'],
          tree: o['--candidate-tree'],
          paths,
        }),
      )
      .digest('hex');
    if (pr.head.ref !== `intake-publish/${proof}`)
      throw new Error('公開識別値が一致しません');
    // GitHubの現在の木を公開済みGiteaコミットの子に載せ、明示した基点から三者マージする。
    const virtual = spawnSync(
      'git',
      [
        '-c',
        'user.name=Preview',
        '-c',
        'user.email=preview@invalid',
        'commit-tree',
        `${baseHead}^{tree}`,
        '-p',
        o['--source-sha'],
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
        },
      },
    );
    if (virtual.status !== 0 || !sha(virtual.stdout.trim()))
      throw new Error('候補の基点を作成できません');
    const merged = run([
      'merge-tree',
      '--write-tree',
      virtual.stdout.trim(),
      sourceHead,
    ]);
    if (merged.status === 1) throw new Error('競合があります');
    if (merged.status !== 0 || !sha(merged.stdout.trim()))
      throw new Error('候補を計算できません');
    if (
      read(source, sourceRef) !== sourceHead ||
      read(url, baseRef) !== baseHead ||
      read(url, headRef) !== publishedHead
    )
      throw new Error('参照が照合中に変わりました');
    let result = sourceHead;
    if (
      apply &&
      !(
        ancestor(baseHead, sourceHead) &&
        must(
          ['rev-parse', `${sourceHead}^{tree}`],
          '宛先の木を確認できません',
        ) === merged.stdout.trim()
      )
    ) {
      result = must(
        [
          '-c',
          'user.name=Reconcile',
          '-c',
          'user.email=reconcile@invalid',
          'commit-tree',
          merged.stdout.trim(),
          '-p',
          sourceHead,
          '-p',
          baseHead,
          '-m',
          'Reconcile merged GitHub changes',
        ],
        '再取り込みコミットを作成できません',
      );
      if (
        read(source, sourceRef) !== sourceHead ||
        read(url, baseRef) !== baseHead ||
        read(url, headRef) !== publishedHead
      )
        throw new Error('参照が更新前に変わりました');
      must(
        [
          'push',
          '--no-follow-tags',
          '--recurse-submodules=no',
          '--',
          source,
          `${result}:${sourceRef}`,
        ],
        '宛先へのpushに失敗しました。両側の参照を確認してください',
      );
      if (
        read(source, sourceRef) !== result ||
        read(url, baseRef) !== baseHead ||
        read(url, headRef) !== publishedHead
      )
        throw new Error(
          '更新後に参照が変わりました。両側の参照を確認してください',
        );
    }
    if (apply) process.stdout.write(`再取り込み結果: ${result}\n`);
    process.stdout.write(
      `対象: ${repo.full_name}\nGitHub: ${baseRef} ${baseHead}\nGitea: ${source} ${sourceRef} ${sourceHead}\n公開済みSHA: ${o['--source-sha']}\nPR: ${o['--pr']}\n公開候補木: ${o['--candidate-tree']}\n再取り込み候補木: ${merged.stdout.trim()}\n`,
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
} catch (error) {
  process.stderr.write(`${(error as Error).message}\n`);
  process.exitCode = 1;
}
