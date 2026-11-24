// 受付画面のルーティング（URLの形・履歴・URLからの画面の復元）。client/src/ui/app.js の一部（Issue #456）で、単独の
// モジュールではない。受付は client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。
const listTabs: ListTab[] = [
  'attention',
  'issues',
  'waiting',
  'stopped',
  'completed',
  'closed',
  'todos',
  'followups',
];
const isListTab = (value: unknown): value is ListTab =>
  typeof value === 'string' && (listTabs as readonly string[]).includes(value);
// 起動時だけ先に読み、meta確定後に同じURLの画面処理が一度だけ受け取る。
let bootRoutingPending = false;
type BootRead = ReturnType<typeof requestJson>;
let bootReads: { hash: string; values: Map<string, BootRead> } | null = null;
function hasBootRead(url: string) {
  return (
    !!bootReads &&
    !bootRoutingPending &&
    bootReads.hash === location.hash &&
    bootReads.values.has(url)
  );
}
function takeBootRead(url: string): BootRead | null {
  if (!hasBootRead(url) || !bootReads) return null;
  const result = bootReads.values.get(url) ?? null;
  bootReads.values.delete(url);
  return result;
}
function startBootReads() {
  const hash = location.hash;
  const route = parseRoute(hash);
  const urls: string[] = [];
  if (
    route.type === 'home' &&
    !(resumed?.hash === hash && resumed.screen?.type === 'new')
  ) {
    urls.push(listRequest(route.tab).url);
  } else if (route.type === 'draft') urls.push(`/api/drafts/${route.id}`);
  else if (route.type === 'todo') urls.push(`/api/todos/${route.id}`);
  else if (route.type === 'issue')
    urls.push(
      route.desk
        ? `/api/issue?desk=${route.desk}&view=summary`
        : `/api/issue?project=${encodeURIComponent(route.project ?? '')}&number=${route.number}&view=summary`,
    );
  else if (route.type === 'projects') urls.push('/api/projects');
  else if (route.type === 'project') urls.push(`/api/projects/${route.id}`);
  else if (route.type === 'close') urls.push(`/api/close-requests/${route.id}`);
  else if (route.type === 'attention') urls.push('/api/attention/alerts');
  else if (route.type === 'ssh-tickets' || route.type === 'ssh-ticket')
    urls.push(
      route.type === 'ssh-ticket'
        ? `/api/ssh-tickets/${route.id}`
        : '/api/ssh-tickets',
    );
  else if (
    route.type === 'permission-grants' ||
    route.type === 'permission-grant'
  )
    urls.push(
      route.type === 'permission-grant'
        ? `/api/permission-grants/${route.id}`
        : '/api/permission-grants?status=all',
    );
  else if (route.type === 'models') urls.push('/api/model-overrides');
  else if (route.type === 'tokens') urls.push('/api/tokens');
  else if (route.type === 'stats')
    urls.push(
      '/api/stats/overview?range=7d&project=',
      '/api/stats?details=true',
    );
  const values = new Map<string, BootRead>();
  for (const url of urls) {
    // エラー処理や認証切れの副作用は、実際に画面が採用するときだけ行う。
    const request = requestJson(url, {
      method: 'GET',
      csrf: session.csrf || '',
    });
    // 画面遷移やmeta失敗で未使用になる拒否も処理する。
    void request.catch(() => {});
    values.set(url, request);
  }
  bootReads = { hash, values };
}
// 受付が保存した下書きIDと同じ形（UUID）だけを受け取る。受付側の経路と判定をずらさない。
const draftId = (x: unknown): x is string =>
  typeof x === 'string' &&
  /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/.test(x);
const draftHash = (id: string) => `#/drafts/${id}`;
// 受付のTODOの経路（/api/todos/<id>）と同じ形だけを受け取る。
const todoId = (x: unknown): x is string =>
  typeof x === 'string' && /^[a-f0-9-]{36}$/.test(x);
