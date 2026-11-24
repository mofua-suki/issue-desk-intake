import {
  shownProject,
  shownRepositoryReference,
} from './repository-display.js';
import type { RequestRow, RequestGroups, ListData } from './list-types.js';
import type { ProjectWait } from './issue-types.js';
import {
  familyProgressText,
  familyProgressNote,
  followupMarker,
  followupDraft,
  interruptDelayed,
  closeRequestStatus,
  draftStatus,
  handoffDraft,
  investigationStates,
  investigationDraft,
  investigatedIssue,
  issueProject,
  unassignedProjectLabel,
  requestRows,
  rowGroup,
  isFinished,
  fullTestRows,
  attentionRows,
  selectAttention,
  monitoringRequired,
  workState,
  parentConsultationIDs,
} from './request-classification.js';
export {
  familyProgressText,
  familyProgressNote,
  followupMarker,
  followupDraft,
  interruptDelayed,
  closeRequestStatus,
  draftStatus,
  handoffDraft,
  investigationStates,
  investigationDraft,
  investigatedIssue,
  issueProject,
  unassignedProjectLabel,
  fullTestRows,
  attentionRows,
  selectAttention,
};
// 一覧の組み立てが使う状態ごとの案内文（client/src/ui/request-list-guidance.js）と、新しいプロジェクトの相談の表示名・
// TODOの見出し（client/src/ui/request-list-rows.js）。Issue #1489 で分けた。
import { guidance } from './request-list-guidance.js';
import { todoHeadline } from './request-list-rows.js';

const intakeErrorGuidance: Record<string, string> = {
  intake_error_fixed: '受付の止まりが直った結果です。',
  intake_error_stopped:
    '自動の修正を止めました。通常のPRで直してから、直し相談の画面の「再試行する」で続けてください。',
};
// クローズ理由の表示名と状態ごとの案内文（closeReasons・guidance）は
// client/src/ui/request-list-guidance.js へ分けた（Issue #1489。上の import と下の再エクスポート）。
// 利用量の見直し（Issue #589）の見直し用の相談・調査Issueの行に添える案内。
const usageReviewLabel = (round: unknown) =>
  Number.isSafeInteger(round) ? `自動（利用量の見直し 第${round}回）` : null;
// 優先度の見直し（Issue #744。server/src/intake-priority-review.mjs）の相談・調査Issueの行に添える案内。
const priorityReviewLabel = (round: unknown) =>
  Number.isSafeInteger(round) ? `自動（優先度の見直し 第${round}回）` : null;
// 開いている依頼の棚卸し（Issue #754。server/src/intake-inventory.mjs）の相談・調査Issueの行に添える案内。
const inventoryLabel = (round: unknown) =>
  Number.isSafeInteger(round)
    ? `自動（開いている依頼の棚卸し 第${round}回）`
    : null;
// 着手の遅れた割り込み（Issue #697）。受付サービスが、開いていて実行待ちの割り込みで待ち始めから
// interruptDelayMinutes 以上たった行にだけ interruptDelay（src/intake-interrupt-alert.mjs）を付ける。実行待ちの間だけ
// 要対応に入れ（フォローアップでも）、着手して実行待ちでなくなれば元の分類へ戻る。
export function interruptDelayText(
  d?: RequestRow['interruptDelay'],
  projects: readonly unknown[] = [],
) {
  const behind =
    Array.isArray(d?.behind) && d.behind.length
      ? d.behind
          .map((reference) => shownRepositoryReference(reference, projects))
          .join('、')
      : '記録なし';
  const text = `割り込みが ${d?.minutes} 分以上着手されていません。待っている相手: ${behind}\n${guidance.interrupt_delayed}`;
  // 待っている相手が60分以上変わらない（Issue #1069。受付が unchangedMinutes を付けた行だけ）は先頭で強調する。
  if (!Number.isSafeInteger(d?.unchangedMinutes)) return text;
  return `【要確認】待っている相手が ${d?.unchangedMinutes} 分変わっていません。${text}`;
}
// プロジェクト排他待ちの詳細（src/project-lock.mjs）。先行PR待ちとは別に、相手・理由・解除条件を示す。
// 依存や占有を無視して開始する操作へは案内しない。
export function projectWaitText(
  wait?: ProjectWait | null,
  projects: readonly unknown[] = [],
) {
  if (!wait) return `プロジェクト排他待ちです。${guidance.project_waiting}`;
  const holders = (wait.holders || []).map(
    (h) =>
      `- ${shownProject(h.project, projects)} #${h.issue}（担当 ${h.host}${h.pr ? `、PR #${h.pr}` : ''}）${h.note ? ` ${h.note}` : ''}`,
  );
  return [
    '依存関係のあるプロジェクトとの排他を待っています。',
    wait.reason,
    ...(holders.length ? ['相手:', ...holders] : []),
    `解除条件: ${wait.condition}`,
  ].join('\n');
}
// 受付が起票した調査Issue（受付サービスの investigation）の完了は、分類は「完了」のまま「報告済み」と表示する。
const reportedLabel = '報告済み';
const isReported = (row: Pick<RequestRow, 'investigation' | 'status'>) =>
  row?.investigation === true && row.status === 'completed';
