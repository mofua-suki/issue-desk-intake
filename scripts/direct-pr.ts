// 直接PRの作成（pr_create）・マージ（merge_manual）を受付の /api/v1/requests へ依頼し、同じIDで結果まで待つ。
// 手順と条件は skills/merge-manual-pr/SKILL.md・refs/request-api.md。依頼JSONは送る前に --save へ保存し、
// 結果が不明なときは同じファイルで resume する（IDを作り直さない）。項目の決まりは shared/src/request-rules.ts と同じ。
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { isDeepStrictEqual } from 'node:util';
import {
  openDesk,
  connectionArgs,
  connection,
  connectionUsage,
  configuredRemote,
} from './desk-client.ts';
import { invokedAs } from './lib/invoked.ts';
import {
  newRequestId,
  patchHash,
  requestIdPattern,
  requestPayload,
  sha40,
  validRepoName,
} from '../shared/src/request-rules.ts';
import {
  applyChoiceApproval,
  approvalInstructionAt,
  choiceApprovalGuidance,
  instructionAtGuidance,
  meaningfulApprovalStatement,
} from '../shared/src/approval-statement.ts';

const usage = `使い方:
  direct-pr.ts create --repo <owner/repo> --save <依頼JSONの絶対パス> --title <題> --body-file <PR本文のファイル>
                      (--branch <pushしたブランチ> --expect-head <pushした40桁> | --patch-file <パッチ> --base <40桁の基点SHA>)
                      [--approval <利用者の発言の原文> | --approval-question <見せた質問文> --approval-answer <記号>] [--instruction-at <日時>]
  direct-pr.ts merge  --repo <owner/repo> --save <依頼JSONの絶対パス> --number <PR番号> --head <40桁>
                      [--approval <利用者の発言の原文> | --approval-question <見せた質問文> --approval-answer <記号>] [--instruction-at <日時>]
                      [--approval-request <承認を付けた作成依頼のID>]
  direct-pr.ts resume --save <保存した依頼JSON> [--expect-head <40桁。ブランチ方式の作成では必須>]
共通: [--wait-minutes <1〜720。既定60>] [--interval-seconds <5〜600。既定15>]
--save はホームの下（実パスで判定）の、新しい依頼では存在しないパスを指定し、送る前にそこへ保存します。結果が不明・待ち切れなかったときは同じ --save で resume します。
承認（--approval か --approval-question・--approval-answer）は保護対象の個別承認が要るマージだけに付け、--instruction-at を必ず添えます。
PR番号が決まる前の承認（事前承認）は create に付け、マージでは承認の代わりに --approval-request へその作成依頼のIDを指定します。
${choiceApprovalGuidance}
${instructionAtGuidance}
終了コード: 0 作成・マージの完了を照合できた、1 拒否・失敗・取消・照合の不一致・読取不能、2 引数の誤り、75 待ち切れなかった（未完了）。
${connectionUsage}`;

const flagNames: Record<string, string[]> = {
  create: [
    'repo',
    'save',
    'title',
    'body-file',
    'branch',
    'patch-file',
    'base',
    'expect-head',
    'approval',
    'approval-question',
    'approval-answer',
    'instruction-at',
  ],
  merge: [
    'repo',
    'save',
    'number',
    'head',
    'approval',
    'approval-question',
    'approval-answer',
    'instruction-at',
    'approval-request',
  ],
  resume: ['save', 'expect-head'],
};
const common = ['wait-minutes', 'interval-seconds'];

export class UsageError extends Error {}
const refuse = (message: string): never => {
  throw new UsageError(`${message}\n${usage}`);
};

// 厳密なUTF-8として読む（不正なバイトを置換文字に変えて送らない）。先頭のBOMは除く。
function readText(file: string, label: string) {
  let bytes: Buffer;
  try {
    bytes = fs.readFileSync(file);
  } catch (e) {
    return refuse(`${label}（${file}）を読めません: ${e.code || e.message}`);
  }
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes);
  } catch {
    return refuse(`${label}（${file}）が正しいUTF-8ではありません。`);
  }
}

