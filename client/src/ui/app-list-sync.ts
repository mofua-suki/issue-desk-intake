// 受付画面の依頼一覧の更新管理（Issue #2417、docs/live-updates.md）。client/src/ui/app.js の一部で、単独のモジュールではない
// （client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する）。
// 表示した一覧を保持し、変更分だけを反映する。取得は3通り: 初回・全体照合は GET /api/list（カーソル付きの全体）、15秒ごとの
// 定期確認は GET /api/list?since=<カーソル>（追加・更新・削除だけ）、対象を識別できる即時通知は GET /api/list/entry（1件）。
// 通知（SSE）は更新待ちとして覚え、約300ms（前の取得の開始から3秒以上空ける）でまとめて取る。一覧以外を表示中・初回取得中の
// 通知も捨てず、一覧に戻ったとき・取得が終わったときに回収する。種類や識別子が不明な通知・切断や引き継ぎからの復帰（null）は
// sinceを使わない全体照合にする。
const listFields = {
  issue: 'issues',
  draft: 'drafts',
  todo: 'todos',
  closeRequest: 'closes',
} as const;
const listEntityTypes: ListEntityType[] = [
  'issue',
  'draft',
  'todo',
  'closeRequest',
];
const listAuxNames = [
  'reviews',
  'fullTests',
  'stalls',
  'attentionAlerts',
] as const;
const listAuxLabels: Record<string, string> = {
  reviews: '利用量の見直し',
  fullTests: '全部のテスト',
  stalls: '止まり',
  attentionAlerts: '要対応の自動処理',
  hosts: '各PC',
  closeRequests: 'クローズ依頼',
};
const listText = (
  a: string | number | null | undefined,
  b: string | number | null | undefined,
) => ((a ?? '') < (b ?? '') ? -1 : (a ?? '') > (b ?? '') ? 1 : 0);
// 行の並び順。応答の並びに頼らず、全体と差分の適用後が同じ並びになるよう、サーバー（server/src/intake-list-source.mjs の orders）と
// 同じ規則で並べ直す。
const listOrders: Record<
  ListEntityType,
  (a: RequestRow, b: RequestRow) => number
> = {
  issue: (a, b) =>
    listText(b.updatedAt ?? '', a.updatedAt ?? '') ||
    listText(a.project, b.project) ||
    (a.number ?? 0) - (b.number ?? 0),
  draft: (a, b) =>
    listText(b.createdAt ?? '', a.createdAt ?? '') || listText(a.id, b.id),
  todo: (a, b) =>
    listText(b.createdAt ?? '', a.createdAt ?? '') || listText(a.id, b.id),
  closeRequest: (a, b) =>
    listText(a.createdAt ?? '', b.createdAt ?? '') || listText(a.id, b.id),
};
// 種類と識別子から作る行のキー（Issueは project#number、ほかは id）。サーバーの listKey と同じ。
const listItemKey = (
  type: string,
  x: { project?: string | null; number?: number; id?: string },
) => (type === 'issue' ? `issue:${x.project}#${x.number}` : `${type}:${x.id}`);
const listUuid = /^[a-f0-9-]{36}$/;
const listPendingMax = 50;
const listEntryTries = 3;
const listRetryMs = 10000;
// 更新待ち。pending: 個別に取る対象（キー → 対象）、needFull: 全体照合が要る。
// entrySeq: 個別取得の結果を置いた対象と取得の通し番号（その後に始めた全体・差分の応答より新しいものを上書きさせない）。
// wideSeq: 置いた全体・差分の応答の最大の通し番号（それより前に始めた個別取得の結果は捨てる）。
let listPending = new Map<string, ListTarget>(),
  listNeedFull = false,
  listEntryTimer: number | null | undefined = null,
  listEntrySeq = new Map<string, number>(),
  listWideSeq = 0,
  listBaseline: ListData | null = null,
  listCandidate: ListData | null = null;
