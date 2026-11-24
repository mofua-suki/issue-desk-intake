// 受付画面のIssueの詳細の画面の組み立て。「内容・状況」の2節を1画面に並べる（Issue #2419）。
// 開く・読み直す処理と部品は client/src/ui/app-issue.js。client/src/ui/app.js の一部（Issue #457）で、単独のモジュールではない。
// 受付は client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。
// 画面が受け取るのは GET /api/issue?view=summary の軽量応答（server/src/intake-issue-summary.mjs）。全文・添付画像と
// 会話履歴は、開いたときだけ従来の詳細（view 指定なし）を取得して描く。
// 全文・会話履歴を開いているか。同じIssueの自動更新では保ち、別のIssueを開くと閉じる（renderIssue が戻す）。
let issueBodyOpen = false,
  issueHistoryOpen = false,
  issueProgressOpen = false,
  issueProgressSnapshot = '',
  // 開いたときに取得した従来の詳細 { key, loading, error, detail, commentCount, status, revision }。閉じている間は取得しない。
  issueFull: IssueFullState | null = null,
  // 今の画面の全文・会話履歴の描き直し。取得が終わったときに、入力欄を描き直さずにこの部分だけ更新する。
  issueFullPainters: (() => void)[] = [];
// 操作を促さず、現在の状態を説明する。
function issueStatusText(i: Issue) {
  const descriptions: Record<string, string> = {
    waiting: '質問への回答を待っています。',
    review_held: 'レビューが保留されています。',
    stopped: '作業は停止しています。',
    failed: '作業が失敗しました。',
    prerequisite_waiting: '必要な前提成果物を待っています。',
    dependency_waiting: '先行PRのマージを待っています。',
    untracked: '作業はまだ開始されていません。',
    completed: '完了しました。',
    closed: '依頼は閉じられています。',
  };
  if (i.investigation === true && i.status === 'completed')
    return guidance.reported;
  if (i.status === 'completed' && i.executionProgress?.unavailable)
    return 'マージ済みですが、受入確認を読み取れません。';
  if (i.status === 'completed' && (i.executionProgress?.pending ?? 0) > 0)
    return `マージ済みですが、受入確認が残り${i.executionProgress!.pending}件あります。`;
  if (i.status === 'dependency_waiting' && i.dependencyReason)
    return i.dependencyReason;
  if (descriptions[i.status]) return descriptions[i.status];
  if (i.status === 'project_waiting')
    return projectWaitText(i.projectWait, repositoryProjects());
  if (i.followup && i.status === 'queued')
    return '開始できる状態になると自動で開始します。';
  return '現在の状態は上の帯に表示されています。';
}
// 会話履歴の件数（実行履歴の経過は数えない）。軽量応答は commentCount を持つ。
const issueCommentCount = (i: Issue) =>
  i.commentCount ??
  i.comments?.filter((c: { kind: string }) => c.kind !== 'log').length ??
  0;
