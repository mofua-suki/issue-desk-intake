// 要対応の読み取り（scripts/desk-alerts.ts）と起票前の照合（scripts/lib/intake-preflight.ts・duplicate-search.ts）が使う読み取りだけの部品。
export const alertRoutes = [
  { kind: 'stall', route: '/api/stalls/alerts', label: '止まり' },
  {
    kind: 'full_test',
    route: '/api/full-test/alerts',
    label: 'main の全部のテスト',
  },
  {
    kind: 'usage_review',
    route: '/api/usage-review/alerts',
    label: '利用量の見直し',
  },
  {
    kind: 'attention',
    route: '/api/attention/alerts',
    label: '要対応の自動処理',
  },
];
export const draftsRoute = {
  kind: 'draft',
  route: '/api/drafts',
  label: '回答待ちの相談',
};

export const draftPageLimit = 200;

const invalidDraftPage = (value, page) =>
  !value ||
  !Array.isArray(value.items) ||
  typeof value.hasMore !== 'boolean' ||
  value.page !== page ||
  !Number.isInteger(value.total) ||
  value.total < 0 ||
  !Number.isInteger(value.limit) ||
  value.limit < 1 ||
  value.limit > draftPageLimit ||
  (value.hasMore && value.items.length === 0);

/**
 * 相談の一覧（/api/drafts）を最後のページまで読む。途中の失敗は失敗を返し、取得済み行をエラーに添える。
 * 読んでいる途中でデータが変わったとき（total が変わる、同じ行（id）が2ページに出る、集めた件数が total と合わない）は、
 * そこまでの結果を捨てて最初から読み直す。読み直しは最大 maxRereads 回で、それでも安定しなければ失敗にする。
 * 同数の行が重複なく入れ替わる変化は検出できない。
 * @param shapeError 応答の形が崩れたときの文言
 */
export async function readAllDraftPages(
  readPage: (page: number) => Promise<any>,
  shapeError: string,
  maxRereads: number = 2,
) {
  for (let attempt = 0; attempt <= maxRereads; attempt++) {
    const items = [];
    const seen = new Set();
    let total;
    let stable = true;
    for (let page = 1; stable; page++) {
      let value;
      try {
        value = await readPage(page);
      } catch (error) {
        if (stable && items.length && error && typeof error === 'object')
          error.partialItems = items;
        throw error;
      }
      if (invalidDraftPage(value, page)) {
        const error: Error & { partialItems?: unknown[] } = Error(shapeError);
        if (stable && items.length) error.partialItems = items;
        throw error;
      }
      if (total === undefined) total = value.total;
      else if (value.total !== total) stable = false;
      for (const item of value.items) {
        const id = item?.id;
        if (id !== undefined && id !== null) {
          if (seen.has(id)) stable = false;
          seen.add(id);
        }
        items.push(item);
      }
      if (!value.hasMore) {
        if (items.length !== total) stable = false;
        break;
      }
    }
    if (stable) return items;
  }
  throw Error(
    '受付の一覧を読む途中でデータが変わり続け、安定して読めませんでした。',
  );
}

// 起票済み・終了した相談は全体の大半を占め、要対応の行にならない（rowGroup の attention に submitted・closed は無い）ので読まない。
// 親の判定は親自身の splitChildren・consultationChildren で足り、終了した子を読まなくてよい。
async function readDraftPages(call) {
  return readAllDraftPages(
    (page) =>
      call(
        'GET',
        `${draftsRoute.route}?page=${page}&limit=${draftPageLimit}&excludeStatus=submitted,closed`,
      ),
    '受付の応答が一覧の形ではありません。',
  );
}

/**
 * 画面と同じ7種類の入力を読む。分類は呼び出し側の共通判定に任せる。
 * 読めなかった入力は undefined、ページ途中の失敗は取得済み行と失敗を返す。
 */
export async function readDeskAttention(
  call: (method: string, route: string) => Promise<any>,
  query = new URLSearchParams(),
) {
  const routes = [
    // 対応中の範囲（画面の /api/list?tabs=active と同じ絞り込み）だけを読む。終わった依頼は一覧の大半を占め、
    // 要対応の行にならない。受付は、読んだ相談が記録した起票結果・調査Issueの行を閉じていても残すので、
    // requestRows が一覧に無いIssueを起票待ちとして補うことはない。scope を知らない古い受付は全件を返す。
    { kind: 'issues', route: '/api/issues?scope=active', label: 'Issue' },
    { kind: 'drafts', route: draftsRoute.route, label: '相談' },
    { kind: 'closes', route: '/api/close-requests', label: 'クローズ依頼' },
    ...alertRoutes.map((r) => ({
      ...r,
      kind: {
        stall: 'stalls',
        full_test: 'fullTests',
        usage_review: 'reviews',
        attention: 'attentionAlerts',
      }[r.kind],
    })),
  ];
  const results = await Promise.all(
    routes.map(async ({ kind, route, label }) => {
      try {
        const value =
          route === draftsRoute.route
            ? await readDraftPages(call)
            : kind === 'closes'
              ? await readAllDraftPages(
                  (page) =>
                    call(
                      'GET',
                      `${route}?page=${page}&limit=${draftPageLimit}`,
                    ),
                  '受付の応答が一覧の形ではありません。',
                )
              : await call(
                  'GET',
                  kind === 'attentionAlerts' && query.toString()
                    ? `${route}?${query}`
                    : route,
                );
        if (!Array.isArray(value))
          throw Error('受付の応答が一覧の形ではありません。');
        return { kind, value };
      } catch (e) {
        return {
          kind,
          value: e.partialItems,
          failure: {
            kind,
            route,
            label,
            status: e.status ?? null,
            error: e.message,
          },
        };
      }
    }),
  );
  const inputs = Object.fromEntries(results.map((r) => [r.kind, r.value]));
  return {
    inputs,
    failures: results.flatMap((r) => (r.failure ? [r.failure] : [])),
  };
}
