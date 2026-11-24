// 受付画面の新版への自動更新（保留の判断・戻る先の記録・版の確認の開始）と、再読み込みをまたぐ依頼一覧の取得結果の
// 保持。client/src/ui/app.js の一部（Issue #457）で、単独のモジュールではない。受付は client/scripts/build.ts の順に app.js と
// つないだ1つのモジュールとして /app.js を配信する。
// 画面の新版への自動更新。版の確認・保留・再読み込みの判断は ui-update.js が行い、ここでは画面の状態を渡す。
const reloadKey = 'issue-desk:ui-reload';
let updates: ReturnType<typeof updateWatcher> | null = null,
  // 前回の自動更新の記録（読み取り前は undefined）。resumed は画面の復元に1度だけ使う。
  reloadRecord: ReloadState | null | undefined,
  resumed: ReloadState | null = null;
const composerHold = '未送信の入力・画像があるか、入力の復元・画像の処理中です';
function updateHold() {
  if (operations || issuePosting) return '保存・送信の処理中です';
  // 編集状態を持つ入力部品（TODOの画面）は保存済みの本文も持つので、未保存かどうかは画面の保留理由で判断する。
  if (activeComposer?.hasContent() && !activeComposer.edit?.())
    return composerHold;
  if ($('#phone-dialog')?.open) return '接続コードを表示中です';
  return holdReason?.() || null;
}
// 再読み込みの直前に、戻る先の画面を記録する。書けなければ例外になり、更新を保留する。
// 本文・画像・資格情報は記録しない（未送信の入力がある間はそもそも再読み込みしない）。
// TODO・相談・Issueは画面のURLから戻るので、URLが一覧のままの新規依頼だけを記録する。
function keepScreen(record: ReloadRecord) {
  const screen =
    current?.type === 'new'
      ? { type: 'new', share: current.share === true }
      : null;
  sessionStorage.setItem(
    reloadKey,
    JSON.stringify({
      ...record,
      hash: location.hash,
      screen,
      sort: issueSort,
      kindFilter,
    }),
  );
}
// 依頼一覧の取得結果は同じタブの sessionStorage にも置き、再読み込みの後に読み戻す。配信の版（data-ui）と一緒に
// 保存し、版が違う・読めない・形が違うものは使わない。別のタブ・端末へは持ち出さない（localStorage・IndexedDB は使わない）。
const listKey = 'issue-desk:list',
  listRefreshMs = 15000;
const listVersion = () => document.documentElement?.dataset?.ui ?? null;
// 一覧の取得の前後を比べる世代（client/src/ui/app-home.js の refresh()）。
// listGen: 一覧が変わった（保存・送信・削除・即時通知で古くした、要対応を保持から外した、保持を捨てた）たびに進む。
//   取得中に進んだら、取れた結果は変わる前の内容かもしれないので保持に置かない（行は押せるまま。すぐ取り直す）。
//   取得結果を置くだけ（取得時刻が 0 でない keepList）では進めない（前の画面と今の画面の取得が重なっても変更扱いにしない）。
// listDrops: 認証切れなどで保持を捨てたたびに進む。取得中に進んだら、取れた結果を置きも描きもしない。
// listFetches: 取得の通し番号。古い取得の結果で新しい保持を上書きしないために使う（client/src/ui/app-list-sync.js の listWideSeq・
//   listEntrySeq）。
let listGen = 0,
  listDrops = 0,
  listFetches = 0;
// 保存・送信・削除の後も一覧を変えない要求（接続・接続コード・画像の受け渡し）。保持の取得時刻を古くしない。
const listNeutral = [
  '/api/desktop',
  '/api/pair-code',
  '/api/pair',
  '/api/images',
  '/api/list/timing',
];
// 保持した行の並びが基準と同じ中身か。差分が空の取得は基準と同じ行（同じ参照）を並べ直すだけなので、参照がすべて同じなら
// 全行の文字列化を省く。参照が違う行があるときだけ中身を比べる。
const listSameRows = (a: unknown[] | undefined, b: unknown[] | undefined) =>
  a === b ||
  (Array.isArray(a) &&
    Array.isArray(b) &&
    a.length === b.length &&
    a.every((row, i) => row === b[i])) ||
  JSON.stringify(a) === JSON.stringify(b);
