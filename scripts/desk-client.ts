import {
  deskContractHeader,
  deskContractVersion,
} from './lib/desk-contract.generated.ts';
import { draftWaitText } from '../shared/src/desk-attention-summary.ts';
import { draftStatus as draftCheckStatus } from '../shared/src/desk-attention.mts';
import {
  checkContractResponse,
  showSplitChildren,
  failureCause,
  actionsText,
} from './lib/desk-contract-client.ts';
// The request scripts (create-issue.ts, reply-issue.ts, list-issues.ts and other shared clients)
// talk to the running intake service instead of Gitea: the Gitea token, the project allowlist and the
// ownership checks stay there. The ways to reach it:
// - On the intake's own PC (inside the intake's container on the NAS), with no connection given, each request
//   carries a one-use key written into the store right before it (desk-local.mjs), so nothing reusable is left
//   behind and no browser session is taken.
// - From another PC, --url and --token-file (or ISSUE_DESK_URL and ISSUE_DESK_TOKEN_FILE) send the PC's
//   token as a Bearer header. That path reads no admin settings, no SQLite and loads no local module but
//   desk-remote-config.ts and lib/desk-retry.ts, and uses only built-ins (fetch, node:fs, node:crypto). lib/desk-retry.ts and
//   lib/desk-contract.generated.ts are TypeScript, so this needs Node 24 or later (type stripping), not Node 18.
// - With neither given, K2's serverUrl and serverTokenFile select the PC token connection.
//   configuredRemote reads only the three local connection settings and never reads admin credentials.
import { randomUUID } from 'node:crypto';
import fs from 'node:fs';
import { configuredRemote, remoteBase } from './desk-remote-config.ts';
import { fetchDesk, retryableDeskCall } from './lib/desk-retry.ts';
import { positiveIssueNumbers } from './desk-target.ts';
import { handoffDeadline, handoffFirstRun } from './lib/desk-handoff.ts';

export {
  configFile,
  configuredRemote,
  remoteBase,
  remoteTokenName,
} from './desk-remote-config.ts';
// create-issue.ts が送る前に使う、閉じた参照の確認（Node の組み込みと desk-target.ts だけを読み込む）。
export {
  takeHistoryRead,
  closedReferenceCheck,
  draftProject,
} from './lib/closed-reference-notice.ts';

export const connectionUsage = `接続先: 他のPCからは --url <受付のURL> --token-file <トークンのファイル>
（または環境変数 ISSUE_DESK_URL・ISSUE_DESK_TOKEN_FILE。引数が優先）。両方とも無ければ、config.local.json に
K2 の serverUrl・serverTokenFile・workerId があればその受付へトークン接続し、無ければこのPC上の受付を一時キーで使います
（受付と同じPCで実行するときだけ。受付がNASのコンテナにあるときは、そのコンテナの中）。`;

const connectionFlags = new Map([
  ['--url', 'url'],
  ['--token-file', 'tokenFile'],
]);
// Takes --url and --token-file out of the arguments; each script parses the rest as before. A Map, so that
// a business value such as "constructor" or "toString" is never mistaken for one of them.
export function connectionArgs(argv: string[], usage = '') {
  const rest: string[] = [];
  const out: { url?: string; tokenFile?: string } = {};
  for (let i = 0; i < argv.length; i++) {
    const name = connectionFlags.get(argv[i]);
    if (!name) {
      rest.push(argv[i]);
      continue;
    }
    if (out[name] !== undefined)
      throw Error(`引数を確認してください: ${argv[i]}\n${usage}`);
    const value = argv[++i];
    if (value === undefined || value.startsWith('--') || !value.trim())
      throw Error(`${argv[i - 1]} の値がありません。\n${usage}`);
    out[name] = value;
  }
  return { argv: rest, ...out };
}

