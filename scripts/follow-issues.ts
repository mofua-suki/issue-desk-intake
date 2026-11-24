// 起票した依頼をマージ・クローズまで機械的に見張り、利用者の対処が要る変化・停滞だけを1行ずつ出す。
// 判断はしない。Claude Code の Monitor が各行を会話へ届け、チャットのAIはその通知の回に対処と報告を行う
// （docs/chat-operations.md『起票後の見張り』）。状態はファイルへ残すので、張り直しても同じ通知を繰り返さない。
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { handoffDeadline, handoffFirstRun } from './lib/desk-handoff.ts';
import {
  openDesk,
  connectionArgs,
  connection,
  connectionUsage,
} from './desk-client.ts';

const usage = `使い方: ~/.issue-desk/desk follow-issues.ts [--desk <共通番号,…>] [--draft <下書きID,…>] --state <絶対パス> [--interval <秒>] [--stall <分>] [--run <分>]

Claude Code の Monitor で timeout_ms を 1800000 にして起動します。各行が通知です。
--interval 秒（既定1800）ごとに読み、変化が無ければ何も出しません。読み取りの失敗と判断待ちの確かめ直しは60秒後に読み直します。
  【要対処】 保留・失敗・回答待ちなど、対処が要る状態になった
  【停滞】   待ち・実行中の状態のまま --stall 分（既定60）更新が無い
  【読取失敗】読み取りや相談の起票結果の照合が5回続けて失敗した（要対処なしの意味ではない）
  【完了】   マージ・クローズされた
  【見張り終了】全件が完了した（プロセスも終わる）
  【判断待ち】残りの全件が利用者の判断待ちになった（プロセスも終わる）。返事を送ったら同じコマンドで Monitor を起動し直す
  【張り直し】--run 分（既定28）が経った。変化は無いので、返答の文を書かずに同じコマンドで Monitor を起動し直す
--state は見張りごとの記録ファイルで、同じ通知を繰り返さないために使います。
${connectionUsage}`;

/** 実装・公開が進む途中か、順番を待っている状態。これ以外の未完了は利用者の対処が要るものとして扱う。 */
const moving = new Set([
  'pending',
  'submitted',
  'untracked',
  'queued',
  'dependency_waiting',
  'project_waiting',
  'prerequisite_waiting',
  'scope_waiting',
  'running',
  'reply_pending',
  'retry_pending',
  'publishing',
  'releasing',
  'deploying',
  'remote',
  'close_reviewing',
  'close_closing',
  'investigation_waiting',
]);
const finished = new Set(['completed', 'merged', 'closed']);
const failuresBeforeNotice = 5;
/** 読み取りの失敗と判断待ちの確かめ直しは、長い間隔を待たずに読み直す。 */
const recheckMs = 60000;

export type Issue = {
  desk: number;
  title?: string;
  state?: string;
  status?: string;
  updatedAt?: string;
  closedAt?: string | null;
  pr?: { merged?: boolean } | null;
  nextActor?: string;
  nextAction?: string;
  result?: { error?: unknown; message?: unknown } | null;
  pendingFinalApprovals?: { id: string; headSha: string }[] | null;
};
type Tracked = {
  mark: string;
  since: number;
  stallNoticed?: boolean;
  done?: boolean;
  failures?: number;
  failureNoticed?: boolean;
};
type FollowState = { issues: Record<string, Tracked> };

const detail = (issue: Issue) => {
  const text = issue.result?.error ?? issue.result?.message;
  return typeof text === 'string' && text.trim()
    ? ` 理由: ${text.trim().replace(/\s+/g, ' ').slice(0, 200)}`
    : '';
};
const actorLabels: Record<string, string> = {
  user: '利用者',
  ai: 'チャットのAI',
  system: '受付',
};
/** 次に誰が何をするか。通知だけで読めるように行へ添える。 */
const nextStep = (issue: Issue) => {
  const actor = actorLabels[issue.nextActor ?? ''];
  return actor
    ? ` 次=${actor}${issue.nextAction ? `: ${issue.nextAction}` : ''}`
    : '';
};
const isFinished = (issue: Issue) =>
  issue.state === 'closed' ||
  !!issue.closedAt ||
  issue.pr?.merged === true ||
  finished.has(issue.status ?? '');