// 窓口はコンテナ（--rm）で動き、ホームだけを同じパスで見せる。ホームの外に保存すると消えるので、実パスで断る。
function underHome(file: string, home: string) {
  let base = file;
  const rest: string[] = [];
  while (!fs.existsSync(base)) {
    const parent = path.dirname(base);
    if (parent === base) return false;
    rest.unshift(path.basename(base));
    base = parent;
  }
  let real: string, root: string;
  try {
    real = path.join(fs.realpathSync(base), ...rest);
    root = fs.realpathSync(home);
  } catch {
    return false;
  }
  const relative = path.relative(root, real);
  return (
    relative !== '' &&
    relative !== '..' &&
    !relative.startsWith(`..${path.sep}`) &&
    !path.isAbsolute(relative)
  );
}

function bounded(
  value: string | undefined,
  fallback: number,
  min: number,
  max: number,
  name: string,
) {
  if (value === undefined) return fallback;
  const n = Number(value);
  if (!/^\d+$/.test(value) || n < min || n > max)
    refuse(`--${name} は ${min}〜${max} の整数です。`);
  return n;
}

interface Plan {
  command: 'create' | 'merge' | 'resume';
  save: string;
  request?: any;
  expectHead?: string;
  waitMs: number;
  intervalMs: number;
}

// 引数から送る依頼（resumeでは保存先だけ）を作る。接続より前に全部確かめる。
export function parseArgs(
  argv: string[],
  now = Date.now(),
  home = os.homedir(),
): Plan {
  const command = argv[0] as Plan['command'];
  if (!Object.hasOwn(flagNames, command))
    refuse('create・merge・resume のどれかを先頭に指定してください。');
  const allowed = new Set([...flagNames[command], ...common]);
  const flags: Record<string, string> = {};
  for (let i = 1; i < argv.length; i += 2) {
    const name = argv[i]?.startsWith('--') ? argv[i].slice(2) : '';
    const value = argv[i + 1];
    if (!allowed.has(name))
      refuse(`${command} では使えない引数です: ${argv[i]}`);
    if (Object.hasOwn(flags, name)) refuse(`--${name} が重なっています。`);
    if (value === undefined || value.startsWith('--'))
      refuse(`--${name} の値がありません。`);
    flags[name] = value;
  }
  if (!flags.save || !path.isAbsolute(flags.save))
    refuse('--save に依頼JSONの絶対パスを指定してください。');
  if (
    !underHome(path.resolve(flags.save), home) ||
    !underHome(path.dirname(path.resolve(flags.save)), home)
  )
    refuse(
      `--save（${flags.save}）とその親はホーム（${home}）の下に置いてください。窓口のコンテナの中に保存されて消えるためです。`,
    );
  const plan: Plan = {
    command,
    save: path.resolve(flags.save),
    waitMs: bounded(flags['wait-minutes'], 60, 1, 720, 'wait-minutes') * 60_000,
    intervalMs:
      bounded(flags['interval-seconds'], 15, 5, 600, 'interval-seconds') * 1000,
  };
  if (flags['expect-head'] !== undefined) {
    if (!sha40.test(flags['expect-head']))
      refuse('--expect-head は小文字40桁のSHAです。');
    plan.expectHead = flags['expect-head'];
  }
  if (command === 'resume') {
    const saved = loadSaved(plan.save);
    if (saved.kind === 'merge_manual' && plan.expectHead)
      refuse(
        'マージの依頼では --expect-head を使いません（head は依頼に入っています）。',
      );
    if (
      saved.kind === 'pr_create' &&
      saved.payload.branch !== undefined &&
      !plan.expectHead
    )
      refuse(
        'ブランチ方式の作成を resume するときは --expect-head（pushした40桁）が必須です。',
      );
    return plan;
  }
  if (fs.existsSync(plan.save))
    refuse(
      `--save（${plan.save}）は既にあります。同じ依頼の続きは resume --save で読み、新しい依頼には別のパスを指定してください。`,
    );
  if (!validRepoName(flags.repo))
    refuse('--repo に owner/name を指定してください。');
  let payload: any;
  if (command === 'create') {
    if (!flags.title || !flags['body-file'])
      refuse('--title と --body-file を指定してください。');
    const body = readText(flags['body-file'], 'PR本文');
    if ((flags.branch === undefined) === (flags['patch-file'] === undefined))
      refuse('--branch か --patch-file のどちらか1つを指定してください。');
    if (flags.branch !== undefined && flags.base !== undefined)
      refuse('--base は --patch-file と一緒に指定します。');
    if (flags.branch !== undefined && !plan.expectHead)
      refuse('ブランチ方式では --expect-head（pushした40桁）が必須です。');
    if (flags.branch === undefined && plan.expectHead)
      refuse(
        '--expect-head はブランチ方式で使います（パッチ方式は patchSha256 で照合します）。',
      );
    if (flags.branch !== undefined)
      payload = { branch: flags.branch, title: flags.title, body };
    else {
      const patch = readText(flags['patch-file'], 'パッチ');
      payload = {
        title: flags.title,
        body,
        patch,
        base: flags.base,
        patchSha256: patchHash(patch),
      };
    }
  } else {
    if (!/^[1-9]\d*$/.test(flags.number ?? ''))
      refuse('--number に正のPR番号を指定してください。');
    payload = { number: Number(flags.number), head: flags.head };
    if (flags['approval-request'] !== undefined) {
      if (
        flags.approval !== undefined ||
        flags['approval-question'] !== undefined ||
        flags['approval-answer'] !== undefined
      )
        refuse(
          '--approval-request は承認（--approval か --approval-question・--approval-answer）と同時に指定できません。',
        );
      payload.approvalRequest = flags['approval-request'];
    }
  }
  Object.assign(payload, approvalPayload(flags, now));
  const kind = command === 'create' ? 'pr_create' : 'merge_manual';
  const checked = requestPayload(kind, flags.repo, payload);
  if (checked.problem) refuse(checked.problem);
  plan.request = {
    id: newRequestId(now),
    kind,
    repo: flags.repo,
    payload: checked.payload,
  };
  return plan;
}

