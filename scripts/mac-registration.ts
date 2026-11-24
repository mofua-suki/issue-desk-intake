// Macの登録内容を作業ツリー内で準備する。本番の登録・トークン発行は行わない。
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

/** 登録する Mac。releases は実装兼用（role implementation）のときだけ要る。 */
interface MacOptions {
  name: string;
  role: string;
  home: string;
  repo?: string;
  releases?: any;
  gate?: boolean;
}
// fleet・assignments・releases は JSON の設定そのもの（fleet.json・assignments.json・worker-release.json）。
export function registration(
  fleet: any,
  assignments: any,
  {
    name,
    role,
    home,
    repo = 'mofua-suki/sample-expense-client',
    releases,
    gate = true,
  }: MacOptions,
) {
  if (
    !/^[a-z0-9][a-z0-9-]{0,23}$/.test(name ?? '') ||
    !['test-only', 'implementation'].includes(role) ||
    !/^\/Users\/[^/\s]+$/.test(home ?? '') ||
    !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo)
  )
    throw Error(
      '名前（24文字以内）・役割・Macのホーム（/Users/名前）・リポジトリを確認してください。',
    );
  releases = releases ? structuredClone(releases) : undefined;
  fleet = structuredClone(fleet);
  assignments = structuredClone(assignments);
  const tester = `${name}-test`,
    worker = `${name}-worker`;
  for (const host of [tester, ...(role === 'implementation' ? [worker] : [])])
    if (
      fleet.hosts[host] ||
      assignments.hosts[host] ||
      Object.values<any>(assignments.hosts).some(
        (entry) => entry.machineName === host,
      )
    )
      throw Error(`登録済みのPC名です: ${host}`);
  const implementers = Object.entries<any>(assignments.hosts).filter(
    ([, entry]) => entry.repositories?.[repo],
  );
  if (!implementers.length)
    throw Error('対象リポジトリの実装担当がありません。');
  const effective = (host) => ({ ...fleet.defaults, ...fleet.hosts[host] });
  const first = implementers.sort(
    (a, b) => a[1].repositories[repo].rank - b[1].repositories[repo].rank,
  )[0][0];
  const template = effective(first);
  const repository = template.repositories?.find(
    (entry) => entry.name === repo,
  );
  if (!repository) throw Error('対象リポジトリの実装設定がありません。');
  if (
    implementers.some(
      ([host]) =>
        effective(host).repositories?.find((entry) => entry.name === repo)
          ?.nativeTestHost,
    )
  )
    throw Error(
      'Mac検証先が登録済みです。既存担当の切替は別途確認してください。',
    );
  const state = `${home}/.issue-desk/apple`;
  fleet.hosts[tester] = {
    giteaUrl: template.giteaUrl,
    gitUser: template.gitUser || 'issue-desk',
    git: '/usr/bin/git',
    runtimeDir: `${state}/runtime`,
    workerTokenFile: `${state}/private/gitea-token`,
    jobTimeoutSeconds: 3600,
    testTimeoutSeconds: 1800,
    environment: {
      platform: 'darwin',
      realDevice: null,
      liveMeasurement: null,
      network: null,
    },
    repositories: [
      {
        name: repo,
        nativeTestHost: tester,
        nativeNetwork: true,
        nativeToolRoots: [`${home}/development/flutter`],
        testCommand: ['sh', 'scripts/verify-apple.sh', 'ios'],
      },
    ],
  };
  assignments.hosts[tester] = {
    role: 'test-only',
    maxImplementations: 1,
    repositories: {},
    testRepositories: [repo],
  };
  // 停止のまま登録するときは、Giteaにまだ無いテスト専用アカウントを担当表へ入れない。
  if (gate) {
    assignments.workerAccounts ??= {};
    assignments.workerAccounts[tester] = `desk-${tester}`;
  }
  if (role === 'implementation') {
    if (!releases?.hosts?.[first] || releases.hosts[worker])
      throw Error('実装担当のイメージ配布設定を確認してください。');
    releases.hosts[worker] = releases.hosts[first];
    const implementation = structuredClone(template);
    const group = assignments.repositoryGroups?.find((group) =>
      group.includes(repo),
    ) || [repo];
    const ranks = {};
    implementation.repositories = group.map((name) => {
      const settings = template.repositories?.find(
        (entry) => entry.name === name,
      );
      const owners = Object.values<any>(assignments.hosts).filter(
        (entry) => entry.repositories?.[name],
      );
      if (!settings || !owners.length)
        throw Error(`関連リポジトリの実装設定がありません: ${name}`);
      ranks[name] = {
        rank:
          Math.max(...owners.map((entry) => entry.repositories[name].rank)) + 1,
      };
      return {
        ...structuredClone(settings),
        ...(gate && name === repo ? { nativeTestHost: tester } : {}),
      };
    });
    fleet.hosts[worker] = implementation;
    assignments.hosts[worker] = {
      role: 'implementation',
      maxImplementations: 1,
      repositories: ranks,
    };
    // 実装用IDは引き取れる担当になるので、停止のまま登録するときもアカウントを書く。
    assignments.workerAccounts ??= {};
    assignments.workerAccounts[worker] = `desk-${worker}`;
  }
  // 個別設定にしてdefaultsや他リポジトリを変えない。代替担当も同じ検証を必須にする。
  // gate=false は登録だけ行い、検証要求をMacへ送らない（停止のまま）。
  for (const [host] of gate ? implementers : []) {
    const config = effective(host);
    fleet.hosts[host] = {
      ...config,
      repositories: config.repositories.map((entry) =>
        entry.name === repo ? { ...entry, nativeTestHost: tester } : entry,
      ),
    };
  }
  return {
    fleet,
    assignments,
    releases: role === 'implementation' ? releases : undefined,
    tester,
    worker: role === 'implementation' ? worker : null,
  };
}

