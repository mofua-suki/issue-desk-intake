// 受付画面の相談（下書き）を開く・読み直す・起票後の移動と、分割案・相談の優先度の部品。画面の組み立て（renderDraft）は
// client/src/ui/app-draft-view.js。client/src/ui/app.js の一部（Issue #457）で、単独のモジュールではない。受付は client/scripts/build.ts の
// 順に app.js とつないだ1つのモジュールとして /app.js を配信する。
// 下書きは個別URLで開く。貼り付け・別タブ・再読み込み・認証後の復元が同じ相談へ着く。
// posted: 取得済みの相談（操作のPOSTの応答のうち draftPosted が通したもの）。渡されたら開くときの取得を省く。
async function draftView(
  consultationId: string,
  notice: unknown = null,
  navigate: Navigation = true,
  posted: Draft | null = null,
): Promise<void> {
  const changingConsultation =
    current?.type !== 'draft' || current.id !== consultationId;
  if (navigate !== false)
    remember(draftHash(consultationId), navigate === 'replace');
  const id = reset();
  // 別の相談を開くときは依頼内容タブの選択を引き継がない。同じ相談の再描画では利用者の選択を保つ。
  if (changingConsultation) mobileView = 'chat';
  current = { type: 'draft', id: consultationId };
  // 不正・破棄済み・アクセス拒否・取得失敗はどれも案内と一覧への戻り口を出す。別の下書きは表示しない。
  // 通知は相談を表示できたときの知らせ。開けなかったときは実際の失敗理由を出す。
  let returned = false;
  const loaded =
    posted ??
    (await api<Draft>(`/api/drafts/${consultationId}`).catch((e) => {
      // TODOに戻された相談は、同じIDのTODOへ移る。ほかの理由の409は移らずに理由を出す。
      if (returnedToTodo(e)) returned = true;
      else if (isCurrent(id)) routeError(e, draftMissing);
      return null;
    }));
  if (returned && isCurrent(id)) return returnedDraft(consultationId);
  if (!loaded || !isCurrent(id)) return;
  const d = loaded;
  if (
    d.status === 'submitted' &&
    !d.familyProgress &&
    !Array.isArray(d.splitChildren)
  )
    return submittedDraft(d);
  if (d.status === 'project_handoff') {
    // 引き継いだ相談はこれ以上変わらないので、元の会話を出すだけで読み直さない。
    renderDraft(d);
    if (notice) error(notice);
    return;
  }
  renderDraft(d);
  if (notice) error(notice);
  // 周期は最後に読めた相談で決める。描き直しを見送った回（比べる欄が同じ・優先度の選び直し中）も、動いているかは新しい応答に従う。
  let latest = d;
  const moving = () => draftMoving(latest);
  pollDraft(refresh, moving);
  async function refresh() {
    let next;
    try {
      next = await api<Draft>(`/api/drafts/${consultationId}`);
    } catch (caughtValue) {
      const e = uiError(caughtValue);
      // 開いている間に別の画面でTODOに戻された。
      if (returnedToTodo(e) && isCurrent(id))
        return returnedDraft(consultationId);
      if (e.status === 404 && isCurrent(id)) return routeError(e, draftMissing);
      throw e;
    }
    if (!isCurrent(id)) return;
    if (
      next.status === 'submitted' &&
      !next.familyProgress &&
      !Array.isArray(next.splitChildren)
    )
      return submittedDraft(next);
    // 更新中に引き継がれた場合も入力を退避し、元の会話を表示する。
    if (next.status === 'project_handoff') {
      const owner = activeComposer;
      await owner?.checkpoint();
      if (!isCurrent(id) || owner !== activeComposer) return;
      renderDraft(next);
      return;
    }
    // 選び直して送る前の優先度は、定期更新で描き直して消さない（送るか元に戻すまで待つ）。
    latest = next;
    if (draftPriorityHeld(d)) return pollDraft(refresh, moving);
    // draftStatus also follows the AI queue (順番待ち ↔ 整理中) and automatic recovery, which change
    // neither status nor revision; retryAt moves with every further automatic retry.
    if (
      draftStatus(next) !== draftStatus(d) ||
      next.revision !== d.revision ||
      next.error !== d.error ||
      next.stallCause !== d.stallCause ||
      next.retryAt !== d.retryAt ||
      // 別の画面で変えた優先度と、変えられるかどうか（Issue #448）。
      next.priority !== d.priority ||
      JSON.stringify(next.prioritySource) !==
        JSON.stringify(d.prioritySource) ||
      next.priorityChangeable !== d.priorityChangeable ||
      // 起票の待ち件数・詰まり判定は同じ状態とrevisionのまま時間で変わる。
      JSON.stringify(next.familyProgress) !==
        JSON.stringify(d.familyProgress) ||
      JSON.stringify(next.filingQueue) !== JSON.stringify(d.filingQueue) ||
      // 受付AIのターンが終わりきるなど、状態を変えずに押せる操作だけが変わることもある。
      String(next.actions) !== String(d.actions) ||
      // 前回の処理を終了中の案内（Issue #471）。
      next.finishingTurn !== d.finishingTurn
    ) {
      const owner = activeComposer;
      await owner?.checkpoint();
      if (!isCurrent(id) || owner !== activeComposer) return;
      if (!next.familyProgress) delete d.familyProgress;
      if (next.stallCause === undefined) delete d.stallCause;
      if (next.priority === undefined) delete d.priority;
      if (next.prioritySource === undefined) delete d.prioritySource;
      Object.assign(d, next);
      renderDraft(d);
    }
    pollDraft(refresh, moving);
  }
}
// 受付の処理が動いている相談か（受付AIのターン・順番待ち・自動の再試行・起票の処理中・分割した子の残り）。動いている間は
// 相談の保存の通知だけに頼らず5秒ごとに読む（client/src/ui/app-live.ts の pollDraft）。executionActive・executionScheduled は
// 受付が実行中のターンから決める（server/internal/consultations/screen_views.go）。子の進み具合（familyProgress）は
// 子のIssueの変化で変わり、親の相談の通知は届かないので、残りが0と確かめられるまで動いているとみなす。
const draftMoving = (d: Draft) =>
  !!(
    d.executionActive ||
    d.executionScheduled ||
    d.finishingTurn ||
    d.queued ||
    d.retryAt ||
    d.filingQueue ||
    ['submitting', 'auto_ready'].includes(d.status) ||
    (d.familyProgress &&
      !(d.familyProgress.remaining === 0 && !d.familyProgress.unknown))
  );