// 状態バッジの表示名の上書き（無ければ状態の表示名のまま）。
export const statusLabel = (
  row: Pick<RequestRow, 'investigation' | 'status' | 'executionProgress'>,
) =>
  isReported(row)
    ? reportedLabel
    : row?.status === 'completed' && row.executionProgress?.unavailable
      ? 'マージ済み・受入確認を読取不能'
      : row?.status === 'completed' && (row.executionProgress?.pending ?? 0) > 0
        ? `マージ済み・受入確認 残り${row.executionProgress!.pending}件`
        : row?.status === 'stall_alert'
          ? '止まり'
          : row?.status === 'family_in_progress'
            ? '進行中'
            : null;
// 元の依頼の表示（Issue #1976。題名にはIssue番号を入れないので、元は題名と別に示す）。row.origin は受付の一覧・詳細の
// 「元」（server/src/issue-origins.mjs）: { sources: [{ project, number, desk }] }、desk は共通番号で対応が不明なら null。
// 元が確定していない依頼は null。対応が不明な元は「共通番号未確定」と示し、Gitea番号を共通番号の代わりに出さない。
export function originLabel(row: Pick<RequestRow, 'origin'>) {
  const sources = row?.origin?.sources;
  if (!Array.isArray(sources) || !sources.length) return null;
  const known = [
    ...new Set(
      sources
        .map((x) => x?.desk)
        .filter((n) => Number.isSafeInteger(n) && (n ?? 0) > 0),
    ),
  ];
  const unknown = sources.some(
    (x) => !(Number.isSafeInteger(x?.desk) && (x.desk ?? 0) > 0),
  );
  return `元: ${[...known.map((n) => `共通番号${n}`), ...(unknown ? ['共通番号未確定'] : [])].join('・')}`;
}
// 相談の案内文。了承後の自動の具体化が上限で止まった下書きは、未送信の一般の案内ではなく止まった理由を示す。
// 調査の結果（届いた報告・報告なしで終わった理由。server/src/intake-reports.mjs の investigationNotice）は案内に添える。
export const draftGuidance = (d: RequestRow) =>
  handoffDraft(d)
    ? handoffGuidance()
    : d.status === 'draft' && d.continuationLimited
      ? guidance.continuation_limit
      : investigationDraft(d) && d.investigationNotice
        ? `${guidance[d.status]}\n${d.investigationNotice}`
        : guidance[draftStatus(d)];
const handoffGuidance = () => '新しいプロジェクトの相談へ引き継いだ記録です。';
// 相談画面の報告の見出し。
export const reportAuthor = (
  m: { source?: unknown },
  projects: readonly unknown[] = [],
) =>
  `調査の報告（${typeof m.source === 'string' ? shownRepositoryReference(m.source, projects) || '参照元不明' : m.source || '参照元不明'}）`;