function main(argv: string[]) {
  const flags: Record<string, string> = {};
  for (let i = 0; i < argv.length; i += 2) {
    if (
      ![
        '--name',
        '--role',
        '--home',
        '--repo',
        '--gate',
        '--root',
        '--out',
      ].includes(argv[i]) ||
      !argv[i + 1] ||
      flags[argv[i]]
    )
      throw Error('引数が不正です。');
    flags[argv[i]] = argv[i + 1];
  }
  if (!flags['--out'])
    throw Error(
      '使い方: node scripts/mac-registration.ts --name macbook --role test-only|implementation --home /Users/名前 [--gate on|off] --out .tmp/mac-registration',
    );
  if (flags['--gate'] && !['on', 'off'].includes(flags['--gate']))
    throw Error('--gate は on か off です。');
  const root = path.resolve(
    flags['--root'] ||
      path.join(path.dirname(fileURLToPath(import.meta.url)), '..'),
  );
  const out = path.resolve(root, flags['--out']);
  if (!out.startsWith(root + path.sep + '.tmp' + path.sep))
    throw Error('出力先は作業ツリーの.tmp/配下にしてください。');
  if (fs.existsSync(out) && fs.readdirSync(out).length)
    throw Error('出力先は空のフォルダーを指定してください。');
  const result = registration(
    JSON.parse(fs.readFileSync(path.join(root, 'fleet.json'), 'utf8')),
    JSON.parse(fs.readFileSync(path.join(root, 'assignments.json'), 'utf8')),
    {
      name: flags['--name'],
      role: flags['--role'],
      home: flags['--home'],
      repo: flags['--repo'],
      releases: JSON.parse(
        fs.readFileSync(
          path.join(root, 'deploy/nas/worker-release.json'),
          'utf8',
        ),
      ),
      gate: flags['--gate'] !== 'off',
    },
  );
  fs.mkdirSync(out, { recursive: true });
  for (const [file, value] of [
    ['fleet.json', result.fleet],
    ['assignments.json', result.assignments],
  ])
    fs.writeFileSync(
      path.join(out, file),
      JSON.stringify(value, null, 2) + '\n',
    );
  if (result.releases) {
    fs.mkdirSync(path.join(out, 'deploy/nas'), { recursive: true });
    fs.writeFileSync(
      path.join(out, 'deploy/nas/worker-release.json'),
      JSON.stringify(result.releases, null, 2) + '\n',
    );
  }
  console.log(
    `登録案: ${out}\nネイティブテスト: ${result.tester}\n実装担当: ${result.worker || '追加しません'}\nMac検証の必須化: ${flags['--gate'] === 'off' ? 'しません（停止のまま）' : 'します'}\n内容をPRで確認・反映後、PC別トークンを発行してMacへ設定してください。`,
  );
}
if (import.meta.url === pathToFileURL(process.argv[1] || '').href) {
  try {
    main(process.argv.slice(2));
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