// 全文・会話履歴の従来の詳細を取得する。同じ取得を重ねず、成功した結果は同じIssueの再描画で使う。失敗は覚えておき、
// 「再取得」（retry）で取り直す（定期更新では取り直さない）。件数や状態が変わったときだけ、開いている間に取り直す。
// 取り直す間も失敗した後も、取得済みの内容（detail）は残す。
function ensureIssueFull(i: Issue, retry = false) {
  const f = issueFull;
  if (
    f?.key === issueKey &&
    (f.loading ||
      (f.error && !retry) ||
      (!f.error &&
        f.detail &&
        f.commentCount === issueCommentCount(i) &&
        f.status === i.status &&
        f.revision === (i.contentRevision ?? null)))
  )
    return;
  const next: IssueFullState = {
    key: issueKey,
    loading: true,
    error: null,
    detail: f?.key === issueKey ? f.detail : null,
    commentCount: issueCommentCount(i),
    status: i.status,
    // 取得した時点の軽量応答の内容リビジョン。本文末尾や既存コメントだけの変更もこれで見分ける。
    revision: i.contentRevision ?? null,
  };
  issueFull = next;
  const done = () => issueFullPainters.forEach((paint) => paint());
  issueFullLoader().then(
    (detail) => {
      if (issueFull !== next) return;
      // 別のIssueへ移った後に届いた応答は表示しない。
      if (!detail) {
        issueFull = null;
        return;
      }
      next.loading = false;
      next.detail = detail;
      done();
    },
    (value: unknown) => {
      const e = uiError(value);
      if (issueFull !== next) return;
      next.loading = false;
      next.error = e;
      done();
    },
  );
}
// 開いたときだけ中身を作る欄。閉じている間は中身のDOMを作らず、取得もしない。
// labels: [開くボタンの文言, 閉じるボタンの文言]、isOpen/setOpen: 開閉の状態、fill: 取得済みの詳細から中身のノードの配列を返す。
function lazyFold(
  i: Issue,
  {
    labels,
    isOpen,
    setOpen,
    fill,
  }: {
    labels: [string, string];
    isOpen(): boolean;
    setOpen(open: boolean): void;
    fill(issue: Issue): (HTMLElement | string)[];
  },
) {
  const box = el('div', { class: 'lazy-fold' });
  const toggle = el('button', {
    type: 'button',
    class: 'quiet request-toggle',
  });
  const paint = () => {
    const open = isOpen();
    toggle.textContent = open ? labels[1] : labels[0];
    toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (!open) return box.replaceChildren();
    ensureIssueFull(i);
    const f = issueFull;
    // 取得に失敗しても、取得済みの内容は消さずにエラーと「再取得」を上に添える。
    const failure = f?.error
      ? [
          el('div', {
            class: 'error-box',
            text: `取得できませんでした: ${f.error.message || f.error}`,
          }),
          el('button', {
            type: 'button',
            class: 'quiet',
            text: '再取得',
            onClick: retry,
          }),
        ]
      : [];
    if (!f?.detail)
      return box.replaceChildren(
        ...(failure.length ? failure : [hint('読み込んでいます…')]),
      );
    box.replaceChildren(...failure, ...fill(f.detail));
  };
  // 取り直す間も取得済みの内容を残す。本文と会話履歴は同じ取得なので、開いている両方を描き直す。
  const retry = () => {
    ensureIssueFull(i, true);
    issueFullPainters.forEach((p) => p());
  };
  toggle.onclick = () => {
    setOpen(!isOpen());
    // 閉じて開き直したときは、前の失敗を引きずらずに取り直す。
    if (isOpen() && issueFull?.key === issueKey && issueFull.error)
      return retry();
    paint();
  };
  issueFullPainters.push(() => {
    if (box.isConnected !== false) paint();
  });
  paint();
  return [toggle, box];
}
function renderIssue(i: Issue) {
  const key = `${i.project}#${i.number}`;
  if (issueKey !== key) {
    issueKey = key;
    issueOlder = new Set();
    mobileView = 'chat';
    // 全文・会話履歴は別のIssueを開くと既定（閉じる）に戻す。
    issueBodyOpen = false;
    issueHistoryOpen = false;
    issueProgressOpen = false;
    issueFull = null;
    issueProgressSnapshot = '';
  }
  const progressSnapshot = JSON.stringify(i.executionProgress ?? null);
  if (issueProgressSnapshot && issueProgressSnapshot !== progressSnapshot)
    issueFull = null;
  issueProgressSnapshot = progressSnapshot;
  issueFullPainters = [];
  const scope = { project: i.project, number: i.number };
  const progressFold =
    i.status === 'completed' &&
    i.executionProgress &&
    (i.executionProgress.unavailable || (i.executionProgress.pending ?? 0) > 0)
      ? lazyFold(i, {
          labels: ['受入条件の詳細を見る ▼', '受入条件の詳細を閉じる ▲'],
          isOpen: () => issueProgressOpen,
          setOpen: (open: boolean) => (issueProgressOpen = open),
          fill: (full) => [
            full.executionProgress
              ? executionProgressView(full.executionProgress)
              : hint('受入条件の詳細を取得できませんでした。'),
          ],
        })
      : [];
  // 内容: 本文の冒頭（要約のAIは使わない）。全文と添付画像は「全文を見る」で開いたときに取得する。
  const bodyFold = lazyFold(i, {
    labels: ['全文を見る ▼', '全文を閉じる ▲'],
    isOpen: () => issueBodyOpen,
    setOpen: (open: boolean) => (issueBodyOpen = open),
    fill: (full) => [
      el(
        'div',
        { class: 'issue-body request-body' },
        // コントローラーが書いた依存先などの番号は、共通番号を添えた表示用の本文で見せる。
        markdown(
          (full.shownBody ?? full.body)
            .replace(/<!-- issue-desk:[\s\S]*?-->/g, '')
            .trim(),
        ),
      ),
      gallery(full.images, scope),
    ],
  });
  const commentCount = issueCommentCount(i);
  // 会話履歴: 新しい投稿から並べ、開いたときに取得・描画する。実行履歴（経過）は出さない。
  const historyFold = lazyFold(i, {
    labels: [`会話履歴を見る ▼（${commentCount}件）`, '会話履歴を閉じる ▲'],
    isOpen: () => issueHistoryOpen,
    setOpen: (open: boolean) => (issueHistoryOpen = open),
    fill: (full) => {
      const rows = full.comments
        .filter((c: { kind: string }) => c.kind !== 'log')
        .reverse();
      const older = issueOlder.has('talk')
        ? 0
        : Math.max(0, rows.length - historyShown);
      return [
        ...rows.slice(0, rows.length - older).map((c) => messageNode(c, scope)),
        ...(older
          ? [
              el('button', {
                type: 'button',
                class: 'quiet show-older',
                text: `以前のやりとりを表示（${older}件）`,
                onClick: () => {
                  issueOlder.add('talk');
                  issueFullPainters.forEach((paint) => paint());
                },
              }),
            ]
          : []),
        ...(rows.length
          ? []
          : [el('div', { class: 'muted', text: 'まだ投稿はありません。' })]),
      ];
    },
  });
  const statusText = issueStatusText(i);
  // 本文の冒頭は最大200文字（受付の bodyExcerpt）。軽量でない応答でも同じ長さで切る。
  const excerptText = Array.from(
    String(i.bodyExcerpt ?? i.shownBody ?? i.body ?? '')
      .replace(/<!-- issue-desk:[\s\S]*?-->/g, '')
      .trim(),
  )
    .slice(0, 200)
    .join('');
  const section = (
    name: string,
    className: string,
    ...nodes: (HTMLElement | null)[]
  ) =>
    el(
      'section',
      { class: `panel issue-section ${className}` },
      el('div', { class: 'panel-head' }, el('h2', { text: name })),
      ...nodes,
    );
  const sections = el(
    'div',
    { class: 'issue-sections' },
    section(
      '内容',
      'issue-content',
      el('p', {
        class: 'issue-excerpt',
        text: excerptText || '（本文はありません）',
      }),
      ...bodyFold,
    ),
    section(
      '状況',
      'issue-status',
      el(
        'div',
        { class: 'issue-status-line' },
        badge(i.status, statusLabel(i)),
      ),
      el('p', { text: `作業の種類: ${i.workTypeLabel || '不明'}` }),
      statusText
        ? el('p', { class: 'issue-status-text', text: statusText })
        : null,
      i.status === 'closed' && i.closeReason
        ? hint(
            `終了理由: ${closeReasons[i.closeReason.reason] || i.closeReason.reason}`,
          )
        : null,
      i.error ? el('div', { class: 'error-box', text: i.error }) : null,
      i.pr
        ? el('a', {
            class: 'result-link',
            href: i.pr.url,
            target: '_blank',
            rel: 'noreferrer',
            text: `PR #${i.pr.number} を確認する ↗`,
          })
        : null,
      executionProgressView(i.executionProgress),
      ...progressFold,
      finalApprovalView(
        i,
        (payload) => api('/api/comment', payload),
        error,
        repositoryProjects(),
      ),
      ...historyFold,
    ),
    el('a', {
      href: i.url,
      target: '_blank',
      rel: 'noreferrer',
      text: 'Giteaで開く ↗',
    }),
  );
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    head(
      `${shownProject(i.project)} · ${deskLabel(i)}`,
      i.shownTitle || i.title,
      // 仕組みが作った依頼の元は、題名にIssue番号を入れないので題名と別の行に示す（Issue #1976）。
      originLabel(i),
      badge(i.status, statusLabel(i)),
      'issue-page-heading',
      // 題名に「[フォローアップ]」を付けないので、フォローアップは見出しの目印で示す（詳細APIの判定をそのまま使う）。
      i.followup === true
        ? el('span', { class: 'kind-mark', text: 'フォローアップ' })
        : null,
    ),
    sections,
  );
}