function listForget() {
  cancelTimer(listEntryTimer);
  listEntryTimer = null;
  listPending = new Map<string, ListTarget>();
  listNeedFull = false;
  listEntrySeq = new Map<string, number>();
  listWideSeq = 0;
  listBaseline = null;
  listCandidate = null;
}
// 保持した一覧の配列（issues・drafts・todos・closes）をキーごとの行にする。
function listRowsOf(data: ListData | null) {
  const rows = new Map<string, ListEntry>();
  for (const type of listEntityTypes) {
    const field = listFields[type];
    for (const item of data?.[field] ?? []) {
      const key = listItemKey(type, item);
      rows.set(key, { key, type, item });
    }
  }
  return rows;
}
// キーごとの行を、並び順を付けた配列（保持した一覧の形）へ戻す。配列以外の項目（要対応・取得時刻など）は base のまま。
// 全体・差分・個別のどの取得でも、応答の並びに頼らず種類ごとに listOrders で並べる（同じデータなら必ず同じ並びになる）。
function listDataFrom(
  rows: Map<string, ListEntry>,
  base: ListData | null,
): ListData {
  const data: ListData = {
    at: 0,
    issues: [],
    drafts: [],
    todos: [],
    closes: [],
    reviews: [],
    fullTests: [],
    attentionAlerts: [],
    ...base,
  };
  const lists: Record<ListEntityType, RequestRow[]> = {
    issue: [],
    draft: [],
    todo: [],
    closeRequest: [],
  };
  for (const { type, item } of rows.values()) lists[type].push(item);
  for (const type of listEntityTypes)
    data[listFields[type]] = lists[type].sort(listOrders[type]);
  return data;
}
const listValidRow = (r: ListEntry) =>
  !!r &&
  Object.hasOwn(listFields, r.type) &&
  !!r.item &&
  typeof r.item === 'object' &&
  r.key === listItemKey(r.type, r.item);
