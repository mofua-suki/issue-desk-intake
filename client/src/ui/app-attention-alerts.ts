// 要対応の自動処理（Issue #1190。受付の GET /api/attention/alerts、docs/intake-attention.md）。client/src/ui/app.js の一部で、
// 単独のモジュールではない（client/scripts/build.ts の順に app.js とつなぐ）。依頼一覧の分類（client/src/ui/request-list.js の
// requestGroups）の後に要対応へ足す。扱っている件ごとに1行で、自動の処理の状態（調査中・再試行済み・止めた理由など）を出す。
// 開いた画面から依頼と調査Issueを開け、運用の見張りの通知は確認済みにできる。
const attentionGuidance =
  '受付が失敗・レビュー保留を見つけ、調査Issueで原因を調べて再試行・修正・お知らせに振り分けています。利用者の判断が要るもの（品質に響く案・トークンの発行・sudo が要る作業）と、同じ原因で続けて失敗して自動の処理を止めたものは、依頼の画面から対応してください。';
// 件の対象（依頼の共通番号・Gitea番号）。
const attentionTarget = (a: RequestRow) =>
  `${deskLabel(a)}・${giteaNumber(a.number)}`;
// 受付の応答（配列でないもの・形の違う項目は出さない）を要対応の行にする。状態バッジは元の依頼の状態で出し、題名に自動の処理の状態を書く。
function attentionAlertRows(alerts: RequestRow[] | null | undefined) {
  return attentionRows(alerts).map((a) => ({
    ...a,
    title:
      a.caseKind === 'consultation_budget'
        ? `相談の呼び出しの上限で停止: ${a.title}`
        : a.caseKind === 'prerequisite_missing' ||
            a.caseKind === 'split_scope_held' ||
            a.caseKind === 'acceptance_transfer' ||
            a.caseKind === 'consultation_stop'
          ? a.title
          : a.caseKind === 'unfiled_proposal'
            ? `後続案 ${a.proposalId || '不明'}・${a.stateLabel}`
            : a.caseKind === 'main_broken' ||
                a.caseKind === 'rc_update_missing' ||
                a.caseKind === 'incident' ||
                a.caseKind === 'stage_watch' ||
                a.caseKind === 'attention_pending' ||
                a.caseKind === 'usage_limit_wait' ||
                a.caseKind === 'heavy_rework'
              ? a.title
              : a.caseKind === 'ops_watch'
                ? `運用の見張り: ${a.title}`
                : `要対応の自動処理: ${a.stateLabel}（${attentionTarget(a)}）: ${a.title || ''}`,
    guidance:
      a.reason ||
      (a.caseKind === 'split_scope_held' ? a.detail : attentionGuidance),
    updatedAt: a.updatedAt,
  }));
}
// 件の内容の行（依頼・種類・自動の処理の状態と理由・原因の要旨・回数・調査Issue）。
const jstIncidentTime = (value: string | number | Date) =>
  new Date(value).toLocaleString('ja-JP', { timeZone: 'Asia/Tokyo' });
