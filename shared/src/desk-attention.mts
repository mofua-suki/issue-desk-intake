// 担当表で受付とWorkerを同じ仕事枠として扱う完全名。表示名の短縮とは独立した照合値。
export const deskRepositories: readonly string[] = Object.freeze([
  'mofua-suki/issue-desk-intake',
  'mofua-suki/issue-desk-worker',
]);
import type { RequestRow, WorkState } from '../../client/src/ui/list-types.js';
import type { Draft, FamilyProgress } from '../../client/src/ui/draft-types.js';
import type { ListTab } from '../../client/src/ui/models.js';
// @ts-check
// ブラウザーと Node で共用する要対応の分類。I/O と表示の案内文は持たない。

// 依頼の「完了」は自動処理の最終段階までの成功、引き継ぎ相談は元の相談の引き継ぎを表す。
// それ以外で閉じた依頼は「クローズ済み」に分け、終了理由は詳細で区別する。
const finished = new Set(['completed', 'closed']);
const inFlight = [
  'running',
  'publishing',
  'scope_waiting',
  'releasing',
  'deploying',
];

export const followupMarker = '<!-- issue-desk:followup -->';
/** @param {Row} d */
export const followupDraft = (d: RequestRow) =>
  d?.proposal
    ? d.proposal.kind !== 'split'
    : String(d?.body || '').includes(followupMarker);

/** @param {Row} row */
export const interruptDelayed = (row: RequestRow) =>
  row.kind === 'issue' && row.status === 'queued' && !!row.interruptDelay;

const closeStatuses: Record<string, string> = {
  confirm: 'close_confirm',
  reviewing: 'close_reviewing',
  closing: 'close_closing',
};
/** @param {Row} c */
export const closeRequestStatus = (c: RequestRow) =>
  closeStatuses[c.status] || c.status;

/** @param {Row} d */
export const draftStatus = (d: RequestRow) =>
  d.status !== 'thinking'
    ? d.status
    : d.turnStopped
      ? 'ai_stopped'
      : d.retryAt
        ? 'ai_recovering'
        : d.queued
          ? 'ai_queued'
          : 'thinking';

/** @param {Row} d */
export const splitChildAttention = (d: RequestRow) =>
  !!d.parentSplit &&
  (draftStatus(d) === 'ai_stopped' ||
    ['error', 'stalled', 'submit_error', 'submit_unknown'].includes(d.status) ||
    (['intake_waiting', 'draft'].includes(d.status) &&
      Array.isArray(d.questions) &&
      d.questions.length > 0));

/** @param {Row} d */
export const handoffDraft = (d: { status: string; handoffRow?: boolean }) =>
  d?.status === 'project_handoff' || d?.handoffRow === true;

export const investigationStates = [
  'investigation_waiting',
  'investigation_reported',
  'investigation_ended',
];
/** @param {Row} d */
export const investigationDraft = (d: { status: string }) =>
  investigationStates.includes(d?.status);
// 相談が記録した調査Issue（行 i がそのIssueか）。続けた後の相談の行を、調査Issueの行が隠さないようにする。
/** @param {Row} i @param {Row} d */
export const investigatedIssue = (
  i: Pick<RequestRow, 'project' | 'number'>,
  d: Pick<RequestRow, 'investigations'>,
) =>
  (d.investigations || []).some(
    (e) => e.project === i.project && e.number === i.number,
  );

/** @param {Row} d */
export const issueProject = (
  d: Pick<RequestRow, 'project' | 'issue'>,
  i = d.issue,
) => i?.project || d.project;
// プロジェクト未定で始めた相談（Issue #689。受付画面の「該当無し」）。対象が決まって起票するまで、一覧の相談の行の
// プロジェクトは「プロジェクト未定」と出す（起票後の行は起票したリポジトリ）。
export const unassignedProjectLabel = 'プロジェクト未定';
/** @param {Row} d @param {Row} extra */
const draftRow = (d: RequestRow, extra: Partial<RequestRow>): RequestRow => ({
  ...d,
  ...(d.unassigned === true && !d.project
    ? { project: unassignedProjectLabel }
    : {}),
  ...extra,
});

