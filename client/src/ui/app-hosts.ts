// ホームのPC担当、停止状態と負荷の表示。

// 最後に読めた GET /api/hosts の応答（server/src/intake-hosts.mjs の形）。読めなければ null（表の欄を出さない）。
let hostsState: HostsResponse | null = null;

const hostsValid = (v: unknown): v is HostsResponse =>
  !!v &&
  typeof v === 'object' &&
  !Array.isArray(v) &&
  'hosts' in v &&
  Array.isArray(v.hosts) &&
  v.hosts.every(hostsRowValid);
const hostsRowValid = (h: unknown): h is Host =>
  !!h &&
  typeof h === 'object' &&
  'name' in h &&
  typeof h.name === 'string' &&
  h.name.length > 0;
// 一覧の取得（GET /api/list。client/src/ui/app-list-sync.js の listCommit）の応答に変わったときだけ入る hosts で置き換える
// （Issue #2417。形の違う応答は null）。
const hostsNumber = (n: unknown): n is number =>
  typeof n === 'number' && Number.isFinite(n);
// 値の無い項目は「−」。
const hostsDash = '−';
const hostsMB = (n: number | undefined) =>
  hostsNumber(n) ? `${Math.round(n).toLocaleString('ja-JP')} MB` : hostsDash;
// GPU の使用率と VRAM（Issue #1203）。空きが分かれば空き、分からなければ使用量。測っていないPCは「−」。
function hostsGpu(h: Host) {
  const load = h.load;
  const parts = [];
  if (hostsNumber(load?.gpuPercent))
    parts.push(`${Math.round(load.gpuPercent)}%`);
  if (hostsNumber(load?.freeVramMB))
    parts.push(`VRAM空き ${hostsMB(load.freeVramMB)}`);
  else if (hostsNumber(load?.usedVramMB))
    parts.push(`VRAM使用 ${hostsMB(load.usedVramMB)}`);
  return parts.length ? parts.join('・') : hostsDash;
}
const hostsTime = (t: string | undefined) =>
  typeof t === 'string' && !Number.isNaN(Date.parse(t)) ? date(t) : hostsDash;
const hostsAlive = (h: Host) =>
  typeof h.updatedAt !== 'string'
    ? hostsDash
    : h.alive === true
      ? '稼働中'
      : '期限切れ';
const hostsDrained = (h: Host): h is Host & { drain: { until?: string } } =>
  !!h.drain && typeof h.drain === 'object';
// 実行環境の問題（作業ツリーの外のパスの権限。Issue #783）でそのPCの監視が新しい引き取りを止めている記録。
// 理由の文（reason）が無い形は出さない（旧版の受付の応答・形の違う応答）。
const hostsHeld = (h: Host): h is Host & { environmentHold: HostHold } =>
  !!h.environmentHold &&
  typeof h.environmentHold === 'object' &&
  typeof h.environmentHold.reason === 'string';