// 承認（--approval か --approval-question・--approval-answer）と指示日時を payload の項目にする。無ければ空。
function approvalPayload(flags: Record<string, string>, now: number) {
  try {
    applyChoiceApproval(flags, 'approval');
  } catch (e) {
    refuse(e.message);
  }
  const approval = flags.approval;
  if (approval === undefined) {
    if (flags['instruction-at'] !== undefined)
      refuse(
        '--instruction-at は承認（--approval か --approval-question・--approval-answer）と一緒に指定します。',
      );
    return {};
  }
  if (!meaningfulApprovalStatement(approval))
    refuse(
      '--approval には利用者の承認の発言の原文（8文字以上、定型語だけでないもの）を指定してください。',
    );
  if ([...approval.trim()].length > 500)
    refuse('--approval は500文字以内です。');
  if (flags['instruction-at'] === undefined)
    refuse(
      '承認を付けるときは --instruction-at（利用者がその発言をした日時）が必須です。',
    );
  try {
    return {
      approval: approval.trim(),
      instructionAt: approvalInstructionAt(flags['instruction-at'], now),
    };
  } catch (e) {
    return refuse(e.message);
  }
}

// 受付が認証・照合から足してよい項目か。運用代行トークンの承認には approvalDelegated、
// 事前承認を指すマージ（approvalRequest）には作成依頼から写した approval・instructionAt・approvalDelegated。
function deskAddition(sent: any, key: string, value: any) {
  const preapproved =
    sent.kind === 'merge_manual' && sent.payload.approvalRequest;
  if (key === 'approvalDelegated')
    return (
      value === true && (Boolean(sent.payload.approval) || Boolean(preapproved))
    );
  return (
    Boolean(preapproved) &&
    ['approval', 'instructionAt'].includes(key) &&
    typeof value === 'string' &&
    value.trim() !== ''
  );
}