export function familyProgressNote(d: Draft): string | null {
  if (
    d.status !== 'submitted' ||
    !Array.isArray(d.splitChildren) ||
    d.parentSplit
  )
    return null;
  if (!d.familyProgress) return '子の進捗を確認できません。';
  return familyProgressText(d.familyProgress);
}

export function familyProgressText(progress: FamilyProgress): string {
  const blocked =
    progress.stopped
      .map(
        (child) =>
          `${child.desk != null ? `共通番号${child.desk} ` : ''}${child.project}#${child.number ?? '不明'} ${child.status}（${child.reason}）`,
      )
      .join('、') || 'なし';
  return `完了 ${progress.completed + progress.closed}/${progress.total}、残り ${progress.remaining}、止まっている: ${blocked}${progress.unknown ? `、確認不能 ${progress.unknown}` : ''}${progress.remaining === 0 && !progress.unknown ? ' · 完了' : ''}`;
}

/** @param {Row[]} issues @param {Row[]} drafts @param {Row[]} closes @param {Row[]} reviews @param {Row[]} stalls */
// Saved links identify parents; a proposed split alone is still an independent consultation.
export function parentConsultationIDs(drafts: RequestRow[]): Set<string> {
  const ids = new Set<string>();
  for (const draft of drafts) {
    for (const parent of [draft.parentSplit, draft.parentConsultation])
      if (typeof parent === 'string' && parent) ids.add(parent);
    if (
      draft.id &&
      (draft.hasChildren === true ||
        [draft.splitChildren, draft.consultationChildren].some(
          (children) =>
            Array.isArray(children) &&
            children.some((id) => typeof id === 'string' && id),
        ))
    )
      ids.add(draft.id);
  }
  return ids;
}