function keepList(data: ListData) {
  if (data.at === 0) listGen++;
  listCache = data;
  try {
    const unbased =
      listEntrySeq.size > 0 ||
      (listBaseline &&
        (['issues', 'drafts', 'todos', 'closes'] as const).some(
          (field) => !listSameRows(data[field], listBaseline![field]),
        ));
    const stored = {
      ui: listVersion(),
      ...data,
      cursor: unbased ? null : data.cursor,
    };
    sessionStorage.setItem(listKey, JSON.stringify(stored));
  } catch {
    // 書けない（容量超過など）ときは画面内の保持だけで動く。古い版の記録は残さない。
    try {
      sessionStorage.removeItem(listKey);
    } catch {}
  }
}
function dropList() {
  listGen++;
  listDrops++;
  listCache = null;
  // 更新待ち・カーソルも破棄する（進行中の応答は listDrops の違いで捨てる）。
  listForget();
  try {
    sessionStorage.removeItem(listKey);
  } catch {}
}
// 取得時刻を古くして、次に一覧を開いたときにすぐ裏で取り直させる。既に古い保持でも世代は進め、取得中の結果で
// 上書きさせない。
function staleList() {
  listGen++;
  if (listCache && listCache.at !== 0) keepList({ ...listCache, at: 0 });
}
function readList(): ListData | null {
  try {
    const v = JSON.parse(sessionStorage.getItem(listKey) || 'null');
    const lists = ['issues', 'drafts', 'todos', 'closes'];
    if (
      v &&
      v.ui === listVersion() &&
      Number.isFinite(v.at) &&
      lists.every((k) => Array.isArray(v[k]))
    )
      return {
        at: v.at,
        issues: v.issues,
        drafts: v.drafts,
        todos: v.todos,
        closes: v.closes,
        scope: v.scope === 'active' || v.scope === 'full' ? v.scope : undefined,
        cursor: typeof v.cursor === 'string' ? v.cursor : null,
        // 利用量の見直しの要対応（Issue #591）。無い記録（更新前の画面が保存したもの）は空として読む。
        reviews: Array.isArray(v.reviews) ? v.reviews : [],
        // main の全部のテストの失敗の要対応（Issue #790）。無い記録は空として読む。
        fullTests: Array.isArray(v.fullTests) ? v.fullTests : [],
        stalls: Array.isArray(v.stalls) ? v.stalls : [],
        // 要対応の自動処理の状態（Issue #1190）。無い記録は空として読む。
        attentionAlerts: Array.isArray(v.attentionAlerts)
          ? v.attentionAlerts
          : [],
      };
    if (v !== null) sessionStorage.removeItem(listKey);
  } catch {}
  return null;
}
function takeReload(): ReloadState | null {
  try {
    const value = JSON.parse(sessionStorage.getItem(reloadKey) || 'null');
    sessionStorage.removeItem(reloadKey);
    return value && typeof value === 'object' ? value : null;
  } catch {
    return null;
  }
}
function showUpdate(message: string | null) {
  const node = $('#update');
  if (!node) return;
  node.replaceChildren();
  if (message)
    node.append(
      el('span', { text: message }),
      el('button', {
        class: 'primary',
        text: '今すぐ更新',
        onclick: () => updates?.requestUpdate(),
      }),
    );
  node.hidden = !message;
}
function startUpdates() {
  if (updates) return;
  updates = updateWatcher({
    // 配信時に index.html へ埋め込まれた、読み込んだ画面の版。無ければ（直接開いた検証用の画面など）確認しない。
    baseline: document.documentElement?.dataset?.ui,
    last: reloadRecord,
    async health({ signal } = {}) {
      const r = await fetch('/api/health', { cache: 'no-store', signal });
      if (!r.ok) throw Error('受付が応答しません。');
      return r.json();
    },
    hold: updateHold,
    prepare: keepScreen,
    reload: () => location.reload(),
    show: showUpdate,
    schedule: (fn: TimerHandler, ms: number | undefined) => setTimeout(fn, ms),
    cancel: (id: number | undefined) => cancelTimer(id),
    visible: () => document.visibilityState !== 'hidden',
  });
  if (!updates.enabled) return;
  // タブの復帰、スリープ明け（ページの再表示）、通信の回復ですぐ確かめる。確認は常に1本だけ。
  document.addEventListener?.('visibilitychange', () =>
    document.visibilityState === 'hidden' ? updates?.hidden() : updates?.wake(),
  );
  window.addEventListener('hashchange', () => updates?.navigate());
  for (const k of ['focus', 'pageshow', 'online'])
    window.addEventListener(k, () => updates?.wake());
  updates.start();
}
