import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const usage =
  '使い方: node scripts/github-import-to-gitea.ts --source <参照元> --destination <宛先> --branch <ブランチ> --expected-destination <SHAまたはempty>';
const options: Record<string, string> = {};
try {
  const args = process.argv.slice(2);
  for (let i = 0; i < args.length; i += 2) {
    const key = args[i];
    const value = args[i + 1];
    if (
      ![
        '--source',
        '--destination',
        '--branch',
        '--expected-destination',
      ].includes(key) ||
      !value ||
      value.startsWith('--') ||
      options[key]
    )
      throw new Error(usage);
    options[key] = value;
  }
  const source = options['--source'];
  const destination = options['--destination'];
  const branch = options['--branch'];
  const expected = options['--expected-destination'];
  if (
    !source ||
    !destination ||
    !branch ||
    !expected ||
    source === destination ||
    source.startsWith('-') ||
    destination.startsWith('-') ||
    branch.startsWith('-') ||
    !/^(empty|[a-f0-9]{40}|[a-f0-9]{64})$/.test(expected)
  )
    throw new Error(usage);
  const dir = mkdtempSync(join(tmpdir(), 'github-import-'));
  try {
    const run = (args: string[]) =>
      spawnSync('git', args, {
        cwd: dir,
        encoding: 'utf8',
        env: {
          ...process.env,
          GIT_TERMINAL_PROMPT: '0',
          GCM_INTERACTIVE: 'Never',
        },
      });
    const must = (args: string[], reason: string) => {
      const result = run(args);
      if (result.status !== 0) throw new Error(reason);
      return result.stdout.trim();
    };
    const ref = `refs/heads/${branch}`;
    must(['check-ref-format', ref], 'ブランチ名が不正です');
    must(['init', '--quiet'], '一時リポジトリを作成できません');
    const read = (remote: string) => {
      const result = must(
        ['ls-remote', '--heads', '--', remote, ref],
        'リモートの対象枝を確認できません',
      );
      if (!result) return 'empty';
      const lines = result.split('\n');
      const match =
        lines.length === 1 &&
        lines[0].match(/^([a-f0-9]{40}|[a-f0-9]{64})\s+(.+)$/);
      if (!match || match[2] !== ref) throw new Error('対象枝が一致しません');
      return match[1];
    };
    const sourceSha = read(source);
    if (sourceSha === 'empty') throw new Error('参照元の対象枝がありません');
    const destSha = read(destination);
    if (destSha !== expected) throw new Error('宛先SHAが指定と一致しません');
    must(
      ['fetch', '--no-tags', '--', source, `+${ref}:refs/import/source`],
      '参照元の取得に失敗しました',
    );
    if (
      must(
        ['rev-parse', 'refs/import/source'],
        '取得した参照元を確認できません',
      ) !== sourceSha
    )
      throw new Error('取得中に参照元が変わりました');
    if (destSha !== 'empty' && destSha !== sourceSha) {
      must(
        [
          'fetch',
          '--no-tags',
          '--',
          destination,
          `+${ref}:refs/import/destination`,
        ],
        '宛先の取得に失敗しました',
      );
      if (
        must(
          ['rev-parse', 'refs/import/destination'],
          '取得した宛先を確認できません',
        ) !== destSha
      )
        throw new Error('取得中に宛先が変わりました');
      if (run(['merge-base', '--is-ancestor', destSha, sourceSha]).status !== 0)
        throw new Error('宛先にのみコミットがあるか履歴が分岐しています');
    }
    if (read(destination) !== destSha)
      throw new Error('更新前に宛先が変わりました');
    if (destSha !== sourceSha)
      must(
        ['push', '--no-follow-tags', '--', destination, `${sourceSha}:${ref}`],
        'pushが拒否されました。宛先を照合してください',
      );
    if (read(destination) !== sourceSha)
      throw new Error('更新後の宛先SHAが一致しません。宛先を照合してください');
    process.stdout.write(
      `対象枝: ${branch}\n参照元SHA: ${sourceSha}\n更新前宛先SHA: ${destSha}\n結果SHA: ${sourceSha}\n`,
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
} catch (error) {
  process.stderr.write(`${(error as Error).message}\n`);
  process.exitCode = 1;
}