export function requestRows(
  issues: RequestRow[],
  drafts: RequestRow[],
  closes: RequestRow[] = [],
  reviews: RequestRow[] = [],
  stalls: RequestRow[] = [],
  options: {
    includeParentDiagnostics?: boolean;
    unreadProjects?: readonly string[];
  } = {},
): RequestRow[] {
  /** @type {Row[]} */
  const parents = parentConsultationIDs(drafts);
  // 読めなかった候補のリポジトリ（一覧の unreadProjects）のIssueは、行が無くても起票待ちとして補わない。
  // 一覧に無いのは読めなかったためで、Issueが無いことを示さない。
  const unread = new Set(options.unreadProjects ?? []);
  const rows: RequestRow[] = issues.map((i) => ({
    ...i,
    followup: i.followup === true,
    kind: 'issue',
  }));
  // 起票結果はリポジトリ名とIssue番号の組で照合する。相談から起票したIssueは、相談のプロジェクトか
  // 分割案の対象リポジトリにだけある。
  /** @param {Row} i @param {Row} d */
  const sameIssue = (i: RequestRow, d: RequestRow) =>
    [d.issue, ...(d.issues || [])].some(
      (x) => x && i.project === issueProject(d, x) && i.number === x.number,
    );
  // 相談が記録した調査Issue（Issue #409）は、同じ相談から起票したIssueでも相談の行の代わりにしない。
  /** @param {Row} i @param {Row} d */
  const fromDraft = (i: RequestRow, d: RequestRow) =>
    i.intakeId === d.id &&
    !investigatedIssue(i, d) &&
    [
      d.project,
      ...(d.splitIssues || []).map((p) => p.project || d.project),
    ].includes(i.project);
  for (const d of drafts) {
    if (d.id && parents.has(d.id)) {
      // Diagnostics retain the parent record without letting its filing metadata replace child work.
      if (options.includeParentDiagnostics)
        rows.push(
          draftRow(d, {
            status: draftStatus(d),
            followup: followupDraft(d),
            kind: 'draft',
            hasChildren: true,
          }),
        );
      continue;
    }
    // Filed split children are represented by their actual Issue rows, never by a guessed pending row.
    if (d.parentSplit && d.status === 'submitted') continue;
    // Closed legacy records may retain an Issue reference without cleanup metadata.
    // Keep their history before any missing-Issue or attachment recovery fallback.
    if (d.status === 'closed') {
      rows.push(
        draftRow(d, { status: 'closed', followup: false, kind: 'draft' }),
      );
      continue;
    }
    // 元の相談の引き継ぎを完了として扱う。引き継ぎ先の起票状態は問わない。
    if (handoffDraft(d)) {
      rows.push(
        draftRow(d, {
          status: 'completed',
          followup: false,
          kind: 'draft',
          handoffRow: true,
        }),
      );
      continue;
    }
    // 優先度の見直し（Issue #744）の調査Issueは、相談の状態（報告待ち・報告後の自動ターン・質問待ち・終了）に関わらず、
    // 一覧に載った行に回番号を添える（案内「自動（優先度の見直し 第N回）」）。
    if (Number.isSafeInteger(d.priorityReviewRound))
      for (const e of d.investigations || []) {
        const listed = rows.find(
          (i) =>
            i.kind === 'issue' &&
            i.project === e.project &&
            i.number === e.number,
        );
        if (listed) listed.priorityReviewRound = d.priorityReviewRound;
      }
    // 調査Issueを起票した相談は、調査Issueの行（一覧に無ければ補う）と相談の行の両方を出す。
    if (investigationDraft(d)) {
      for (const e of d.investigations || []) {
        if (
          !unread.has(e.project ?? '') &&
          !rows.some((i) => i.project === e.project && i.number === e.number)
        )
          rows.push({
            ...e,
            id: d.id,
            intakeId: d.id,
            title: d.title,
            investigation: true,
            status: 'pending',
            executionActive: false,
            executionScheduled: false,
            followup: false,
            kind: 'issue',
            usageReviewRound: d.usageReviewRound,
            priorityReviewRound: d.priorityReviewRound,
            inventoryRound: d.inventoryRound,
          });
      }
      rows.push(
        draftRow(d, { status: draftStatus(d), followup: false, kind: 'draft' }),
      );
      continue;
    }
    // The server attests that the legacy parent's saved children can be finished.
    const progress = d.familyProgress;
    if (
      ['draft', 'submit_error'].includes(d.status) &&
      (!d.splitIssues ||
        (Array.isArray(d.splitIssues) && d.splitIssues.length === 0)) &&
      progress &&
      progress.total > 0 &&
      progress.completed + progress.closed === progress.total &&
      progress.remaining === 0 &&
      progress.unknown === 0 &&
      'actions' in d &&
      Array.isArray(d.actions) &&
      d.actions.includes('finish')
    ) {
      rows.push(
        draftRow(d, { status: 'completed', followup: false, kind: 'draft' }),
      );
      continue;
    }
    // Image publication has more steps after Issue creation. Keep its recovery
    // screen reachable until all attachments and the final body are confirmed.
    if (
      d.status !== 'submitted' &&
      (d.splitIssues?.length || d.messages?.some((m) => m.images?.length))
    ) {
      for (let n = rows.length - 1; n >= 0; n--) {
        const i = rows[n];
        if (i.kind === 'issue' && (fromDraft(i, d) || sameIssue(i, d)))
          rows.splice(n, 1);
      }
      rows.push(
        draftRow(d, {
          status: draftStatus(d),
          followup: followupDraft(d),
          kind: 'draft',
        }),
      );
      continue;
    }
    // 保存済みの起票結果は、リポジトリ名とIssue番号の組ごとに一覧に無いものだけを補う。同じ相談のIssueが
    // 一覧に1件あっても、残りの結果を省かない（起票と一覧取得が重なると、一部だけ一覧に載ることがある）。
    if (d.issue || d.status === 'submitted') {
      for (const issue of d.issues?.length
        ? d.issues
        : d.issue
          ? [d.issue]
          : [])
        if (
          !unread.has(issueProject(d, issue) ?? '') &&
          !rows.some(
            (i) =>
              i.project === issueProject(d, issue) && i.number === issue.number,
          )
        )
          rows.push({
            ...d,
            ...issue,
            project: issueProject(d, issue),
            status: 'pending',
            executionActive: false,
            executionScheduled: false,
            followup: followupDraft(d),
            kind: 'issue',
          });
      continue;
    }
    // The Issue can be visible before its POST acknowledgement is saved locally.
    if (issues.some((i) => fromDraft(i, d))) continue;
    rows.push(
      draftRow(d, {
        status: draftStatus(d),
        followup: followupDraft(d),
        kind: 'draft',
      }),
    );
  }
  // 他のPCからのクローズ依頼。対象のIssueの行とは別の行で、確認待ちは「要対応」に出す。対象がフォローアップ
  // なら、その行と同じく「フォローアップ」タブに出す。
  /** @param {Row} c */
  const followupTarget = (c: RequestRow) =>
    c.target?.followup === true ||
    rows.some(
      (r) =>
        r.kind === 'issue' &&
        r.followup &&
        r.project === c.project &&
        r.number === c.number,
    );
  for (const c of Array.isArray(closes) ? closes : [])
    rows.push({
      ...c,
      kind: 'close',
      status: closeRequestStatus(c),
      state: undefined,
      followup: followupTarget(c),
      title: `閉じてよいか: ${c.target?.title || c.title || ''}`,
    });
  // 利用量の見直しで元が取れていない回（受付の /api/usage-review/alerts。Issue #591）。回ごとに1件、要対応に出す。
  for (const r of Array.isArray(reviews) ? reviews : [])
    if (Number.isSafeInteger(r?.round))
      rows.push({
        ...r,
        id: `usage-review-${r.round}`,
        kind: 'usage_review',
        status: 'usage_review_alert',
        followup: false,
        project: 'mofua-suki/issue-desk-intake',
        title: `利用量の見直し 第${r.round}回: 見直しの費用が減った量を上回っています`,
        updatedAt: r.startedAt,
      });
  // 未解消の止まり（受付の /api/stalls/alerts。Issue #848）。止まりごとに1件、要対応に出す（解消すると受付が返さなくなる）。
  // 監視が状態DBに書いた実装の止まり（Issue #678）は受付が status を stalled_pipeline に、ホームの監視の停止
  // （Issue #982。ホーム以外の実装PCも Issue #1568）は monitor_stopped に、監視の自動の起動し直し（Issue #1568）は
  // monitor_restarted にして返す。
  for (const a of Array.isArray(stalls) ? stalls : [])
    if (
      typeof a?.id === 'string' &&
      typeof a.title === 'string' &&
      typeof a.detail === 'string'
    )
      rows.push({
        ...a,
        id: `stall-${a.id}`,
        stallKind: a.kind ?? '',
        kind: 'stall',
        status: [
          'stalled_pipeline',
          'monitor_stopped',
          'monitor_restarted',
          'intake_error_fixed',
          'intake_error_stopped',
          'filing_check_stopped',
        ].includes(a.status)
          ? a.status
          : 'stall_alert',
        followup: false,
        updatedAt: a.detectedAt,
      });
  for (const row of rows) {
    if (
      row.state === 'closed' &&
      !inFlight.includes(row.status) &&
      row.status !== 'completed' &&
      !unfinishedExecution(row)
    )
      row.status = 'closed';
  }
  return rows.filter((row) => !row.draftId || !parents.has(row.draftId));
}
/** @param {string} status */
export const isFinished = (status: string) => finished.has(status);
export function unfinishedExecution(row: RequestRow): boolean {
  return (
    row.kind === 'issue' &&
    row.status !== 'closed' &&
    (row.status === 'completed' || row.state === 'closed') &&
    !!row.executionProgress &&
    (!!row.executionProgress.unavailable ||
      (row.executionProgress.pending ?? 0) > 0)
  );
}
/** @param {Row} row @returns {'attention'|'issues'|'waiting'|'stopped'|'completed'|'closed'|'followups'} */
export function rowGroup(row: RequestRow): Exclude<ListTab, 'todos'> {
  if (finished.has(row.status) && !unfinishedExecution(row))
    return row.status as 'completed' | 'closed';
  if (
    row.followup &&
    !interruptDelayed(row) &&
    !(row.kind === 'draft' && attentionClassification(row) === 'handoff')
  )
    return 'followups';
  const state = workState(row).state;
  return state === 'running' ? 'issues' : state;
}
/** @param {Row[] | unknown} alerts @returns {Row[]} */
export function fullTestRows(
  alerts: RequestRow[] | null | undefined,
): RequestRow[] {
  return (Array.isArray(alerts) ? alerts : [])
    .filter((a) => a?.id === 'full-test' && typeof a.title === 'string')
    .map((a) => ({
      ...a,
      kind: 'full_test',
      status: 'full_test_alert',
      followup: false,
      project: a.repo || 'mofua-suki/issue-desk-intake',
      updatedAt: a.finishedAt,
    }));
}
/** @param {Row[] | unknown} alerts @returns {Row[]} */
export function attentionRows(
  alerts: RequestRow[] | null | undefined,
): RequestRow[] {
  return (Array.isArray(alerts) ? alerts : [])
    .filter((a) => a !== null && typeof a === 'object' && !Array.isArray(a))
    .map((a, index) => ({
      ...a,
      id: `attention:${a.key ?? a.id ?? `unkeyed:${index}`}`,
      kind: 'attention',
      caseKind: a.kind ?? '',
      status:
        a.kind === 'prerequisite_missing'
          ? 'prerequisite_waiting'
          : [
                'incident',
                'job_recovery',
                'ops_watch',
                'rc_update_missing',
                'main_broken',
                'attention_pending',
                'usage_limit_wait',
                'heavy_rework',
                'unfiled_proposal',
                'split_scope_held',
                'consultation_budget',
                'acceptance_transfer',
                'consultation_stop',
              ].includes(a.kind ?? '')
            ? 'waiting'
            : (a.kind ?? ''),
      followup: false,
      issueTitle: a.title || '',
    }));
}

