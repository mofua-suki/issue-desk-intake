import {
  splitAttention,
  attentionActivity as activity,
} from './attention-split.generated.ts';
// 要対応の元の行から、安全なメタデータだけをまとめる。I/O・実時刻は持たない。
// Node 18 のトークン接続でも使う下書きの待ちの説明。scripts/lib/draft-wait-text.ts も同じ実装を呼ぶ。
export function draftWaitText(d: Record<string, any>, status: string): string {
  if (d.status === 'thinking') {
    switch (status) {
      case 'ai_stopped':
        return '止まっている（受付AIの実行記録がありません）。受付画面か desk-alerts.ts で要対応を確認してください。';
      case 'ai_recovering':
        return `自動再試行待ち（${d.retryAt}）。操作は不要です。`;
      case 'ai_queued':
        return '受付AIの順番待ち。操作は不要です。';
      default:
        return '受付AIが依頼を整理中です。';
    }
  }
  const q = d.filingQueue;
  if (!['auto_ready', 'submitting'].includes(d.status) || !q) return '';
  const reasons = {
    filing_slot: 'ほかの起票が枠を使用中、または枠の取得待ち',
    drain_hold: '受付の入れ替えのため起票を一時停止中',
    running: 'この下書きを起票中',
    not_running: 'この下書きの待ち・実行の記録がありません',
  };
  return [
    q.stuck
      ? `詰まりの疑い（${q.inactiveMinutes} 分進んでいません）。受付画面か desk-alerts.ts で要対応を確認してください。`
      : q.reason === 'running'
        ? '起票中です。操作は不要です。'
        : `起票の順番待ち（前に ${q.ahead} 件。起票の枠は待ち始めた順を保証しないため目安）。操作は不要です。`,
    q.holder
      ? `起票の枠: 下書き ${q.holder.id}（${q.holder.at} から）`
      : '起票の枠: 使用記録なし',
    `最後に起票が進んだ時刻: ${q.lastProgressAt ?? '受付の起動以降に記録なし'}`,
    `理由: ${reasons[q.reason] ?? '受付画面で確認してください'}`,
  ].join('\n');
}

type Row = Record<string, any>;
const issueStates = new Set(['failed', 'review_held', 'waiting']);
const draftStates = new Set([
  'error',
  'stalled',
  'submit_error',
  'submit_unknown',
  'intake_waiting',
  'draft',
  'investigation_reported',
  'investigation_ended',
]);
const actions = {
  pending: '調査の起票待ち',
  investigating: '調査の起票',
  retrying: '再試行',
  retried: '再試行',
  fix: '修正の印',
  notify: '止めた',
  unreadable: '止めた',
  ended: '止めた',
  retry_error: '止めた',
  file_error: '止めた',
  stopped: '止めた',
};
function timestamp(value: unknown): number | null {
  if (typeof value !== 'string' || !value.trim()) return null;
  // オフセットのない日時も UTC として扱う。
  const text = value.trim();
  const time = Date.parse(
    /(?:Z|[+-]\d{2}:?\d{2})$/i.test(text) ? text : `${text}Z`,
  );
  return Number.isFinite(time) ? time : null;
}
const latest = (values: (number | null)[]): number | null => {
  const times = values.filter((v) => v !== null);
  return times.length ? Math.max(...times) : null;
};
const issueNumber = (row: Row) =>
  Number.isSafeInteger(row.number)
    ? row.number
    : Number.isSafeInteger(row.issue)
      ? row.issue
      : null;
/**
 * @param rows 画面と同じ判定で組んだ要対応の元の行
 */