// 相談の難易度の行・新しいプロジェクトの相談の行・TODOの見出し（client/src/ui/request-list-rows.js）は Issue #1489 で分けた
// （下の再エクスポート）。
// コントローラー生成のフォローアップ（受付サービスの followup、案から起票する下書き、それらを対象とする
// クローズ確認）は「フォローアップ」タブに入れ、4分類の一覧・件数と初期タブの判定に含めない。
// 完了・クローズ済みになったフォローアップは、相談・確認も通常の依頼と同じ「完了」「クローズ済み」へ移す
// （followup の区分は残し、両タブの絞り込みと目印に使う）。フォローアップタブの件数は未完了の数になる。
export function requestGroups(
  issues: RequestRow[],
  drafts: RequestRow[],
  todos: RequestRow[] = [],
  closes: RequestRow[] = [],
  reviews: RequestRow[] = [],
  stalls: RequestRow[] = [],
  projects: readonly unknown[] = [],
  unreadProjects: readonly string[] = [],
): RequestGroups {
  const rows = requestRows(issues, drafts, closes, reviews, stalls, {
    unreadProjects,
  });
  const groups: RequestGroups = {
    attention: [],
    issues: [],
    waiting: [],
    stopped: [],
    completed: [],
    closed: [],
    todos: [],
    followups: [],
    monitoring: [],
  };
  // TODOは「要対応」「進行中」「完了」「クローズ済み」に混ぜない。相談を始めた時点で下書きへ移り、そちらに現れる。
  for (const t of todos)
    groups.todos.push({
      ...t,
      kind: 'todo',
      status: 'saved_todo',
      title: todoHeadline(t.body),
      guidance: guidance.saved_todo,
    });
  for (const row of rows) {
    row.work = workState(row);
    // Same rule as the service: a closed issue keeps showing only the work still in flight, and a
    // request whose automation finished stays 完了 after its Issue is closed.
    const usageReviewNote =
      usageReviewLabel(row.usageReviewRound) ??
      priorityReviewLabel(row.priorityReviewRound) ??
      inventoryLabel(row.inventoryRound);
    row.guidance =
      `${usageReviewNote ? `${usageReviewNote}\n` : ''}` +
      ((isReported(row) ? guidance.reported : '') ||
        (interruptDelayed(row)
          ? interruptDelayText(row.interruptDelay, projects)
          : '') ||
        row.dependencyReason ||
        (row.status === 'project_waiting' && row.projectWait
          ? `${row.projectWait.reason}\n解除条件: ${row.projectWait.condition}`
          : '') ||
        (row.status === 'scope_waiting' ? row.scopeWait?.note : '') ||
        // 起動し直し（monitor_restarted）は案内を detail に含めて返すので、案内文の無い状態は detail だけにする。
        (row.kind === 'stall'
          ? [
              row.detail,
              intakeErrorGuidance[row.status] || guidance[row.status],
            ]
              .filter(Boolean)
              .join('\n')
          : '') ||
        (row.kind === 'draft' ? draftGuidance(row) : '') ||
        (row.followup && row.status === 'queued'
          ? guidance.followup_queued
          : '') ||
        guidance[row.status] ||
        (row.status === 'closed'
          ? '終了理由は詳細で確認できます。'
          : isFinished(row.status)
            ? '結果を確認できます。'
            : '自動処理が進行中です。'));
    if (row.pendingFinalApprovals?.length)
      row.guidance = row.pendingFinalApprovals.map((a) => a.reason).join('\n');
    if (row.finalApprovalsUnavailable)
      row.guidance = row.finalApprovalsUnavailable;
    if (monitoringRequired(row) && !row.executionProgress)
      row.guidance = `受付側の監視記録です。利用者の判断待ちではありません。${row.kind === 'stall' && row.detail ? '\n' + row.detail : ''}`;
    // 相談・クローズ確認自身が終了したら履歴へ移す。対象Issueが終了していても、未処理の確認は残す。
    const group = acceptanceOnlyCompleted(row) ? 'completed' : rowGroup(row);
    if (['attention', 'issues', 'waiting', 'stopped'].includes(group)) {
      row.guidance = `${row.interruptDelay ? interruptDelayText(row.interruptDelay, projects) + '\n' : ''}${row.work.reason}\n次の担当: ${row.work.nextActor} · 次の行動: ${row.work.nextAction}${row.work.waitCondition ? `\n待機条件: ${row.work.waitCondition} · 再開: ${row.work.resumeBy}` : ''}`;
    }
    if (row.executionProgress?.unavailable)
      row.guidance = row.executionProgress.unavailable;
    else if (row.executionProgress && (row.executionProgress.pending ?? 0) > 0)
      row.guidance = `${row.guidance}\n受入条件の未確認: ${row.executionProgress.pending}件（担当と確認結果は詳細）`;
    groups[group].push(row);
  }
  groups.monitoring = rows.filter(
    (row) =>
      ['attention', 'issues', 'waiting', 'stopped'].includes(rowGroup(row)) &&
      monitoringRequired(row),
  );
  const selected = selectAttention(groups.attention);
  groups.attention = selected.attention;

  return groups;
}
function acceptanceOnlyCompleted(row: RequestRow): boolean {
  return (
    row.kind === 'issue' &&
    row.state === 'closed' &&
    row.status === 'completed' &&
    !row.executionProgress?.unavailable &&
    (row.executionProgress?.pending ?? 0) > 0 &&
    row.executionActive !== true &&
    row.executionScheduled !== true &&
    workState({ ...row, executionActive: true }).state === 'running'
  );
}
export const initialTab = (groups: RequestGroups) =>
  groups.attention.length
    ? 'attention'
    : groups.stopped.length
      ? 'stopped'
      : groups.issues.length
        ? 'issues'
        : groups.waiting.length
          ? 'waiting'
          : 'issues';