const hostsHeldLabel = '実行環境の問題で停止中';
// 止めたPCの状態。止めていなければ「−」。実装中・公開中が0件になり「再起動してよい」（受付の restartSafe）なら、
// それを出す（印は共有DBの hosts 表で、server は印のあるPCへ配らないので、反映待ちは無い）。監視が実行環境の問題で
// 止めていれば「実行環境の問題で停止中」を出す（理由と直し方はその行の下の行）。
function hostsDrainText(h: Host) {
  const parts = [];
  if (hostsHeld(h)) parts.push(hostsHeldLabel);
  // 形の違う値（文字列でない版）は出さない（表全体の描画を止めない）。
  if (
    h.driftHold &&
    typeof h.driftHold === 'object' &&
    typeof h.driftHold.current === 'string' &&
    typeof h.driftHold.desired === 'string'
  )
    parts.push(
      `版のずれで停止中（監視 ${h.driftHold.current.slice(0, 12)} / 発行 ${h.driftHold.desired.slice(0, 12)}、${hostsTime(h.driftHold.since)}から）`,
    );
  if (!hostsDrained(h)) return parts.length ? parts.join('・') : hostsDash;
  parts.push(
    h.drain.indefinite === true
      ? '停止中（期限なし）'
      : `停止中（期限 ${hostsTime(h.drain.until)}）`,
  );
  if (h.restartSafe === true) parts.push('再起動してよい');
  return parts.join('・');
}
// 実行環境の問題で止めたPCの行の下に置く、表の幅いっぱいの行。止めた時刻・エラーコード・操作・パスと、監視が書いた
// 理由の文（直し方の icacls のコマンドを含む）を文字として出す（textContent。HTMLとして解釈しない）。
function hostsHeldRow(h: Host & { environmentHold: HostHold }) {
  const hold = h.environmentHold;
  const what = [
    hold.code,
    hold.syscall,
    typeof hold.path === 'string' ? `'${hold.path}'` : null,
  ]
    .filter((v) => typeof v === 'string' && v.length > 0)
    .join(' ');
  const since =
    typeof hold.at === 'string' && !Number.isNaN(Date.parse(hold.at))
      ? `${hostsTime(hold.at)}から`
      : '';
  const head = [since, what].filter(Boolean).join('。');
  return el(
    'tr',
    { class: 'host-environment' },
    el('td', {
      colspan: String(hostsColumns.length),
      text: `${h.name}: ${hostsHeldLabel}${head ? `（${head}）` : ''}。${hold.reason}`,
    }),
  );
}
// 各PCの様子（Issue #846。応答の activity）。項目の無い応答（旧版の受付・旧版の監視のPC）は「−」で、行の下にも出さない。
const hostsActivity = (h: Host) =>
  h.activity && typeof h.activity === 'object' && !Array.isArray(h.activity)
    ? h.activity
    : null;
const hostsIssue = (project: string | undefined, issue: number | undefined) =>
  `${shownProject(project)} #${issue}`;
const hostsFilled = <T extends object, K extends keyof T>(
  v: T | undefined,
  key: K,
): v is T & Record<K, string> =>
  !!v && typeof v === 'object' && typeof v[key] === 'string';
// 行の下に出す文: 占有ごとの進み（最後の進み・開いている工程・同じ失敗が続いた回数・最後の失敗）と、そのPCの最後の失敗。
function hostsActivityLines(h: Host) {
  const a = hostsActivity(h);
  if (!a) return [];
  const lines = [];
  for (const r of Array.isArray(a.cliVersions) ? a.cliVersions : []) {
    if (!r || (!r.reason && r.current === r.expected)) continue;
    lines.push(
      `${h.name}: CLIの版がイメージと違います: ${r.cli} 今 ${r.current ?? '不明'} / イメージの定義 ${r.expected}${r.reason ? `（${r.reason}）` : ''}`,
    );
  }
  for (const c of Array.isArray(a.claims) ? a.claims : []) {
    if (!hostsFilled(c, 'project')) continue;
    const parts = [
      `最後の進み ${hostsTime(c.progressAt)}`,
      `工程 ${typeof c.openPhase === 'string' ? c.openPhase : 'なし'}`,
    ];
    if (hostsNumber(c.failures) && c.failures > 0)
      parts.push(`同じ失敗が続けて ${c.failures}回`);
    if (hostsFilled(c.failure, 'reason'))
      parts.push(
        `最後の失敗 ${hostsTime(c.failure.at)} ${c.failure.phase}: ${c.failure.reason}`,
      );
    lines.push(
      `${h.name}: 占有 ${hostsIssue(c.project, c.issue)}（${parts.join('・')}）`,
    );
  }
  const f = a.failure;
  if (hostsFilled(f, 'reason'))
    lines.push(
      `${h.name}: 最後の失敗 ${hostsTime(f.at)} ${hostsIssue(f.project, f.issue)} ${f.phase}: ${f.reason}`,
    );
  return lines;
}
// 行の下の表の幅いっぱいの行。文字として出す（textContent。HTMLとして解釈しない）。折り返しは実行環境の問題の行と同じ。
const hostsActivityRow = (text: string) =>
  el(
    'tr',
    { class: 'host-environment host-activity' },
    el('td', { colspan: String(hostsColumns.length), text }),
  );