// 操作のPOSTの応答（受付の DraftView）を、開き直すときの取得（GET /api/drafts/{id}）の代わりに使えるときだけ返す。
// GET だけが足す内容を持つ相談は null を返して読み直させる: 分割の親（splitChildren）は子の進み具合（familyProgress）、
// 受付票のある相談（表示中の receiptState）は受付票の状態。処理を受け付けただけで結果が後から出る応答（202 の chat・submit）と、
// TODOを返す todo は、この関数へ渡さず従来どおり読み直す。
function draftPosted(shown: Draft, posted: Draft): Draft | null {
  return posted?.id === shown.id &&
    posted.splitChildren == null &&
    !('receiptState' in shown)
    ? posted
    : null;
}
// TODOに戻された相談（Issue #458）のURLは、同じIDのTODOへ移る。開始済みのTODO → 相談画面の対になる動きで、
// 置き換えにして戻る操作が相談とTODOの間で循環しないようにする。
// TODOに戻された相談の409か（Issue #514）。状態番号だけでなく受付の識別子（server/src/intake-draft-rules.mjs の returnedCode）で見分ける。
const returnedToTodo = (e: unknown) => {
  const problem = uiError(e);
  return problem.status === 409 && problem.code === 'draft_returned_to_todo';
};
function returnedDraft(consultationId: string): Promise<void> {
  return todoView(
    consultationId,
    Error('この相談はTODOに戻されました。TODOの画面へ移りました。'),
    'replace',
  );
}
async function submittedDraft(d: Draft) {
  const id = routeId;
  let key = `draft:${d.id}`;
  let input = activeComposer || composer(key, el('textarea'));
  await input.adopt(d);
  if (!isCurrent(id) || !input.valid()) return;
  if (!input.hasContent()) {
    key = 'new-request';
    input = composer(key, el('textarea'));
    await input.ready;
    if (!isCurrent(id) || !input.valid()) return;
    if (input.scope()?.draft !== d.id && input.creation()?.id !== d.id)
      return submittedLanding(d);
    await input.adopt(d);
  }
  if (!isCurrent(id) || !input.valid()) return;
  if (input.hasContent()) renderDraft(d, key);
  else {
    await input.clear();
    if (input.valid()) return submittedLanding(d);
  }
}
// 起票済み下書きのURLは対応Issueへ自動で移る。置き換えにして戻る操作が循環しないようにする。
// 複数のIssueへ分けて起票した相談は、どれか1件へ移さず、各Issueへ移動できる一覧を出す。
function submittedLanding(d: Draft) {
  if (!d.issue || (d.issues?.length ?? 0) > 1) return renderDraft(d);
  return issueView(
    issueProject(d) ?? null,
    d.issue.number,
    'replace',
    d.issue.desk,
  );
}
// 分割案（実施順）と、複数プロジェクトにまたがる理由・対象範囲。起票後は各Issueへの移動口を添える。
function splitPlan(d: Draft) {
  const plans = d.splitIssues || [];
  // プロジェクト未定の相談（Issue #689）で対象の無い案は、親のプロジェクトへの案にならないので「プロジェクト未定」と出す。
  const target = (p: SplitPlan) => p.project || d.project || unassignedText;
  // labelで包むと、中のどこを押しても最初のボタンが押されるため、見出し付きのdivにする。
  const field = (label: string, node: HTMLElement) =>
    el('div', { class: 'field' }, el('span', { text: label }), node);
  const created = (n: number) => d.issues?.[n];
  const open = (i: DraftIssue) =>
    el('button', {
      type: 'button',
      class: 'quiet',
      text: `${shownProject(issueProject(d, i))} ${deskLabel(i)}（${giteaNumber(i.number)}）を開く`,
      onClick: () =>
        issueView(issueProject(d, i) ?? null, i.number, true, i.desk),
    });
  return [
    d.crossProposal
      ? field(
          '複数プロジェクトにまたがる理由',
          el(
            'div',
            { class: 'issue-body cross-proposal' },
            `${d.crossProposal.reason}\n${d.crossProposal.scopes.map((s) => `・${shownProject(s.project)}: ${s.scope}`).join('\n')}`,
            hint(
              d.crossProposal.approved
                ? '依頼者が会話で了承した範囲です。範囲内の具体化は再確認せずに自動起票します。'
                : '了承待ちです。この理由・対象・範囲で進めてよいか、相談欄から返信してください。修正したい点を送った場合は了承として扱いません。',
            ),
          ),
        )
      : null,
    plans.length
      ? field(
          '分割案（実施順）',
          el(
            'ol',
            { class: 'split-plan' },
            plans.map((p, n: number) =>
              el(
                'li',
                {},
                el('strong', { text: `${n + 1}. ${p.title}` }),
                hint(`リポジトリ · ${shownProject(target(p))}`),
                el('div', { class: 'issue-body', text: `目的: ${p.purpose}` }),
                el('div', {
                  class: 'issue-body',
                  text: `受入条件:\n${p.acceptance.map((x) => `・${x}`).join('\n')}`,
                }),
                hint(
                  `依存先 · ${(p.dependsOn || []).map((k: number) => `${k}. ${plans[k - 1]?.title || ''}（${plans[k - 1] ? shownProject(target(plans[k - 1])) : '不明'}）`).join('、') || 'なし'}`,
                ),
                created(n) ? open(created(n)!) : null,
              ),
            ),
          ),
        )
      : null,
    // 分割案の無い相談でも、複数のIssueへ起票した結果があれば各Issueへ移動できるようにする。
    !plans.length && (d.issues?.length ?? 0) > 1
      ? field('起票したIssue', el('div', {}, d.issues!.map(open)))
      : null,
  ];
}
// 代理の回答（Issue #757・#758。docs/intake-proxy.md）。role=proxy の発言は受付の代理のAIが資料・コード・共通番号の
// 解決結果・過去の決定を根拠に答えたもので、利用者の発言ではない。根拠の種類は server/src/intake-proxy.mjs の proxyEvidenceKinds。
const proxyEvidenceKinds: Record<string, string> = {
  document: '資料',
  decision: '過去の決定',
  reference: '共通番号の解決結果',
  recommendation: '受付AIの推奨',
};
function proxyEvidenceText(e: ProxyEvidence | null | undefined) {
  const at = [e?.source, e?.location].filter(Boolean).join(' ');
  return `${proxyEvidenceKinds[e?.kind ?? ''] || e?.kind || ''}: ${at}${e?.detail ? `（${e.detail}）` : ''}`;
}
// 代理の発言の中身。構造（answers）があれば質問・回答・根拠を1件ずつ出し、無ければ本文のまま出す。
function proxyAnswerList(m: DraftMessage) {
  if (!Array.isArray(m.answers) || !m.answers.length) return markdown(m.text);
  return el(
    'ol',
    { class: 'proxy-answers' },
    m.answers.map((a) =>
      el(
        'li',
        {},
        el('div', { text: `質問: ${a.question}` }),
        el('div', { text: `回答: ${a.answer}` }),
        hint(
          `根拠: ${(a.evidence || []).map(proxyEvidenceText).join(' / ') || '記載なし'}`,
        ),
      ),
    ),
  );
}
// 代理の回答の取り消し（Issue #758。POST /api/drafts/{id}/proxy-undo）。出せるかは受付の判断（proxy_undo）。
// 会話がこの発言の前まで戻るので、破棄と同じく1回目で確認に変わり、2回目で取り消す。断られたら理由を出して開き直す。
function proxyUndoButton(
  d: Draft,
  m: DraftMessage,
  inputImages: ComposerController,
) {
  return el('button', {
    type: 'button',
    class: 'quiet',
    text: '取り消して、この質問から答え直す',
    onClick: (e) => {
      const button = e.currentTarget;
      if (button.dataset.armed !== '1') {
        button.dataset.armed = '1';
        button.textContent =
          'もう一度押すと取り消します（この後の会話も戻ります）';
        return;
      }
      return action(button, async () => {
        let posted: Draft;
        try {
          posted = await inputImages.live(
            api<Draft>(`/api/drafts/${d.id}/proxy-undo`, {
              messageId: m.id,
            }),
          );
        } catch (caughtValue) {
          const e = uiError(caughtValue);
          if (inputImages.valid()) await draftView(d.id, e);
          throw e;
        }
        if (!inputImages.valid()) return;
        toast('代理の回答を取り消しました。質問に答えてください。');
        await draftView(d.id, null, true, draftPosted(d, posted));
      });
    },
  });
}
// 回答待ちの相談の、代理のAIが答えた質問（最後の利用者の発言の後の代理の発言）と、利用者に回した質問ごとの理由
// （questionReasons。連続の上限・代理のAIの失敗・取り消しを含む）。どちらも無ければ出さない。
function proxyWaitingSummary(d: Draft) {
  if (d.status !== 'intake_waiting') return null;
  const from = d.messages.findLastIndex(
    (m: { role: string }) => m.role === 'user',
  );
  const answered = d.messages
    .slice(from + 1)
    .filter((m: { role: string }) => m.role === 'proxy')
    .flatMap((m) => (Array.isArray(m.answers) ? m.answers : []));
  const reasons = Array.isArray(d.questionReasons) ? d.questionReasons : [];
  if (!answered.length && !reasons.length) return null;
  return el(
    'div',
    { class: 'proxy-summary' },
    answered.length
      ? el(
          'div',
          {},
          el('strong', { text: '代理のAIが答えた質問' }),
          el(
            'ul',
            {},
            answered.map((a) =>
              el('li', { text: `${a.question} → ${a.answer}` }),
            ),
          ),
        )
      : null,
    reasons.length
      ? el(
          'div',
          {},
          el('strong', { text: 'あなたに回した質問と理由' }),
          el(
            'ul',
            {},
            reasons.map((r) =>
              el(
                'li',
                {},
                el('div', { text: r.question }),
                hint(`理由: ${r.reason}`),
              ),
            ),
          ),
        )
      : null,
  );
}
// 相談画面の優先度（Issue #448）。選択肢の現在値は利用者の指定、無ければ「自動」。
const draftPriorityChoice = (d: Draft) =>
  typeof d.priority === 'string' && ['high', 'normal'].includes(d.priority)
    ? d.priority
    : 'auto';