// 一覧の更新日時の短い形と本文の簡易Markdown・URLの表示の解析（client/src/ui/request-list-markdown.js）、一覧の絞り込み・
// 並べ替え（client/src/ui/request-list-sort.js）は Issue #1488 で分けた。既存の import を保つため、ここから再エクスポートする。
export {
  shortTime,
  markdownBlocks,
  longIdParts,
  urlLabel,
  inlineTokens,
} from './request-list-markdown.js';
export {
  sortFields,
  defaultSort,
  statusOrder,
  kindFilters,
  defaultKindFilter,
  validKindFilter,
  filterRows,
  nextSort,
  validSort,
  priorityRank,
  sortRows,
} from './request-list-sort.js';

// 稼働実績の表（client/src/ui/request-list-stats.js）・AIの使用量の表（client/src/ui/request-list-usage.js）・相談の難易度と結末・フォロー
// アップの実績・モデルの切り替えの表（client/src/ui/request-list-outcomes.js）・相談の難易度とプロジェクトの作成の行
// （client/src/ui/request-list-rows.js）・状態ごとの案内文（client/src/ui/request-list-guidance.js）は Issue #1489 で分けた。既存の
// import を保つため、ここから再エクスポートする。
export { closeReasons, guidance } from './request-list-guidance.js';
export {
  workStatsNotes,
  durationText,
  jstTime,
  ratioText,
  workStatsRows,
} from './request-list-stats.js';
export { workDayHeaders, workDayCells } from './request-list-stats.js';
export {
  usageStatsNotes,
  usageKindNames,
  usageHeaders,
  usageMetricText,
  usageCells,
} from './request-list-usage.js';
export {
  usageIssueLabel,
  usageTables,
  usageCorrectionText,
} from './request-list-usage.js';
export {
  tierNames,
  tierSourceNames,
  tierName,
} from './request-list-outcomes.js';
export {
  pendingTierLevelText,
  pendingTierSettingText,
  pendingTierReasonText,
} from './request-list-outcomes.js';
export {
  tierOutcomeNames,
  tierOutcomeNotes,
  tierOutcomeHeaders,
  tierOutcomeCells,
} from './request-list-outcomes.js';
export {
  followupKindNames,
  followupOutcomeOrder,
  followupStatsNotes,
} from './request-list-outcomes.js';
export {
  followupOutcomeText,
  followupPendingText,
  followupPendingProjects,
} from './request-list-outcomes.js';
export {
  followupStatsRows,
  followupRowHeaders,
  followupRowCells,
} from './request-list-outcomes.js';
export {
  overrideStepNames,
  overrideProviderNames,
  overrideScopeNote,
} from './request-list-outcomes.js';
export {
  overrideUseList,
  overrideSettingText,
  overrideRows,
  overrideDefaultRows,
} from './request-list-outcomes.js';
export {
  overrideNoEffort,
  overrideUseText,
  jstDeadline,
} from './request-list-outcomes.js';
export {
  tierRows,
  projectStatusNames,
  projectGuidance,
  projectFilingNames,
} from './request-list-rows.js';
export {
  projectIssueLabel,
  projectCreationSteps,
  projectCreationRows,
  todoHeadline,
} from './request-list-rows.js';
export {
  projectTechnologyNames,
  projectPlacementNames,
  projectDefinitionRows,
} from './request-list-rows.js';