/** 1件の読み取り結果から、出す行と次の記録を決める。同じ状態の間は同じ行を出さない。 */
export function followIssue(
  previous: Tracked | undefined,
  issue: Issue,
  now: number,
  stallMs: number,
): { line: string | null; next: Tracked } {
  const name = `共通番号${issue.desk}`;
  const mark = `${issue.state}/${issue.status}/${issue.updatedAt}`;
  const changed = previous?.mark !== mark;
  const next: Tracked = {
    mark,
    since: changed ? now : (previous?.since ?? now),
    stallNoticed: changed ? false : previous?.stallNoticed,
  };
  if (isFinished(issue)) {
    next.done = true;
    return {
      line: previous?.done
        ? null
        : `【完了】${name}: 状態=${issue.status ?? issue.state}（${issue.title ?? ''}）`,
      next,
    };
  }
  const status = issue.status ?? '';
  if (!moving.has(status)) {
    // 更新時刻や承認の順序だけでは通知せず、状態・理由・承認の組を比較する。
    const reason = detail(issue);
    next.mark = `${issue.state}/${status}/${reason}`;
    const approvals = issue.pendingFinalApprovals ?? [];
    let approvalNotice = '';
    if (approvals.length) {
      const pairs = approvals.map(({ id, headSha }) => [id, headSha]);
      pairs.sort((a, b) => {
        const left = JSON.stringify(a),
          right = JSON.stringify(b);
        return left < right ? -1 : left > right ? 1 : 0;
      });
      next.mark += `/pendingFinalApprovals:${JSON.stringify(pairs)}`;
      approvalNotice = ` 未了の最終承認: ${pairs.length}件（${pairs.map(([, head]) => head.slice(0, 8)).join(', ')}）`;
    }
    return {
      line:
        previous?.mark === next.mark
          ? null
          : `【要対処】${name}: 状態=${status || '不明'}${reason}${approvalNotice}（${issue.title ?? ''}）${nextStep(issue)}`,
      next,
    };
  }
  if (!next.stallNoticed && now - next.since >= stallMs) {
    next.stallNoticed = true;
    return {
      line: `【停滞】${name}: 状態=${status} のまま ${Math.round((now - next.since) / 60000)} 分更新がありません（${issue.title ?? ''}）${nextStep(issue)}`,
      next,
    };
  }
  return { line: null, next };
}

type Options = {
  drafts?: string[];
  desks: number[];
  stateFile: string;
  intervalMs: number;
  stallMs: number;
  runMs: number;
};

export function parseFollowArgs(argv: string[]): Options {
  const out: Record<string, string> = {};
  for (let i = 0; i < argv.length; i++) {
    const name = argv[i].match(
      /^--(desk|draft|state|interval|stall|run)$/,
    )?.[1];
    const value = argv[i + 1];
    if (
      !name ||
      out[name] !== undefined ||
      value === undefined ||
      value.startsWith('--')
    )
      throw Error(`引数を確認してください: ${argv[i]}\n${usage}`);
    out[name] = value;
    i++;
  }
  const desks = out.desk ? out.desk.split(',').map(Number) : [];
  const drafts = out.draft ? out.draft.split(',') : [];
  if (
    (!out.desk && !out.draft) ||
    desks.some((n) => !Number.isSafeInteger(n) || n < 1)
  )
    throw Error(
      `--desk に共通番号をカンマ区切りで指定してください。\n${usage}`,
    );
  if (
    (out.draft !== undefined && !out.draft) ||
    drafts.some((id) => !id.trim() || id !== id.trim())
  )
    throw Error(`--draft を確認してください。\n${usage}`);
  if (!out.state || !path.isAbsolute(out.state))
    throw Error(`--state に絶対パスを指定してください。\n${usage}`);
  const number = (key: string, fallback: number) => {
    const value = out[key] === undefined ? fallback : Number(out[key]);
    if (!(value > 0))
      throw Error(`--${key} は正の数で指定してください。\n${usage}`);
    return value;
  };
  return {
    desks,
    drafts,
    stateFile: out.state,
    intervalMs: number('interval', 1800) * 1000,
    stallMs: number('stall', 60) * 60000,
    runMs: number('run', 28) * 60000,
  };
}