// 全体・差分の応答を、保持した一覧へ反映した新しい一覧にする（置かない・描かない。捨てるときは null）。
// since: 要求に付けたカーソル。差分は、その要求の後も保持のカーソルが同じときだけ反映する（別の応答が先に置いていたら捨てる）。
function listIntegrate(
  response: ListResponse,
  seq: number,
  since: string | null,
) {
  if (!response || typeof response !== 'object' || Array.isArray(response))
    throw Error('一覧の応答を読めませんでした。');
  const cur = listCache;
  if (seq < listWideSeq) return null;
  const full = response.full !== false;
  if (
    !full &&
    (!since ||
      !cur ||
      cur.cursor !== since ||
      (listBaseline &&
        (listBaseline.cursor !== since || listBaseline.scope !== cur.scope)) ||
      (response.scope === 'full' ? 'full' : 'active') !== cur.scope)
  )
    return null;
  // 個別取得の結果のうち、この要求より後に始めたものは、この応答（古い内容かもしれない）で上書きしない。
  const newer = (key: string) => (listEntrySeq.get(key) ?? 0) > seq;
  const unread = listUnreadOf(response);
  const unreadNames = new Set(unread.map((u) => u.project));
  const unreadRow = (entry: ListEntry | undefined) =>
    entry?.type === 'issue' && unreadNames.has(entry.item.project ?? '');
  let rows: Map<string, ListEntry>;
  if (full) {
    rows = new Map<string, ListEntry>();
    for (const r of Array.isArray(response.rows) ? response.rows : [])
      if (listValidRow(r))
        rows.set(r.key, { key: r.key, type: r.type, item: r.item });
  } else {
    rows = listRowsOf(listBaseline ?? cur);
    for (const r of [...(response.added ?? []), ...(response.updated ?? [])])
      if (listValidRow(r))
        rows.set(r.key, { key: r.key, type: r.type, item: r.item });
    for (const key of response.removed ?? [])
      if (!unreadRow(rows.get(key))) rows.delete(key);
  }
  // 読めなかった候補のリポジトリの行は、応答に無くても削除とせず前回の内容で残し、読取失敗の目印を付ける。
  if (full)
    for (const [key, entry] of listRowsOf(listBaseline ?? cur))
      if (!rows.has(key) && unreadRow(entry)) rows.set(key, entry);
  for (const [key, entry] of rows)
    if (entry.type === 'issue' && (unreadRow(entry) || entry.item.readFailed)) {
      const item = { ...entry.item };
      delete item.readFailed;
      rows.set(key, {
        ...entry,
        item: unreadRow(entry) ? { ...item, readFailed: true } : item,
      });
    }
  const data = listDataFrom(rows, cur);
  data.unreadProjects = unread;
  const aux =
    response.aux && typeof response.aux === 'object' ? response.aux : {};
  // 読めなかった種類（unavailable）は空や削除として扱わず、保持していたものを残す。
  for (const name of listAuxNames)
    data[name] = Array.isArray(aux[name])
      ? aux[name]
      : Array.isArray(cur?.[name])
        ? cur[name]
        : [];
  data.at = Date.now();
  data.cursor = typeof response.cursor === 'string' ? response.cursor : null;
  data.scope = response.scope === 'full' ? 'full' : 'active';
  listCandidate = data;
  const kept = listRowsOf(cur);
  // 読めなかったプロジェクトの表示は、個別取得による変更も含めて前回のまま残す。
  const previousBaseline = listRowsOf(listBaseline ?? cur);
  for (const [key, entry] of rows)
    if (
      unreadRow(entry) &&
      !kept.has(key) &&
      (previousBaseline.has(key) || listEntrySeq.has(key))
    )
      rows.delete(key);
  for (const [key, entry] of kept)
    if (unreadRow(entry))
      rows.set(key, { ...entry, item: { ...entry.item, readFailed: true } });
  for (const key of listEntrySeq.keys())
    if (newer(key)) {
      if (kept.has(key)) {
        const entry = kept.get(key)!;
        rows.set(
          key,
          unreadRow(entry)
            ? { ...entry, item: { ...entry.item, readFailed: true } }
            : entry,
        );
      } else rows.delete(key);
    }
  return listDataFrom(rows, data);
}
// 応答の unreadProjects（読めなかった候補のリポジトリ）。形の合わない項目は使わない。
function listUnreadOf(response: ListResponse): ListUnreadProject[] {
  const list = Array.isArray(response?.unreadProjects)
    ? response.unreadProjects
    : [];
  return list.filter(
    (u) =>
      !!u &&
      typeof u.project === 'string' &&
      !!u.project &&
      typeof u.reason === 'string',
  );
}
// listIntegrate の結果を置くとき（保持の更新と同時）に呼ぶ。PC負荷は応答にあるとき（変わったとき）だけ置き換える。
// 読めなかったリポジトリが差分で読めるようになったら、残していた行に読めない間に消えたものがあり得るので全体照合にする。
function listCommit(seq: number, response: ListResponse) {
  listBaseline = listCandidate;
  listWideSeq = Math.max(listWideSeq, seq);
  if (response.full === false) {
    const now = new Set(listUnreadOf(response).map((u) => u.project));
    if ((listCache?.unreadProjects ?? []).some((u) => !now.has(u.project)))
      listNeedFull = true;
  }
  for (const [key, s] of listEntrySeq) if (s <= seq) listEntrySeq.delete(key);
  if ('hosts' in response)
    hostsState = hostsValid(response.hosts) ? response.hosts : null;
}
const listUrl = (since: string | null, scope: 'active' | 'full') => {
  const query = new URLSearchParams();
  if (since) query.set('since', since);
  if (scope === 'active') query.set('tabs', 'active');
  return `/api/list${query.size ? `?${query}` : ''}`;
};
// 全体・差分の要求を始める。それより前に届いた通知はこの要求が含むので、更新待ちから外す（外したものを返す）。
// 全体照合の要求（listNeedFull）は、呼び出し側がカーソルを選ぶ前に見て全体にする（client/src/ui/app-home.js の refresh）。
function listTake() {
  const taken = { pending: listPending, needFull: listNeedFull };
  listPending = new Map<string, ListTarget>();
  listNeedFull = false;
  return taken;
}
// 要求が失敗した・応答を置かなかったときは、外した更新待ちと全体照合の要求を戻す（置けるまで保つ）。
function listRestore(taken: {
  pending: Map<string, ListTarget>;
  needFull: boolean;
}) {
  for (const [key, t] of taken.pending)
    if (!listPending.has(key)) listPending.set(key, t);
  if (taken.needFull) listNeedFull = true;
  if (listPending.size > listPendingMax) {
    listPending = new Map<string, ListTarget>();
    listNeedFull = true;
  }
}
// 読めなかった種類・候補のリポジトリがあれば、表示済みのまま理由を知らせる（次の更新で読み直す）。
function listUnavailable(response: ListResponse, id: number) {
  const names = Array.isArray(response?.unavailable)
    ? response.unavailable
    : [];
  const unread = listUnreadOf(response);
  if (!isCurrent(id)) return;
  if (names.length)
    error(
      Error(
        `${names.map((n: string | number) => listAuxLabels[n] || n).join('・')}を読み込めませんでした。表示済みの内容は残し、次の更新で読み直します。`,
      ),
    );
  if (unread.length)
    error(
      Error(
        `候補のリポジトリ ${unread.map((u) => `${u.project}（${u.reason}）`).join('・')} を読み込めませんでした。一覧はそろっていません。表示済みの行は「読取失敗」の目印を付けて残し、次の更新で読み直します。`,
      ),
    );
}
// 対象を識別できる通知は個別に取る対象、そうでなければ null（全体照合）。
function listNoticeTarget(n: ListNotice): ListTarget | null {
  if (!n || typeof n !== 'object') return null;
  if (
    n.type === 'issue' &&
    typeof n.project === 'string' &&
    n.project &&
    Number.isSafeInteger(n.number) &&
    (n.number ?? 0) > 0
  )
    return {
      type: 'issue',
      project: n.project,
      number: n.number!,
      key: listItemKey('issue', n),
    };
  if (
    Object.hasOwn(listFields, n.type) &&
    n.type !== 'issue' &&
    typeof n.id === 'string' &&
    listUuid.test(n.id)
  )
    return {
      type: n.type as Exclude<ListEntityType, 'issue'>,
      id: n.id,
      key: listItemKey(n.type, n),
    };
  return null;
}
// 即時通知（liveNotice）を一覧の更新待ちとして覚える。新しいプロジェクトの通知は一覧に関係しない（既存の画面へ渡す）。
function listNote(n: ListNotice | null) {
  if (n !== null && n.type === 'project') return;
  const target = n === null ? null : listNoticeTarget(n);
  if (target) listPending.set(target.key, target);
  else listNeedFull = true;
  if (listPending.size > listPendingMax) {
    listPending = new Map<string, ListTarget>();
    listNeedFull = true;
  }
  listFlushSchedule();
}
// 更新待ちを取る予約。一覧を表示していない間・初回の取得中は覚えておくだけ（表示したとき・取得が終わったときに予約する）。
// 通知のまとめは300ms、更新取得の開始間隔は前の取得の開始から3秒以上（listSoon）。
function listFlushSchedule(delay?: number) {
  if (listScreen?.id !== routeId || !listCache || listEntryTimer) return;
  if (!listNeedFull && !listPending.size) return;
  const id = routeId;
  listEntryTimer = setTimeout(() => {
    listEntryTimer = null;
    listFlush(id);
  }, delay ?? listSoon());
}
const listEntryUrl = (t: ListTarget) =>
  `/api/list/entry?${new URLSearchParams(t.type === 'issue' ? { type: 'issue', project: t.project, number: String(t.number) } : { type: t.type, id: t.id })}`;