// 保存した依頼を読む。形が崩れていれば送らない。
function loadSaved(file: string) {
  let doc: any;
  try {
    doc = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (e) {
    return refuse(
      `保存した依頼（${file}）を読めません: ${e.code || e.message}`,
    );
  }
  if (
    !doc ||
    typeof doc !== 'object' ||
    !requestIdPattern.test(doc.id) ||
    !['pr_create', 'merge_manual'].includes(doc.kind) ||
    !validRepoName(doc.repo) ||
    Object.keys(doc).sort().join(',') !== 'id,kind,payload,repo'
  )
    refuse(
      `保存した依頼（${file}）が pr_create・merge_manual の依頼の形ではありません。`,
    );
  const checked = requestPayload(doc.kind, doc.repo, doc.payload);
  if (checked.problem) refuse(`保存した依頼が不正です: ${checked.problem}`);
  return doc;
}

// 受付の記録が送った依頼と同じか。受付が足してよい項目は deskAddition。
function mismatch(sent: any, record: any): string | null {
  if (!record || typeof record !== 'object')
    return '応答に request がありません';
  for (const key of ['id', 'kind', 'repo'])
    if (record[key] !== sent[key]) return `${key} が送った値と違います`;
  const payload = record.payload;
  if (!payload || typeof payload !== 'object') return 'payload がありません';
  for (const [key, value] of Object.entries(sent.payload))
    if (!isDeepStrictEqual(payload[key], value))
      return `payload.${key} が送った値と違います`;
  for (const key of Object.keys(payload)) {
    if (Object.hasOwn(sent.payload, key)) continue;
    if (deskAddition(sent, key, payload[key])) continue;
    return `payload.${key} は送っていない項目です`;
  }
  return null;
}

// 完了した依頼の結果を判定する。成功の条件に欠けがあれば成功にしない。
function judge(
  sent: any,
  record: any,
  expectHead?: string,
): { ok: boolean; message: string } {
  const result = record.result;
  if (!result || typeof result !== 'object')
    return {
      ok: false,
      message: '完了したのに result がありません（読取失敗として扱います）',
    };
  const status = result.status;
  if (['refused', 'failed', 'cancelled'].includes(status))
    return {
      ok: false,
      message: `${status}: ${result.message ?? result.reason ?? '理由の記録なし'}`,
    };
  if (sent.kind === 'pr_create') {
    if (status !== 'created')
      return { ok: false, message: `未知の結果です: ${status}` };
    if (
      !Number.isSafeInteger(result.number) ||
      result.number < 1 ||
      typeof result.url !== 'string' ||
      !result.url ||
      !sha40.test(result.head ?? '')
    )
      return {
        ok: false,
        message: 'PRの番号・URL・40桁のheadのどれかが欠けています',
      };
    if (expectHead && result.head !== expectHead)
      return {
        ok: false,
        message: `作られたPRのhead ${result.head} が期待した ${expectHead} と違います`,
      };
    if (
      sent.payload.patchSha256 &&
      result.patchSha256 !== sent.payload.patchSha256
    )
      return {
        ok: false,
        message: 'パッチのSHA256が結果に無いか、送ったものと違います',
      };
    return {
      ok: true,
      message: `PR #${result.number} を作りました（head ${result.head}）: ${result.url}`,
    };
  }
  if (status !== 'merged')
    return { ok: false, message: `未知の結果です: ${status}` };
  for (const key of ['number', 'head'])
    if (result[key] !== undefined && result[key] !== sent.payload[key])
      return { ok: false, message: `結果の ${key} が依頼と違います` };
  return {
    ok: true,
    message: `PR #${sent.payload.number}（head ${sent.payload.head}）をマージしました`,
  };
}

const denied = (e: any) => [401, 403].includes(e?.status);

interface Deps {
  env?: NodeJS.ProcessEnv;
  configured?: () => any;
  open?: typeof openDesk;
  log?: (line: string) => void;
  err?: (line: string) => void;
  sleep?: (ms: number) => Promise<void>;
  now?: () => number;
}

async function directPr(
  call: (method: string, route: string, payload?: any) => Promise<any>,
  plan: Plan,
  deps: Deps = {},
) {
  const log = deps.log ?? console.log;
  const err = deps.err ?? console.error;
  const sleep =
    deps.sleep ?? ((ms) => new Promise((resolve) => setTimeout(resolve, ms)));
  const now = deps.now ?? Date.now;
  let doc = plan.request;
  let send = true;
  if (plan.command === 'resume') {
    doc = loadSaved(plan.save);
    try {
      const found = (await call('GET', `/api/v1/requests/${doc.id}`))?.request;
      const problem = mismatch(doc, found);
      if (problem) {
        err(
          `受付の記録が保存した依頼と合いません（${problem}）。送り直しません。`,
        );
        return 1;
      }
      send = false;
    } catch (e) {
      if (e?.status !== 404) {
        err(
          `依頼 ${doc.id} を読めません（状態コード ${e?.status ?? '不明'}）: ${e?.message ?? e}`,
        );
        return denied(e) ? 1 : 75;
      }
      err(
        `受付に依頼 ${doc.id} の記録が無いため、保存した同じ本文で送ります。`,
      );
    }
  } else {
    fs.mkdirSync(path.dirname(plan.save), { recursive: true });
    fs.writeFileSync(plan.save, JSON.stringify(doc), {
      flag: 'wx',
      mode: 0o600,
    });
    err(`依頼 ${doc.id}（${doc.kind}）を ${plan.save} に保存しました。`);
  }
  if (send) {
    try {
      const problem = mismatch(
        doc,
        (await call('POST', '/api/v1/requests', doc))?.request,
      );
      if (problem) {
        err(
          `受付の応答が送った依頼と合いません（${problem}）。依頼 ${doc.id} は受付に残っている可能性があります。新しい依頼を作らず、同じIDで読み（GET /api/v1/requests/${doc.id}）、要らなければ取り消してください（refs/request-api.md）。`,
        );
        return 1;
      }
    } catch (e) {
      err(
        `依頼 ${doc.id} を送れませんでした（状態コード ${e?.status ?? '不明'}）: ${e?.message ?? e}`,
      );
      if (e?.status && !(e.status >= 500)) return 1;
      err(
        `受理されたかは不明です。resume --save ${plan.save} で同じIDを照合してください。`,
      );
      return 75;
    }
  }
  const deadline = now() + plan.waitMs;
  let last = '';
  for (;;) {
    let record: any;
    try {
      record = (await call('GET', `/api/v1/requests/${doc.id}`))?.request;
      const problem = mismatch(doc, record);
      if (problem) {
        err(`受付の記録が送った依頼と合いません（${problem}）。`);
        return 1;
      }
    } catch (e) {
      if (denied(e) || e?.status === 404) {
        err(
          `依頼 ${doc.id} を読めません（状態コード ${e.status}）: ${e.message}`,
        );
        return 1;
      }
      err(
        `読取に失敗しました（成功・未処理とは扱いません。読み直します）: ${e?.message ?? e}`,
      );
      record = null;
    }
    if (record?.status === 'done') {
      const verdict = judge(doc, record, plan.expectHead);
      err(verdict.message);
      log(
        JSON.stringify({
          id: doc.id,
          kind: doc.kind,
          repo: doc.repo,
          status: record.status,
          result: record.result,
        }),
      );
      return verdict.ok ? 0 : 1;
    }
    if (record && !['queued', 'claimed'].includes(record.status)) {
      err(`未知の依頼の状態です: ${record.status}`);
      return 1;
    }
    const state = record
      ? `${record.status}${record.claimedBy ? `（${record.claimedBy}）` : ''}`
      : '';
    if (state && state !== last) {
      err(`依頼 ${doc.id}: ${state}`);
      last = state;
    }
    if (now() + plan.intervalMs > deadline) {
      err(
        `待ち時間を過ぎました（未完了。依頼は受付に残っています）。続きは resume --save ${plan.save} で同じIDを読みます。`,
      );
      return 75;
    }
    await sleep(plan.intervalMs);
  }
}

export async function runCommand(
  argv: string[],
  {
    env = process.env,
    configured = configuredRemote,
    open = openDesk,
    home = os.homedir(),
    ...deps
  }: Deps & { home?: string } = {},
) {
  let parsed: ReturnType<typeof connectionArgs>;
  try {
    parsed = connectionArgs(argv, '');
  } catch (e) {
    return refuse(e.message.trim());
  }
  const plan = parseArgs(parsed.argv, (deps.now ?? Date.now)(), home);
  let conn: any;
  try {
    conn = connection(parsed, env, configured);
  } catch (e) {
    // 引数・環境変数で接続先を指定した誤りは引数の誤り。設定ファイルの読取失敗はそのまま。
    if (
      parsed.url !== undefined ||
      parsed.tokenFile !== undefined ||
      env.ISSUE_DESK_URL ||
      env.ISSUE_DESK_TOKEN_FILE
    )
      refuse(e.message);
    throw e;
  }
  const desk = await open(conn);
  try {
    return await directPr(desk.call, plan, deps);
  } finally {
    await desk.close();
  }
}

if (invokedAs(import.meta.url, process.argv[1]))
  runCommand(process.argv.slice(2)).then(
    (code) => {
      process.exitCode = code;
    },
    (error) => {
      console.error(error.message);
      process.exitCode = error instanceof UsageError ? 2 : 1;
    },
  );
