// 固定SSH操作 verify の容器内の実行部。接続先の稼働Workerと同じイメージの中で、送られた作業ツリー（/work）に
// Workerの固定テストと同じ順（setupCommand → package.json があれば pnpm install → testCommand）でコマンドを流す。
// 指定は呼出元（scripts/ssh-verify.ts）が fleet.json の担当PCの設定から作り、環境変数 HOST_SSH_VERIFY_SPEC で渡す。
// 整形（prettier --write・go fmt）は作業ツリーを書き換えるだけで呼出元へ戻らないため流さない。
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import type { SpawnSyncOptions } from 'node:child_process';
import { pathToFileURL } from 'node:url';

const repositoryName =
  /^[A-Za-z0-9][A-Za-z0-9_.-]*\/[A-Za-z0-9][A-Za-z0-9_.-]*$/;

type VerifySpec = {
  version: number;
  repository: string;
  setupCommand: string[];
  testCommand: string[];
  pnpm: string;
  setupTimeoutSeconds: number;
  testTimeoutSeconds: number;
};
type Step = { name: string; argv: string[]; timeout: number };
// 段の実行部（spawnSync、テストでは差し替え）。
type Run = (
  command: string,
  args: string[],
  options: SpawnSyncOptions,
) => { status: number | null; signal?: NodeJS.Signals | null; error?: Error };
type Log = (line: string) => void;

const command = (value: unknown, allowEmpty: boolean): boolean => {
  if (
    !Array.isArray(value) ||
    value.length > 64 ||
    (!allowEmpty && value.length === 0)
  )
    return false;
  return value.every(
    (part) =>
      typeof part === 'string' &&
      part !== '' &&
      part.length <= 4096 &&
      !part.includes('\0'),
  );
};
const seconds = (value: unknown): boolean =>
  typeof value === 'number' &&
  Number.isInteger(value) &&
  value >= 1 &&
  value <= 3600;

export function validateSpec(value: any): VerifySpec {
  if (
    !value ||
    typeof value !== 'object' ||
    Array.isArray(value) ||
    value.version !== 1 ||
    typeof value.repository !== 'string' ||
    !repositoryName.test(value.repository) ||
    !command(value.setupCommand, true) ||
    !command(value.testCommand, false) ||
    typeof value.pnpm !== 'string' ||
    (value.pnpm !== '' && !path.isAbsolute(value.pnpm)) ||
    !seconds(value.setupTimeoutSeconds) ||
    !seconds(value.testTimeoutSeconds)
  )
    throw Error('固定テストの指定が不正です。');
  return value;
}

// Workerの dependencyPolicy と同じく、依存の公開後14日制限を回避できる設定がある作業ツリーでは何も流さない。
export function dependencyPolicy(tree: string): void {
  const walk = (dir: string): void => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const file = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        if (!['.git', 'node_modules', '.pnpm'].includes(entry.name)) walk(file);
        continue;
      }
      if (
        !['package.json', 'pnpm-workspace.yaml', '.npmrc'].includes(entry.name)
      )
        continue;
      if (!entry.isFile())
        throw Error('依存設定は作業ツリーの通常ファイルにしてください。');
      if (fs.statSync(file).size > 2 << 20)
        throw Error('依存設定の大きさが上限を超えました。');
      for (const raw of fs.readFileSync(file, 'utf8').split('\n')) {
        const line = raw.trim();
        if (line.startsWith('#') || line.startsWith(';')) continue;
        const folded = line.toLowerCase().replace(/[-_]/g, '');
        if (
          folded.includes('minimumreleaseageexclude') ||
          folded.includes('configdependencies')
        )
          throw Error(
            '依存パッケージの公開後14日制限を回避できる設定があります。',
          );
      }
    }
  };
  walk(tree);
}

// Workerの固定テストの環境（testSandboxEnv と ToolchainEnv）に合わせる。資格情報は渡さない。
export function testEnv(
  base: NodeJS.ProcessEnv = process.env,
  temp = '/tmp',
): Record<string, string> {
  const env: Record<string, string> = {
    PATH:
      base.PATH ??
      '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
  };
  for (const key of ['JAVA_HOME', 'LANG']) if (base[key]) env[key] = base[key];
  return Object.assign(env, {
    HOME: temp,
    CI: 'true',
    TEMP: temp,
    TMPDIR: temp,
    TZ: 'UTC',
    XDG_CONFIG_HOME: path.join(temp, 'config'),
    XDG_CACHE_HOME: path.join(temp, 'cache'),
    XDG_DATA_HOME: path.join(temp, 'data'),
    XDG_STATE_HOME: path.join(temp, 'state'),
    GOCACHE: path.join(temp, 'go-build'),
    GOMODCACHE: path.join(temp, 'go-mod'),
    GOPATH: path.join(temp, 'go'),
    GOPROXY: 'https://proxy.golang.org',
    GOSUMDB: 'sum.golang.org',
    GOTOOLCHAIN: 'local',
    GOTELEMETRY: 'off',
    GRADLE_USER_HOME: path.join(temp, 'gradle'),
    PUB_CACHE: path.join(temp, 'pub-cache'),
    JAVA_TOOL_OPTIONS: '-Djava.io.tmpdir=' + temp,
    GRADLE_OPTS:
      '-Dorg.gradle.daemon=false -Dorg.gradle.project.kotlin.compiler.execution.strategy=in-process',
    FLUTTER_SUPPRESS_ANALYTICS: 'true',
  });
}