// 取得の開始直前に、前の取得の開始から最短間隔が空くまで待つ（Issue #2530。通知の予約後に定期取得が始まった場合も守る）。
// 通知の予約は置き換えるが、取得開始を待つ各呼び出しは独立して完了させる。
// 待たなくてよいときは null を返し、呼び出し側は待たずに続ける（余分な非同期の切れ目を作らない）。
function listStartWait() {
  const ms = listReserve();
  if (ms <= 0) {
    const short = listBegin();
    return short > 0 ? listStartWaitFor(short) : null;
  }
  return listStartWaitFor(ms);
}
// 待機が終わったら、実際の開始から最短間隔を確かめ直し、足りなければ不足ぶんだけ待ってから開始を記録する。
function listStartWaitFor(ms: number) {
  cancelTimer(listEntryTimer);
  listEntryTimer = null;
  return new Promise<void>((resolve) => {
    const wait = (n: number | undefined) => {
      setTimeout(() => {
        const short = listBegin();
        if (short > 0) wait(short);
        else resolve();
      }, n);
    };
    wait(ms);
  });
}
async function listFlush(id: number) {
  if (listScreen?.id !== id || !isCurrent(id) || !listCache) return;
  if (!listNeedFull && !listPending.size) return;
  const waiting = listStartWait();
  if (waiting) {
    await waiting;
    if (listScreen?.id !== id || !isCurrent(id) || !listCache) return;
  }
  if (listNeedFull) {
    // 全体照合（sinceを使わない）。更新待ちはこの要求が含むので refresh が外す。待ちは済んでいる。
    try {
      await listScreen.refresh(true, true);
    } catch (caughtValue) {
      const e = uiError(caughtValue);
      if (!isCurrent(id)) return;
      listNeedFull = true;
      error(e);
      listFlushSchedule(listRetryMs);
    }
    return;
  }
  const targets = [...listPending.values()];
  if (!targets.length) return;
  listPending = new Map<string, ListTarget>();
  const drops = listDrops,
    seq = ++listFetches;
  const results = await Promise.all(
    targets.map(async (t) => {
      try {
        return { t, value: await api<ListEntry>(listEntryUrl(t)) };
      } catch (caughtValue) {
        const e = uiError(caughtValue);
        return { t, failure: e };
      }
    }),
  );
  // 取得中に認証切れなどで保持を捨てた。取れた結果は置きも描きもしない。
  if (listDrops !== drops || !listCache) return;
  // これより後に始めた全体・差分の応答が先に置かれた（通知はそれが含む）。古い個別の結果は捨てる。
  if (seq < listWideSeq) return listFlushSchedule();
  const rows = listRowsOf(listCache);
  if (!listBaseline) listBaseline = listCache;
  let failure = null;
  for (const { t, value, failure: e } of results) {
    // 同じ対象で、より後に始めた個別取得の結果が既に置かれている。古い成功・404・再試行は無視する。
    if ((listEntrySeq.get(t.key) ?? 0) > seq) continue;
    if (e === undefined) {
      if (
        value?.key === t.key &&
        value.type === t.type &&
        value.item &&
        typeof value.item === 'object'
      ) {
        const readFailed =
          t.type === 'issue' &&
          (listCache.unreadProjects ?? []).some((u) => u.project === t.project);
        rows.set(t.key, {
          key: t.key,
          type: t.type,
          item: readFailed ? { ...value.item, readFailed: true } : value.item,
        });
        listEntrySeq.set(t.key, seq);
      } else {
        failure = Error('一覧の更新を読めませんでした。');
        if ((t.tries ?? 0) + 1 < listEntryTries)
          listPending.set(t.key, { ...t, tries: (t.tries ?? 0) + 1 });
      }
    } else if (e.status === 404) {
      // 存在しないことを確かめられたときだけ削除する。
      rows.delete(t.key);
      listEntrySeq.set(t.key, seq);
    } else if (e.status === 409) {
      // 状態が移った（TODOから相談など）。移行先は個別の応答では決められないので全体を照合する。
      listNeedFull = true;
    } else if (e.status !== 403) {
      // 403は許可外で、削除と決めつけない（定期確認が回収する）。通信失敗は表示済みのまま理由を知らせて再試行する。
      failure = e;
      if ((t.tries ?? 0) + 1 < listEntryTries)
        listPending.set(t.key, { ...t, tries: (t.tries ?? 0) + 1 });
    }
  }
  const data = listDataFrom(rows, listCache);
  keepList(data);
  if (isCurrent(id) && listScreen?.id === id) {
    listScreen.apply(data);
    if (failure) error(failure);
  }
  listFlushSchedule(failure ? listRetryMs : undefined);
}

