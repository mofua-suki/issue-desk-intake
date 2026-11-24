// 許可の付与の反映係（docs/permission-grants.md の『NAS の反映係』）。NAS のユーザータイマー
// （deploy/nas/issue-desk-permission-grants.timer、1分ごと）が ~/.issue-desk/node 経由で動かす。手で変えない。
//
// - 有効化の印 ~/.issue-desk/remote-control/permission-grants-enabled があるときだけ受付の有効な付与を読む。
//   印が無い・受付を読めないときは何も足さず、前に足した規則を外して元の許可へ戻す。
// - 足す規則は deploy/nas/permission-grant-rules.json の規則IDに対応する文字列だけ（受付の応答の文字列は使わない）。
// - 各 ~/rc/*/.claude/settings.json の全内容を「元の許可 + 有効な付与の規則」から作り、内容が違うときだけ
//   一時ファイルから置き換える。リンク・Git 追跡済み・利用者が書いた設定は置き換えない。
// - 置いた内容は ~/.issue-desk/remote-control/permission-grants-applied/<名前>/settings.json に記録し、
//   deploy/nas/update-remote-control-guard.sh はこれと一致する設定を利用者の設定（custom）と判定しない。
// - 稼働中の claude-rc@ は再起動しない。
import { spawnSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const grantIdPattern = /^[0-9a-f]{32}$/;
// 規則ファイルの値は Bash 規則1つだけ。括弧・改行・シェルの区切りを含むものは使わない。
const ruleForm = /^Bash\([^()\n;&|<>`$]+\)$/;
const defaultUrl = 'https://issue-desk.example.ts.net/';
const requestTimeoutMs = 20000;

type Grant = {
  id: string;
  kind: string;
  status: string;
  target: unknown;
  expiresAt: string | null;
  appliedAt?: string | null;
};
type UsedGrant = {
  id: string;
  ruleId: string;
  appliedAt?: string | null;
  rule?: string;
};
type ProjectResult =
  | 'written'
  | 'reverted'
  | 'unchanged'
  | 'missing'
  | 'none'
  | 'link'
  | 'custom'
  | 'tracked'
  | 'git-error'
  | 'failed';
type Tracked = (dir: string) => boolean | null;
type Log = (line: string) => void;
type Context = {
  base: string;
  control: string | null;
  desired: string;
  records: string;
  tracked: Tracked;
};
type ApplyOptions = {
  home?: string;
  fetch?: typeof fetch;
  tracked?: Tracked;
  now?: () => number;
  log?: Log;
  sourceFile?: string;
  rulesFile?: string;
  url?: string;
  tokenFile?: string;
};
type ApplyResult = {
  status: 'failed' | 'disabled' | 'skipped' | 'enabled';
  results?: Record<string, ProjectResult>;
  rules?: string[];
  unknown?: string[];
  reported?: string[];
};

// 規則ファイルを読み、規則ID → 規則の Map を返す。形の違う項目は使わずに invalid へ入れる。
export function readGrantRules(file: string): {
  rules: Map<string, string>;
  invalid: string[];
} {
  const data = JSON.parse(fs.readFileSync(file, 'utf8'));
  if (!data || typeof data !== 'object' || Array.isArray(data))
    throw new Error('規則ファイルの形が違います');
  const rules = new Map<string, string>();
  const invalid: string[] = [];
  for (const [id, rule] of Object.entries(data)) {
    if (
      /^[a-z0-9][a-z0-9-]*$/.test(id) &&
      typeof rule === 'string' &&
      ruleForm.test(rule)
    )
      rules.set(id, rule);
    else invalid.push(id);
  }
  return { rules, invalid };
}

// 元の許可に規則を足した全内容。足す規則が無ければ元のファイルの内容そのもの。deny と元の allow は変えない。
function grantedSettings(baseText: string, rules: string[]): string {
  if (rules.length === 0) return baseText;
  const settings = JSON.parse(baseText);
  const allow: string[] = settings.permissions.allow;
  for (const rule of rules) if (!allow.includes(rule)) allow.push(rule);
  return JSON.stringify(settings, null, 2) + '\n';
}

// .claude/settings.json が Git 追跡済みなら true、そうでなければ false、判定できなければ null。
// update-remote-control-guard.sh と同じく、Git の作業ツリーでなければ追跡されていないとみなす。
function gitTracked(dir: string): boolean | null {
  const inside = spawnSync('git', ['-C', dir, 'rev-parse', '--show-prefix'], {
    stdio: 'ignore',
    timeout: 10000,
  });
  if (inside.error) return null;
  if (inside.status !== 0) return false;
  const files = spawnSync(
    'git',
    ['-C', dir, 'ls-files', '--error-unmatch', '--', '.claude/settings.json'],
    {
      stdio: 'ignore',
      timeout: 10000,
    },
  );
  if (files.status === 0) return true;
  if (files.status === 1) return false;
  return null;
}

function readText(file: string): string | null {
  try {
    return fs.readFileSync(file, 'utf8');
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw error;
  }
}

const isLink = (file: string): boolean => {
  try {
    return fs.lstatSync(file).isSymbolicLink();
  } catch {
    return false;
  }
};

const isDirectory = (file: string): boolean => {
  try {
    return fs.statSync(file).isDirectory();
  } catch {
    return false;
  }
};

// 同じフォルダーの一時ファイルへ書いてから rename で置き換える。
function writeAtomic(file: string, text: string): void {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const tmp = `${file}.${randomUUID()}.tmp`;
  try {
    fs.writeFileSync(tmp, text, { flag: 'wx' });
    fs.renameSync(tmp, file);
  } catch (error) {
    fs.rmSync(tmp, { force: true });
    throw error;
  }
}

// 受付の有効な nas-claude-rule の付与。読めなければ ok=false（呼び出し側は何も足さない）。
async function activeGrants({
  fetchImpl,
  base,
  tokenFile,
  now,
}: {
  fetchImpl: typeof fetch;
  base: string;
  tokenFile: string;
  now: () => number;
}): Promise<
  { ok: false; reason: string } | { ok: true; grants: Grant[]; token: string }
> {
  let token: string;
  try {
    token = fs.readFileSync(tokenFile, 'utf8').trim();
  } catch {
    return { ok: false, reason: 'トークンを読めません' };
  }
  if (!token) return { ok: false, reason: 'トークンが空です' };
  let body: { grants?: unknown };
  try {
    const response = await fetchImpl(
      new URL('api/permission-grants?status=active&kind=nas-claude-rule', base),
      {
        headers: {
          authorization: `Bearer ${token}`,
          accept: 'application/json',
        },
        signal: AbortSignal.timeout(requestTimeoutMs),
      },
    );
    if (!response.ok)
      return { ok: false, reason: `受付が ${response.status} を返しました` };
    body = await response.json();
  } catch (error) {
    return {
      ok: false,
      reason: `受付に届きません（${error?.name ?? 'Error'}）`,
    };
  }
  if (!body || !Array.isArray(body.grants))
    return { ok: false, reason: '受付の応答の形が違います' };
  const time = now();
  const grants = (body.grants as Grant[]).filter(
    (grant) =>
      grant &&
      grant.kind === 'nas-claude-rule' &&
      grant.status === 'active' &&
      typeof grant.id === 'string' &&
      grantIdPattern.test(grant.id) &&
      (grant.expiresAt === null || Date.parse(grant.expiresAt) > time),
  );
  return { ok: true, grants, token };
}

// 1つの作業フォルダーの設定を揃える。戻り値は結果の種類（written・reverted・unchanged・missing・none・
// link・custom・tracked・git-error）。書けなければ例外（呼び出し側が failed にする）。
function reflectProject(
  dir: string,
  name: string,
  ctx: Context,
): ProjectResult {
  const dest = path.join(dir, '.claude', 'settings.json');
  if (isLink(dir) || isLink(path.join(dir, '.claude')) || isLink(dest))
    return 'link';
  const recordDir = path.join(ctx.records, name);
  const recordFile = path.join(recordDir, 'settings.json');
  const nextFile = path.join(recordDir, 'next.json');
  const current = readText(dest);
  const record = readText(recordFile);
  const next = readText(nextFile);
  const adding = ctx.desired !== ctx.base;
  // 設定が無いフォルダーは、更新（update-remote-control-guard.sh）が元の許可を置いた後に扱う。
  if (current === null) return 'missing';
  if (current === ctx.desired) {
    if (adding) {
      if (record !== current) writeAtomic(recordFile, current);
      if (next !== null) fs.rmSync(nextFile, { force: true });
    } else if (record !== null || next !== null)
      fs.rmSync(recordDir, { recursive: true, force: true });
    return 'unchanged';
  }
  const ours =
    (record !== null && current === record) ||
    (next !== null && current === next);
  // 反映係が置いた後に別の内容へ変わった。記録を外し、以後は元の許可か利用者の設定として扱う。
  if (!ours && (record !== null || next !== null))
    fs.rmSync(recordDir, { recursive: true, force: true });
  if (!adding && !ours) return 'none';
  if (!ours && current !== ctx.base && current !== ctx.control) return 'custom';
  const tracked = ctx.tracked(dir);
  if (tracked === null) return 'git-error';
  if (tracked) return 'tracked';
  if (adding) {
    // next は置き換えの途中で止まっても次の周期で自分の内容と分かるよう、置き換えより先に書く。
    writeAtomic(nextFile, ctx.desired);
    writeAtomic(dest, ctx.desired);
    writeAtomic(recordFile, ctx.desired);
    fs.rmSync(nextFile, { force: true });
    return 'written';
  }
  writeAtomic(dest, ctx.base);
  fs.rmSync(recordDir, { recursive: true, force: true });
  return 'reverted';
}

const projectNotice: Partial<Record<ProjectResult, string>> = {
  link: 'リンクのため置き換えません',
  custom: '利用者が書いた設定のため置き換えません',
  tracked: 'Git 追跡済みのため置き換えません',
  'git-error': 'Git の追跡を確かめられないため置き換えません',
};

// 1回分の反映。options はテスト用の差し替え（home・fetch・tracked・now・log・sourceFile・rulesFile・url・tokenFile）。
async function applyPermissionGrants(
  options: ApplyOptions = {},
): Promise<ApplyResult> {
  const home = options.home ?? process.env.HOME;
  const log = options.log ?? ((line: string) => console.log(line));
  if (!home || !path.isAbsolute(home)) {
    log('[permission-grants-failed] HOME が絶対パスではないため、反映しません');
    return { status: 'failed' };
  }
  const fetchImpl = options.fetch ?? globalThis.fetch;
  const now = options.now ?? (() => Date.now());
  const tracked = options.tracked ?? gitTracked;
  const sourceFile =
    options.sourceFile ?? path.join(here, 'remote-control-settings.json');
  const rulesFile =
    options.rulesFile ?? path.join(here, 'permission-grant-rules.json');
  const url = options.url ?? process.env.ISSUE_DESK_URL ?? defaultUrl;
  const base = url.endsWith('/') ? url : `${url}/`;
  const tokenFile =
    options.tokenFile ??
    process.env.ISSUE_DESK_TOKEN_FILE ??
    path.join(home, '.issue-desk/token');
  const state = path.join(home, '.issue-desk/remote-control');
  const records = path.join(state, 'permission-grants-applied');
  const enabled = fs.existsSync(path.join(state, 'permission-grants-enabled'));
  // 印が無く、前に足した記録も無ければ何もしない（書かない・受付も読まない）。
  if (!enabled && !fs.existsSync(records))
    return { status: 'disabled', results: {} };

  let baseText: string;
  try {
    baseText = fs.readFileSync(sourceFile, 'utf8');
    JSON.parse(baseText).permissions.allow.slice();
  } catch {
    log(
      `[permission-grants-failed] 元の許可 ${sourceFile} を読めないため、反映しません`,
    );
    return { status: 'failed' };
  }
  // 更新（update.sh）の配置・復旧の途中は、退避と復元の判定を乱さないよう触らない。
  for (const pending of [
    path.join(state, 'project-pending'),
    path.join(home, '.config/systemd/user/claude-rc@.service.update-pending'),
  ]) {
    if (fs.existsSync(pending)) {
      log(
        `[permission-grants] 更新の途中のため、この周期は反映しません: ${pending}`,
      );
      return { status: 'skipped' };
    }
  }

  const notices: string[] = [];
  const unknown: string[] = [];
  let used: UsedGrant[] = [];
  let token = '';
  if (enabled) {
    let rules = new Map<string, string>();
    try {
      const read = readGrantRules(rulesFile);
      rules = read.rules;
      for (const id of read.invalid)
        notices.push(`規則ファイルの形が違う項目を使いません: ${id}`);
    } catch {
      notices.push(
        `規則ファイル ${rulesFile} を読めないため、規則を足しません`,
      );
    }
    const found = await activeGrants({ fetchImpl, base, tokenFile, now });
    if ('reason' in found)
      notices.push(
        `受付の許可の付与を読めないため、規則を足しません: ${found.reason}`,
      );
    else {
      token = found.token;
      for (const grant of found.grants) {
        if (rules.has(grant.target as string))
          used.push({
            id: grant.id,
            ruleId: grant.target as string,
            appliedAt: grant.appliedAt,
          });
        else unknown.push(String(grant.target).slice(0, 80));
      }
      for (const id of unknown)
        notices.push(
          `規則ファイルに無い規則IDを無視しました: ${JSON.stringify(id)}`,
        );
    }
    const order = [...rules.keys()];
    used.sort((a, b) => order.indexOf(a.ruleId) - order.indexOf(b.ruleId));
    used = used.map((grant) => ({ ...grant, rule: rules.get(grant.ruleId) }));
  }
  const rules: string[] = [...new Set(used.map((grant) => grant.rule))];
  const desired = grantedSettings(baseText, rules);
  let control: string | null = null;
  try {
    control = readText(path.join(state, 'settings.json'));
  } catch {
    // 控えを読めなければ、控えと同じ設定を置き換えの対象にしない。
  }
  const ctx: Context = { base: baseText, control, desired, records, tracked };

  const results: Record<string, ProjectResult> = {};
  const changes: string[] = [];
  let entries: fs.Dirent[];
  try {
    entries = fs.readdirSync(path.join(home, 'rc'), { withFileTypes: true });
  } catch {
    entries = [];
  }
  for (const entry of entries) {
    const dir = path.join(home, 'rc', entry.name);
    if (!entry.isDirectory() && !(entry.isSymbolicLink() && isDirectory(dir)))
      continue;
    let result: ProjectResult;
    try {
      result = reflectProject(dir, entry.name, ctx);
    } catch (error) {
      result = 'failed';
      notices.push(
        `書けないため反映できません（${error?.code ?? 'Error'}）: ${path.join(dir, '.claude/settings.json')}`,
      );
    }
    results[entry.name] = result;
    if (result === 'written')
      changes.push(
        `規則を反映しました: ${path.join(dir, '.claude/settings.json')}`,
      );
    if (result === 'reverted')
      changes.push(
        `元の許可へ戻しました: ${path.join(dir, '.claude/settings.json')}`,
      );
    if (projectNotice[result])
      notices.push(
        `${projectNotice[result]}: ${path.join(dir, '.claude/settings.json')}`,
      );
  }
  for (const line of changes) log(`[permission-grants] ${line}`);
  reportNotices(state, notices, log);
  if (!enabled) {
    // 戻し終えて空になった記録のフォルダーを消し、次の周期からは何も読まない。
    try {
      fs.rmdirSync(records);
    } catch {
      // 残りの記録がある（戻せなかったフォルダーがある）ときは消さない。
    }
  }

  // 反映済みの印。どれかの作業フォルダーに今の内容が置けたときだけ、まだ印の無い付与へ送る。届かなくても反映は続ける。
  const reported: string[] = [];
  const placed =
    rules.length > 0 &&
    Object.values(results).some((r) => r === 'written' || r === 'unchanged');
  if (placed) {
    for (const grant of used) {
      if (grant.appliedAt) continue;
      try {
        const response = await fetchImpl(
          new URL(`api/permission-grants/${grant.id}/applied`, base),
          {
            method: 'POST',
            headers: {
              authorization: `Bearer ${token}`,
              accept: 'application/json',
            },
            signal: AbortSignal.timeout(requestTimeoutMs),
          },
        );
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        reported.push(grant.id);
      } catch (error) {
        log(
          `[permission-grants-failed] 反映済みを受付へ送れませんでした（次の周期で再送）: ${grant.id} ${(error as Error)?.message ?? ''}`,
        );
      }
    }
  }
  return {
    status: enabled ? 'enabled' : 'disabled',
    rules: used.map((g) => g.ruleId),
    unknown,
    results,
    reported,
  };
}

// 置き換えない理由などのお知らせは、前の周期と同じ内容なら出さない（1分ごとに同じ行を重ねない）。
function reportNotices(state: string, notices: string[], log: Log): void {
  const file = path.join(state, 'permission-grants-notices');
  const text = notices.map((line) => `${line}\n`).join('');
  let previous: string;
  try {
    previous = readText(file) ?? '';
  } catch {
    previous = '';
  }
  if (text === previous) return;
  for (const line of notices) log(`[permission-grants] ${line}`);
  try {
    if (text) writeAtomic(file, text);
    else fs.rmSync(file, { force: true });
  } catch {
    // お知らせの記録に失敗しても反映の結果は変えない。次の周期で同じ行をもう一度出す。
  }
}

// 直接起動したときだけ動く（テストの import では動かない）。リンクを経由した起動でも本体のパスで比べる。
const invokedPath = (): string => {
  try {
    return fs.realpathSync(process.argv[1]);
  } catch {
    return '';
  }
};
if (process.argv[1] && invokedPath() === fileURLToPath(import.meta.url)) {
  const result = await applyPermissionGrants();
  if (result.status === 'failed') process.exitCode = 1;
}
