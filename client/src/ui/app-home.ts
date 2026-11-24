// 受付画面の依頼一覧（home。タブ・並び替え・絞り込み）。client/src/ui/app.js の一部（Issue #456）で、単独のモジュールでは
// ない。受付は client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。
const sortLabels: Record<string, string> = {
  project: 'プロジェクト・Issue',
  updated: '最終更新日時',
  status: 'ステータス',
  priority: '優先度',
};
// 優先度は最初の操作（昇順）が「高い順」（割り込み→優先→通常）。実行待ちでない行は昇降どちらでも末尾。
const directionLabels: Record<string, Record<string, string>> = {
  project: { asc: '昇順', desc: '降順' },
  updated: { asc: '古い順', desc: '新しい順' },
  status: { asc: '処理順', desc: '逆順' },
  priority: { asc: '高い順', desc: '低い順' },
};
// ボタンは作り直さずに表示だけ更新する。定期更新でもキーボードのフォーカスが外れない。
function sortBar(onChange: () => void) {
  const summary = el('span', { class: 'sort-summary', 'aria-live': 'polite' });
  const buttons = Object.keys(sortLabels).map(
    (field): [string, HTMLButtonElement] => [
      field,
      el('button', {
        type: 'button',
        class: 'sort',
        'data-sort': field,
        onClick: () => {
          issueSort = nextSort(issueSort, field);
          update();
          onChange();
        },
      }),
    ],
  );
  function update() {
    for (const [field, button] of buttons) {
      const active = issueSort.field === field;
      const direction = directionLabels[field][issueSort.direction];
      button.className = active ? `sort active ${issueSort.direction}` : 'sort';
      button.setAttribute('aria-pressed', active ? 'true' : 'false');
      button.setAttribute(
        'aria-label',
        active
          ? `${sortLabels[field]}で並び替え中（${direction}）。押すと逆順にします`
          : `${sortLabels[field]}で並び替え（${directionLabels[field].asc}）`,
      );
      button.replaceChildren(
        document.createTextNode(sortLabels[field]),
        el('span', {
          class: 'sort-direction',
          'aria-hidden': 'true',
          text: active
            ? `${issueSort.direction === 'asc' ? '▲' : '▼'} ${direction}`
            : '⇅',
        }),
      );
    }
    summary.textContent = `並び順：${sortLabels[issueSort.field]}の${directionLabels[issueSort.field][issueSort.direction]}`;
  }
  update();
  return el(
    'div',
    {
      id: 'sorts',
      class: 'sort-bar',
      role: 'group',
      'aria-label': '一覧の並び替え',
    },
    buttons.map(([, button]) => button),
    summary,
  );
}
const kindLabels: Record<string, string> = {
  all: 'すべて',
  ordinary: '通常の依頼',
  followup: 'フォローアップ',
};
// 全範囲（tabs を付けない GET /api/list）を要するタブ。ほかのタブは有効範囲（tabs=active）で足りる。
function listTabNeedsFull(tabValue: string | null) {
  return (
    isListTab(tabValue) &&
    !['attention', 'issues', 'waiting', 'stopped'].includes(tabValue)
  );
}
// 全範囲を要するタブを離れた後も、この時間は全範囲の保持と差分取得を続ける（すぐ戻ったときに全範囲を読み直さない）。
// 過ぎたら次の取得で有効範囲へ戻す。全範囲のままだと、定期取得のたびに終わった依頼まで読み続ける。
const listFullKeepMs = 60000;
// 全範囲を要するタブを離れた時刻。null は表示中。起動直後（再読み込みで全範囲の保持を復元した直後を含む）は、
// 離れた時刻が分からないので、離れて久しい扱いにする。
let listFullLeftAt: number | null = 0;
// 表示するタブが決まるたびに呼ぶ（home の draw）。
function listFullNote(tabValue: string | null) {
  if (listTabNeedsFull(tabValue)) listFullLeftAt = null;
  else if (listFullLeftAt === null) listFullLeftAt = Date.now();
}
function listRequest(tabValue: string | null, full = false) {
  const keepFull =
    listCache?.scope === 'full' &&
    (listFullLeftAt === null || Date.now() - listFullLeftAt < listFullKeepMs);
  const scope = listTabNeedsFull(tabValue) || keepFull ? 'full' : 'active';
  const since =
    full || listNeedFull || scope !== listCache?.scope
      ? null
      : listCache?.cursor || null;
  return { scope, since, url: listUrl(since, scope) };
}
// 「完了」「クローズ済み」タブの絞り込み。並び替えと同じく、ボタンを作り直さずに表示だけ更新する。
// 表示中の件数（#kind-summary）は一覧を描くたびに draw() が書く。
function kindBar(onChange: () => void) {
  const buttons = kindFilters.map((kind): [typeof kind, HTMLButtonElement] => [
    kind,
    el('button', {
      type: 'button',
      class: 'kind',
      'data-kind': kind,
      text: kindLabels[kind],
      onClick: () => {
        kindFilter = kind;
        update();
        onChange();
      },
    }),
  ]);
  function update() {
    for (const [kind, button] of buttons) {
      const active = kindFilter === kind;
      button.className = active ? 'kind active' : 'kind';
      button.setAttribute('aria-pressed', active ? 'true' : 'false');
    }
  }
  update();
  return el(
    'div',
    {
      id: 'kinds',
      class: 'kind-bar',
      role: 'group',
      'aria-label': '通常の依頼とフォローアップの絞り込み',
    },
    buttons.map(([, button]) => button),
    el('span', {
      id: 'kind-summary',
      class: 'kind-summary',
      'aria-live': 'polite',
    }),
  );
}
// 要対応の止まりの行（Issue #848）。起票した直す依頼があればそのIssue画面を開き、相談に結び付く通知（起票前確認の
// 上限通知の draftId）はその相談の画面（#/drafts/<相談ID>）を開き、どちらも無ければ「各PC」へ移る。
function stallAlertOpen(row: RequestRow) {
  const fix = row.fix?.issue;
  if (fix) return issueView(fix.project ?? null, fix.number, true, fix.desk);
  if (typeof row.draftId === 'string' && row.draftId)
    return draftView(row.draftId);
  $('#hosts')?.scrollIntoView?.();
}
async function home(navigate: Navigation = true) {
  if (navigate !== false)
    remember(tab ? `#/list/${tab}` : '#/', navigate === 'replace');
  // 一覧を表示中（初回の取得中を含み、取得に失敗した後は除く）のタブの切り替え・戻る/進むは、画面を作り直さず、
  // 読み込み表示も出さず、取得済みの一覧を描き直すだけにする。内容は接続確認に応じた周期の定期更新で新しくする。
  if (listScreen?.id === routeId && !listScreen.failed) {
    // pushState と戻る/進むでは routeHash が変わらない。合わせないと isCurrent() が偽になり定期更新が止まる。
    routeHash = location.hash;
    return listScreen.show();
  }
  const id = reset();
  // 最後に取得した一覧。並び替えはこれを並べ直すだけで、取得し直さない。
  let issues: RequestRow[] = [],
    groups: RequestGroups | undefined;
  // 開き直した一覧（詳細からの「← 依頼一覧」・戻る/進む・別画面からの遷移・再読み込み）は、保持した取得結果があれば
  // 読み込み表示を出さずにそれで描く。無い・組み立てられないときは従来どおり読み込み表示から取得する。
  if (listCache)
    try {
      use(listCache);
    } catch {
      dropList();
      issues = [];
      groups = undefined;
    }
  const kept = groups ? listCache : null;
  // 保存・送信・削除の後の古い保持（取得時刻 0）や取得中の通知で変わった一覧は、削除済みの行を含みうるが、行は押せる
  // ままにする（Issue #2416）。削除済みの行を開いて404になったときは、詳細の既存のエラー表示で扱う。
  const scopeLoadingNode = el('div', {
    id: 'list-scope-loading',
    class: 'loading',
    hidden: true,
    text: '全体を読み込んでいます…',
    'aria-live': 'polite',
  });
  $('#app').replaceChildren(
    head(
      'WORKSPACE',
      '依頼一覧',
      '相談から実装結果まで、ここで確認できます。',
      el(
        'div',
        { class: 'heading-actions' },
        // スマホ幅では入口のリンクを縦に重ね、見出しを1行に収める（style.css の .heading-links）。
        el(
          'div',
          { class: 'heading-links' },
          el('button', {
            class: 'quiet stats-link',
            onClick: () => statsView(),
            text: '稼働実績',
          }),
          el('button', {
            class: 'quiet models-link',
            onClick: () => modelsView(),
            text: 'モデルの切り替え',
          }),
          // PC別トークンの運用の代行（Issue #1120）。モデルの切り替えと同じくPC幅だけに出し、スマホ幅は稼働実績から開く。
          el('button', {
            class: 'quiet models-link tokens-link',
            onClick: () => tokensView(),
            text: 'PC別トークン',
          }),
        ),
      ),
      'list-heading',
    ),
    el('div', { class: 'toolbar', id: 'tabs' }),
    scopeLoadingNode,
    sortBar(() => {
      if (isCurrent(id) && groups) draw();
    }),
    kindBar(() => {
      if (isCurrent(id) && groups) draw();
    }),
    el(
      'div',
      { id: 'items', class: 'list' },
      kept
        ? null
        : el('div', { class: 'loading', text: '依頼を読み込んでいます…' }),
    ),
    // PCごとの負荷の表（Issue #619。client/src/ui/app-hosts.js）。応答が無ければ隠したまま。
    el('section', { id: 'hosts', class: 'hosts-panel', hidden: true }),
  );
  $('#sorts').hidden = true;
  $('#kinds').hidden = true;
  const screen: ListScreen = (listScreen = {
    id,
    failed: false,
    show() {
      // 初回の取得中は選んだタブを覚えておくだけ。取得が終わると refresh() がそのタブで描く（取得は増やさない）。
      if (!groups) return;
      if (!isListTab(tab)) tab = initialTab(groups!);
      remember(`#/list/${tab}`, true);
      draw();
      ensureFull();
    },
    // 個別取得で変わった一覧を描く（client/src/ui/app-list-sync.js の listFlush）。
    apply(data: ListData) {
      use(data);
      if (!isListTab(tab)) tab = initialTab(groups!);
      remember(`#/list/${tab}`, true);
      draw();
      ensureFull();
    },
    // 全体照合（sinceを使わない）は通知から呼ぶ。定期確認は poll が refresh() を呼ぶ。
    refresh: (full: boolean | undefined, waited: boolean | undefined) =>
      refresh(full, true, waited),
  });
  function use(data: ListData) {
    issues = data.issues;
    groups = requestGroupsWithAux(
      data,
      fullTestAlertRows,
      attentionAlertRows,
      meta.projects?.map((project) => project.id) || [],
    );
  }
  // 一覧の取得（client/src/ui/app-list-sync.js）。初回・全体照合（full または保持のカーソルが無い）は GET /api/list、定期確認は
  // GET /api/list?since=<カーソル> で追加・更新・削除だけを受け取り、保持した一覧へ反映する。
  // notice: 通知による全体照合（client/src/ui/app-list-sync.js の listFlush）。待っている定期確認の予約は動かさない。
  // waited: 呼び出し側（listFlush）が開始前の待ち（listStartWait）を済ませている。
  let listTimingPending = !kept;
  let scopeLoading = false;
  let fullFetching = false;
  const needsFull = () => listTabNeedsFull(tab);
  function ensureFull() {
    if (
      !listCache ||
      listCache.scope === 'full' ||
      !needsFull() ||
      scopeLoading
    )
      return;
    scopeLoading = true;
    scopeLoadingNode.hidden = false;
    void refresh()
      .catch((e) => {
        if (isCurrent(id)) {
          error(uiError(e));
          poll(refresh, 10000);
        }
      })
      .finally(() => {
        scopeLoading = false;
        if (isCurrent(id)) scopeLoadingNode.hidden = true;
      });
  }
  async function refresh(full = false, notice = false, waited = false) {
    // 開き直しで保持が無いときの最初の取得だけを、開くまでの所要として受付へ知らせる。
    const timingStart = listTimingPending ? performance.now() : undefined;
    listTimingPending = false;
    const waiting = waited ? null : listStartWait();
    if (waiting) {
      await waiting;
      if (!isCurrent(id)) return;
    }
    const gen = listGen,
      drops = listDrops,
      seq = ++listFetches;
    // 全体照合の要求（切断・復帰・種類の分からない通知など）があれば、定期確認でもカーソルを使わない。
    const { scope, since, url } = listRequest(tab, full);
    const expanding = scope === 'full' && listCache?.scope !== 'full';
    const shrinking = scope === 'active' && listCache?.scope === 'full';
    if (expanding && fullFetching) return;
    // この要求より前に届いた通知は、この要求が含む。置けなかったら戻す（全体照合の要求は置けるまで保つ）。
    const taken = listTake();
    // 起動時の先行読取は通知より前に始まっているため、取り除いた通知は取得後も回収する。
    const bootListRead = hasBootRead(url);
    // 取得に失敗しても保持した一覧のまま行は押せる（理由は呼び出し側が知らせる）。
    let response;
    let fetchMs: number;
    try {
      const fetchStart = performance.now();
      if (expanding) {
        fullFetching = true;
        if (isCurrent(id)) scopeLoadingNode.hidden = false;
      }
      try {
        response = await api<ListResponse>(url);
      } finally {
        if (expanding) {
          fullFetching = false;
          if (isCurrent(id) && !scopeLoading) scopeLoadingNode.hidden = true;
        }
      }
      fetchMs = performance.now() - fetchStart;
    } catch (caughtValue) {
      const e = uiError(caughtValue);
      if (listDrops === drops) listRestore(taken);
      throw e;
    }
    // 取得中に認証切れなどで保持を捨てた（再読み込みに入る）。取れた結果は置きも描きもしない。
    if (listDrops !== drops) return;
    // 有効範囲へ戻す取得の間に、全範囲を要するタブへ戻っていた。全範囲の保持を捨てず、応答を置かずに差分取得を続ける。
    if (shrinking && needsFull() && listCache?.scope === 'full') {
      listRestore(taken);
      if (isCurrent(id)) poll(refresh, listSoon());
      return;
    }
    if (bootListRead) listRestore(taken);
    const changed = listGen !== gen;
    let data;
    const integrateStart = performance.now();
    try {
      data = listIntegrate(response, seq, since);
    } catch (caughtValue) {
      const e = uiError(caughtValue);
      listRestore(taken);
      throw e;
    }
    const integrateMs = performance.now() - integrateStart;
    if (!data) {
      // 別の応答が先に保持を置いた（差分の基準が変わった）。この応答は捨て、更新待ちを戻して続きを取り直す。
      listRestore(taken);
      if (isCurrent(id)) poll(refresh, listSoon());
      return;
    }
    // 取得中に別の画面へ移っても、取れた結果は保持に置く（次の開き直しで取り直さない）。取得中に保存・送信・削除で一覧が
    // 変わっていたら、変わる前の内容かもしれないので保持に置かない。基準点は進んでいるので、カーソルを外して次は全体を取る。
    if (changed) {
      if (listCache) listCache.cursor = null;
    } else {
      listCommit(seq, response);
      keepList(data);
    }
    if (!isCurrent(id)) return;
    // 一覧が変わっていたら、取れた内容で描き（行は押せるまま）、すぐ取り直す。
    const drawStart = performance.now();
    screen.apply(data);
    if (timingStart !== undefined) {
      const drawEnd = performance.now();
      listTimingReport(timingStart, drawEnd, {
        fetchMs,
        integrateMs,
        drawMs: drawEnd - drawStart,
        firstDisplayMs: drawEnd - timingStart,
        rows: Array.isArray(response.rows) ? response.rows.length : 0,
      });
    }
    listUnavailable(response, id);
    // 取り直しも通知による読み直しと同じ最短間隔を空ける（client/src/ui/app-live.js の listSoon）。通知による全体照合の後は、
    // 待っている定期確認の予約をそのまま残す（前倒しも先送りもしない）。
    const periodic =
      polling?.fn === refresh && polling.id === routeId && polling.waiting;
    if (changed) poll(refresh, listSoon());
    else if (!notice || !periodic) pollList(refresh);
    // 取得中に届いた通知を捨てない。
    listFlushSchedule();
  }
  function draw() {
    if (!groups || !isListTab(tab)) return;
    listFullNote(tab);
    // PCごとの負荷の表は、応答で変わったとき（保持の hostsState が置き換わったとき）だけ描き直す。
    if (screen.hostsShown !== hostsState) {
      screen.hostsShown = hostsState;
      hostsDraw();
    }
    // タブのボタンと行は作り直さず、件数と変わった行だけを更新する（client/src/ui/app-list-rows.js）。
    listTabsRender(screen, groups);
    // 並び替えは実行中・待機中・停止中・フォローアップで同じ並び順を使う。他のタブは分類結果の順のまま表示する。
    const sorted = ['issues', 'waiting', 'stopped', 'followups'].includes(tab);
    // 通常の依頼／フォローアップの絞り込みは完了・クローズ済みタブだけ。タブの件数は総数のまま。
    const filtered = tab === 'completed' || tab === 'closed';
    const all = sorted ? sortRows(groups[tab], issueSort) : groups[tab];
    const rows = filtered ? filterRows(all, kindFilter) : all;
    $('#sorts').hidden = !sorted;
    $('#kinds').hidden = !filtered;
    const summary = !filtered
      ? ''
      : kindFilter === 'all'
        ? `表示: すべて ${all.length}件`
        : `表示: ${kindLabels[kindFilter]} ${rows.length}件（全 ${all.length}件）`;
    // aria-live のため、定期更新で同じ文言を書き直して読み上げを繰り返さない。
    if ($('#kind-summary').textContent !== summary)
      $('#kind-summary').textContent = summary;
    // タブ名のクラスで、スマホ幅の行の表示（案内文は要対応タブだけ）を style.css が切り替える。
    $('#items').className =
      `list list-${tab}${tab === 'issues' ? ' in-progress' : ''}`;
    // 絞り込みで行が無くなったときは、そのタブが空のときと区別して示す。
    const filteredOut = !rows.length && all.length > 0;
    listRowsRender(screen, rows, {
      tabName: tab,
      filtered,
      empty: {
        sig: JSON.stringify([tab, filteredOut, all.length, issues.length > 0]),
        build: () => emptyView(filteredOut, all.length),
      },
    });
  }
  function emptyView(filteredOut: boolean, total: number) {
    if (filteredOut)
      return el(
        'div',
        { class: 'empty' },
        el('strong', { text: 'この条件に合う依頼はありません' }),
        `このタブには ${total}件あります。「すべて」かほかの絞り込みを選ぶと表示します。`,
      );
    return el(
      'div',
      { class: 'empty' },
      el('strong', {
        text:
          tab === 'completed'
            ? '完了した依頼はまだありません'
            : tab === 'closed'
              ? 'クローズ済みの依頼はありません'
              : tab === 'todos'
                ? '保存したTODOはありません'
                : tab === 'followups'
                  ? 'フォローアップはありません'
                  : tab === 'attention'
                    ? '対応が必要な依頼はありません'
                    : tab === 'waiting'
                      ? '待機中の依頼はありません'
                      : tab === 'stopped'
                        ? '停止中の依頼はありません'
                        : '実行中の依頼はありません',
      }),
      tab === 'followups'
        ? '実装・レビューで見つかった改善点からコントローラーが起票したIssueのうち、未完了のものをここに表示します。完了したものは「完了」、閉じたものは「クローズ済み」タブにあります。'
        : null,
    );
  }
  if (kept) {
    screen.show();
    // 一覧以外を表示中に届いた通知は、戻ったこの時点で回収する。
    listFlushSchedule();
    // 保持した一覧にも現在の接続確認に応じた周期を使い、前回の取得からの残りを待つ。
    const wait = listPollMs() - (Date.now() - kept.at);
    if (wait > 0 && !hasBootRead(listRequest(tab).url))
      return pollList(refresh, kept.at);
    try {
      await refresh();
    } catch (caughtValue) {
      const e = uiError(caughtValue);
      // 保持した一覧を残したまま知らせ、定期更新の失敗と同じく10秒後に取り直す。
      if (!isCurrent(id)) return;
      error(e);
      poll(refresh, 10000);
    }
    return;
  }
  try {
    await refresh();
  } catch (caughtValue) {
    const e = uiError(caughtValue);
    // 保持している一覧が無いので、この後のタブの切り替えは一覧を開き直して取り直す。
    screen.failed = true;
    if (!isCurrent(id)) return;
    error(e);
    poll(refresh, 10000);
    $('#items').replaceChildren(
      el('div', {
        class: 'empty',
        text: '依頼を読み込めませんでした。Giteaへの接続を確認して、画面を再読み込みしてください。',
      }),
    );
  }
}
