import {
  alertRoutes,
  draftsRoute,
  readDeskAttention,
} from '../shared/src/desk-alert-reader.ts';
export { alertRoutes, draftsRoute };
import {
  summarizeDeskAttention,
  attentionCaseLine,
} from '../shared/src/desk-attention-summary.ts';
import {
  requestRows,
  rowGroup,
  fullTestRows,
  attentionRows,
  selectAttention,
} from '../shared/src/desk-attention.mts';
import {
  readDecisionDetails,
  aggregateDecisions,
} from '../shared/src/decision-waits.ts';
import {
  nextStepForRow,
  attentionNextSteps,
  nextStepSuffix,
  userNumbersLine,
} from './lib/consultation-link-output.generated.ts';
// 受付画面の「要対応」を7経路（Issue・相談・クローズ依頼・4種類の補助データ）から読み、
// shared/src/desk-attention.mts の分類で画面相当の行と依頼ごとのまとめを表示する。読むだけ。
// - A9 MAX上で接続の指定が無ければ一時キー。--url/--token-file（または環境変数）と、受付が別のPCにある設定
//   （config.local.json の intake.remoteUrl）では、PC別トークン（scripts/desk-client.ts の connection・openDesk）。
//   受付がトークンからこれらを読ませるのは、利用者が受付画面でそのトークンに運用の代行を許したときだけ（Issue #1120）。
// - 経路ごとの失敗（403を含む）はその経路の理由を表示して他の経路の表示を続け、失敗があれば終了コード1。
// - --json は機械向けの JSON（items・failures・thisHost・summary・cases）。
// - 止まりの要対応があれば、テキストの最後にこのPCの診断の入口（scripts/worker-diagnose.mjs）を1行案内する（Issue #1558）。
import fs from 'node:fs';
import {
  budgetStopped,
  consultationOutput,
  stoppedConsultationText,
} from './lib/consultation-link-output.generated.ts';
import {
  openDesk,
  connectionArgs,
  connection,
  connectionUsage,
  configuredRemote,
  configFile,
} from './desk-client.ts';
import { invokedAs } from './lib/invoked.ts';

const usage = `使い方: node scripts/desk-alerts.ts [--json] [--user-only] [--stale-minutes <正の整数>] [--project <名前>] [--kind <種類>] [--exclude-project <名前>] [--exclude-kind <種類>]

7経路から、受付画面の「要対応」と同じ全件数と依頼・相談ごとのまとめを表示します。
project と kind は全7経路の行に適用します。各オプションは繰り返せます。
kind は出力の items[].caseKind があればそれ、無ければ items[].kind に完全一致します（例: main_broken、stall、issue）。
--stale-minutes は「止まっている」の基準（既定30分）です。
--user-only は承認・上限判断待ちだけをテキストで表示します。--json 併用時は既存のJSONを出します。
--json は機械向けの JSON（items・failures・thisHost・summary・cases）を出します。読めなかった経路があれば、その理由を表示して
ほかの経路は表示を続け、終了コード1で終わります。止まりがあれば、最後にこのPCの診断の入口
（node scripts/worker-diagnose.mjs）を案内します。
A9 MAX上では一時キーで読みます。他のPCからと、受付が別のPCにある設定（config.local.json の intake.remoteUrl）では、
受付画面で運用の代行を許したPC別トークンで読みます（許していないトークンは受付が断ります）。
${connectionUsage}`;

// トークン接続で受付が403で断ったとき（運用の代行を許していないトークン）の案内。トークンの値は含めない。
export const forbiddenHint =
  '受付画面でこのトークンに運用の代行を許すと読めます（それまでは受付画面の依頼一覧の「要対応」で確かめてください）。';

// このPCの名前（config.local.json の workerId）。読めない・無いときは null（Issue #1558）。
function configuredWorkerId(file = configFile()) {
  try {
    const c = JSON.parse(fs.readFileSync(file, 'utf8').replace(/^\uFEFF/, ''));
    return typeof c?.workerId === 'string' && c.workerId.trim()
      ? c.workerId.trim()
      : null;
  } catch {
    return null;
  }
}

// 止まりの要対応があるときの最後の案内（scripts/worker-diagnose.mjs、docs/worker-diagnose.md）。
const diagnoseHint = (host) =>
  `${host ? `このPC（${host}）` : 'このPC'}の状態DB・runner.log は node scripts/worker-diagnose.mjs で読めます（読み取りのみ）`;