async function hostsRefresh() {
  const value = await api<HostsResponse>('/api/hosts');
  if (!hostsValid(value)) throw new Error('PC一覧を読み取れませんでした');
  hostsState = value;
  hostsDraw();
}
function hostsActions(h: Host) {
  const route = `/api/hosts/${encodeURIComponent(h.name)}/drain`;
  const stopped = hostsDrained(h);
  const button = el('button', {
    type: 'button',
    text: stopped ? '受け付けに戻す' : '無期限で止める',
  });
  button.onclick = () =>
    action(button, async () => {
      if (stopped) await api(route, undefined, 'DELETE');
      else
        await api(route, {
          indefinite: true,
          reason: '受付画面から無期限停止',
        });
      await hostsRefresh();
    });
  return button;
}
function hostsRepositories(h: Host): HTMLElement | string {
  if (
    !Array.isArray(h.repositories) ||
    !h.repositories.every((repo) => typeof repo === 'string' && repo.length > 0)
  )
    return '不明';
  if (!h.repositories.length) return '担当なし';
  const desk = new Set(deskRepositories);
  const groups: string[][] = [];
  for (const repo of h.repositories) {
    const group = desk.has(repo)
      ? groups.find((g) => desk.has(g[0]))
      : undefined;
    if (group) group.push(repo);
    else groups.push([repo]);
  }
  return el(
    'div',
    {},
    ...groups.map((group) =>
      el('div', {
        text: desk.has(group[0]) ? 'issue-desk' : shownProject(group[0]),
      }),
    ),
  );
}
const hostsCount = (n: unknown): n is number =>
  typeof n === 'number' && Number.isSafeInteger(n) && n >= 0;
function hostsSlots(h: Host, kind: 'total' | 'used' | 'free'): string {
  if (h.role === 'test-only') return hostsDash;
  const total =
    hostsCount(h.maxImplementations) && hostsCount(h.capacity)
      ? Math.min(h.maxImplementations, h.capacity)
      : null;
  const used =
    hostsCount(h.hostLoad?.implementing) && hostsCount(h.hostLoad?.publishing)
      ? h.hostLoad.implementing + h.hostLoad.publishing
      : null;
  const value =
    kind === 'total'
      ? total
      : kind === 'used'
        ? used
        : total === null || used === null
          ? null
          : Math.max(0, total - used);
  return value === null ? '不明' : `${value}枠`;
}
const hostsColumns: [string, (host: Host) => string | HTMLElement][] = [
  ['PC', (h) => h.name],
  ['役割', (h) => (h.role === 'test-only' ? 'テスト専用' : '実装担当')],
  ['状態', hostsAlive],
  [
    '担当リポジトリ',
    (h) =>
      h.role === 'test-only'
        ? h.testRepositories?.map(shownProject).join('、') || '担当なし'
        : hostsRepositories(h),
  ],
  ['総枠', (h) => hostsSlots(h, 'total')],
  ['使用中', (h) => hostsSlots(h, 'used')],
  ['空き', (h) => hostsSlots(h, 'free')],
  [
    '停止',
    (h) =>
      el('div', {}, el('div', { text: hostsDrainText(h) }), hostsActions(h)),
  ],
];
const hostsLoadColumns: [string, (host: Host) => string | HTMLElement][] = [
  ['PC', (h) => h.name],
  [
    'CPU',
    (h) =>
      hostsNumber(h.load?.cpuPercent)
        ? `${Math.round(h.load.cpuPercent)}%`
        : hostsDash,
  ],
  ['空きメモリ', (h) => hostsMB(h.load?.freeMemoryMB)],
  ['空きディスク', (h) => hostsMB(h.load?.freeDiskMB)],
  ['GPU', hostsGpu],
  ['最後の heartbeat', (h) => hostsTime(h.updatedAt)],
];
const hostsRowClass = (h: Host) =>
  [
    h.alive === true ? 'host-alive' : 'host-stale',
    hostsDrained(h) ? 'host-drained' : '',
    hostsDrained(h) && h.restartSafe === true ? 'host-restart-safe' : '',
    hostsHeld(h) ? 'host-held' : '',
  ]
    .filter(Boolean)
    .join(' ');