// 画面が使う一覧全体。aux の表示だけは呼び出し側で案内文・題名を添える。
export function requestGroupsWithAux(
  data: ListData,
  fullTests = fullTestRows,
  alerts = attentionRows,
  projects: readonly unknown[] = [],
) {
  const groups = requestGroups(
    data.issues,
    data.drafts,
    data.todos,
    data.closes,
    data.reviews,
    data.stalls || [],
    projects,
    (data.unreadProjects ?? []).map((u) => u.project),
  );
  const parents = parentConsultationIDs(data.drafts);
  const auxiliary = [
    ...fullTests(data.fullTests),
    ...alerts(data.attentionAlerts),
  ].filter((row) => !row.draftId || !parents.has(row.draftId));
  for (const row of auxiliary) row.work = workState(row);
  const selected = selectAttention(auxiliary);
  groups.monitoring.push(...selected.monitoring);
  for (const row of [...selected.attention, ...selected.monitoring]) {
    row.work ??= workState(row);
    const group = rowGroup(row);
    if (
      group === 'completed' ||
      group === 'closed' ||
      row.state === 'resolved' ||
      row.state === 'closed'
    )
      continue;
    row.guidance = `${row.work.reason}\n次の担当: ${row.work.nextActor} · 次の行動: ${row.work.nextAction}${row.work.waitCondition ? `\n待機条件: ${row.work.waitCondition} · 再開: ${row.work.resumeBy}` : ''}`;
    const target = [
      ...groups.attention,
      ...groups.issues,
      ...groups.waiting,
      ...groups.stopped,
    ].find(
      (i) =>
        (i.kind === 'issue' &&
          row.project === i.project &&
          row.number === i.number) ||
        (i.kind === 'draft' &&
          typeof row.draftId === 'string' &&
          !!row.draftId &&
          row.draftId === i.id &&
          (row.project === i.project ||
            (i.unassigned === true && !row.project))),
    );
    if (target) {
      (target.relatedAlerts ??= []).push(
        ...(row.attentionMembers || [row]).map((a) => ({
          ...a,
          work: a.work || workState(a),
        })),
      );
      const rank = { attention: 0, stopped: 1, running: 2, waiting: 3 };
      const monitoringOnly =
        selected.monitoring.includes(row) &&
        workState({ ...row, executionActive: true }).state === 'running';
      if (!monitoringOnly && rank[row.work.state] < rank[target.work!.state]) {
        for (const name of [
          'attention',
          'issues',
          'waiting',
          'stopped',
        ] as const) {
          const index = groups[name].indexOf(target);
          if (index >= 0) groups[name].splice(index, 1);
        }
        target.work = row.work;
        const next = row.work.state === 'running' ? 'issues' : row.work.state;
        groups[next].push(target);
      }
      target.guidance += `\n関連通知: ${row.guidance}`;
      continue;
    }
    groups[group].push(row);
  }

  groups.attention = selectAttention(groups.attention).attention;
  return groups;
}
