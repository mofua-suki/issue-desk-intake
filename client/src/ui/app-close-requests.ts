// 受付画面の他のPCからのクローズ依頼（要対応の「閉じてよいか」の確認）。client/src/ui/app.js の一部（Issue #457）で、単独の
// モジュールではない。受付は client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。
// --- 他のPCからのクローズ依頼（要対応の「閉じてよいか」） ------------------------------------------------
const closeStates: Record<string, string> = {
  reviewing: '受付AIが確認しています',
  confirm: '受付AIが確認できなかったため、閉じてよいかの判断を待っています',
  closing: '閉じる操作の結果を照合しています',
  closed: '閉じました',
  already_closed: '既に閉じていました（終了理由・履歴は変更していません）',
  declined: '「閉じない」を選びました（Issueは変更していません）',
  expired: '依頼の後に再オープンされたため適用しませんでした',
};
// 任意終了（voluntary）は受付AIを呼ばず機械判定する（server/src/close-voluntary.mjs）ので、判断の主体をそう表示する。
const closeByMachine = (r: CloseRequest) =>
  r.reason === 'voluntary' || r.review?.source === 'machine';
const closeStateText = (r: CloseRequest) =>
  closeByMachine(r) && r.status === 'reviewing'
    ? '機械判定で確認しています'
    : closeByMachine(r) && r.status === 'confirm'
      ? '機械判定で閉じてよいと確認できなかったため、閉じてよいかの判断を待っています'
      : closeStates[r.status] || r.status;
