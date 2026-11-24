// 受付画面の相談（下書き）の画面の組み立て（会話・入力欄・依頼内容・再試行・破棄・TODOに戻す）。開く・読み直すは
// client/src/ui/app-draft.js。client/src/ui/app.js の一部（Issue #457）で、単独のモジュールではない。受付は client/scripts/build.ts の
// 順に app.js とつないだ1つのモジュールとして /app.js を配信する。
// エージェントの判断で割り込みにした理由の1行（Issue #646）。相談画面は下書きの prioritySource、Issue画面は詳細の
// priorityRequest から出す。理由の無い記録には出さない。理由は text のまま出す（Markdownとして解釈しない）。
function priorityRequestText(r: PriorityRequest | null | undefined) {
  if (typeof r?.reason !== 'string' || !r.reason) return '';
  return `割り込みの理由（エージェントの判断・${r.pc || r.via || 'script'}）: ${r.reason}`;
}
const priorityRequestLine = (r: PriorityRequest | null | undefined) =>
  priorityRequestText(r) ? hint(priorityRequestText(r)) : null;
function renderDraft(d: Draft, inputKey = `draft:${d.id}`) {
  const id = routeId;
  // 押せる操作は受付が返した actions だけで決める（server/src/intake-actions.mjs。docs/intake-api.md）。
  const can = (name: string) => (d.actions || []).includes(name);
  // 入力欄は、相談へ送れるとき（chat）と、起票済みの相談に残った入力をIssueへ送るとき（comment）に使える。
  const writable = can('chat') || can('comment');
  const text = el('textarea', {
    rows: 3,
    placeholder: '追加したいことや、質問への回答を入力',
    'aria-label': 'AIへのメッセージ',
    disabled: !writable,
  });
  const scope = { project: d.project, draft: d.id };
  const inputImages = composer(inputKey, text, !writable, () => fitRows(text));
  inputImages.ready.then(() => fitRows(text)).catch(() => {});
  inputImages.reconcile(d.messages).catch((e) => {
    if (inputImages.valid()) error(e);
  });
  // 選び直して送る前の優先度（Issue #448）は、Issue画面と同じく画面の自動更新でも消さない。
  holdReason = () =>
    draftPriorityHeld(d) ? '優先度の選び直しが未送信です' : null;
  // 相談の途中で依頼先が改名されても、この端末に残った入力を捨てずに送り先を合わせられるようにする。
  const retarget = retargetNotice(inputImages, () => scope);
  const send = el('button', {
    class: 'primary',
    text: '送る ↑',
    disabled: !writable,
  });
  const chat = el(
    'div',
    { class: 'conversation' },
    d.messages.map((m) =>
      el(
        'div',
        { class: `message ${m.role}` },
        // 調査の報告（role=report）は受付が調査Issueから取り込んだ参照データ。参照元の調査Issueを見出しに出す。
        // 代理の回答（role=proxy。Issue #758）は受付AI・利用者と分けて「代理のAI」と出し、根拠を添える。
        el('span', {
          class: 'who',
          text:
            m.role === 'user'
              ? 'あなた'
              : m.role === 'report'
                ? reportAuthor(m, repositoryProjects())
                : m.role === 'proxy'
                  ? '代理のAI'
                  : '受付AI',
        }),
        m.role === 'proxy' ? proxyAnswerList(m) : markdown(m.text),
        m.role === 'proxy'
          ? hint(
              '受付の代理のAIが根拠を示して答えました（あなたの発言ではありません）。',
            )
          : null,
        m.role === 'proxy' && can('proxy_undo')
          ? proxyUndoButton(d, m, inputImages)
          : null,
        gallery(m.images, scope),
        m.references?.length
          ? hint(`参照: ${referenceNotes(m.references)}`)
          : null,
      ),
    ),
  );
  // 回答待ちの相談の、代理のAIが答えた質問と、あなたに回した質問ごとの理由（Issue #758）。
  const summary = proxyWaitingSummary(d);
  if (summary) chat.append(summary);
  const shown = draftStatus(d);
  if (shown === 'ai_queued')
    chat.append(
      el('div', {
        class: 'busy',
        text: 'AIの順番待ちです。操作は不要です。実行枠が空くと自動で相談を続けます。画面を閉じても相談は保持され、自動で再開します。',
      }),
    );
  else if (shown === 'ai_recovering')
    chat.append(
      el(
        'div',
        { class: 'busy' },
        `受付AIの処理が時間内に終わらなかったため、${date(d.retryAt)}ごろに自動で続けます（自動再試行 ${d.attempts || 1}回目）。操作は不要です。画面を閉じても続きます。`,
        d.lastFailure ? hint(`直近の失敗理由: ${d.lastFailure.message}`) : null,
      ),
    );
  else if (d.status === 'thinking')
    chat.append(
      el('div', {
        class: 'busy',
        text: 'AIが依頼を整理しています。実装中の場合は順番に処理します。',
      }),
    );
  // 起票待ち（Issue #5272）。APIの補助情報は案内文に必要な数値だけ使い、下書きID・理由コードは表示しない。
  if (['auto_ready', 'submitting'].includes(d.status) && d.filingQueue) {
    const queue = d.filingQueue;
    chat.append(
      el('div', {
        class: 'busy',
        text: queue.stuck
          ? `詰まりの疑い（${queue.inactiveMinutes} 分進んでいません）。要対応を確認してください。`
          : `起票の順番待ち（前に ${queue.ahead} 件）。操作は不要です。`,
      }),
    );
  }
  // 応答待ちの間は、24時間の期限の起点を示す。旧形式の相談は保存済みの最終更新日時で代用したことを明示する。
  if (d.status === 'thinking' && d.waitStartedAt)
    chat.append(
      hint(
        `${waitStartNote(d.waitStartedAt, d.waitSource)}。24時間以内に応答を返せない場合は自動処理を停止します。`,
      ),
    );
  if (d.error)
    chat.append(
      el(
        'div',
        { class: 'error-box' },
        d.error,
        d.status === 'stalled' && d.stalled
          ? el(
              'div',
              {},
              hint(waitStartNote(d.stalled.since, d.stalled.sinceSource)),
              hint(`停止日時: ${date(d.stalled.stoppedAt)}`),
              hint(
                d.stalled.lastFailure
                  ? `直近の失敗理由（${date(d.stalled.lastFailure.at)}）: ${d.stalled.lastFailure.message}`
                  : d.stallCause === 'timeout'
                    ? '直近の失敗理由: 記録はありません（応答待ち・処理中のまま期限に達しました）。'
                    : '直近の失敗理由: 記録はありません。',
              ),
              d.stallHistory?.length
                ? hint(
                    `以前の停止: ${d.stallHistory.length}件（記録は保持しています）`,
                  )
                : null,
            )
          : null,
        can('retry')
          ? el(
              'div',
              {},
              el('button', {
                class: 'quiet',
                // 停止した相談は同じ会話のまま再開し、新しい24時間の処理期間を始める。
                text: d.status === 'stalled' ? '相談を再開' : '相談を再試行',
                onClick: (e) =>
                  action(e.currentTarget, async () => {
                    // 202（処理の受け付けだけ）の応答は処理前の状態なので使わず、読み直す。
                    await inputImages.live(
                      api(`/api/drafts/${d.id}/chat`, { retry: true }),
                    );
                    if (inputImages.valid()) draftView(d.id);
                  }),
              }),
            )
          : null,
      ),
    );
  // 受付AIのターンの後片付けが終わるまで、送信・再開・破棄は actions に出ない（Issue #471）。理由を示す。
  if (d.finishingTurn)
    chat.append(
      el('div', {
        class: 'busy',
        text: '前回の処理を終了中です。終わると送信・再開・破棄ができるようになります（画面は自動で更新します）。',
      }),
    );
  const note = el('small', {
    text:
      d.status === 'submitted'
        ? familyProgressNote(d)
          ? familyProgressNote(d)!
          : (d.issues?.length ?? 0) > 1
            ? `起票済みです。残った入力は最初のIssue（${shownProject(issueProject(d))} ${giteaNumber(d.issue!.number)}）への追加指示として送信します。`
            : '起票済みです。残った入力はIssueへの追加指示として送信します。'
        : investigationDraft(d)
          ? '返信すると、届いた報告を含む会話のまま同じ相談を続けます（報告を待たずに返信すると、その後に届く報告は取り込みません）。'
          : '相談は自動保存されます。',
  });
  const form = el(
    'form',
    { class: 'composer' },
    retarget.node,
    dock(text, inputImages, note, send),
  );
  form.onsubmit = (e: { preventDefault: () => void }) => {
    e.preventDefault();
    action(send, async () => {
      try {
        const message = await inputImages.prepare(scope);
        await inputImages.send(
          { project: d.project, draft: d.id },
          {
            ...message,
            ...((message.text.trim() === '了承します' ||
              message.text.trim() === '承認します') &&
            d.revision != null &&
            Number.isSafeInteger(Number(d.revision))
              ? { expectedRevision: Number(d.revision) }
              : {}),
          },
        );
        await inputImages.clear();
        if (inputImages.valid()) await draftView(d.id);
      } finally {
        inputImages.release();
      }
    });
  };
  const retrySend = can('resubmit')
    ? el('button', {
        class: 'primary',
        text:
          d.status === 'submit_unknown' ? '送信結果を照合する' : '起票を再試行',
        onClick: (e) =>
          action(e.currentTarget, async () => {
            try {
              // 202（起票の受け付けだけ）で、起票は応答の後に進むので、応答を使わず読み直す。
              await inputImages.live(
                api(`/api/drafts/${d.id}/submit`, { revision: d.revision }),
              );
            } catch (caughtValue) {
              const e = uiError(caughtValue);
              if (inputImages.valid()) await draftView(d.id, e);
              throw e;
            }
            if (inputImages.valid()) await draftView(d.id);
          }),
      })
    : null;
  // 調査Issueの表示と、actions が許可した相談を終える操作。未起票の相談・分割の子にも終了操作を出す。
  // ボタンを含むので label ではなく div の欄にする（fieldBlock と同じ理由）。
  const investigations =
    investigationDraft(d) || can('finish')
      ? el(
          'div',
          { class: 'field' },
          el('span', {
            text: investigationDraft(d) ? '調査Issue' : '相談の終了',
          }),
          el(
            'div',
            {},
            (d.investigations || []).map((e: DraftIssue) =>
              el(
                'div',
                {},
                el('button', {
                  type: 'button',
                  class: 'quiet',
                  text: `${shownProject(e.project)}（${giteaNumber(e.number)}）を開く`,
                  onClick: () =>
                    issueView(e.project ?? null, e.number, true, e.desk),
                }),
                hint(
                  e.status === 'reported'
                    ? '報告を取り込みました'
                    : ['ended', 'skipped'].includes(e.status ?? '')
                      ? e.reason
                      : '報告を待っています',
                ),
              ),
            ),
            can('finish')
              ? el('button', {
                  type: 'button',
                  class: 'quiet',
                  text: 'この相談を終える',
                  onClick: (e) =>
                    action(e.currentTarget, async () => {
                      const posted = await inputImages.live(
                        api<Draft>(`/api/drafts/${d.id}/finish`, {}),
                      );
                      if (inputImages.valid())
                        await draftView(
                          d.id,
                          null,
                          true,
                          draftPosted(d, posted),
                        );
                    }),
                })
              : null,
          ),
          can('finish')
            ? hint(
                investigationDraft(d)
                  ? '終えると、この後に届く報告は相談へ取り込みません。起票した調査Issueはそのまま残ります。'
                  : '起票せずに相談を終え、記録を残します。',
              )
            : null,
        )
      : null;
  // A consultation that never reached Gitea is the only thing that can be thrown away, and it cannot be
  // brought back, so the button arms itself on the first press. Whether it can is the server's call (discard).
  const discard = can('discard')
    ? el('button', {
        class: 'quiet',
        text: 'この相談を破棄',
        onClick: (e) => {
          const button = e.currentTarget;
          if (button.dataset.armed !== '1') {
            button.dataset.armed = '1';
            button.textContent = 'もう一度押すと破棄します';
            return;
          }
          action(button, async () => {
            await api(`/api/drafts/${d.id}`, undefined, 'DELETE');
            await inputImages.clear();
            if (inputImages.valid()) await home();
          });
        },
      })
    : null;
  // 起票前の相談をTODOに戻す（Issue #458）。戻せるかは受付の判断（todo）。質疑と下書きは戻らないので、押したら確かめる。
  // 戻ったらURLをTODOへ置き換える（戻る操作で消えた相談を経由しない）。断られたら理由を出して相談を開き直す。
  const toTodo = can('todo')
    ? el('button', {
        class: 'quiet',
        text: 'TODOに戻す',
        onClick: (e) => {
          if (!window.confirm(returnToTodoConfirm)) return;
          return action(e.currentTarget, async () => {
            const request = api(`/api/drafts/${d.id}/todo`, {});
            try {
              await inputImages.live(request);
            } catch (caughtValue) {
              const e = uiError(caughtValue);
              if (inputImages.valid()) await draftView(d.id, e);
              else {
                // 待つ間に画面を離れても、戻せていればこの相談の入力を端末から消す（Issue #514。下と同じ）。
                const returned = await request.then(
                  () => true,
                  () => false,
                );
                if (returned) await inputImages.clear(true);
              }
              throw e;
            }
            // 戻した相談の入力は、待つ間に書き足して版が変わっていても端末から消す（Issue #514）。版を照合する通常の
            // clear() だと残り、同じIDで相談を始め直したときに古い入力が復元される。
            if (!(await inputImages.clear(true))) return;
            toast('相談をTODOに戻しました');
            await todoView(d.id, null, 'replace');
          });
        },
      })
    : null;
  const draft = el(
    'aside',
    { class: 'panel draft-panel' },
    el('div', { class: 'panel-head' }, el('h2', { text: '依頼内容' })),
    el(
      'div',
      { class: 'panel-body' },
      el('div', {
        class: 'preview-note',
        text:
          (d.status === 'stalled'
            ? d.stallCause === 'explicit'
              ? '明示の停止により、この相談の自動処理を停止しました。内容を確認して相談を再開してください。'
              : d.stallCause === 'timeout'
                ? '24時間応答を返せず、この相談の自動処理を停止しました。原因を確認して相談を再開してください。'
                : '停止原因を特定できませんでした。内容を確認して相談を再開してください。'
            : draftGuidance(d)) ||
          '確認が完了すると自動起票します。変更点は相談欄へ入力してください。',
      }),
      field(
        '下書きID',
        el('code', { class: 'draft-id', text: d.id }),
        'この相談を指し示すIDです。アドレスバーのURL（#/drafts/…）をコピーしても同じ相談を開けます。開くには受付の認証とプロジェクトの許可が必要です。',
      ),
      field('タイトル', el('div', { class: 'issue-body', text: d.title })),
      fieldBlock('本文', el('div', { class: 'issue-body' }, markdown(d.body))),
      gallery(
        d.messages.flatMap((m) => m.images || []),
        scope,
      ),
      el(
        'div',
        { class: 'image-reviews' },
        (d.imageReviews || []).map((r) =>
          el('p', {
            text: `画像 ${r.id.slice(0, 8)}：${r.screen} / ${r.location} / ${r.change}${r.uncertainty ? '（未確定：' + r.uncertainty + '）' : ''}`,
          }),
        ),
      ),
      splitPlan(d),
      d.familyProgress
        ? hint(familyProgressText(d.familyProgress))
        : Array.isArray(d.splitChildren) && !d.parentSplit
          ? hint('子の進捗を確認できません。')
          : null,
      hint(`実行先 · ${providerName(d.provider)}`),
      // 起票前の相談は、受付が変更できると返した間（priorityChangeable。Issue #448）だけ相談画面から変えられる。
      // 起票の処理を始めた後は表示だけ（起票後、実行待ちの間は依頼の画面から変えられる）。
      d.priorityChangeable
        ? draftPriorityBox(d, inputImages)
        : d.priority
          ? hint(
              `優先度 · ${userPriorityNames[d.priority] || d.priority}（利用者の指定）`,
            )
          : null,
      priorityRequestLine(d.prioritySource),
      // 受付の相談の難易度（Issue #430）。実装のモデルではない。
      tierRecord(d.tier, d.modelOverrides),
      // 起票時に決めた実装の難易度（Issue #489）。記録の無い下書きには出さない。
      implementationTierRecord(d.implementationTier),
      handoffDraft(d)
        ? hint(
            `新しいプロジェクトへ引き継いだ記録です。引き継ぎ先の相談ID: ${d.projectHandoff?.id?.trim() || '不明'}`,
          )
        : null,
      d.issue
        ? hint(
            (d.issues?.length ?? 0) > 1
              ? `起票済み · ${d.issues!.length}件（各Issueへは分割案の「開く」から移動できます）`
              : `起票済み · ${deskLabel(d.issue)}（${giteaNumber(d.issue!.number)}）`,
          )
        : null,
      investigations,
      retrySend,
      toTodo,
      discard,
    ),
  );
  const workspace = el(
    'div',
    { class: 'workspace', 'data-view': mobileView },
    el(
      'section',
      { class: 'panel chat-panel docked' },
      el(
        'div',
        { class: 'panel-head' },
        el('h2', { text: 'AIと相談' }),
        badge(
          shown,
          d.status === 'stalled'
            ? d.stallCause === 'explicit'
              ? '停止中（明示の停止）'
              : d.stallCause === 'timeout'
                ? '24時間応答なしで停止'
                : '停止中（原因不明）'
            : null,
        ),
      ),
      chat,
      form,
    ),
    draft,
  );
  const tabs = el(
    'div',
    { class: 'mobile-tabs' },
    ...['chat', 'draft'].map((v) =>
      el('button', {
        class: mobileView === v ? 'active' : '',
        text: v === 'chat' ? '相談' : '依頼内容',
        onClick: () => {
          mobileView = v;
          workspace.dataset.view = v;
          for (const b of tabs.children)
            b.classList.toggle(
              'active',
              b.textContent === (v === 'chat' ? '相談' : '依頼内容'),
            );
        },
      }),
    ),
  );
  // スマホ幅は会話の枠が伸びてページ全体でスクロールするので、ページを会話の末尾（入力欄の直前）まで送る。
  // 送るのは相談を開いたときと、相談タブで末尾を見ていたときだけ。依頼内容タブを見ている間や会話を
  // 読み返している間は、受付AIの応答や定期更新で描き直しても位置を動かさない。
  const budgetStopped = !!d.consultationBudget?.stopped;
  const follow =
    !budgetStopped &&
    narrow() &&
    (draftScrolled !== id || (mobileView === 'chat' && atPageEnd()));
  $('#app').replaceChildren(
    el('button', {
      class: 'back',
      onClick: () => home(),
      text: '← 依頼一覧へ戻る（自動保存）',
    }),
    ...(budgetStopped ? [consultationBudgetNotice(d)] : []),
    // プロジェクト未定の相談（Issue #689）は、起票するまで「プロジェクト未定」と出す（client/src/ui/app-new-request.js）。
    head('DRAFT', '依頼を整理する', draftProjectText(d)),
    tabs,
    workspace,
  );
  chat.scrollTop = chat.scrollHeight;
  if (follow) window.scrollTo(0, document.documentElement.scrollHeight);
  if (budgetStopped && draftScrolled !== id) window.scrollTo(0, 0);
  if (narrow() || budgetStopped) draftScrolled = id;
}
// 描き直す前のページが末尾（固定した入力欄の高さ程度の誤差まで）を表示しているか。
function atPageEnd() {
  return (
    window.scrollY + window.innerHeight >=
    document.documentElement.scrollHeight - 80
  );
}