// 一覧を開くまでの所要（GET /api/meta の listMeasurements.browser で読む）を受付へ知らせる。計測の失敗で画面を止めない。
// 通信の大きさは PerformanceResourceTiming の値で、encodedBytes が圧縮後、decodedBytes が圧縮前。
function listTimingReport(
  start: number,
  end: number,
  values: Record<string, number>,
) {
  const report: Record<string, number> = {};
  for (const [name, value] of Object.entries(values))
    report[name] = Math.round(value);
  if (start < 60000 && end < 120000) report.fromNavigationMs = Math.round(end);
  try {
    const entries = performance.getEntriesByType('resource').filter((entry) => {
      const url = new URL(entry.name);
      return url.origin === location.origin && url.pathname === '/api/list';
    }) as PerformanceResourceTiming[];
    const entry = entries[entries.length - 1];
    if (entry) {
      report.encodedBytes = entry.encodedBodySize;
      report.decodedBytes = entry.decodedBodySize;
      report.ttfbMs = Math.round(
        Math.max(0, entry.responseStart - entry.requestStart),
      );
    }
  } catch {
    // 通信の大きさを読めないブラウザでも、所要は送る。
  }
  const send = () => void api('/api/list/timing', report).catch(() => {});
  if (document.hidden || typeof requestAnimationFrame !== 'function') {
    send();
    return;
  }
  // 描いた行が画面に出るのは次のフレームの後。裏へ回って進まなければ時間切れで送る。
  let sent = false;
  const finish = (painted: boolean) => {
    if (sent) return;
    sent = true;
    if (painted) report.paintMs = Math.round(performance.now() - start);
    send();
  };
  requestAnimationFrame(() => setTimeout(() => finish(true), 0));
  setTimeout(() => finish(false), 3000);
}