export function steps(spec: VerifySpec, tree: string): Step[] {
  const list: Step[] = [];
  if (spec.setupCommand.length)
    list.push({
      name: 'setupCommand',
      argv: spec.setupCommand,
      timeout: spec.setupTimeoutSeconds,
    });
  if (spec.pnpm && fs.existsSync(path.join(tree, 'package.json')))
    list.push({
      name: 'pnpmInstall',
      argv: [
        spec.pnpm,
        'install',
        '--frozen-lockfile',
        '--ignore-scripts',
        '--ignore-pnpmfile',
        '--config.minimum-release-age=20160',
      ],
      timeout: spec.setupTimeoutSeconds,
    });
  list.push({
    name: 'testCommand',
    argv: spec.testCommand,
    timeout: spec.testTimeoutSeconds,
  });
  return list;
}

// 容器全体の打ち切り（ssh-verify-host.sh の timeout）と、段ごとの上限の合計の上限。合計が全体を超える指定は流す前に断る。
export const containerSeconds = 5400;
export const stepBudgetSeconds = containerSeconds - 100;
export function checkBudget(spec: VerifySpec, tree: string): number {
  const total = steps(spec, tree).reduce((sum, step) => sum + step.timeout, 0);
  if (total > stepBudgetSeconds)
    throw Error(
      `段ごとの上限の合計（${total}秒）が容器全体の上限（${stepBudgetSeconds}秒）を超えます。`,
    );
  return total;
}

// Workerの addIntakeBrowserTools と同じく、issue-desk-intake ではイメージのCaddy・Chromiumが起動できるときだけブラウザ試験に渡す。
export function intakeBrowserTools(
  spec: VerifySpec,
  env: Record<string, string>,
  run: Run = spawnSync,
  log: Log = console.log,
  caddy = '/opt/agent-tools/bin/caddy',
  chromium = '/usr/lib/chromium/chromium-headless-shell',
): void {
  if (spec.repository !== 'mofua-suki/issue-desk-intake') return;
  for (const file of [caddy, chromium]) {
    try {
      const info = fs.statSync(file);
      if (!info.isFile() || (info.mode & 0o111) === 0) return;
    } catch {
      return;
    }
  }
  const probe = run(
    chromium,
    [
      '--headless',
      '--no-sandbox',
      '--disable-gpu',
      '--disable-background-networking',
      '--disable-dev-shm-usage',
      '--user-data-dir=' + path.join(env.TMPDIR, 'chromium-probe'),
      '--dump-dom',
      'about:blank',
    ],
    { env, stdio: 'ignore', timeout: 60000 },
  );
  if (probe.error || probe.status !== 0) {
    log('Chromiumを起動できない実行環境のためブラウザ試験を省略します');
    return;
  }
  env.ISSUE_DESK_TEST_CADDY = caddy;
  env.ISSUE_DESK_TEST_CHROMIUM = chromium;
}

// 最初に失敗した段の終了コードを返す（すべて成功なら0）。
export function runVerify(
  spec: VerifySpec,
  tree: string,
  run: Run = spawnSync,
  log: Log = console.log,
): number {
  validateSpec(spec);
  checkBudget(spec, tree);
  const env = testEnv();
  intakeBrowserTools(spec, env, run, log);
  for (const step of steps(spec, tree)) {
    dependencyPolicy(tree);
    log(`step=${step.name} start command=${JSON.stringify(step.argv)}`);
    const result = run(step.argv[0], step.argv.slice(1), {
      cwd: tree,
      env,
      stdio: 'inherit',
      timeout: step.timeout * 1000,
      killSignal: 'SIGTERM',
    });
    let code = result.status;
    if (
      (result.error as NodeJS.ErrnoException | undefined)?.code === 'ETIMEDOUT'
    ) {
      log(`${step.name}が${step.timeout}秒の上限を超えました`);
      code = 124;
    } else if (result.error) {
      log(
        `${step.name}を起動できません: ${(result.error as NodeJS.ErrnoException).code ?? result.error.message}`,
      );
      code = 127;
    } else if (code === null)
      code = 128 + (os.constants.signals[result.signal as NodeJS.Signals] ?? 0);
    log(`step=${step.name} exit=${code}`);
    if (code !== 0) return code;
  }
  return 0;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  try {
    const spec = JSON.parse(
      Buffer.from(process.env.HOST_SSH_VERIFY_SPEC ?? '', 'base64').toString(
        'utf8',
      ),
    );
    if (
      process.env.HOST_SSH_DIRECT_PR_HEAD &&
      (validateSpec(spec).repository !== 'mofua-suki/issue-desk-intake' ||
        spec.setupCommand.length !== 0 ||
        JSON.stringify(spec.testCommand) !==
          JSON.stringify(['sh', 'scripts/verify.sh']) ||
        spec.pnpm !== '/opt/agent-tools/bin/pnpm')
    )
      throw Error(
        '直接PRの証明書はissue-desk-intakeの固定テストにだけ使えます。',
      );
    process.exitCode = runVerify(spec, process.cwd());
  } catch (error) {
    console.log((error as Error).message);
    process.exitCode = 1;
  }
}