const oneLine = (value, max = 200) => {
  const text = String(value ?? '')
    .replace(/\s+/g, ' ')
    .trim();
  return text.length > max ? `${text.slice(0, max)}…` : text;
};
// 対象のIssue（project#番号（受付の共通番号 #n））。番号が無ければプロジェクトだけ、どちらも無ければ空。
function targetText(a) {
  const number = Number.isSafeInteger(a.number)
    ? a.number
    : Number.isSafeInteger(a.issue)
      ? a.issue
      : null;
  const project =
    typeof a.project === 'string'
      ? a.project
      : typeof a.repo === 'string'
        ? a.repo
        : '';
  if (!project) return '';
  return `${project}${number ? `#${number}` : ''}${a.desk ? `（受付の共通番号 #${a.desk}）` : ''}`;
}

// 分類の結果だけを表示する。本文・会話・収集画像は出力へ持ち込まない。
const labels = {
  issue: 'Issue',
  draft: '相談',
  close: 'クローズ依頼',
  stall: '止まり',
  full_test: 'main の全部のテスト',
  usage_review: '利用量の見直し',
  attention: '要対応の自動処理',
};
const itemView = (r) => {
  const view = Object.fromEntries(
    [
      'id',
      'parentSplit',
      'kind',
      'project',
      'repo',
      'number',
      'desk',
      'status',
      'title',
      'issueTitle',
      'caseKind',
      'kindName',
      'stateLabel',
      'reason',
      'key',
      'error',
      'requiresUser',
      'causeKey',
      'attentionClass',
      'pendingFinalApprovals',
      'finalApprovalsUnavailable',
      'permissionApproval',
      'deletionApproval',
      'handoff',
      'recoveryState',
      'state',
      'gist',
      'updatedAt',
      'startedAt',
      'excerpt',
    ]
      .filter((key) => r[key] !== undefined)
      .map((key) => [key, r[key]]),
  );
  // 止まりのAPIは対象番号を issue で返す。詳細を省く前に出力の number にそろえる。
  if (!Number.isSafeInteger(view.number) && Number.isSafeInteger(r.issue))
    view.number = r.issue;
  if (r.kind === 'stall' && typeof r.detail === 'string')
    view.detail = r.detail;
  if (r.caseKind === 'acceptance_transfer') view.startedAt = r.startedAt;
  return view;
};
function itemLine(r) {
  const target = targetText(r);
  const title = oneLine(r.title) || oneLine(r.kindName) || '（題名なし）';
  const stage =
    r.kind === 'attention'
      ? ` 段: ${oneLine(r.kindName || r.caseKind)}/${oneLine(r.stateLabel)} 理由: ${oneLine(r.reason)}`
      : '';
  const notice =
    r.kind === 'attention' &&
    r.caseKind === 'ops_watch' &&
    r.key?.startsWith('ops-watch:')
      ? ` 通知ID: ${r.key.slice('ops-watch:'.length)}`
      : '';
  const error = r.error ? ` 理由: ${oneLine(r.error)}` : '';
  return `[${labels[r.kind] || r.kind}] ${target || 'プロジェクト未定'}${r.id ? ` ID:${oneLine(r.id)}` : ''}${r.parentSplit ? ` 親相談ID:${oneLine(r.parentSplit)}` : ''} ${oneLine(r.status)}: ${title}${stage}${notice}${error}`;
}

/** parseArgs が返す引数。 */
interface AlertArgs {
  filters?: URLSearchParams;
  json?: boolean;
  userOnly?: boolean;
  staleMinutes?: number;
}
export function parseArgs(argv: string[]): AlertArgs {
  const out: AlertArgs = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (
      ['--project', '--kind', '--exclude-project', '--exclude-kind'].includes(
        arg,
      )
    ) {
      const value = argv[++i];
      if (
        !value ||
        value.trim() !== value ||
        // eslint-disable-next-line no-control-regex -- 入力や表示から制御文字を除外する安全性検証。
        /[\u0000-\u001f\u007f]/.test(value) ||
        value.startsWith('--')
      )
        throw Error(`引数を確認してください: ${arg}\n${usage}`);
      const key = {
        '--project': 'project',
        '--kind': 'kind',
        '--exclude-project': 'excludeProject',
        '--exclude-kind': 'excludeKind',
      }[arg];
      (out.filters ??= new URLSearchParams()).append(key, value);
    } else if (arg === '--json' && !out.json) out.json = true;
    else if (arg === '--user-only' && !out.userOnly) out.userOnly = true;
    else if (arg === '--stale-minutes' && out.staleMinutes === undefined) {
      const value = argv[++i];
      if (
        !/^[1-9]\d*$/.test(value || '') ||
        !Number.isSafeInteger(Number(value))
      )
        throw Error(`引数を確認してください: ${arg}\n${usage}`);
      out.staleMinutes = Number(value);
    } else throw Error(`引数を確認してください: ${arg}\n${usage}`);
  }
  return out;
}