function hostsCell(value: string | HTMLElement) {
  return value && typeof value === 'object'
    ? el('td', {}, value)
    : el('td', { text: String(value) });
}
function hostsTable(
  rows: Host[],
  columns: typeof hostsColumns,
  details: boolean,
) {
  return el(
    'div',
    {
      class: details
        ? 'hosts-table hosts-assignment'
        : 'hosts-table hosts-load',
    },
    el(
      'table',
      {},
      el(
        'thead',
        {},
        el(
          'tr',
          {},
          ...columns.map(([name]) => el('th', { scope: 'col', text: name })),
        ),
      ),
      el(
        'tbody',
        {},
        ...rows.flatMap((h) => [
          el(
            'tr',
            { class: hostsRowClass(h) },
            ...columns.map(([, value]) => hostsCell(value(h))),
          ),
          ...(details && hostsHeld(h) ? [hostsHeldRow(h)] : []),
          ...(details ? hostsActivityLines(h).map(hostsActivityRow) : []),
        ]),
      ),
    ),
  );
}
// 一覧を描くたびに呼ぶ（client/src/ui/app-home.js の draw）。応答が無ければ欄ごと隠し、一覧はそのまま出す。
function hostsDraw() {
  const box = $('#hosts');
  if (!box) return;
  box.hidden = !hostsState;
  if (!hostsState) return box.replaceChildren();
  const rows = hostsState.hosts;
  // replaceChildren は null を文字「null」として入れるので、出す要素だけを渡す。
  const parts: HTMLElement[] = [el('h2', { text: '各PC' })];
  // ホームの監視の停止（Issue #982。応答の homeMonitor）。PCの表が空でも見出しのすぐ下に出す。
  const monitor = hostsState.homeMonitor;
  if (
    monitor &&
    typeof monitor === 'object' &&
    monitor.stopped === true &&
    typeof monitor.host === 'string'
  )
    parts.push(
      el('p', {
        class: 'hint hosts-monitor',
        text: `監視が止まっています（${monitor.host}、最後の heartbeat: ${typeof monitor.lastHeartbeatAt === 'string' && !Number.isNaN(Date.parse(monitor.lastHeartbeatAt)) ? date(monitor.lastHeartbeatAt) : '不明'}）`,
      }),
    );
  if (rows.length) {
    parts.push(
      el('h3', { text: '担当と受け持ち' }),
      hostsTable(rows, hostsColumns, true),
      el('h3', { text: '負荷' }),
      hostsTable(rows, hostsLoadColumns, false),
      hint(
        `記録: ${hostsState.source ? `${shownProject(hostsState.source.repo)} の${giteaNumber(hostsState.source.issue)}` : hostsDash} · 読み直し ${hostsTime(hostsState.checkedAt)} · 期限 ${hostsNumber(hostsState.leaseMinutes) ? `${hostsState.leaseMinutes}分` : hostsDash}`,
      ),
      hint(
        '再起動の前は「無期限で止める」を押し、「再起動してよい」が出てから再起動してください（実装中・公開中の依頼は止めずに最後まで進みます）。終わったら「受け付けに戻す」を押します。',
      ),
    );
    if (rows.some(hostsHeld))
      parts.push(
        hint(
          '「実行環境の問題で停止中」は、そのPCの監視が作業ツリーの外のパスの権限でテストを実行できないことを見つけ、新しい引き取りを止めている印です。行の下の直し方をそのPCで実行すると、監視が定期確認のたびに確かめて自動で止めを外します（「受け付けに戻す」では外れません）。',
        ),
      );
  } else
    parts.push(
      el('p', {
        class: 'hint hosts-reason',
        text: `表示できるPCはありません: ${hostsState.reason || '理由は不明です'}`,
      }),
    );
  box.replaceChildren(...parts);
}