// 新種や状態名だけの失敗は監視側へ送る。分類は表示の責務であり、操作の認可には使わない。
export function attentionClassification(
  row: RequestRow,
): 'decision' | 'handoff' | 'monitoring' {
  if (
    !unfinishedExecution(row) &&
    (finished.has(row.status) ||
      row.state === 'resolved' ||
      row.state === 'closed')
  )
    return 'monitoring';
  if (row.work?.state === 'attention' || workState(row).state === 'attention')
    return 'decision';
  if (
    row.kind === 'draft' &&
    [
      'stalled',
      'ai_stopped',
      'submit_error',
      'submit_unknown',
      'error',
    ].includes(row.status) &&
    row.executionActive !== true &&
    row.executionScheduled !== true &&
    !row.retryAt &&
    row.hasChildren !== true &&
    ![row.splitChildren, row.consultationChildren].some(
      (children) =>
        Array.isArray(children) &&
        children.some((id) => typeof id === 'string' && id),
    )
  )
    return 'handoff';
  if (
    (row.handoff === true && row.reason?.trim()) ||
    (row.reason?.trim() &&
      [
        'abandoned',
        'send_unknown',
        'send_rejected',
        'permission_denied',
        'action_failed',
        'limit_reached',
        'stopped',
      ].includes(row.recoveryState || row.state || ''))
  )
    return 'handoff';
  return 'monitoring';
}