// 7経路を読み、画面と同じ要対応の項目を表示する。終了コード: 0 は全部読めた、1 は読めなかった経路がある。
// remote: トークン接続（403に運用の代行の案内を添える）。host: このPCの名前（workerId。読めなければ null）。
export async function deskAlerts(
  call,
  args: AlertArgs = {},
  log = console.log,
  {
    remote = false,
    host = null,
    now = Date.now(),
    base = null,
    publicUrl = null,
    actionBase = base || 'http://127.0.0.1:8787/',
  } = {},
) {
  const { inputs, failures } = await readDeskAttention(call, args.filters);
  const {
    issues,
    drafts,
    closes,
    reviews,
    stalls,
    fullTests,
    attentionAlerts,
  } = inputs;
  const filters = Object.fromEntries(
    ['project', 'kind', 'excludeProject', 'excludeKind'].map((key) => [
      key,
      args.filters?.getAll(key) || [],
    ]),
  );
  const candidates = [
    ...requestRows(
      issues || [],
      drafts || [],
      closes || [],
      reviews || [],
      stalls || [],
      { includeParentDiagnostics: true },
    ).filter((r) =>
      ['attention', 'issues', 'waiting', 'stopped'].includes(rowGroup(r)),
    ),
    ...fullTestRows(fullTests),
    ...attentionRows(attentionAlerts),
  ].filter((row) => {
    const project = row.project || row.repo;
    const kind = row.caseKind || row.kind;
    return (
      (!filters.project.length || filters.project.includes(project)) &&
      !filters.excludeProject.includes(project) &&
      (!filters.kind.length || filters.kind.includes(kind)) &&
      !filters.excludeKind.includes(kind)
    );
  });
  const { attention: rows, monitoring } = selectAttention(candidates);
  for (const row of monitoring as Array<
    (typeof monitoring)[number] & { ready?: boolean; hold?: boolean }
  >) {
    if (row.kind !== 'draft' || row.parentSplit) continue;
    let reason;
    if (row.status === 'submit_error') {
      reason = `起票失敗: ${oneLine(row.error) || '原因不明'}`;
    } else if (
      row.status === 'draft' &&
      row.ready !== true &&
      row.hold !== true &&
      (!Array.isArray(row.questions) || row.questions.length === 0) &&
      typeof row.updatedAt === 'string' &&
      Number.isFinite(Date.parse(row.updatedAt)) &&
      new Date(now).getTime() - Date.parse(row.updatedAt) >= 60 * 60 * 1000
    ) {
      reason = '質問も確定も無いまま止まっている';
    }
    if (reason) row.reason = [row.reason, reason].filter(Boolean).join(' / ');
  }
  const checked = await readDecisionDetails(rows, (project, number) =>
    call(
      'GET',
      `/api/issue?${new URLSearchParams({ project, number: String(number) })}`,
    ),
  );
  for (const entry of checked) {
    if (!entry.failure) continue;
    const route = `/api/issue?${new URLSearchParams({ project: entry.row.project || '', number: String(entry.row.number ?? entry.row.issue ?? '') })}`;
    if (!failures.some((f) => f.route === route))
      failures.push({
        kind: 'issueDetail',
        label: 'Issue詳細',
        route,
        status: entry.status ?? null,
        error: entry.failure,
      });
  }
  const { groups: decisions, remaining } = aggregateDecisions(checked);
  const individualRows = remaining.map((e) =>
    e.failure || e.decision?.reason
      ? {
          ...e.row,
          reason: [e.row.reason, e.failure || e.decision.reason]
            .filter(Boolean)
            .join(' / '),
        }
      : e.row,
  );
  const items: Record<string, any>[] = rows.map((row) => ({
    ...itemView(row),
    attentionMembers: row.attentionMembers?.map(itemView),
    ...nextStepForRow(row, actionBase),
    ...(budgetStopped(row) ? consultationOutput(row, { base, publicUrl }) : {}),
  }));
  const summarized = summarizeDeskAttention(
    [
      ...individualRows,
      ...monitoring.filter((row) => row.caseKind === 'attention_pending'),
    ],
    {
      now,
      staleMinutes: args.staleMinutes,
      partial: failures.length > 0,
    },
  );
  // 画面相当の行数と判断待ちの件数を足して表示・--json に出す。
  const summary: typeof summarized.summary & {
    screenTotal?: number;
    decisionGroups?: number;
    decisionMembers?: number;
  } = summarized.summary;
  const { cases: grouped, stopped } = summarized;
  // 元の対象別分類を保持する。別通知も含む元の活動時刻・自動処理段階を参照する。
  const original = summarizeDeskAttention(candidates, {
    now,
    staleMinutes: args.staleMinutes,
  });
  const originalCases = [
    ...original.cases,
    ...original.waiting,
    ...original.stopped,
  ];
  // 判断対象の停止は従来の stopped 件数にも残し、判断行を別途表示する。
  const shownStopped = stopped.filter((entry) =>
    individualRows.some(
      (row) =>
        row.kind === 'issue' &&
        row.project === entry.project &&
        row.number === entry.number &&
        row.reason,
    ),
  );
  const next = attentionNextSteps(
    individualRows,
    [...grouped, ...shownStopped],
    actionBase,
  );
  const cases = next.cases;
  const userActions = next.userActions;
  for (const group of decisions) {
    const members = group.members.map((member) => ({
      ...member,
      url: nextStepForRow(
        { ...member, kind: 'issue', nextActor: 'user' },
        actionBase,
      ).url,
    }));
    const action =
      group.operation === 'permission'
        ? '権限ファイルの変更を確認する'
        : group.operation === 'resume'
          ? 'issue-desk-issue-control Skillの resume 手順で対象ごとに停止解除する'
          : '上限超過の再試行を判断する';
    const nums = members.map((m) => m.number).sort((a, b) => a - b);
    const first = group.entries[0].row;
    const originals = originalCases.filter((c) =>
      members.some((m) => c.project === m.project && c.number === m.number),
    );
    const lastActivityAt =
      originals
        .map((c) => c.lastActivityAt)
        .filter(Boolean)
        .sort()
        .at(-1) ?? null;
    const elapsedMinutes =
      lastActivityAt === null
        ? null
        : Math.max(
            0,
            (new Date(now).getTime() - Date.parse(lastActivityAt)) / 60000,
          );
    const entry = {
      key: `decision:${group.key}`,
      project: first.project,
      number: first.number,
      desk: first.desk ?? null,
      title: `${members.length}件（#${nums[0]}〜#${nums.at(-1)}） ${action}${group.files ? `: ${group.files.join(', ')}` : ''}`,
      rows: group.entries.map((e) => ({
        kind: e.row.kind,
        status: e.row.status,
        reason: e.row.reason,
      })),
      automatic: originals.flatMap((c) => c.automatic),
      lastActivityAt,
      elapsedMinutes,
      stale: originals.some((c) => c.stale),
      members,
      operation: group.operation,
      classificationEvidence: group.evidence,
      nextActor: 'user',
      nextAction: action,
      evidence: {
        id: `issue:${first.project}#${first.number}`,
        at: first.updatedAt ?? null,
      },
      url: members[0].url,
    };
    cases.push(entry);
    if (entry.stale) summary.stale++;
    userActions.push({
      project: first.project,
      number: first.number,
      desk: first.desk ?? null,
      nextAction: action,
      url: members[0].url,
      members,
    });
  }
  summary.rawTotal = rows.length;
  summary.screenTotal = rows.length;
  summary.total +=
    shownStopped.reduce((n, entry) => n + entry.rows.length, 0) +
    decisions.reduce((n, group) => n + group.entries.length, 0);
  summary.cases = cases.length;
  summary.decisionGroups = decisions.length;
  summary.decisionMembers = decisions.reduce(
    (n, group) => n + group.members.length,
    0,
  );
  summary.stopped.cases += decisions
    .filter((group) => group.operation === 'resume')
    .reduce((n, group) => n + group.members.length, 0);

  const failureText = (f) =>
    `[${f.label}] 読めませんでした（${f.route}）: ${oneLine(f.error, 500)}${remote && f.status === 403 ? ` ${forbiddenHint}` : ''}`;
  if (args.json) {
    log(
      JSON.stringify(
        {
          items,
          monitoring: monitoring.map(itemView),
          ...(args.filters?.toString() ? { filters } : {}),
          screenScope: {
            filtered: !!args.filters?.toString(),
            partial: failures.length > 0,
          },
          summary,
          cases,
          userActions,
          failures: failures.map((f) => ({
            kind: f.kind,
            route: f.route,
            status: f.status,
            error: f.error,
            ...(remote && f.status === 403 ? { hint: forbiddenHint } : {}),
          })),
          thisHost: host || null,
        },
        null,
        2,
      ),
    );
    return failures.length ? 1 : 0;
  }
  if (args.userOnly) {
    const selected = [];
    const identify = (r) =>
      `共通番号:${Number.isSafeInteger(r.desk) && r.desk > 0 ? r.desk : '不明'} ${targetText(r) || (r.id ? `相談ID:${oneLine(r.id)}` : '対象不明')}`;
    for (const group of decisions.filter((g) => g.operation !== 'resume')) {
      const action =
        group.operation === 'permission'
          ? '権限ファイルの変更を承認するか判断'
          : '上限超過の再試行を判断';
      const members = group.members.map(
        (m, i) =>
          `${identify(m)} ${oneLine(group.entries[i].row.title) || '（題名なし）'}`,
      );
      log(
        `${members.join(' / ')}: ${action}${group.files ? ` (${group.files.join(', ')})` : ''}`,
      );
      selected.push(...group.members);
    }
    for (const entry of remaining) {
      const row = entry.row;
      if (budgetStopped(row)) {
        log(
          `${identify(row)} ${oneLine(row.title) || '（題名なし）'}: 相談予算上限後の続行を判断`,
        );
        selected.push(row);
      }
      if (entry.failure) {
        log(
          `${identify(row)} ${oneLine(row.title) || '（題名なし）'}: 判断対象を確認できません: ${oneLine(entry.failure)}`,
        );
        continue;
      }
      const detail = entry.detail || row;
      if (row.kind !== 'issue') continue;
      const prefix = `${identify(row)} ${oneLine(row.title) || '（題名なし）'}`;
      const approvals = Array.isArray(detail.pendingFinalApprovals)
        ? detail.pendingFinalApprovals
        : [];
      for (const approval of approvals) {
        const valid =
          typeof approval?.id === 'string' &&
          approval.id.trim() &&
          /^[a-f0-9]{40}$/i.test(approval.headSha || '') &&
          /^[a-f0-9]{64}$/i.test(approval.fingerprint || '');
        log(
          `${prefix}: ${valid ? `最終承認を判断 ID:${oneLine(approval.id)} head:${approval.headSha}` : '最終承認の対象・条件を照合できません'}`,
        );
        selected.push(row);
      }
      if (
        detail.finalApprovalsUnavailable ||
        (row.pendingFinalApprovals?.length && !approvals.length)
      ) {
        log(
          `${prefix}: 最終承認を確認できません: ${oneLine(detail.finalApprovalsUnavailable || '詳細と一覧が一致しません')}`,
        );
        selected.push(row);
      }
      if (detail.deletionApproval || row.deletionApproval) {
        const d = detail.deletionApproval;
        const valid =
          d &&
          /^[a-f0-9]{40}$/i.test(d.head || '') &&
          Array.isArray(d.items) &&
          d.items.length &&
          d.items.every(
            (item) =>
              ['delete', 'rename', 'type', 'changed'].includes(item?.kind) &&
              typeof item.path === 'string' &&
              item.path &&
              (item.kind !== 'rename' ||
                (typeof item.from === 'string' && item.from)),
          );
        log(
          `${prefix}: ${valid ? `削除承認を判断 head:${d.head}` : '削除承認の対象・条件を照合できません'}`,
        );
        selected.push(row);
      }
    }
    for (const f of failures) log(failureText(f));
    if (!selected.length && !failures.length)
      log('該当する承認・上限判断待ちはありません。');
    log(userNumbersLine(selected));
    return failures.length ? 1 : 0;
  }
  const scope = args.filters?.toString() ? '絞り込み後' : '取得範囲';
  log(
    `画面相当の要対応行数（${scope}${failures.length ? '・欠測あり暫定値' : ''}）: ${summary.screenTotal}件`,
  );
  if (args.filters?.toString())
    log(`適用中の絞り込み（7経路の行）: ${args.filters}`);
  log(`対処集計行数: ${summary.total}件、対象別の集約件数: ${summary.cases}件`);
  const pending = summary.pendingWait;
  const wait = pending
    ? ` 調査の起票待ち ${pending.count}件・${pending.notClearing ? '解消しない見込み' : `さばききるまで約${pending.estimatedDays}日`}（入り ${Number(pending.inPerDay.toFixed(2))}件/日・出 ${pending.outPerDay}件/日）`
    : '';
  log(
    `対処集計 ${summary.total}件（依頼・相談 ${summary.cases}件、止まっている ${summary.stale}件。基準 ${summary.staleMinutes}分）${summary.partial ? ' 読めない経路あり・取得範囲の暫定値' : ''}${wait}`,
  );
  log(`受付側の監視: ${monitoring.length}件（--json の monitoring に記録）`);
  log('画面相当の全行:');
  for (const item of items) log(itemLine(item));
  for (const entry of cases) {
    const child = items.find(
      (r) =>
        r.kind === 'draft' && entry.key === `draft:${r.id}` && r.parentSplit,
    );
    log(
      `${attentionCaseLine(entry)}${child ? ` 親相談ID:${oneLine(child.parentSplit)}` : ''}${nextStepSuffix(entry)}`,
    );
    if (entry.operation)
      for (const member of entry.members) {
        log(
          `  ${member.project}#${member.number} 共通番号:${member.desk ?? '不明'} head:${member.head ?? '不明'}${member.files ? ` files:${member.files.join(', ')}` : ''} URL:${member.url}`,
        );
      }
    if (entry.operation)
      log(
        `  判断根拠: ${[...new Set(entry.classificationEvidence)].map((value) => oneLine(value, 500)).join(' / ')}`,
      );
    for (const stall of items.filter(
      (r) =>
        r.kind === 'stall' &&
        entry.rows.some((row) => row.kind === 'stall' && row.id === r.id),
    )) {
      if (!stall.detail) continue;
      log(`  [止まり] ID:${oneLine(stall.id)}`);
      for (const line of stall.detail.split(/\r?\n/)) log(`  ${line}`);
    }
  }
  for (const f of failures) log(failureText(f));
  for (const row of rows.filter(budgetStopped))
    log(stoppedConsultationText(row, { base, publicUrl }));
  if (!items.length && !failures.length) log('要対応はありません。');
  if (items.some((r) => r.kind === 'draft'))
    log(
      '上限停止以外の相談への回答は create-issue.ts の --draft <下書きID> --text "<回答>"、読むだけは --check <下書きID>（失敗・停止の相談の再試行は受付画面。上限停止は対象相談の画面で続行）。',
    );
  if (items.some((a) => a.kind === 'stall')) log(diagnoseHint(host));
  log(
    userNumbersLine(
      userActions.flatMap((action) => action.members || [action]),
    ),
  );
  return failures.length ? 1 : 0;
}