const todoHash = (id: string) => `#/todos/${id}`;
function parseRoute(hash: string): ParsedRoute {
  if (!hash || hash === '#' || hash === '#/')
    return { type: 'home', tab: null };
  const list = hash.match(
    /^#\/list\/(attention|issues|waiting|stopped|drafts|completed|closed|todos|followups)$/,
  );
  if (list)
    return { type: 'home', tab: list[1] === 'drafts' ? 'attention' : list[1] };
  // 共有シートから開く入口。本文はURLへ載せないので、区別するのは share だけ。
  const draft = hash.match(/^#\/new(?:\?(.*))?$/);
  if (draft)
    return {
      type: 'new',
      share: new URLSearchParams(draft[1] || '').get('share') === '1',
    };
  if (hash === '#/ssh-tickets') return { type: 'ssh-tickets' };
  const sshTicket = hash.match(/^#\/ssh-tickets\/(.*)$/);
  if (sshTicket)
    return draftId(sshTicket[1])
      ? { type: 'ssh-ticket', id: sshTicket[1] }
      : { type: 'invalid' };
  if (hash === '#/permission-grants') return { type: 'permission-grants' };
  const permissionGrant = hash.match(/^#\/permission-grants\/(.*)$/);
  if (permissionGrant)
    return /^[0-9a-f]{32}$/.test(permissionGrant[1])
      ? { type: 'permission-grant', id: permissionGrant[1] }
      : { type: 'invalid' };
  if (hash === '#/stats') return { type: 'stats' };
  if (hash === '#/proposal-dismissals') return { type: 'home', tab: null };
  if (hash === '#/models') return { type: 'models' };
  // PC別トークンの一覧と運用の代行の付け外し（docs/intake-client.md）。
  if (hash === '#/tokens') return { type: 'tokens' };
  // 下書きは保存済みのUUIDだけを載せる。会話本文・画像・資格情報はURLへ入れない。
  // 余分なパスや欠けたIDでも下書きへのURLとして扱い、相談向けの案内を出す。
  // 他のPCからのクローズ依頼の確認。依頼IDだけを載せる。
  const closing = hash.match(/^#\/close-requests\/(.*)$/);
  if (closing)
    return draftId(closing[1])
      ? { type: 'close', id: closing[1] }
      : { type: 'invalid' };
  const consultation = hash.match(/^#\/drafts\/(.*)$/);
  if (consultation)
    return draftId(consultation[1])
      ? { type: 'draft', id: consultation[1] }
      : { type: 'invalid', scope: 'draft' };
  // TODOも保存済みのIDだけを載せる。本文・画像はURLへ入れない。
  const todo = hash.match(/^#\/todos\/(.*)$/);
  if (todo)
    return todoId(todo[1])
      ? { type: 'todo', id: todo[1] }
      : { type: 'invalid', scope: 'todo' };
  // 新しいプロジェクトの相談（#/projects と #/projects/<相談ID>）。相談IDは下書きと同じUUIDの形だけ。
  if (hash === '#/projects') return { type: 'projects' };
  const newProject = hash.match(/^#\/projects\/(.*)$/);
  if (newProject)
    return draftId(newProject[1])
      ? { type: 'project', id: newProject[1] }
      : { type: 'invalid', scope: 'project' };
  // 要対応の行（#/attention/<行のキー>）。キーは受付の要対応の key（例 consultation-budget:<相談ID>）だけを載せ、開くときに
  // GET /api/attention/alerts から同じキーの行を探す（client/src/ui/app-attention-alerts.js の attentionRouteView）。
  const attention = hash.match(/^#\/attention\/(.+)$/);
  if (attention) {
    try {
      const key = decodeURIComponent(attention[1]);
      // eslint-disable-next-line no-control-regex -- 入力や表示から制御文字を除外する安全性検証。
      if (!/[\s\x00-\x1f]/.test(key)) return { type: 'attention', key };
    } catch {}
    return { type: 'invalid', scope: 'attention' };
  }
  // IssueのURLは共通番号だけの #/issues/<共通番号>。共通番号の付く前に共有した旧形式
  // （#/issues/<owner>/<repo>/<Gitea番号>）も開き、表示後に共通番号のURLへ置き換える（issueView）。
  const desk = hash.match(/^#\/issues\/([1-9][0-9]*)$/);
  if (desk)
    return Number.isSafeInteger(Number(desk[1]))
      ? { type: 'issue', desk: Number(desk[1]) }
      : { type: 'invalid' };
  const issue = hash.match(/^#\/issues\/([^/]+)\/([^/]+)\/([1-9][0-9]*)$/);
  if (issue && Number.isSafeInteger(Number(issue[3]))) {
    try {
      const parts = issue.slice(1, 3).map(decodeURIComponent);
      if (
        parts.every(
          (p: string) =>
            // eslint-disable-next-line no-control-regex -- 入力や表示から制御文字を除外する安全性検証。
            p && !/[/\\\s?#\x00-\x1f]/.test(p) && p !== '.' && p !== '..',
        )
      )
        return {
          type: 'issue',
          project: parts.join('/'),
          number: Number(issue[3]),
        };
    } catch {}
  }
  return { type: 'invalid' };
}
function remember(hash: string, replace = false) {
  const state = { tab: isListTab(tab) ? tab : null };
  if (location.hash !== hash)
    history[replace ? 'replaceState' : 'pushState'](
      state,
      '',
      location.pathname + location.search + hash,
    );
  else history.replaceState(state, '', location.href);
  if (replace) routeHash = location.hash;
  providerDisableRoute({ replace });
}
function routeError(
  e: unknown,
  text = '依頼を開けませんでした。URL・アクセス権・接続状態を確認してください。',
) {
  reset();
  error(e);
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    el('div', { class: 'empty', text }),
  );
}
const draftMissing =
  '相談を開けませんでした。下書きIDの誤り、破棄済み、アクセス権、接続状態を確認してください。依頼一覧から開き直せます。';
// 「TODOに戻す」の確認（Issue #458）。戻すと残るものと消えるものを示す。
const returnToTodoConfirm =
  'この相談をTODOに戻します。\n残るのは最初の依頼文と、そのメッセージの添付画像・実行先だけです。受付AIとの質疑・下書き・2件目以降のメッセージとその画像・入力中の内容は破棄され、元に戻せません。\nよろしいですか？';
const todoMissing =
  'TODOを開けませんでした。URLのTODO IDの誤り、削除済み、アクセス権、接続状態を確認してください。依頼一覧から開き直せます。';
const projectMissing =
  '新しいプロジェクトの相談を開けませんでした。URLの相談IDの誤り、受付の設定（新しいプロジェクトの作成が無効）、アクセス権、接続状態を確認してください。依頼一覧から開き直せます。';
async function restoreRoute() {
  if (!session?.authenticated || !meta) return;
  const route = parseRoute(location.hash);
  if (isListTab(history.state?.tab)) tab = history.state.tab;
  // 画面の新版への自動更新の直後。URLだけでは戻せない画面（一覧から開いた新規依頼）と並び順・絞り込みを戻す。
  // TODOは #/todos/<id> のURLから戻る。
  const saved = resumed;
  resumed = null;
  if (saved?.hash === location.hash) {
    if (
      saved.sort &&
      Object.hasOwn(sortLabels, saved.sort.field) &&
      ['asc', 'desc'].includes(saved.sort.direction)
    )
      issueSort = { field: saved.sort.field, direction: saved.sort.direction };
    kindFilter = validKindFilter(saved.kindFilter);
    if (saved.screen?.type === 'new') {
      // 一覧のボタンから開いた新規依頼は一覧のURLのまま。戻り先のタブはそのURLから取る。
      if (route.type === 'home' && route.tab) tab = route.tab;
      return newDraft({ share: saved.screen.share === true, navigate: false });
    }
  }
  if (route.type === 'home') {
    // 旧URLは現在の一覧タブを引き継ぎ、履歴に残さず置き換える。
    if (location.hash === '#/proposal-dismissals') return home('replace');
    tab = route.tab;
    return home(false);
  }
  if (route.type === 'new')
    return newDraft({ share: route.share, navigate: false });
  if (route.type === 'draft') return draftView(route.id, null, false);
  if (route.type === 'todo') return todoView(route.id, null, false);
  if (route.type === 'issue')
    return route.desk
      ? issueView(null, null, false, route.desk)
      : issueView(route.project ?? null, route.number ?? null, false);
  if (route.type === 'close') return closeRequestView(route.id, false);
  if (route.type === 'attention') return attentionRouteView(route.key);
  if (route.type === 'ssh-tickets') return sshTicketsView(null, false);
  if (route.type === 'ssh-ticket') return sshTicketsView(route.id, false);
  if (route.type === 'permission-grants')
    return permissionGrantsView(null, false);
  if (route.type === 'permission-grant')
    return permissionGrantsView(route.id, false);
  if (route.type === 'stats') return statsView(false);
  if (route.type === 'models') return modelsView(false);
  if (route.type === 'tokens') return tokensView(false);
  // 管理側で projectCreation を設定していない受付では、新しいプロジェクトの画面を開かない。
  if (route.type === 'projects' && meta.projectCreation)
    return projectsView(false);
  if (route.type === 'project' && meta.projectCreation)
    return projectView(route.id, false);
  routeError(
    Error('URLの形式が正しくありません。依頼一覧から開き直してください。'),
    ('scope' in route ? route.scope : null) === 'draft'
      ? draftMissing
      : ('scope' in route ? route.scope : null) === 'todo'
        ? todoMissing
        : ('scope' in route ? route.scope : null) === 'attention'
          ? attentionMissing
          : ('scope' in route ? route.scope : null) === 'project' ||
              route.type === 'projects' ||
              route.type === 'project'
            ? projectMissing
            : undefined,
  );
}
// 経路の画面は開くときに同期して reset() する。その画面の待ち合わせ（API取得・入力の復元）が終わる前に
// 別の画面へ移っていれば、古い画面の失敗を新しい画面へ通知・上書きしない（API取得失敗・定期更新と同じ世代確認）。
async function openRoute() {
  const shown = restoreRoute();
  providerDisableRoute();
  const id = routeId;
  try {
    await shown;
  } catch (caughtValue) {
    const e = uiError(caughtValue);
    if (isCurrent(id)) throw e;
  }
}
// pushState does not emit hashchange; back/forward and manual hash edits do.
window.addEventListener('hashchange', () => {
  if (bootRoutingPending) return;
  openRoute().catch(error);
});