// Keep diagnostics and unfinished drafts, without treating ordinary running work
// as an incident. Unknown auxiliary kinds remain available to the watcher.
export function monitoringRequired(row: RequestRow): boolean {
  if (unfinishedExecution(row))
    return attentionClassification(row) === 'monitoring';
  if (attentionClassification(row) !== 'monitoring' || finished.has(row.status))
    return false;
  if (row.kind === 'issue')
    return [
      'failed',
      'review_held',
      'prerequisite_waiting',
      'scope_waiting',
    ].includes(row.status);
  if (row.kind === 'draft')
    return [
      'draft',
      'intake_waiting',
      'error',
      'stalled',
      'submit_error',
      'submit_unknown',
      'ai_stopped',
    ].includes(row.status);
  if (row.kind === 'close')
    return !['close_reviewing', 'close_closing'].includes(row.status);
  return true;
}

// 表示を原因単位でまとめても、元の行と承認対象は全て保持する。
export function selectAttention(rows: RequestRow[]) {
  const attention: RequestRow[] = [],
    monitoring: RequestRow[] = [];
  const causes = new Map<string, RequestRow>();
  for (const row of rows) {
    const classification = attentionClassification(row);
    if (classification === 'monitoring') {
      if (monitoringRequired(row)) monitoring.push(row);
      continue;
    }
    const target = row.number ?? row.issue ?? row.draftId ?? row.id ?? row.key;
    const independent = ['issue', 'draft', 'close'].includes(row.kind || '');
    const key = independent
      ? null
      : row.causeKey
        ? JSON.stringify([
            row.project ?? row.repo,
            row.causeKey,
            row.number ?? row.draftId ?? null,
          ])
        : classification === 'handoff' && target != null
          ? JSON.stringify([
              row.project ?? row.repo,
              target,
              row.caseKind ?? row.kind,
              row.reason?.normalize('NFKC').replace(/\s+/g, ' ').trim(),
              row.generation,
            ])
          : null;
    // 個別承認は原因が一致しても別の判断として残す。
    const approval =
      row.pendingFinalApprovals?.length ||
      row.permissionApproval ||
      row.deletionApproval;
    const existing = key && !approval ? causes.get(key) : undefined;
    const shown = {
      ...row,
      attentionClass: classification,
      work: row.work || workState(row),
      nextActor: classification === 'decision' ? 'user' : row.nextActor || 'ai',
      nextAction:
        row.nextActor === 'user' && row.nextAction
          ? row.nextAction
          : classification === 'handoff'
            ? '原因と再開条件を確認する（自動再開の予定なし）'
            : (row.work || workState(row)).nextAction,
      attentionMembers: [...(row.attentionMembers ?? [row])],
    };
    if (existing) {
      const members = [
        ...(existing.attentionMembers || [existing]),
        ...shown.attentionMembers,
      ];
      const rank = { attention: 0, stopped: 1, running: 2, waiting: 3 };
      if (
        rank[shown.work.state] <
        rank[(existing.work || workState(existing)).state]
      )
        Object.assign(existing, shown);
      existing.attentionMembers = members;
      continue;
    }
    attention.push(shown);
    if (key && !approval) causes.set(key, shown);
  }
  return { attention, monitoring };
}