// コマンドの本体。接続先は他のスクリプトと同じ connection（引数・環境変数・intake.remoteUrl の設定・一時キーの順）。
// env・configured・open・log・workerId はテストで差し替えるためのもの。
export async function runCommand(
  argv,
  {
    env = process.env,
    configured = configuredRemote,
    open = openDesk,
    log = console.log,
    workerId = configuredWorkerId,
    now,
  }: {
    env?: NodeJS.ProcessEnv;
    configured?: () => any;
    open?: typeof openDesk;
    log?: (line: string) => void;
    workerId?: typeof configuredWorkerId;
    now?: number;
  } = {},
) {
  const parsed = connectionArgs(argv, usage);
  const args = parseArgs(parsed.argv);
  const conn = connection(parsed, env, configured);
  const { call, close, config } = await open(conn);
  try {
    return await deskAlerts(call, args, log, {
      remote: !!conn,
      host: workerId(),
      now,
      base: conn?.base,
      publicUrl: config?.intake?.publicUrl,
      actionBase:
        conn?.base || `http://127.0.0.1:${config?.intake?.port || 8787}/`,
    });
  } finally {
    close();
  }
}

// コマンドとして実行したときだけ動く（テストは runCommand などを読み込んで差し替えた受付に対して動かす）。
// 実体のパスで比べる。
if (invokedAs(import.meta.url))
  runCommand(process.argv.slice(2)).then(
    (code) => (process.exitCode = code),
    (e) => {
      console.error(e.message);
      process.exitCode = 1;
    },
  );
