// どのリポジトリでも、その固定テストを担当PCで流す（承認付きhost-sshの固定操作 verify）。
// 固定テストのコマンドはWorkerと同じ fleet.json の担当PCの設定（setupCommand・testCommand・testTimeoutSeconds）から作り、
// ここで別に定義しない。作業ツリーは未コミットの変更を含め、削除済みの追跡ファイルと .tmp/ を除いた tar.gz で送る。
import fs from 'node:fs';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { checkBudget, validateSpec } from './ssh-verify-runner.ts';
import {
  rankedHosts,
  type Assignments,
  type HostPolicy,
} from './ssh-host-select.ts';

const usage =
  '使い方: node scripts/ssh-verify.ts [--host auto|<PC名>] --repo <owner/repo> --cwd <作業ツリー> [--reason <理由>] [--direct-pr <検証対象のコミットSHA>]';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const readJSON = (file: string) =>
  JSON.parse(fs.readFileSync(path.join(root, file), 'utf8'));

// host-ssh.json のPC名（a9-max）に当たる fleet.json の担当名（a9max）。assignments.json の machineName で対応付ける。
export function fleetHost(
  assignments: Assignments,
  fleet: Fleet,
  host: string,
) {
  for (const [name, entry] of Object.entries(assignments.hosts ?? {}))
    if (entry?.machineName === host && fleet.hosts?.[name]) return name;
  return fleet.hosts?.[host] && !assignments.hosts?.[host]?.machineName
    ? host
    : '';
}

// originのURLが対象リポジトリを指すか。末尾の .git・/ の有無と owner/name の大文字小文字の違いは許す。
export function originMatches(remote, repository) {
  const parts = remote
    .trim()
    .replace(/\/+$/, '')
    .replace(/\.git$/i, '')
    .split(/[/:]/);
  return parts.slice(-2).join('/').toLowerCase() === repository.toLowerCase();
}

/** fleet.json のうち、担当PCの固定テストの設定。 */
interface Fleet {
  hosts?: Record<string, any>;
}
export function buildSpec(
  { fleet, assignments }: { fleet: Fleet; assignments: Assignments },
  host: string,
  repository: string,
) {
  const name = fleetHost(assignments, fleet, host);
  const entry = name ? fleet.hosts[name] : undefined;
  const repo = entry?.repositories?.find((item) => item.name === repository);
  if (!repo || !Array.isArray(repo.testCommand) || !repo.testCommand.length) {
    const owners = Object.entries(fleet.hosts ?? {})
      .filter(([, value]) =>
        value.repositories?.some(
          (item) => item.name === repository && item.testCommand?.length,
        ),
      )
      .map(([key]) => assignments.hosts?.[key]?.machineName ?? key);
    throw Error(
      `${host} は ${repository} の固定テストを担当していません。担当PC: ${owners.join(', ') || 'なし'}`,
    );
  }
  if (repo.nativeTestHost || repo.nativeNetwork)
    throw Error('Macのネイティブ検証はこの操作では流せません。');
  const tools = { '{node}': entry.node, '{python}': entry.python };
  const resolve = (command) =>
    (command ?? []).map((part) => {
      if (!Object.hasOwn(tools, part)) return part;
      if (!tools[part])
        throw Error(`${name} に ${part} の道具が設定されていません。`);
      return tools[part];
    });
  const setupTimeout =
    entry.testTimeoutSeconds > 0 ? entry.testTimeoutSeconds : 900;
  return validateSpec({
    version: 1,
    repository,
    setupCommand: resolve(repo.setupCommand),
    testCommand: resolve(repo.testCommand),
    pnpm: entry.pnpm ?? '',
    setupTimeoutSeconds: setupTimeout,
    testTimeoutSeconds:
      repo.testTimeoutSeconds > 0 ? repo.testTimeoutSeconds : setupTimeout,
  });
}