function loadState(file: string): FollowState {
  try {
    const value = JSON.parse(fs.readFileSync(file, 'utf8'));
    return value && typeof value.issues === 'object' && value.issues
      ? value
      : { issues: {} };
  } catch (e: any) {
    if (e?.code === 'ENOENT') return { issues: {} };
    throw e;
  }
}
function saveState(file: string, state: FollowState) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const temporary = `${file}.${process.pid}.tmp`;
  fs.writeFileSync(temporary, `${JSON.stringify(state)}\n`, { mode: 0o600 });
  fs.renameSync(temporary, file);
}

type Draft = {
  id?: string;
  project?: string;
  status?: string;
  updatedAt?: string;
  ready?: boolean;
  questions?: unknown[];
  error?: string;
  finishedWithoutIssue?: boolean;
  issue?: { project?: string; number?: number };
  issues?: { project?: string; number?: number }[];
  splitChildren?: string[];
  splitIssues?: unknown[];
};
type DraftTracked = Tracked & { desks?: number[]; children?: string[] };
type Runtime = {
  readDraft?: (id: string) => Promise<Draft>;
  resolve?: (project: string, number: number) => Promise<Issue>;
  read: (desk: number) => Promise<Issue>;
  print: (line: string) => void;
  now: () => number;
  sleep: (ms: number) => Promise<unknown>;
};

/** 利用者が動くまで状態が変わらない依頼。回答の送信直後の読み違いを避けるため、続けて2回読んでから見張りを止める。 */
const waitsForUser = (issue: Issue) =>
  issue.nextActor === 'user' &&
  !moving.has(issue.status ?? '') &&
  !isFinished(issue);
const waitingRoundsBeforeStop = 2;

/** 時間切れの知らせ。前回から変化が無いので、利用者へ何も書かせない。 */
const rearmLine = (desks: number[]) =>
  `【張り直し】${desks.length ? `共通番号${desks.join(',')}` : '相談'} に新しい変化はありません。返答の文は一切書かず（「変化なし」「張り直しました」も書かない）、同じコマンドで Monitor を起動し直すだけで返答を終えてください`;