function heavyReworkStatsSelection(alert: RequestRow) {
  const { fromDate, toDate } = alert.period || {};
  const validDate = (value: unknown): value is string => {
    if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(value))
      return false;
    const timestamp = Date.parse(`${value}T00:00:00Z`);
    return (
      Number.isFinite(timestamp) &&
      new Date(timestamp).toISOString().slice(0, 10) === value
    );
  };
  return validDate(fromDate) && validDate(toDate) && fromDate <= toDate
    ? {
        project: '',
        beforeFrom: fromDate,
        beforeTo: toDate,
        afterFrom: fromDate,
        afterTo: toDate,
      }
    : null;
}
function attentionAlertLines(alert: RequestRow) {
  const inv =
    typeof alert.investigation === 'object' ? alert.investigation : null;
  if (alert.caseKind === 'job_recovery')
    return [
      alert.issueTitle,
      alert.stateLabel,
      alert.reason,
      alert.gist,
    ].filter(Boolean);
  if (alert.caseKind === 'consultation_stop')
    return [alert.stateLabel, alert.reason];
  if (alert.caseKind === 'acceptance_transfer')
    return [alert.stateLabel, alert.reason, `待機開始: ${alert.startedAt}`];
  if (alert.caseKind === 'consultation_budget') {
    const stop = alert.budgetStop;
    if (!stop) return ['相談の停止記録がありません。'];
    return [
      `相談ID: ${alert.draftId}`,
      `回数 ${stop.calls} / 上限 ${stop.limits.calls}・量 ${stop.weighted} / 上限 ${stop.limits.weighted}`,
      `最後の未確認事項: ${stop.questions.join('、').slice(0, 120) || '質問の記録なし'}`,
    ];
  }
  if (alert.caseKind === 'split_scope_held')
    return [alert.stateLabel, alert.detail, `停止日時: ${alert.startedAt}`];
  if (alert.caseKind === 'prerequisite_missing')
    return [
      alert.stateLabel,
      alert.reason,
      ...(alert.artifacts ?? []).map(
        (a) =>
          `不足: ${a.artifact}／依存先: ${shownProject(a.repo)}・Gitea番号 ${a.issue}`,
      ),
    ];
  if (alert.caseKind === 'unfiled_proposal')
    return [
      `元${alert.sourceKind === 'pulls' ? 'PR' : 'Issue'}: ${shownProject(alert.project)}・Gitea番号 ${alert.number}`,
      `案ID: ${alert.proposalId || '不明'}`,
      `状態: ${alert.stateLabel}${alert.proposalStatus ? `（${alert.proposalStatus}）` : ''}`,
      `投稿時刻: ${alert.postedAt || '不明'}`,
      `確認時刻: ${alert.checkedAt || '不明'}`,
      `理由: ${alert.reason || '理由の記録なし'}`,
    ];
  if (alert.caseKind === 'heavy_rework') {
    const { fromDate, toDate } = alert.period || {};
    return [
      alert.title ?? '',
      `期間: ${fromDate}〜${toDate}`,
      `件数: ${alert.count}件`,
      `手戻り率: ${((alert.reworkRate ?? NaN) * 100).toFixed(1)}%（しきい値 ${(alert.threshold ?? NaN) * 100}%）`,
      alert.reason,
    ];
  }
  if (alert.caseKind === 'incident')
    return [
      `種類: ${alert.kindName}`,
      `状態: ${alert.stateLabel}`,
      `対応期限: ${jstIncidentTime(alert.deadline ?? NaN)}`,
      ...(alert.history ?? []).map(
        (entry) =>
          `${jstIncidentTime(entry.at)}・${entry.event}・${entry.reason}`,
      ),
    ];
  if (alert.stageWatch === true)
    return [
      `状態: ${alert.stateLabel}`,
      `期限: ${alert.deadline || '設定なし'}`,
      `対象: ${alert.project ? `${shownProject(alert.project)}・#${alert.number}` : '未割当て'}`,
      `保持者: ${alert.holder}（${alert.host}）`,
      `heartbeat: ${alert.heartbeatAt || '未受信'}`,
      `監視の観測経路: ${alert.monitorObservation || alert.observation}`,
      `成功確認の観測経路: ${alert.successObservation || '未確認'}`,
      `最終成功確認: ${alert.lastSuccessAt || '未確認'}`,
      `未完了の対処: ${alert.reason}`,
      ...(alert.event === 'result'
        ? [
            `process終了コード: ${alert.exitCode}`,
            `結果出力数: ${alert.outputs}`,
          ]
        : []),
    ];
  if (
    alert.caseKind === 'main_broken' ||
    alert.caseKind === 'attention_pending' ||
    alert.caseKind === 'usage_limit_wait'
  )
    return [alert.issueTitle, alert.stateLabel, alert.reason, alert.gist];
  if (alert.caseKind === 'ops_watch')
    return [
      alert.issueTitle,
      `理由: ${alert.reason}`,
      `PC: ${alert.host}`,
      `日時: ${alert.updatedAt}`,
    ];
  return [
    `依頼: ${deskLabel(alert)}（${shownProject(alert.project)}・${giteaNumber(alert.number)}）${alert.issueTitle ? ` ${alert.issueTitle}` : ''}`,
    `種類: ${alert.kindName || alert.caseKind}`,
    `自動の処理: ${alert.stateLabel}${alert.reason ? ` — ${alert.reason}` : ''}`,
    `原因の要旨: ${alert.gist || '記録なし'}`,
    `自動の処理（再試行・修正）の回数: ${alert.autoCount ?? 0}回・同じ原因で続けた回数: ${alert.streak ?? 1}回`,
    `調査Issue: ${inv ? `${deskLabel(inv)}（${shownProject(inv.project)}・${giteaNumber(inv.number)}）` : 'まだありません'}`,
  ];
}
// 要対応の行を一意に開くURL。行のキー（受付の要対応の key）だけを載せる。
const attentionHash = (key: string | number | boolean) =>
  `#/attention/${encodeURIComponent(key)}`;