// 受付が取得した資料の要約。取得元（依頼者の申告・実装パイプラインの記録）と、見つからなかった参照を区別する。
function closeMaterialText(m: CloseMaterial) {
  const reference = (x: CloseReference) => {
    const ref = shownRepositoryReference(x.ref, repositoryProjects());
    const source =
      x.source === 'pipeline' ? '（実装の記録）' : '（依頼者の申告）';
    if (!x.found) return `${ref}${source}: ${x.note}`;
    if (x.kind === 'pull')
      return `${ref}${source}: PR「${x.title}」 ${x.merged ? `マージ済み（${x.base}）` : `未マージ（${x.state}）`}`;
    if (x.kind === 'commit')
      return `${ref}${source}: コミット ${String(x.message || '').split('\n')[0]}`;
    return `${ref}${source}: Issue「${x.title}」（${x.state}${x.desk ? `、共通番号 #${x.desk}` : ''}）`;
  };
  return [
    ...(m.references || []).map(reference),
    ...(m.references?.length
      ? []
      : ['参照（owner/repo#番号・owner/repo@コミット）はありません。']),
    ...(m.ambiguous?.length
      ? [
          `リポジトリ名の無い番号（解決していません）: ${m.ambiguous.map((n) => `#${n}`).join('、')}`,
        ]
      : []),
    ...(m.defaultBranch !== undefined
      ? [`対象ブランチ: ${m.defaultBranch ?? '不明'}`]
      : []),
    ...(m.successors
      ? [
          `後続: ${m.successors.found.length ? m.successors.found.map((x) => `${shownProject(x.project)}#${x.number}「${x.title}」`).join('、') : 'なし'}`,
          ...(m.successors.broken.length
            ? [
                `読めない依存指定: ${m.successors.broken.map((x) => `${shownProject(x.project)}#${x.number}`).join('、')}`,
              ]
            : []),
        ]
      : []),
  ].join('\n');
}
async function closeRequestView(
  requestId: string,
  navigate: Navigation = true,
) {
  // 戻り先は要対応。フォローアップタブから開いた確認はフォローアップタブへ戻す。
  if (tab !== 'followups') tab = 'attention';
  if (navigate) remember(`#/close-requests/${requestId}`);
  const id = reset();
  current = { type: 'close', id: requestId };
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    el('div', { class: 'loading', text: 'クローズ依頼を読み込んでいます…' }),
  );
  let r;
  try {
    r = await api<CloseRequest>(`/api/close-requests/${requestId}`);
  } catch (caughtValue) {
    const e = uiError(caughtValue);
    if (isCurrent(id))
      routeError(
        e,
        'クローズ依頼を開けませんでした。依頼一覧から開き直してください。',
      );
    return;
  }
  if (!isCurrent(id)) return;
  renderCloseRequest(r);
  followCloseRequest(r);
}
// 確認中・照合中の間だけ読み直す。読むたびに受付が結果の照合を進める（server/src/close-requests.mjs の reconcileLater）
// ので、画面の「閉じる」の結果が照合中でも同じように読み直す。判断待ちの表示は利用者の操作まで変えない。
function followCloseRequest(r: CloseRequest) {
  if (!['reviewing', 'closing'].includes(r.status)) return;
  const id = routeId;
  async function refresh() {
    const next = await api<CloseRequest>(`/api/close-requests/${r.id}`);
    if (!isCurrent(id)) return;
    renderCloseRequest(next);
    if (['reviewing', 'closing'].includes(next.status)) poll(refresh, 5000);
  }
  poll(refresh, 5000);
}
function renderCloseRequest(r: CloseRequest) {
  const shown = () => current?.type === 'close' && current.id === r.id;
  const block = (label: string, text: string | undefined) =>
    text
      ? el(
          'div',
          { class: 'close-field' },
          el('strong', { text: label }),
          el('div', { class: 'issue-body', text }),
        )
      : null;
  const decide = (kind: string, label: string) => {
    const button = el('button', {
      type: 'button',
      class: kind === 'close' ? 'primary' : 'quiet',
      text: label,
    });
    button.onclick = () =>
      action(button, async () => {
        let next;
        try {
          // 表示した確認内容を添える。表示の後に更新されていれば受付が断り、最新の内容を出し直す。
          next = await api<CloseRequest>(
            `/api/close-requests/${r.id}/${kind}`,
            { fingerprint: r.fingerprint ?? null },
          );
        } catch (caughtValue) {
          const e = uiError(caughtValue);
          if (e.status !== 409 || !shown()) throw e;
          await closeRequestView(r.id, false);
          if (shown()) error(e);
          return;
        }
        if (!shown()) return;
        renderCloseRequest(next);
        followCloseRequest(next);
        toast(closeStateText(next));
      });
    return button;
  };
  const m = r.material;
  // 閉じる・閉じないは受付の応答の actions にあるものだけを出す（server/src/intake-actions.mjs）。
  const decisions = [
    ['close', '閉じる'],
    ['keep', '閉じない'],
  ].filter(([kind]) => (r.actions || []).includes(kind));
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    head('CLOSE REQUEST', '閉じてよいかの確認', closeStateText(r)),
    el(
      'section',
      { class: 'panel close-request' },
      block('対象リポジトリ', shownProject(r.project) ?? undefined),
      block(
        '番号',
        `受付の共通番号: ${r.desk ? `#${r.desk}` : '未確定'} / ${giteaNumber(r.number)}`,
      ),
      block('依頼元PC（トークンの名前）', r.pc),
      block('終了理由', `${closeReasons[r.reason] || r.reason}（${r.reason}）`),
      block('依頼者の説明', r.description),
      block('補足・根拠（依頼者の申告）', r.comment),
      block('実際の題名', r.target?.title),
      block('実際の本文', r.target?.body),
      block('受付の判断', r.message),
      r.review?.reason && r.review.reason !== r.message
        ? block(
            closeByMachine(r) ? '機械判定の理由' : '受付AIの判断理由',
            r.review.reason,
          )
        : null,
      r.review?.evidence?.length
        ? block(
            closeByMachine(r) ? '機械判定が挙げた根拠' : '受付AIが挙げた根拠',
            r.review.evidence.join('\n'),
          )
        : null,
      m ? block('受付が確認した資料', closeMaterialText(m)) : null,
      m?.unavailable?.length
        ? block('取得できなかった資料', m.unavailable.join('\n'))
        : null,
      r.url
        ? el('a', {
            href: r.url,
            target: '_blank',
            rel: 'noreferrer',
            text: 'Giteaで開く ↗',
          })
        : null,
      decisions.length
        ? el(
            'div',
            { class: 'actions' },
            ...decisions.map(([kind, label]) => decide(kind, label)),
          )
        : null,
      decisions.length
        ? hint(
            '「閉じる」を選んでも、閉じる直前に処理中の状態・内容・根拠を確認し直し、処理中なら閉じません。「閉じない」はIssueを変更しません。',
          )
        : null,
    ),
  );
}