export function main(
  argv: string[],
  options: {
    policy?: HostPolicy;
    config?: { fleet: Fleet; assignments: Assignments };
  } = {},
) {
  const flags: Record<string, string> = {};
  for (let i = 0; i < argv.length; i += 2) {
    if (
      !['--host', '--repo', '--cwd', '--reason', '--direct-pr'].includes(
        argv[i],
      ) ||
      !argv[i + 1] ||
      flags[argv[i]]
    )
      throw Error(usage);
    flags[argv[i]] = argv[i + 1];
  }
  flags['--host'] ??= 'auto';
  if (
    !flags['--repo'] ||
    !flags['--cwd'] ||
    !/^[a-z0-9][a-z0-9-]*$/.test(flags['--host'])
  )
    throw Error(usage);
  const policy = options.policy ?? readJSON('host-ssh.json');
  const config = options.config ?? {
    fleet: readJSON('fleet.json'),
    assignments: readJSON('assignments.json'),
  };
  // --host auto（省略時）は、担当表の順位が最も高く、その固定テストを流せるPCを選ぶ。
  if (flags['--host'] === 'auto') {
    flags['--host'] = rankedHosts(
      config.assignments,
      policy,
      flags['--repo'],
    ).find((host) => {
      try {
        buildSpec(config, host, flags['--repo']);
        return true;
      } catch {
        return false;
      }
    });
    if (!flags['--host'])
      throw Error(
        `${flags['--repo']} の固定テストを流せる担当PCが host-ssh.json にありません。`,
      );
  }
  if (!Object.hasOwn(policy.hosts, flags['--host']))
    throw Error('host-ssh.json に登録されていないPCです。');
  if (!policy.operations.verify)
    throw Error('host-ssh.json に操作 verify がありません。');
  const spec = buildSpec(config, flags['--host'], flags['--repo']);
  const cwd = path.resolve(flags['--cwd']);
  const git = (args) => {
    const result = spawnSync('git', args, {
      cwd,
      encoding: 'utf8',
      maxBuffer: 64 * 1024 * 1024,
    });
    if (result.error || result.status !== 0)
      throw Error(
        result.stderr || result.error?.message || 'Gitの確認に失敗しました。',
      );
    return result.stdout;
  };
  if (!originMatches(git(['remote', 'get-url', 'origin']), spec.repository))
    throw Error('作業ツリーのoriginと対象リポジトリが一致しません。');
  checkBudget(spec, cwd);
  const head = git(['rev-parse', 'HEAD']).trim();
  const dirty = git(['status', '--porcelain']).trim() !== '';
  const directHead = flags['--direct-pr'];
  if (
    directHead &&
    (!/^[a-f0-9]{40}$/.test(directHead) ||
      head !== directHead ||
      dirty ||
      !git(['branch', '--show-current']).trim().startsWith('chat/'))
  )
    throw Error(
      '直接PRの固定検証は、専用の chat/ ブランチの確定済みコミットを指定してください。',
    );
  const files = git(['ls-files', '-co', '--exclude-standard', '-z'])
    .split('\0')
    .filter(
      (file) =>
        file &&
        !file.startsWith('.tmp/') &&
        fs.existsSync(path.join(cwd, file)),
    );
  fs.mkdirSync(path.join(cwd, '.tmp'), { recursive: true });
  const work = fs.mkdtempSync(path.join(cwd, '.tmp', 'ssh-verify-'));
  try {
    const archive = path.join(work, 'tree.tar.gz');
    const proofFile = path.join(work, 'direct-pr.json');
    if (directHead) {
      const base = git(['rev-parse', 'refs/remotes/origin/trunk']).trim();
      const branch = git(['branch', '--show-current']).trim();
      const commits = git([
        'rev-list',
        '--first-parent',
        '--max-count=17',
        base + '..' + directHead,
      ])
        .trim()
        .split('\n')
        .filter(Boolean);
      if (commits.length < 1 || commits.length > 16)
        throw Error('直接PRの基点と確定済みコミットの履歴を確認できません。');
      const objectsProof = commits.map((sha) => {
        const result = spawnSync('git', ['cat-file', 'commit', sha], {
          cwd,
          maxBuffer: 12288,
        });
        if (result.error || result.status !== 0)
          throw Error('直接PRのコミット証拠を読めません。');
        return result.stdout.toString('base64');
      });
      fs.writeFileSync(
        proofFile,
        JSON.stringify({
          repo: spec.repository,
          branch,
          head: directHead,
          base,
          commits: objectsProof,
        }),
        { mode: 0o600 },
      );
      const entries = git(['ls-tree', '-rtz', '--full-tree', directHead])
        .split('\0')
        .filter(Boolean)
        .map((entry) => entry.slice(0, entry.indexOf('\t')).split(' '));
      if (entries.some(([, type]) => type !== 'blob' && type !== 'tree'))
        throw Error('直接PRの固定検証ではサブモジュールを含む木を送れません。');
      const objects = [
        directHead,
        git(['rev-parse', directHead + '^{tree}']).trim(),
        ...entries.map(([, , sha]) => sha),
      ];
      const fd = fs.openSync(archive, 'wx', 0o600);
      try {
        const pack = spawnSync('git', ['pack-objects', '--stdout'], {
          cwd,
          input: [...new Set(objects)].join('\n') + '\n',
          encoding: 'utf8',
          stdio: ['pipe', fd, 'pipe'],
        });
        if (pack.error || pack.status !== 0)
          throw Error('対象コミットのGitオブジェクトを収集できません。');
      } finally {
        fs.closeSync(fd);
      }
    } else {
      const tar = spawnSync('tar', ['--null', '-T', '-', '-czf', archive], {
        cwd,
        input: files.join('\0') + '\0',
        encoding: 'utf8',
      });
      if (tar.error || tar.status !== 0)
        throw Error(
          '作業ツリーのtar.gzを作れません: ' +
            (tar.stderr || tar.error?.message),
        );
    }
    const encoded = (file) =>
      fs.readFileSync(path.join(root, 'scripts', file)).toString('base64');
    const input = path.join(work, 'input');
    fs.writeFileSync(
      input,
      Buffer.concat([
        Buffer.from(
          encoded('ssh-verify-host.sh') +
            '\n' +
            encoded('ssh-verify-runner.ts') +
            '\n' +
            Buffer.from(JSON.stringify(spec)).toString('base64') +
            '\n',
        ),
        fs.readFileSync(archive),
      ]),
      { mode: 0o600 },
    );
    const digest = createHash('sha256')
      .update(fs.readFileSync(input))
      .digest('hex');
    console.error(
      `対象PC: ${flags['--host']} リポジトリ: ${spec.repository} 基点: ${head}${dirty ? '（未コミットの変更を含む）' : ''}\n` +
        `固定テスト: ${JSON.stringify(spec.setupCommand)} → ${JSON.stringify(spec.testCommand)}`,
    );
    const run = spawnSync(
      'sh',
      [
        path.join(root, 'deploy/nas-host/host-ssh.sh'),
        '--host',
        flags['--host'],
        '--operation',
        'verify',
        '--reason',
        flags['--reason'] ?? `${spec.repository}の固定テストを担当PCで流す`,
        '--evidence',
        `repo=${spec.repository} head=${head} dirty=${dirty} sha256=${digest} setupCommand=${JSON.stringify(spec.setupCommand)} testCommand=${JSON.stringify(spec.testCommand)}`,
        '--input',
        input,
        ...(directHead ? ['--direct-pr-proof', proofFile] : []),
      ],
      { stdio: ['ignore', 'inherit', 'inherit'] },
    );
    if (run.error) throw run.error;
    return run.status ?? 1;
  } finally {
    fs.rmSync(work, { recursive: true, force: true });
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