const attentionMissing =
  '要対応の行が見つかりませんでした。済んで消えたか、URLの誤り・アクセス権・接続状態を確認してください。依頼一覧から開き直せます。';
// URLから開いた要対応の行。受付の要対応から同じキーの行を探し、一覧から開いたときと同じ画面にする。
async function attentionRouteView(key: string | undefined) {
  const id = reset();
  current = { type: 'attention' };
  const alerts = await api<RequestRow[]>('/api/attention/alerts').catch((e) => {
    if (isCurrent(id)) routeError(e, attentionMissing);
    return null;
  });
  if (!alerts || !isCurrent(id)) return;
  const alert = attentionAlertRows(alerts).find((a) => a.key === key);
  if (!alert)
    return routeError(Error('要対応の行が見つかりません。'), attentionMissing);
  attentionAlertView(alert, false);
}
// 要対応の行を開いた画面。運用の見張りはIssueが無くても確認済みにできる。
function attentionAlertView(alert: RequestRow, navigate: Navigation = true) {
  if (!alert.key)
    return routeError(Error('通知の識別子がありません。'), attentionMissing);
  const key = alert.key;
  if (navigate !== false) remember(attentionHash(key));
  reset();
  current = { type: 'attention' };
  const inv =
    typeof alert.investigation === 'object' ? alert.investigation : null;
  const heavyReworkSelection =
    alert.caseKind === 'heavy_rework' ? heavyReworkStatsSelection(alert) : null;
  const budget = alert.caseKind === 'consultation_budget' && !!alert.draftId;
  const resumeBudget = budget
    ? budgetContinueButton(alert.draftId!, alert.stopCount)
    : null;
  const done = el('button', {
    type: 'button',
    class: 'primary',
    text: '確認済みにする',
  });
  const respond = el('button', {
    type: 'button',
    class: 'primary',
    text: '自分で対応する',
  });
  respond.onclick = () =>
    action(respond, async () => {
      await api(
        `/api/incidents/${encodeURIComponent(key.slice('incident:'.length))}/respond`,
        {},
      );
      toast('自分で対応することを記録しました。');
      if (listCache) keepList({ ...listCache, at: 0 });
      await home();
    });
  done.onclick = () =>
    action(done, async () => {
      const incident = alert.caseKind === 'incident';
      const id = key.slice(incident ? 'incident:'.length : 'ops-watch:'.length);
      await api(
        incident
          ? `/api/incidents/${encodeURIComponent(id)}/acknowledge`
          : `/api/ops-watch/notices/${encodeURIComponent(id)}/acknowledge`,
        {},
      );
      toast('通知を確認済みにしました。');
      if (listCache)
        keepList({
          ...listCache,
          at: 0,
          attentionAlerts: (listCache.attentionAlerts || []).filter(
            (x) => x.key !== alert.key,
          ),
        });
      await home();
    });
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    ...(budget
      ? [el('a', { href: draftHash(alert.draftId!), text: '相談を開く' })]
      : []),
    head(
      'ATTENTION',
      alert.title ?? '',
      budget
        ? '続けるなら「上限を足して続行する」、やめるなら相談を開いて「この相談を終える」'
        : alert.caseKind === 'ops_watch'
          ? '運用の見張りからのお知らせです。利用者の判断が必要なものを確認してください。'
          : alert.caseKind === 'prerequisite_missing' ||
              alert.caseKind === 'unfiled_proposal' ||
              alert.caseKind === 'acceptance_transfer' ||
              alert.caseKind === 'job_recovery' ||
              alert.caseKind === 'split_scope_held'
            ? alert.reason || alert.detail
            : alert.caseKind === 'heavy_rework'
              ? alert.reason
              : attentionGuidance,
    ),
    el(
      'ul',
      { class: 'attention-lines' },
      attentionAlertLines(alert).map((text) => el('li', { text })),
    ),
    el(
      'div',
      { class: 'actions' },
      ...(budget ? [resumeBudget] : []),
      ...(rejectedProposalAlert(alert)
        ? [proposalDismissalControl(alert)]
        : []),
      ...(['split_scope_held', 'consultation_stop'].includes(
        alert.caseKind ?? '',
      ) && alert.draftId
        ? [
            el('button', {
              type: 'button',
              text: '相談を開く',
              onClick: () => draftView(alert.draftId!),
            }),
          ]
        : []),
      ...(alert.caseKind === 'incident' && alert.state === 'waiting'
        ? [respond]
        : []),
      ...((alert.caseKind === 'ops_watch' &&
        alert.stageWatch !== true &&
        key.startsWith('ops-watch:')) ||
      (alert.caseKind === 'incident' && alert.state !== 'waiting')
        ? [done]
        : []),
      ...(!budget &&
      alert.project &&
      alert.number &&
      !(alert.caseKind === 'unfiled_proposal' && alert.sourceKind === 'pulls')
        ? [
            el('button', {
              type: 'button',
              onClick: () =>
                issueView(
                  alert.project ?? null,
                  alert.number ?? null,
                  true,
                  alert.desk,
                ),
              text: '依頼を開く',
            }),
          ]
        : []),
      ...(alert.caseKind === 'unfiled_proposal' && alert.url
        ? [
            el('a', {
              href: alert.url,
              target: '_blank',
              rel: 'noreferrer',
              text: '元IssueをGiteaで開く ↗',
            }),
          ]
        : []),
      ...(heavyReworkSelection
        ? [
            el('button', {
              type: 'button',
              onClick: () => {
                statsSelection = heavyReworkSelection;
                statsView();
              },
              text: '稼働実績の内訳を開く',
            }),
          ]
        : []),
      ...(inv
        ? [
            el('button', {
              type: 'button',
              onClick: () =>
                issueView(inv.project ?? null, inv.number, true, inv.desk),
              text: '調査Issueを開く',
            }),
          ]
        : []),
    ),
    ...(budget
      ? [
          el(
            'details',
            {},
            el('summary', { text: '停止時の記録をすべて表示' }),
            el('p', { text: alert.reason }),
          ),
        ]
      : []),
  );
}