// 選び直して送る前の優先度があるか。定期更新・画面の自動更新はその間、描き直さない。
function draftPriorityHeld(d: Draft) {
  const choice = $('#draft-priority-choice');
  return !!choice && choice.value !== draftPriorityChoice(d);
}
// 起票前の相談の現在の優先度と「優先度を変更」。変えると受付AIの順番待ちの並びにすぐ効き、起票するIssueにも
// 同じ値が付く。Giteaへは何も送らない。
function draftPriorityBox(d: Draft, inputImages: ComposerController) {
  const choice = el(
    'select',
    { id: 'draft-priority-choice', 'aria-label': '相談の優先度の変更' },
    [
      ['high', '割り込み'],
      ['normal', '通常'],
      ['auto', '自動（AIが判定）'],
    ].map(([value, text]) =>
      el('option', { value, text, selected: value === draftPriorityChoice(d) }),
    ),
  );
  const change = el('button', {
    type: 'button',
    class: 'quiet',
    text: '優先度を変更',
  });
  change.onclick = () =>
    action(change, async () => {
      const posted = await inputImages.live(
        api<Draft>(`/api/drafts/${d.id}/priority`, { priority: choice.value }),
      );
      if (inputImages.valid())
        await draftView(d.id, null, false, draftPosted(d, posted));
    });
  return el(
    'div',
    { class: 'priority-box' },
    el('strong', {
      text: `優先度: ${d.priority ? `${userPriorityNames[d.priority] || d.priority}（利用者の指定）` : '自動'}`,
    }),
    field(
      '優先度を変更',
      choice,
      '「割り込み」にすると、AIの順番待ちで受理の早い相談より先に始めます（実行中の相談は中断しません）。起票するIssueにも同じ優先度が付きます。起票を始めた後は、実行待ちの間にIssueの画面から変更できます。',
    ),
    change,
  );
}