// A value of the printed next command, quoted for a POSIX shell (sh, bash, zsh). Single quotes, so $, `, &, ;, (,
// ), spaces and the like stay literal. Plain paths and URLs are printed as they are.
function shellQuote(value) {
  return /^[A-Za-z0-9_.:/@%+=,-]+$/.test(value)
    ? value
    : `'${value.replace(/'/g, `'\\''`)}'`;
}

// Arguments win over the environment, item by item. Neither given means the intake of the setting above
// (configured, a function for the tests), or else the local one-use key; only one of the two is an error,
// never a silent switch to the local intake.
export function connection(
  parsed: { url?: string; tokenFile?: string },
  env: NodeJS.ProcessEnv = process.env,
  configured: () => any = configuredRemote,
) {
  const quoted = shellQuote;
  const url = parsed.url ?? (env.ISSUE_DESK_URL || undefined);
  const tokenFile =
    parsed.tokenFile ?? (env.ISSUE_DESK_TOKEN_FILE || undefined);
  if (url === undefined && tokenFile === undefined) return configured();
  if (url === undefined || tokenFile === undefined)
    throw Error(
      `接続先の指定が不完全です。--url と --token-file（または ISSUE_DESK_URL と ISSUE_DESK_TOKEN_FILE）を両方指定してください。\n${connectionUsage}`,
    );
  const base = remoteBase(url);
  // The next command printed for an answer keeps what came from the arguments (the environment stays set).
  const hint = [
    parsed.url !== undefined ? ` --url ${quoted(base + '/')}` : '',
    parsed.tokenFile !== undefined
      ? ` --token-file ${quoted(parsed.tokenFile)}`
      : '',
  ].join('');
  return { base, tokenFile, hint };
}

// The token file: one line, written by scripts/intake-token.mjs. Its content is never printed.
function readToken(file) {
  let bytes;
  try {
    bytes = fs.readFileSync(file);
  } catch (e) {
    throw Error(
      `トークンファイルを読めません（${file}）: ${e.code || '読み取りエラー'}`,
    );
  }
  const text = bytes.toString('utf8');
  const token = text.replace(/^\uFEFF/, '').trim();
  if (!token) throw Error(`トークンファイルが空です（${file}）。`);
  if (!/^[A-Za-z0-9_-]+$/.test(token))
    throw Error(
      `トークンファイルの内容がトークンの形式ではありません（${file}）。`,
    );
  return token;
}

// A failure to reach the intake at all (no answer came back) carries unreachable: true, both here and in
// desk-local.mjs; an answer of the intake carries its status instead. consult() tells them apart by that.
const unreachable = (message) =>
  Object.assign(Error(message), { unreachable: true });
// What a restart of the intake looks like while a reply is awaited: no connection, or the gateway of
// Tailscale Serve answering 502・503・504 for it. consult() keeps asking through these until its deadline.
const transientFailure = (e) =>
  e?.unreachable === true || [502, 503, 504].includes(e?.status);

export function remoteDesk({ base, tokenFile }) {
  const token = readToken(tokenFile);
  const call = async (method, route, payload) => {
    let res;
    try {
      res = await fetchDesk(
        base + route,
        {
          method,
          // A redirect would carry the token to wherever it points; stop instead.
          redirect: 'manual',
          headers: {
            [deskContractHeader]: deskContractVersion,
            Authorization: `Bearer ${token}`,
            Accept: 'application/json',
            ...(payload ? { 'Content-Type': 'application/json' } : {}),
          },
          body: payload ? JSON.stringify(payload) : undefined,
        },
        retryableDeskCall(method, route, payload),
      );
    } catch (e) {
      throw unreachable(
        `受付サービス（${base}）へ接続できませんでした: ${e.cause?.code || e.message}`,
      );
    }
    if (
      res.type === 'opaqueredirect' ||
      (res.status >= 300 && res.status < 400)
    )
      throw Error(
        `受付サービス（${base}）が転送を返したため中止しました。トークンは転送先へ送りません。接続先URLを確認してください。`,
      );
    const value = await res.json().catch(() => ({}));
    checkContractResponse(res, value);
    return value;
  };
  return { call, close: () => {}, remote: true };
}

// Opens the connection chosen by connection(): the remote Bearer client, or the local one-use key, which is
// loaded only here so that the remote path never imports it.
export async function openDesk(conn = null) {
  if (conn) return remoteDesk(conn);
  if (typeof globalThis.issueDeskLocal === 'function')
    return globalThis.issueDeskLocal();
  throw Error('受付のURLとPCトークンのファイルを指定してください。');
}

// Arguments of the request scripts. A new request takes --project, an answer takes --draft. --priority is
// the level the user chose (high or normal) and only goes with a new request; without it the monitor's AI
// judges the level, as before. --tier light|standard|heavy is the consultation tier (Issue #430): the consultation
// runs on that tier of intake.tiers. A new request must carry it (create-issue.ts refuses one without it, and the
// intake answers 400 TIER_REQUIRED); the intake no longer judges a tier. With --draft it is optional, goes with the
// answer and changes the tier from the next turn (Issue #3088, POST /api/drafts/<id>/chat).
// --implementation-tier light|standard|heavy is the implementation tier the user chose (Issue #487), sent as
// implementationTier with a new request only; without it the intake decides the tier when it files the Issue.
// --priority-reason is the reason an agent gives for high (Issue #646, docs/scheduling.md): required with high,
// refused with anything else, 1 to 500 characters after trimming, and sent as priorityReason.
const implementationTierValues = ['light', 'standard', 'heavy'];
const priorityReasonMax = 500;
const notDuplicateMax = 500;
/** requestArgs が返す引数。値の形は検査の途中で変わる（--expect-revision は数、--replace-issues は番号の配列など）。 */
export interface RequestArgs {
  project?: string;
  provider?: string;
  consultationProvider?: string;
  draft?: string;
  text?: string;
  file?: string;
  timeout?: any;
  priority?: string;
  priorityReason?: string;
  tier?: string;
  implementationTier?: string;
  notDuplicate?: string;
  overSpec?: string;
  despitePending?: any;
  reason?: string;
  replaceIssues?: any;
  check?: string;
  sendId?: string;
  expectRevision?: any;
  readEvidenceFile?: string;
  readEvidence?: unknown;
  hold?: boolean;
  submit?: boolean;
  retry?: boolean;
}
export function requestArgs(argv: string[], usage: string): RequestArgs {
  const names = [
    'project',
    'provider',
    'consultation-provider',
    'draft',
    'text',
    'file',
    'timeout',
    'priority',
    'priority-reason',
    'tier',
    'implementation-tier',
    'not-duplicate',
    'over-spec',
    'despite-pending',
    'reason',
    'replace-issues',
    'check',
    'send-id',
    'expect-revision',
    'read-evidence-file',
  ];
  const out: RequestArgs = {};
  for (let i = 0; i < argv.length; i++) {
    const name = argv[i].match(/^--([a-z]+(?:-[a-z]+)*)$/)?.[1];
    // --implementation-tier is kept as implementationTier, the name the intake takes.
    const key = name?.replace(/-([a-z])/g, (_, c) => c.toUpperCase());
    // --hold, --submit and --retry take no value.
    if (
      (name === 'hold' || name === 'submit' || name === 'retry') &&
      out[key] === undefined
    ) {
      out[key] = true;
      continue;
    }
    if (!name || !names.includes(name) || out[key] !== undefined)
      throw Error(`引数を確認してください: ${argv[i]}\n${usage}`);
    const value = argv[++i];
    if (value === undefined || value.startsWith('--'))
      throw Error(`--${name} の値がありません。\n${usage}`);
    out[key] = value;
  }
  if (
    out.sendId !== undefined &&
    (!out.project ||
      out.draft ||
      !/^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/i.test(
        out.sendId,
      ))
  )
    throw Error('--send-id は新規相談にUUIDを指定してください。');
  if (out.sendId !== undefined) out.sendId = out.sendId.toLowerCase();
  // --check <下書きID> only reads the draft (Issue #747), so nothing that is sent goes with it.
  if (out.check !== undefined) {
    if (Object.keys(out).some((key) => !['check', 'timeout'].includes(key)))
      throw Error(
        `--check は下書きを読むだけなので、--timeout 以外と同時に指定できません。\n${usage}`,
      );
    if (!/^[A-Za-z0-9-]+$/.test(out.check))
      throw Error(`--check には下書きIDを指定してください。\n${usage}`);
    return out;
  }
  if (out.retry) {
    if (
      !out.draft ||
      Object.keys(out).some(
        (key) => !['retry', 'draft', 'timeout'].includes(key),
      )
    )
      throw Error(`--retry は --draft <下書きID> とだけ使えます。\n${usage}`);
    return out;
  }
  // --draft <ID> --submit confirms a held consultation: it sends no text and goes with nothing else.
  if (out.submit) {
    const others = Object.keys(out).filter(
      (key) => key !== 'submit' && key !== 'draft' && key !== 'timeout',
    );
    if (!out.draft || others.length)
      throw Error(
        `--submit は --draft <下書きID> とだけ使えます（本文・--tier・--hold などとは同時に指定できません）。\n${usage}`,
      );
    return out;
  }
  if (out.expectRevision !== undefined) {
    if (
      !out.draft ||
      !/^(0|[1-9][0-9]*)$/.test(out.expectRevision) ||
      !Number.isSafeInteger(Number(out.expectRevision))
    )
      throw Error(
        '--expect-revision は回答時に0以上の安全な整数を指定してください。',
      );
    out.expectRevision = Number(out.expectRevision);
  }
  if (out.readEvidenceFile !== undefined && (!out.project || out.draft))
    throw Error('--read-evidence-file は新規相談にだけ指定します。');
  if (out.hold && out.draft)
    throw Error(`--hold は新しい依頼（--project）にだけ指定します。\n${usage}`);
  if (out.draft && out.project)
    throw Error(`--draft と --project は同時に指定できません。\n${usage}`);
  if (!out.draft && !out.project) throw Error(usage);
  if (out.text && out.file)
    throw Error(`--text と --file は同時に指定できません。\n${usage}`);
  if (out.draft && out.priority !== undefined)
    throw Error(
      `--priority は新しい依頼（--project）にだけ指定します。\n${usage}`,
    );
  if (out.priority !== undefined && !['high', 'normal'].includes(out.priority))
    throw Error(
      `--priority は high（割り込み）か normal（通常）を指定してください。省略すると自動です。\n${usage}`,
    );
  if (out.draft && out.priorityReason !== undefined)
    throw Error(
      `--priority-reason は新しい依頼（--project）にだけ指定します。\n${usage}`,
    );
  if (out.priority !== 'high' && out.priorityReason !== undefined)
    throw Error(
      `--priority-reason は --priority high（割り込み）のときだけ指定します。\n${usage}`,
    );
  if (out.priority === 'high') {
    if (out.priorityReason === undefined)
      throw Error(
        `--priority high（割り込み）には --priority-reason で理由を添えてください（docs/scheduling.md『Agent Interrupts』）。\n${usage}`,
      );
    out.priorityReason = out.priorityReason.trim();
    const length = [...out.priorityReason].length;
    if (length < 1 || length > priorityReasonMax)
      throw Error(
        `--priority-reason は前後の空白を除いて1〜${priorityReasonMax}文字で指定してください。\n${usage}`,
      );
  }
  if (out.tier !== undefined && !implementationTierValues.includes(out.tier))
    throw Error(
      `--tier は light: 低、standard: 中、heavy: 高のどれかを指定してください。\n${usage}`,
    );
  // --not-duplicate <理由> is the sender's word that the request is no duplicate (the intake then skips its check
  // before the consultation, docs/intake-scope.md): a new request only, 1 to 500 characters after trimming.
  if (out.draft && out.notDuplicate !== undefined)
    throw Error(
      `--not-duplicate は新しい依頼（--project）にだけ指定します。\n${usage}`,
    );
  if (out.notDuplicate !== undefined) {
    out.notDuplicate = out.notDuplicate.trim();
    const length = [...out.notDuplicate].length;
    if (length < 1 || length > notDuplicateMax)
      throw Error(
        `--not-duplicate は前後の空白を除いて1〜${notDuplicateMax}文字の理由で指定してください。\n${usage}`,
      );
  }
  if (out.replaceIssues !== undefined) {
    if (out.draft)
      throw Error(
        '--replace-issues は新しい依頼（--project）にだけ指定できます。',
      );
    if (!out.project)
      throw Error('--replace-issues には --project が必要です。');
    out.replaceIssues = positiveIssueNumbers(out.replaceIssues);
  }
  if (out.draft && out.implementationTier !== undefined)
    throw Error(
      `--implementation-tier は新しい依頼（--project）にだけ指定します。\n${usage}`,
    );
  if (
    out.implementationTier !== undefined &&
    !implementationTierValues.includes(out.implementationTier)
  )
    throw Error(
      `--implementation-tier は light: 低、standard: 中、heavy: 高のどれかを指定してください。省略すると受付が判定します。\n${usage}`,
    );
  if (out.despitePending !== undefined || out.reason !== undefined) {
    if (
      !out.project ||
      out.draft ||
      out.despitePending === undefined ||
      out.reason === undefined
    )
      throw Error(
        `--despite-pending と --reason は新しい依頼（--project）に両方指定してください。\n${usage}`,
      );
    const reason = out.reason.trim();
    if (
      !/^[A-Za-z0-9-]+$/.test(out.despitePending) ||
      [...reason].length < 1 ||
      [...reason].length > 500
    )
      throw Error(
        `--despite-pending には下書きID、--reason には前後の空白を除いて1〜500文字を指定してください。\n${usage}`,
      );
    out.despitePending = { draft: out.despitePending, reason };
    delete out.reason;
  }
  return out;
}

// The request text: --text, --file, or standard input.
export function requestText(args: RequestArgs) {
  if (args.text !== undefined) return args.text.trim();
  if (args.file !== undefined) return fs.readFileSync(args.file, 'utf8').trim();
  if (process.stdin.isTTY) return '';
  try {
    return fs.readFileSync(0, 'utf8').trim();
  } catch {
    return '';
  }
}

// 新しいプロジェクトの相談へ引き継いだ相談（状態 project_handoff）の案内。
const handoffText = (d) =>
  `新しいプロジェクトの相談へ引き継ぎました。受付画面の #/projects/${d.projectHandoff?.id ?? '（引き継ぎ先不明）'} で続けてください（スクリプトからは操作できません）。`;

// Why the POST of consult() left no draft ID. An answer of the intake in 4xx is a refusal: nothing was
// taken. Anything else (no connection, 5xx) may still have reached it with only the reply lost, so the
// text sends the reader to the list of the intake before sending again.
function notStarted(e, draft) {
  const what = draft ? `下書き ${draft} への回答` : '依頼';
  const noId = draft ? '' : '（下書きIDはありません）';
  if (e.status >= 400 && e.status < 500)
    return `受付が断ったため、${what}は受け付けられていません${noId}。`;
  return `${what}を受付へ送れませんでした${noId}。応答が途中で切れただけで届いていることもあるため、送り直す前に受付の依頼一覧${draft ? `（下書き ${draft}）` : ''}で届いていないかを確かめてください。`;
}

// The read-only command that shows a draft again (--check only reads it; --draft would send an answer).
const checkCommand = (script, hint, id) =>
  `node ${script}${hint} --check ${id}`;

// The end of the wait when the result was not confirmed: the connection stayed lost until the deadline
// (expired), or the intake answered something other than a restart. `sent` is true after consult()'s POST,
// when the draft is known to be with the intake.
export function unconfirmed(id, e, expired, sent, script, hint) {
  console.log(
    [
      sent
        ? `\n下書き ${id} は受付済みです。結果は未確認です。`
        : `\n下書き ${id} の結果は未確認です。`,
      expired
        ? `期限まで受付から結果を受け取れませんでした（最後の失敗: ${e.message}）。`
        : `結果の確認を打ち切りました（${e.message}）。`,
      `送り直さず、受付画面の下書き ${id} か、読むだけの --check で確認してください: ${checkCommand(script, hint, id)}`,
    ].join('\n'),
  );
  return 2;
}

// The Issues filed from a draft, one line each by the common number and the title (Issue #1544). A split
// draft lists them all in issues; the title (and a common number the draft lacked) is read with GET
// /api/issue, which both the one-use key and a token can use. Only desk and title are used, so the summary
// (view=summary) is read instead of the full detail. An Issue that cannot be read is still listed, since it
// is filed; the Gitea URL is not printed.
export async function filedText(call, d) {
  const list = d.issues?.length ? d.issues : [d.issue];
  const lines = [];
  let known = 0;
  for (const i of list) {
    if (!i || i.number == null) {
      lines.push(
        '- 起票結果を確認できません。送り直さず --check で確認してください。',
      );
      continue;
    }
    known++;
    const project = i.project || d.project;
    let detail;
    try {
      detail = await call(
        'GET',
        `/api/issue?${new URLSearchParams({ project, number: String(i.number), view: 'summary' })}`,
      );
    } catch (e) {
      if (e?.nasHandoff && process.env.ISSUE_DESK_NAS_HANDOFF === 'read')
        throw e;
      detail = null;
    }
    const desk = i.desk ?? detail?.desk ?? null;
    const title =
      typeof detail?.title === 'string' && detail.title.trim()
        ? detail.title
        : '（題名を読めませんでした）';
    lines.push(
      desk === null
        ? `- 共通番号未確定（${project} の Gitea番号 ${i.number}） ${title}。送り直さず --check で確認してください。`
        : `- 共通番号${desk} ${title}`,
    );
  }
  return [
    known === list.length
      ? `起票しました（${known}件）:`
      : '起票結果（一部未確認）:',
    ...lines,
  ].join('\n');
}

// Waits for the reply on a draft (GET /api/drafts/<id>) and prints it; shared by consult() and checkDraft().
// 止まった・待っている相談の原因（Turn の段階と秘匿済みの原因、最後の呼び出しの診断）。
export function draftFailureText(d) {
  const failure = d.lastFailure;
  if (!failure?.cause || !(d.error || d.retryAt || d.status === 'submit_error'))
    return '';
  const kind = failure.kind ? `・種類 ${failure.kind}` : '';
  const lines = [
    `直近の失敗（段階 ${failure.stage || '不明'}${kind}・${failure.at || '時刻不明'}）: ${failure.cause}`,
  ];
  const diagnostic = d.aiDiagnostics?.at(-1);
  const value = (v) =>
    (typeof v === 'string' && v !== '') || typeof v === 'number' ? v : '不明';
  const hasDiagnosticId =
    (typeof diagnostic?.id === 'string' && diagnostic.id !== '') ||
    typeof diagnostic?.id === 'number';
  if (hasDiagnosticId)
    lines.push(
      `最後の呼び出しの診断: ${value(diagnostic.kind)}（段階 ${value(diagnostic.stage)}・終了コード ${diagnostic.exitCode == null ? 'なし' : value(diagnostic.exitCode)}・診断ID ${diagnostic.id}）`,
    );
  if (diagnostic?.kind === 'timeout_idle') {
    const records = d.inputBudget?.records;
    const lastInput = Array.isArray(records) ? records.at(-1) : undefined;
    const idle = diagnostic.idleProcess;
    lines.push(
      `無出力時間切れの詳細: ${hasDiagnosticId ? '' : `診断ID ${value(diagnostic.id)}・`}model ${value(diagnostic.model)}・effort ${value(diagnostic.effort)}・tier.level ${value(d.tier?.level)}`,
      `最後の入力記録: 送信入力 ${value(lastInput?.sent?.bytes)} bytes・全体入力 ${value(lastInput?.full?.total?.bytes)} bytes（いずれも入力に改行とスキーマを加えたバイト数）`,
      '最後の入力記録は最後の診断と同じ呼び出しに対応するとは限りません。',
      `強制終了直前: プロセス数 ${value(idle?.processes)}・子プロセス数 ${value(idle?.children)}・CPU時間 ${value(idle?.cpuMs)} ms・観測期間 ${value(idle?.windowMs)} ms・その間のCPU時間 ${value(idle?.cpuDeltaMs)} ms`,
      `観測期間中の読取 ${value(idle?.readBytesDelta)} bytes・書込 ${value(idle?.writeBytesDelta)} bytes・TCP確立接続数 ${value(idle?.established)}・その他のTCP接続数 ${value(idle?.otherConnections)}・送信キュー ${value(idle?.txQueueBytes)} bytes・受信キュー ${value(idle?.rxQueueBytes)} bytes`,
    );
  }
  return lines.join('\n');
}

// 起票前判定の各回の結果と理由。要約のない旧応答では空文字を返し、欠けた値は未記録と表示する。
export function filingCheckSummaryText(d) {
  const checks = d?.filingCheckSummary?.checks;
  if (!Array.isArray(checks) || checks.length === 0) return '';
  const label = {
    refused: '拒否',
    evidence_requested: '資料追加の要求',
    ai_failed: 'AI失敗',
    invalid_answer: '回答不正',
    allowed: '許可',
  };
  const count = d.filingCheckSummary.count ?? '未記録';
  const lines = [
    `起票前の判定（現在の版 ${d.filingCheckSummary.revision ?? '未記録'}・${count} 回）:`,
  ];
  checks.forEach((check, index) =>
    lines.push(
      `${index + 1}. 版 ${check?.revision ?? '未記録'}・${label[check?.result] ?? '未記録'}: ${check?.reason ?? '未記録'}`,
    ),
  );
  return lines.join('\n');
}

// A restart of the intake (transientFailure) is waited through until the deadline.
// The wait polls GET /api/drafts/<id>?view=summary (the state only) and reads the full draft once when the
// wait is over or the deadline has passed. An intake that ignores view answers with the full draft, which
// is then used as it is. For the first 30 seconds of the wait it reads every `interval` (3 seconds), then
// every two intervals (6 seconds): a reply takes minutes, and each read on the NAS costs a key issue and revoke.
const quickWaitMs = 30000;
export async function awaitDraft(
  call,
  id,
  timeout,
  sent,
  script,
  hint,
  interval,
  linkContext,
) {
  const duration = Number(timeout || 900) * 1000;
  const deadline = sent ? Date.now() + duration : handoffDeadline(duration);
  const pending = (v) =>
    ['thinking', 'auto_ready', 'submitting'].includes(v.status) &&
    !v.consultationBudget?.stopped;
  // view=summary を知らない受付は全文を返すので、要約の印がある応答だけを要約として扱う。
  const summary = (v) => v?.view === 'summary';
  // 読取の結果: 値（value）、待機を終える終了コード（exit）、期限まで待てる一時的な失敗（failure）。
  const read = async (
    route,
  ): Promise<{ value?: any; exit?: number; failure?: any }> => {
    try {
      return { value: await call('GET', route) };
    } catch (e) {
      if (e?.nasHandoff) {
        if (!sent && process.env.ISSUE_DESK_NAS_HANDOFF === 'read')
          return { exit: 75 };
        return { exit: unconfirmed(id, e, false, sent, script, hint) };
      }
      if (
        ['read', 'notice'].includes(process.env.ISSUE_DESK_NAS_HANDOFF) &&
        e?.status === 503 &&
        e?.code &&
        e.code !== 'TEMPORARY_UNAVAILABLE'
      )
        return { exit: unconfirmed(id, e, false, sent, script, hint) };
      if (!transientFailure(e))
        return { exit: unconfirmed(id, e, false, sent, script, hint) };
      return { failure: e };
    }
  };
  let d;
  let failure = null;
  let firstRead = !sent && handoffFirstRun();
  do {
    if (!firstRead && Date.now() >= deadline) {
      if (d && !failure) break;
      return unconfirmed(
        id,
        failure || Error('待機期限を超えました。'),
        true,
        sent,
        script,
        hint,
      );
    }
    // 待つ間は要約だけを読み、待ちが解けた要約の次は間隔を空けずに全文を読む。
    const settled = d && !failure && summary(d) && !pending(d);
    // 待ち始め（窓口の再起動をまたいでも同じ時刻）から30秒を過ぎたら間隔を2倍にする。
    const pause =
      Date.now() - (deadline - duration) < quickWaitMs
        ? interval
        : interval * 2;
    // 最初の読取は期限が残る限り必ず行う。間隔より短い期限では先に読む。
    if (!settled && (d || failure || pause < deadline - Date.now()))
      await new Promise((r) =>
        setTimeout(r, Math.min(pause, Math.max(0, deadline - Date.now()))),
      );
    if (!firstRead && Date.now() >= deadline) {
      if (d && !failure) break;
      return unconfirmed(
        id,
        failure || Error('待機期限を超えました。'),
        true,
        sent,
        script,
        hint,
      );
    }
    firstRead = false;
    const result = await read(
      `/api/drafts/${id}${settled ? '' : '?view=summary'}`,
    );
    if (result.exit !== undefined) return result.exit;
    failure = result.failure ?? null;
    if (!failure) d = result.value;
  } while ((failure || pending(d) || summary(d)) && Date.now() < deadline);
  if (failure) return unconfirmed(id, failure, true, sent, script, hint);
  // 期限切れで要約しか持っていないときは、表示に使う全文を1回読む。
  if (summary(d)) {
    const result = await read(`/api/drafts/${id}`);
    if (result.exit !== undefined) return result.exit;
    if (result.failure)
      return unconfirmed(id, result.failure, true, sent, script, hint);
    d = result.value;
  }

  const reply = [...(d.messages || [])]
    .reverse()
    .find((m) => m.role === 'assistant')?.text;
  console.log(
    `下書き: ${d.id}\n状態: ${draftCheckStatus(d)}\n版: ${d.revision}`,
  );
  if (draftWaitText(d, draftCheckStatus(d)))
    console.log(draftWaitText(d, draftCheckStatus(d)));
  if (reply) console.log(`\n受付AI:\n${reply}`);
  if (d.questions?.length)
    console.log(
      `\n未確認:\n${d.questions.map((q, i) => `${i + 1}. ${q}`).join('\n')}`,
    );
  // 最終読取が期限をまたいでも、結果の詳細を読むための猶予を確保する。
  const detailDeadline = Math.max(deadline, Date.now() + 5000);
  const readWithinDeadline = async (...args) => {
    if (Date.now() >= detailDeadline) throw Error('待機期限を超えました');
    return call(...args);
  };
  try {
    if (d.issue || d.issues?.length)
      console.log(`\n${await filedText(readWithinDeadline, d)}`);
    const childCount = await showSplitChildren(
      readWithinDeadline,
      d,
      console.log,
      filedText,
    );
    if (d.status === 'submit_error') {
      console.log(
        `起票失敗の原因: ${failureCause(d)}\n操作: ${actionsText(d)}`,
      );
      if (childCount || d.issues?.length || d.splitIssues?.length)
        console.log(
          '全件の起票は完了していません。送り直さず --check で確認してください。',
        );
      else if (!d.issue) console.log('起票されていないことを確認しました。');
    }
  } catch (e) {
    if (e?.nasHandoff && !sent && process.env.ISSUE_DESK_NAS_HANDOFF === 'read')
      return 75;
    throw e;
  }
  if (d.recovery && !d.consultationBudget?.stopped)
    console.log(`対処（${d.code}）: ${d.recovery.guidance}`);
  if (d.error) console.log(`\nエラー: ${d.error}`);
  if (draftFailureText(d)) console.log(draftFailureText(d));
  if (!sent && filingCheckSummaryText(d))
    console.log(`\n${filingCheckSummaryText(d)}`);
  const stillPending = pending(d);
  if (
    stillPending ||
    d.status === 'submit_unknown' ||
    (d.status === 'submitted' && !d.issue && !d.issues?.length)
  )
    console.log(
      `\n${stillPending ? '応答待ちのまま待機を終了しました。' : ''}起票結果は未確認です。送り直さず、読むだけの --check で確認してください: ${checkCommand(script, hint, d.id)}`,
    );
  if (d.consultationBudget?.stopped)
    console.log(
      `\n${(await import('./lib/consultation-link-output.generated.ts')).stoppedConsultationText(d, linkContext)}`,
    );
  else if (d.status === 'project_handoff') console.log(`\n${handoffText(d)}`);
  else if (
    !stillPending &&
    d.status === 'draft' &&
    !d.issue &&
    !d.issues?.length
  ) {
    if (d.hold === true && d.actions?.includes('submit'))
      console.log(
        `\n確定して起票: node ${script}${hint} --draft ${d.id} --submit`,
      );
    if (d.actions?.includes('chat'))
      console.log(
        `\n続きを回答: node ${script}${hint} --draft ${d.id} --expect-revision ${d.revision} --text "<回答>"`,
      );
  }

  return d.error || d.status === 'submit_error' ? 1 : 0;
}

// --check <下書きID>: reads the draft and waits for its reply like consult(), sending nothing. This is how a
// result left unconfirmed (exit code 2) is checked without answering again.
export async function checkDraft(
  call,
  { check, timeout }: RequestArgs,
  script,
  hint = '',
  interval = 3000,
  linkContext = {},
) {
  console.log(`下書き: ${check}`);
  return awaitDraft(
    call,
    check,
    timeout,
    false,
    script,
    hint,
    interval,
    linkContext,
  );
}

// Starts a request (project + provider, POST /api/issues) or answers the intake AI's questions on an
// existing one (draft), then waits for the reply. The intake AI files the Issue itself once nothing is
// missing. The reply is produced in the background and every stage is persisted, so a timeout here only
// ends the wait, never the consultation. `script` is the command printed for the next answer, and `hint`
// the connection arguments it keeps.
// Exit codes (Issue #747): 0 the result was received (also when the wait ended with the reply still being
// written), 1 the request did not reach the intake or the intake AI reported an error, 2 the draft is with
// the intake but its result could not be confirmed. The draft ID is printed as soon as the POST succeeds, so
// a connection lost afterwards never looks like a request that was not sent (and is not sent twice).
export async function consult(
  call,
  {
    project,
    provider,
    consultationProvider,
    draft,
    text,
    timeout,
    priority,
    priorityReason,
    tier,
    implementationTier,
    notDuplicate,
    despitePending,
    replaceIssues,
    hold,
    sendId,
    expectRevision,
    readEvidence,
  }: RequestArgs,
  script,
  hint = '',
  interval = 3000,
  linkContext = {},
) {
  const id = sendId ?? randomUUID();
  if (!draft) console.log(`送信ID: ${id}`);
  let started;
  if (draft && expectRevision === undefined) {
    // 版だけを使うので要約で読む。
    const current = await call('GET', `/api/drafts/${draft}?view=summary`);
    if (!Number.isSafeInteger(current?.revision) || current.revision < 0)
      throw Error('現在の版を取得できないため回答を送信しません。');
    expectRevision = current.revision;
  }
  try {
    started = draft
      ? await call('POST', `/api/drafts/${draft}/chat`, {
          id,
          messageId: id,
          text,
          expectedRevision: expectRevision,
          ...(tier ? { tier } : {}),
        })
      : await call('POST', '/api/issues', {
          project,
          provider,
          ...(consultationProvider ? { consultationProvider } : {}),
          id,
          text,
          ...(readEvidence !== undefined ? { readEvidence } : {}),
          ...(priority ? { priority } : {}),
          ...(priorityReason ? { priorityReason } : {}),
          ...(tier ? { tier } : {}),
          ...(implementationTier ? { implementationTier } : {}),
          ...(notDuplicate ? { notDuplicate } : {}),
          ...(despitePending ? { despitePending } : {}),
          ...(replaceIssues ? { replaceIssues } : {}),
          ...(hold ? { hold: true } : {}),
        });
  } catch (e) {
    if (draft && e.status === 409) {
      console.error(e.message);
      try {
        const latest = await call('GET', `/api/drafts/${draft}`);
        console.error(
          `最新版: ${latest.revision}\n${latest.questions?.length ? latest.questions.map((q, i) => `${i + 1}. ${q}`).join('\n') : '最新の質問はありません。'}`,
        );
      } catch {
        console.error('最新質問は取得できず未確認です。');
      }
      return 1;
    }
    // 重複の疑いで断られたとき（nothing was taken, no draft ID）は、受付の本文（候補と案内）をそのまま表示する。
    if (
      e.status === 409 &&
      ['duplicate_suspected', 'pending_similar_draft'].includes(e.code)
    ) {
      console.error(e.message);
      return 1;
    }
    if (!draft && e.status === 409 && e.code === 'send_id_conflict')
      throw Error(e.message);
    if (!draft && !(e.status >= 400 && e.status < 500))
      throw Error(
        `${e.message}\n応答を確認できません。${sendId ? '同じコマンドで再実行してください。' : `同じ引数に --send-id ${id} を付けて再実行してください。`}`,
      );
    throw Error(`${e.message}\n${notStarted(e, draft)}`);
  }
  console.log(`下書き: ${started.id}`);
  return awaitDraft(
    call,
    started.id,
    timeout,
    true,
    script,
    hint,
    interval,
    linkContext,
  );
}