// Corresponding controls for rejected proposals only; adopted drafts keep their existing finish flow.
function rejectedProposalAlert(alert: RequestRow) {
  return (
    alert.caseKind === 'unfiled_proposal' &&
    ['rejected', 'scan_excluded'].includes(alert.proposalStatus ?? '')
  );
}
function proposalDismissalControl(alert: RequestRow) {
  return el('button', {
    type: 'button',
    text: '対応不要として整理',
    onClick: () => proposalDismissalView(alert),
  });
}
function invalidateProposalList() {
  if (listCache) keepList({ ...listCache, at: 0 });
}
async function proposalDismissalView(alert: RequestRow) {
  const query = new URLSearchParams({
    project: alert.project ?? '',
    number: String(alert.number),
    proposalId: alert.proposalId ?? '',
    commentId: String(alert.commentId),
  });
  const candidate = await api<ProposalDismissalCandidate>(
    '/api/proposals/dismissal?' + query,
  );
  const reason = el('textarea', {
    id: 'proposal-dismissal-reason',
    placeholder: '対応不要とする理由',
    maxLength: 1000,
  });
  const save = el('button', {
    type: 'button',
    class: 'primary',
    text: 'この案を対応不要にする',
  });
  const operationId = crypto.randomUUID();
  save.onclick = () =>
    action(save, async () => {
      if (!reason.value.trim())
        throw Error('対応不要とする理由を入力してください。');
      await api('/api/proposals/dismiss', {
        project: alert.project,
        number: alert.number,
        proposalId: alert.proposalId ?? '',
        commentId: alert.commentId,
        fingerprint: candidate.fingerprint,
        id: operationId,
        reason: reason.value.trim(),
      });
      invalidateProposalList();
      toast('理由を残して対応不要にしました。');
      await home();
    });
  $('#app').replaceChildren(
    el('button', {
      class: 'back',
      text: '← 要対応に戻る',
      onClick: () => attentionAlertView(alert),
    }),
    head(
      'FOLLOW-UP',
      candidate.title || '形式不正の後続案',
      '元のIssueと提案コメントは保持します。',
    ),
    el('p', { text: candidate.purpose || '' }),
    ...(candidate.body !== undefined
      ? [
          el('p', {
            text: `保存時の理由: ${candidate.recordedReason || '当時の詳細未保存'}`,
          }),
          el('p', { text: `現在の解析結果: ${candidate.parseError || ''}` }),
          el('pre', { text: candidate.body }),
        ]
      : []),
    el(
      'ul',
      {},
      (candidate.acceptance ?? []).map((text) => el('li', { text })),
    ),
    el('label', { for: 'proposal-dismissal-reason', text: '対応不要の理由' }),
    reason,
    save,
  );
}
// 同じ停止回では、再描画・通信再送・画面間の移動でも操作IDを変えない。
const budgetOperations = new Map<string, BudgetOperation>();
function budgetContinueButton(draftId: string, stopCount: number | undefined) {
  const key = `${draftId}:${stopCount}`;
  if (!budgetOperations.has(key))
    budgetOperations.set(key, {
      operationId: crypto.randomUUID(),
      pending: false,
    });
  const operation = budgetOperations.get(key)!;
  const button = el('button', {
    type: 'button',
    class: 'primary',
    text: '上限を足して続行する',
    disabled: operation.pending,
  });
  operation.button = button;
  button.onclick = () => {
    if (operation.pending) return;
    operation.pending = true;
    const id = routeId;
    return action(button, async () => {
      let notice = null;
      try {
        // 入力を退避できなければ描き直さず、そのまま再操作できるようにする。
        try {
          await activeComposer?.checkpoint();
        } catch (caughtValue) {
          const e = uiError(caughtValue);
          if (isCurrent(id)) error(e);
          return;
        }
        if (!isCurrent(id)) return;
        try {
          await api(
            `/api/attention/consultation-budget/${encodeURIComponent(draftId)}/continue`,
            {
              operationId: operation.operationId,
              stopCount,
            },
          );
          if (listCache) keepList({ ...listCache, at: 0 });
        } catch (caughtValue) {
          const e = uiError(caughtValue);
          notice = e;
        }
        if (!isCurrent(id)) return;
        // 古い停止回・通信失敗でも最新状態を取得。取得失敗時は表示中の会話を残す。
        let latest;
        try {
          latest = await api<Draft>(
            `/api/drafts/${encodeURIComponent(draftId)}`,
          );
        } catch (caughtValue) {
          const e = uiError(caughtValue);
          if (isCurrent(id)) error(notice || e);
          return;
        }
        if (!isCurrent(id)) return;
        if (current?.type === 'draft' && current.id === draftId) {
          renderDraft(latest);
          if (notice) error(notice);
        } else await draftView(draftId, notice);
      } finally {
        operation.pending = false;
        if (operation.button) operation.button.disabled = false;
      }
    });
  };
  return button;
}
function consultationBudgetNotice(d: Draft) {
  return el(
    'section',
    { class: 'panel panel-body budget-notice', 'aria-label': '相談の上限停止' },
    el('strong', { text: `相談 ${d.id}` }),
    el('p', {
      text: '相談の呼び出し回数または利用量が上限に達したため停止しています。',
    }),
    el('p', {
      text: '追加20回・3000000。この相談を続ける場合は『上限を足して続行する』を選んでください。',
    }),
    (d.actions || []).includes('continue_budget')
      ? budgetContinueButton(d.id, d.consultationBudget?.stops?.length)
      : hint('現在は続行できません。処理中の場合は終了を待ってください。'),
  );
}