/** 全件完了と、残り全件が利用者の判断待ちで0、時間切れ（張り直しが要る）で3を返す。 */
export async function follow(
  options: Options,
  runtime: Runtime,
): Promise<number> {
  const state = loadState(options.stateFile);
  const until = handoffDeadline(options.runMs, runtime.now());
  const drafts: Record<string, DraftTracked> = ((state as any).drafts ??= {});
  const roots = options.drafts ?? [];
  let waitingRounds = 0;
  let firstRound = handoffFirstRun();
  for (;;) {
    if (!firstRound && runtime.now() >= until) {
      saveState(options.stateFile, state);
      runtime.print(
        rearmLine([
          ...new Set([
            ...options.desks,
            ...Object.values(drafts).flatMap((d) => d.desks ?? []),
          ]),
        ]),
      );
      return 3;
    }
    firstRound = false;
    const lines: string[] = [];
    let handoff = false;
    const waiting: Issue[] = [];
    let othersOpen = false;
    let readFailed = false;
    const queue = [...roots];
    const seen = new Set<string>();
    for (let i = 0; i < queue.length; i++) {
      const id = queue[i];
      if (seen.has(id)) continue;
      seen.add(id);
      const previous = drafts[id];
      if (previous?.done) {
        queue.push(...(previous.children ?? []));
        continue;
      }
      try {
        if (!runtime.readDraft || !runtime.resolve)
          throw Error('相談の読取が利用できません');
        const draft = await runtime.readDraft(id);
        const children = Array.isArray(draft.splitChildren)
          ? draft.splitChildren.filter(
              (v): v is string => typeof v === 'string' && !!v,
            )
          : [];
        queue.push(...children);
        const refs = [
          ...(draft.issue ? [draft.issue] : []),
          ...(Array.isArray(draft.issues) ? draft.issues : []),
        ];
        const desks = [...(previous?.desks ?? [])];
        let unresolved =
          (draft.splitIssues?.length ?? 0) > children.length ||
          (Array.isArray(draft.splitChildren) &&
            draft.splitChildren.length !== children.length);
        for (const ref of refs) {
          if (!ref || typeof ref !== 'object') {
            unresolved = true;
            continue;
          }
          const project = ref.project || draft.project;
          if (
            !project ||
            !Number.isSafeInteger(Number(ref.number)) ||
            Number(ref.number) < 1
          ) {
            unresolved = true;
            continue;
          }
          try {
            const found = await runtime.resolve(project, Number(ref.number));
            if (Number.isSafeInteger(found?.desk) && found.desk > 0) {
              if (!desks.includes(found.desk)) desks.push(found.desk);
            } else unresolved = true;
          } catch (e: any) {
            if (e?.nasHandoff) throw e;
            unresolved = true;
          }
        }
        const mark = JSON.stringify([draft.status, draft.updatedAt]);
        const changed = previous?.mark !== mark;
        const updatedAt = Date.parse(draft.updatedAt ?? '');
        const since = Number.isFinite(updatedAt)
          ? Math.min(updatedAt, runtime.now())
          : runtime.now();
        const next: DraftTracked = {
          mark,
          since: changed ? since : (previous?.since ?? since),
          desks,
          children,
        };
        const questions =
          Array.isArray(draft.questions) && draft.questions.length > 0;
        let reason = questions
          ? `質問: ${JSON.stringify(draft.questions)}`
          : ['submit_error', 'error', 'stalled'].includes(draft.status ?? '')
            ? `状態=${draft.status} ${draft.error ?? ''}`
            : '';
        if (
          !reason &&
          draft.status === 'draft' &&
          draft.ready !== true &&
          runtime.now() - next.since >= options.stallMs
        )
          reason = '未確定のまま停滞';
        if (!reason && !changed)
          (next as any).notice = (previous as any)?.notice;
        if (reason) {
          const notice = JSON.stringify([draft.status, reason]);
          (next as any).notice = notice;
          if ((previous as any)?.notice !== notice)
            lines.push(`【要対処】下書き${id}: ${reason.slice(0, 200)}`);
        }
        if (
          !unresolved &&
          (draft.finishedWithoutIssue ||
            draft.status === 'closed' ||
            (draft.status === 'submitted' && !refs.length && !children.length))
        )
          next.done = true;
        else if (
          draft.status === 'submitted' &&
          (refs.length > 0 || children.length > 0) &&
          !unresolved
        )
          next.done = true;
        if (unresolved) {
          othersOpen = true;
          readFailed = true;
          next.failures = (previous?.failures ?? 0) + 1;
          next.failureNoticed = previous?.failureNoticed;
          if (next.failures >= failuresBeforeNotice && !next.failureNoticed) {
            next.failureNoticed = true;
            lines.push(
              `【読取失敗】下書き${id}: 起票結果の照合が${next.failures}回続けてできません。状態は未確認です`,
            );
          }
        } else if (!next.done) othersOpen = true;
        drafts[id] = next;
      } catch (e: any) {
        if (e?.nasHandoff) {
          handoff = true;
          break;
        }
        if (e?.status === 404 || e?.code === 'draft_returned_to_todo') {
          drafts[id] = {
            ...(previous ?? { mark: '', since: runtime.now() }),
            done: true,
          };
          if (e?.status === 404)
            lines.push(
              `【要対処】下書き${id}: 相談が見つからない（破棄されたか、この接続では読めない）`,
            );
        } else {
          othersOpen = true;
          readFailed = true;
          const failures = (previous?.failures ?? 0) + 1;
          const next: DraftTracked = {
            ...(previous ?? { mark: '', since: runtime.now() }),
            failures,
          };
          if (failures >= failuresBeforeNotice && !next.failureNoticed) {
            next.failureNoticed = true;
            lines.push(
              `【読取失敗】下書き${id}: ${failures}回続けて読めません（${String(e?.message ?? e).slice(0, 200)}）。状態は未確認です`,
            );
          }
          drafts[id] = next;
        }
      }
    }
    const allDesks = [
      ...new Set([
        ...options.desks,
        ...[...seen].flatMap((id) => drafts[id]?.desks ?? []),
      ]),
    ];
    for (const desk of allDesks) {
      const previous = state.issues[desk];
      if (previous?.done) continue;
      try {
        const issue = await runtime.read(desk);
        const { line, next } = followIssue(
          previous,
          issue,
          runtime.now(),
          options.stallMs,
        );
        state.issues[desk] = next;
        if (waitsForUser(issue)) waiting.push(issue);
        else if (!next.done) othersOpen = true;
        if (line) lines.push(line);
      } catch (e: any) {
        if (e?.nasHandoff) {
          handoff = true;
          break;
        }
        othersOpen = true;
        readFailed = true;
        const failures = (previous?.failures ?? 0) + 1;
        const tracked: Tracked = {
          ...(previous ?? { mark: '', since: runtime.now() }),
          failures,
        };
        if (failures >= failuresBeforeNotice && !tracked.failureNoticed) {
          tracked.failureNoticed = true;
          lines.push(
            `【読取失敗】共通番号${desk}: ${failures}回続けて読めません（${String(e?.message ?? e).slice(0, 200)}）。状態は未確認です`,
          );
        }
        state.issues[desk] = tracked;
      }
    }
    saveState(options.stateFile, state);
    lines.forEach(runtime.print);
    if (handoff) return 75;
    if (
      [...seen].every((id) => drafts[id]?.done) &&
      allDesks.every((desk) => state.issues[desk]?.done)
    ) {
      runtime.print(`【見張り終了】見張り対象はすべて完了しました`);
      return 0;
    }
    waitingRounds = waiting.length && !othersOpen ? waitingRounds + 1 : 0;
    if (waitingRounds >= waitingRoundsBeforeStop) {
      const list = waiting
        .map(
          (issue) =>
            `共通番号${issue.desk}（${issue.title ?? ''}）${nextStep(issue)}`,
        )
        .join(' / ');
      runtime.print(
        `【判断待ち】利用者の判断が要る依頼だけが残ったので見張りを止めます: ${list}。返事を送ったら同じコマンドで Monitor を起動し直してください`,
      );
      return 0;
    }
    if (runtime.now() >= until) {
      runtime.print(
        rearmLine([
          ...new Set([
            ...options.desks,
            ...Object.values(drafts).flatMap((d) => d.desks ?? []),
          ]),
        ]),
      );
      return 3;
    }
    const wait =
      readFailed || waitingRounds > 0
        ? Math.min(recheckMs, options.intervalMs)
        : options.intervalMs;
    await runtime.sleep(Math.min(wait, Math.max(0, until - runtime.now())));
  }
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href
) {
  (async () => {
    const parsed = connectionArgs(process.argv.slice(2), usage);
    const options = parseFollowArgs(parsed.argv);
    const { call, close } = await openDesk(connection(parsed));
    try {
      process.exitCode = await follow(options, {
        read: (desk) => call('GET', `/api/issue?desk=${desk}`),
        readDraft: (id) => call('GET', `/api/drafts/${encodeURIComponent(id)}`),
        // 共通番号（desk）だけ使うので要約を読む。
        resolve: (project, number) =>
          call(
            'GET',
            `/api/issue?${new URLSearchParams({ project, number: String(number), view: 'summary' })}`,
          ),
        print: (line) => console.log(line),
        now: Date.now,
        sleep: (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
      });
    } finally {
      close();
    }
  })().catch((e) => {
    console.error(e.message);
    process.exitCode = 1;
  });
}