// Work state is a display contract, never authorization to answer/retry/stop.
export function workState(row: RequestRow): WorkState {
  const state = (
    state: 'attention' | 'running' | 'waiting' | 'stopped',
    reason: string,
    nextActor: string,
    nextAction: string,
    waitCondition?: string,
    resumeBy?: string,
  ) => ({
    state,
    reason,
    nextActor,
    nextAction,
    ...(waitCondition ? { waitCondition, resumeBy } : {}),
  });
  const questions = (row.questions || [])
    .filter((q) => typeof q === 'string' && q.trim())
    .join(' / ');
  const approval =
    row.pendingFinalApprovals?.length ||
    row.permissionApproval ||
    row.deletionApproval;
  if (approval)
    return state(
      'attention',
      '利用者の個別承認を待っています。',
      '利用者',
      row.pendingFinalApprovals?.map((a) => a.reason).join(' / ') ||
        (row.permissionApproval
          ? '権限保護の変更を承認するか判断する'
          : '削除・移動を承認するか判断する'),
    );
  if (
    row.kind === 'draft' &&
    questions &&
    [
      'draft',
      'intake_waiting',
      'investigation_reported',
      'investigation_ended',
    ].includes(row.status)
  )
    return state(
      'attention',
      '相談の質問への回答を待っています。',
      '利用者',
      questions,
    );
  if (row.kind === 'close' && row.status === 'close_confirm')
    return state(
      'attention',
      'クローズの判断を待っています。',
      '利用者',
      'この依頼を閉じるか判断する',
    );
  // These two API kinds identify explicit manual continuation decisions. Keep
  // the source notification unchanged so existing archive bindings remain valid.
  const limitAction =
    row.kind === 'attention' && row.requiresUser !== false
      ? row.caseKind === 'consultation_budget'
        ? '相談の呼び出し回数・使用量の上限を足して続行するか判断する'
        : row.caseKind === 'usage_limit_wait'
          ? '再開見込みと停止理由を確認し、再試行するか判断する'
          : null
      : null;
  if (limitAction)
    return state(
      'attention',
      row.reason || '利用者の上限判断・再開判断を待っています。',
      '利用者',
      limitAction,
    );
  if (
    row.requiresUser === true &&
    (row.nextAction?.trim() || row.gist?.trim() || row.kind === 'issue')
  )
    return state(
      'attention',
      row.reason || '利用者の回答・上限判断を待っています。',
      '利用者',
      row.nextAction?.trim() ||
        row.gist?.trim() ||
        '依頼の質問・上限判断に回答する',
    );
  const stopped = [
    'failed',
    'stopped',
    'untracked',
    'review_held',
    'error',
    'stalled',
    'submit_error',
    'submit_unknown',
    'ai_stopped',
    'monitor_stopped',
    'intake_error_stopped',
  ];
  if (
    row.finalApprovalsUnavailable ||
    row.handoff ||
    stopped.includes(row.status) ||
    [
      'abandoned',
      'send_unknown',
      'send_rejected',
      'permission_denied',
      'action_failed',
      'limit_reached',
      'stopped',
    ].includes(row.recoveryState || row.state || '')
  )
    return state(
      'stopped',
      row.finalApprovalsUnavailable ||
        row.reason ||
        row.error ||
        row.detail ||
        '自動処理が停止しています。',
      row.nextActor || '受付・担当AI',
      row.nextAction || '原因と再開条件を確認する（自動再開の予定なし）',
    );
  if (row.executionActive === true)
    return state(
      'running',
      '実行元が処理の所有を確認しています。',
      row.kind === 'issue' ? '担当Worker' : '受付',
      '現在の処理を完了し、結果を反映する',
    );
  if (row.executionScheduled === true) {
    const condition = row.retryAt
      ? `再試行時刻 ${row.retryAt} に達する`
      : row.projectWait?.condition ||
        row.scopeWait?.note ||
        row.dependencyReason ||
        '依存作業・実行順・実行枠の条件が満たされる';
    return state(
      'waiting',
      row.retryAt
        ? '自動再試行を待っています。'
        : '自動の実行順・条件を待っています。',
      row.kind === 'issue' ? '担当Worker' : '受付',
      '条件が満たされたら自動で処理を開始する',
      condition,
      row.kind === 'issue' ? '受付の配布・担当Worker' : '受付の相談Engine',
    );
  }
  if (
    row.kind === 'draft' &&
    row.status === 'investigation_waiting' &&
    row.investigations?.length
  )
    return state(
      'waiting',
      '調査Issueの報告を待っています。',
      '受付',
      '調査報告を取り込み相談を再開する',
      '紐付いた調査Issueの報告が届く',
      '受付の調査報告収集',
    );
  return state(
    'stopped',
    row.reason ||
      row.error ||
      row.detail ||
      '実処理・自動再開の根拠を確認できません。',
    row.nextActor || '受付・担当AI',
    row.nextAction || '担当と再開条件を確認する（未確認のまま実行中にしない）',
  );
}
