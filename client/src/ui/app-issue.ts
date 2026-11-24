// 受付画面のIssueの詳細を開く・読み直す処理と、詳細の部品（送信ボタンの候補・案内・履歴の1件・クローズ理由・優先度・
// 担当の引き継ぎ・削除の承認）。画面の組み立て（renderIssue）は client/src/ui/app-issue-view.js。client/src/ui/app.js の一部（Issue #457）
// で、単独のモジュールではない。受付は client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。
// IssueのURL。共通番号が確定していれば #/issues/<共通番号>、未確定なら旧形式の #/issues/<owner>/<repo>/<Gitea番号>
// （共通番号が確定するまではGitea番号で代用しない）。
const issueHash = (
  project: string | null,
  number: number | null,
  desk: number | null = null,
) =>
  desk
    ? `#/issues/${desk}`
    : '#/issues/' +
      (project ?? '').split('/').map(encodeURIComponent).join('/') +
      '/' +
      number;
// navigate: true=履歴に積む / 'replace'=今の入口を置き換える / false=URLに触れない（表示後の共通番号のURLへの置き換えだけ）。
// desk: 分かっている共通番号。project が null のときは共通番号のURLから開いたときで、受付に共通番号から引かせる。
async function issueView(
  project: string | null,
  number: number | null,
  navigate: Navigation = true,
  desk: number | null = null,
) {
  if (navigate !== false)
    remember(issueHash(project, number, desk), navigate === 'replace');
  const id = reset();
  // 前の画面の取得は応答が捨てられる（issueFullLoader の isCurrent）ので、取得中の印を残さない。取得済みの内容は
  // 同じIssueを開き直したときの表示に残し、件数を忘れて（null）開き直した画面で必ず取り直す。
  if (issueFull?.loading)
    issueFull = { ...issueFull, loading: false, commentCount: null };
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    el('div', { class: 'loading', text: '依頼を読み込んでいます…' }),
  );
  // 共通番号から開いたときは、対象が分かるまでの間に届いたIssueの通知を early に覚える（client/src/ui/app-live.js の liveRelevant）。
  current = project
    ? { type: 'issue', project, number }
    : { type: 'issue', project, number, early: new Set() };
  const opening = current;
  let detail: Issue;
  try {
    detail = await api<Issue>(
      project
        ? `/api/issue?project=${encodeURIComponent(project)}&number=${number}&view=summary`
        : `/api/issue?desk=${desk}&view=summary`,
    );
    // 共通番号のURLには、その番号のIssueだけを出す（別のIssueを代わりに表示しない）。食い違いは受付と画面の
    // 共通番号の対応のずれなので、無い番号とは区別して伝える（Issue #513）。
    if (!project && detail.desk !== desk)
      throw Error(
        `受付が返したIssueの共通番号（${detail.desk ? '#' + detail.desk : '未確定'}）が、開いた #${desk} と一致しないため表示しません。受付と画面の共通番号の対応が食い違っています。依頼一覧から開き直してください。`,
      );
  } catch (caughtValue) {
    const e = uiError(caughtValue);
    if (isCurrent(id)) routeError(e);
    return;
  }
  if (!isCurrent(id)) return;
  // 共通番号から開いたときは、受付が引いたプロジェクトとGitea番号で定期更新・返信を続ける。
  if (!project) {
    project = detail.project;
    number = detail.number;
    current = { type: 'issue', project, number };
    // 最初の取得の間に届いたこのIssueの通知は捨てず、定期取得の予約（poll）で前倒しして読み直す。
    if (opening.early?.has(liveIssueKey(project, number))) hurry();
  }
  // 共通番号が確定していれば、旧形式のURLも共通番号のURLへ置き換える。履歴は増やさない。
  if (detail.desk && location.hash !== issueHash(project, number, detail.desk))
    remember(issueHash(project, number, detail.desk), true);
  // 初回・定期の更新（pollIssue。接続確認中は30秒、それ以外は10秒）・操作後の再取得は軽量応答（view=summary）。
  // 全文と会話履歴は開いたときだけ従来の詳細を取得する。
  const detailPath = `/api/issue?project=${encodeURIComponent(project)}&number=${number}`;
  const load = () => api<Issue>(`${detailPath}&view=summary`);
  // 別のIssueへ移った後に届いた応答は捨てる（null）。開いたままの全文・会話履歴は renderIssue の中で取り直すので、
  // 返信・停止などで同じIssueを開き直したときに前の画面の取得（捨てられる）を使わないよう、描く前に入れ替える。
  issueFullLoader = async () => {
    const full = await api<Issue>(detailPath);
    return isCurrent(id) ? full : null;
  };
  renderIssue(detail);
  pollIssue(refresh);
  async function sync(next: Issue) {
    if (!isCurrent(id) || JSON.stringify(next) === JSON.stringify(detail))
      return;
    detail = next;
    renderIssue(detail);
  }
  async function refresh() {
    const next = await load();
    if (!isCurrent(id)) return;
    await sync(next);
    pollIssue(refresh);
  }
}
// app-updates の共通更新判定が参照する。
const issuePosting = 0;
let issueFullLoader: () => Promise<Issue | null> = async () => null;
const kindLabel: Record<string, string> = {
  ask: '実装AI · 確認依頼',
  notice: '実装AI · 反映されなかった操作',
  result: '実装AI · 結果',
  log: '実装AI · 経過',
};
const messageNode = (c: UIMessage, scope: IssueScope) =>
  el(
    'div',
    { class: `message ${c.kind}` },
    el('span', {
      class: 'who',
      text: `${c.kind === 'you' ? (c.mine ? 'あなた' : c.author) : kindLabel[c.kind]} · ${date(c.createdAt)}`,
    }),
    markdown(c.body),
    c.images?.length ? gallery(c.images, scope) : null,
  );
// 会話履歴・実行履歴は新しい順に5件まで出し、残りは押したときに開く（取得する件数は変えない）。
const historyShown = 5;