export function summarizeDeskAttention(
  rows: Row[],
  {
    now,
    staleMinutes = 30,
    waitLimitMinutes = 1440,
    partial = false,
  }: {
    now: number | string | Date;
    staleMinutes?: number;
    waitLimitMinutes?: number;
    partial?: boolean;
  },
) {
  const nowMs =
    typeof now === 'number'
      ? now
      : now instanceof Date
        ? now.getTime()
        : timestamp(now);
  if (nowMs === null || !Number.isFinite(nowMs))
    throw Error('now は有効な時刻を指定してください');
  if (!Number.isSafeInteger(staleMinutes) || staleMinutes <= 0)
    throw Error('staleMinutes は正の整数を指定してください');
  const split = splitAttention(rows, { now, waitLimitMinutes });
  const summarize = (entry: Row) => {
    const { key, rows: group } = entry;
    const primary = group.find((r) => r.kind === 'issue') || group[0];
    const lastMs = latest(group.map(activity));
    const elapsedMinutes =
      lastMs === null ? null : Math.max(0, (nowMs - lastMs) / 60000);
    const automatic = group
      .filter((r) => r.kind === 'attention')
      .map((r) => ({
        id: r.id ?? null,
        state: r.state ?? null,
        stateLabel: r.stateLabel ?? null,
        lastAction: actions[r.state] ?? null,
        updatedAt:
          timestamp(r.updatedAt) === null
            ? null
            : new Date(timestamp(r.updatedAt)).toISOString(),
      }));
    const autoMs = latest(automatic.map((a) => timestamp(a.updatedAt)));
    const eligible =
      !group.some((r) => r.kind === 'issue' && r.status === 'stopped') &&
      group.some(
        (r) =>
          (r.kind === 'issue' && issueStates.has(r.status)) ||
          (r.kind === 'draft' && draftStates.has(r.status)) ||
          (r.kind === 'attention' &&
            ['failed', 'review_held'].includes(r.caseKind)),
      );
    const stale =
      eligible &&
      (elapsedMinutes === null || elapsedMinutes >= staleMinutes) &&
      (!automatic.length ||
        autoMs === null ||
        nowMs - autoMs > staleMinutes * 60000 ||
        automatic.some((a) => a.lastAction === '止めた'));
    // 各行は許可した表示用フィールドだけ。本文・会話・画像はコピーしない。
    const safeRows = group.map((r) =>
      Object.fromEntries(
        [
          'id',
          'kind',
          'status',
          'state',
          'stateLabel',
          'caseKind',
          'kindName',
          'reason',
          'error',
          'key',
        ]
          .filter((field) => r[field] !== undefined)
          .map((field) => [field, r[field]]),
      ),
    );
    return {
      nextActor: entry.nextActor,
      ...(entry.waitExceeded ? { waitExceeded: true } : {}),
      ...(entry.members ? { members: entry.members, count: entry.count } : {}),
      ...(entry.reasons ? { reasons: entry.reasons } : {}),
      key,
      project: primary.project || primary.repo || null,
      number: issueNumber(primary),
      desk: primary.desk ?? null,
      title: primary.title || primary.kindName || '（題名なし）',
      ids: group.map((r) => r.id ?? null),
      rows: safeRows,
      automatic,
      lastActivityAt: lastMs === null ? null : new Date(lastMs).toISOString(),
      elapsedMinutes,
      stale,
    };
  };
  const cases = split.attention.map(summarize);
  const waiting = split.waiting.map(summarize);
  const stopped = split.stopped.map(summarize);
  const reasons = new Map();
  for (const entry of waiting)
    for (const reason of entry.reasons)
      reasons.set(reason, (reasons.get(reason) || 0) + 1);
  cases.sort((a, b) => Number(b.stale) - Number(a.stale));
  const pending = rows.find(
    (r) => r.caseKind === 'attention_pending' && r.waitingCount !== undefined,
  );
  return {
    summary: {
      total: cases.reduce((sum, entry) => sum + entry.rows.length, 0),
      rawTotal: rows.length,
      cases: cases.length,
      stale: cases.filter((c) => c.stale).length,
      staleMinutes,
      partial,
      pendingWait: pending
        ? {
            count: pending.waitingCount,
            estimatedDays: pending.estimatedDays,
            inPerDay: pending.inPerDay,
            outPerDay: pending.outPerDay,
            notClearing: pending.notClearing,
            over: pending.backlogOver,
          }
        : null,
      waiting: {
        cases: waiting.length,
        rows: waiting.reduce((sum, entry) => sum + entry.rows.length, 0),
        reasons: [...reasons].map(([reason, count]) => ({ reason, count })),
      },
      stopped: { cases: stopped.length },
    },
    cases,
    waiting,
    stopped,
  };
}

const oneLine = (value: unknown) =>
  String(value ?? '')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, 500);
export function attentionCaseLine(
  entry: ReturnType<typeof summarizeDeskAttention>['cases'][number],
) {
  const target = `${entry.project || 'プロジェクト未定'}${entry.number !== null ? `#${entry.number}` : ''}${entry.desk ? `（受付の共通番号 #${entry.desk}）` : ''}`;
  const states = entry.rows
    .map(
      (r) =>
        `${oneLine(r.kind)}${r.id ? ` ID:${oneLine(r.id)}` : ''}: ${oneLine(r.status)}${r.stateLabel ? `/${oneLine(r.stateLabel)}` : ''}${r.reason || r.error ? ` 理由: ${oneLine(r.reason || r.error)}` : ''}${r.caseKind === 'ops_watch' && r.key?.startsWith('ops-watch:') ? ` 通知ID: ${oneLine(r.key.slice(10))}` : ''}`,
    )
    .join('、');
  const stages =
    entry.automatic
      .map(
        (a) =>
          `${oneLine(a.stateLabel) || '不明'}（${oneLine(a.state) || '不明'}） 最後: ${a.lastAction || '不明'} ${a.updatedAt || '時刻不明'}`,
      )
      .join('、') || 'なし';
  return `${entry.stale ? '[止まっている] ' : ''}${oneLine(target)} ${oneLine(entry.title)} ${states} 自動処理の段: ${stages} 最後の動き: ${entry.lastActivityAt || '時刻不明'}（${entry.elapsedMinutes === null ? '経過不明' : `${Math.floor(entry.elapsedMinutes)}分経過`}）`;
}
