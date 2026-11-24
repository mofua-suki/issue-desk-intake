import type { SortableRow, RowSort } from './sort-types.js';
// 一覧の絞り込み・並べ替え（Issue #1488 で client/src/ui/request-list.js から分けた）。client/src/ui/request-list.js が名前を
// そのまま再エクスポートする。このファイルは client/src/ui/request-list.js を import しない。

// 進行中・フォローアップタブの並び替え。取得済みの行を画面内で並べ直すだけで、分類・件数・取得には触れない。
export const sortFields = ['project', 'updated', 'status', 'priority'];
export const defaultSort = { field: 'updated', direction: 'desc' };
// 処理の流れ順（相談・起票→受付・待機→作業→PR作成→レビュー→反映）。待機状態どうしはこの順で固定する。
export const statusOrder = [
  'ai_queued',
  'ai_recovering',
  'thinking',
  'auto_ready',
  'submitting',
  // 調査Issueを起票し、その報告を待っている相談（Issue #409）。
  'investigation_waiting',
  'pending',
  'remote',
  'dependency_waiting',
  'project_waiting',
  'reply_pending',
  // 再試行待ちも担当PCの取り込みを待つ段階。反映待ちと並べ、受付済み（取り込み後）より前に置く。
  'retry_pending',
  'queued',
  'running',
  'publishing',
  'scope_waiting',
  'releasing',
  'deploying',
];
// 「完了」「クローズ済み」タブの絞り込み。取得済みの行を画面内で絞るだけで、分類・件数・取得には触れない。
export const kindFilters = ['all', 'ordinary', 'followup'];
export const defaultKindFilter = 'all';
export const validKindFilter = (value: unknown) =>
  typeof value === 'string' && kindFilters.includes(value)
    ? value
    : defaultKindFilter;
export function filterRows<T extends SortableRow>(rows: T[], filter: string) {
  const kind = validKindFilter(filter);
  return kind === 'all'
    ? [...rows]
    : rows.filter((r) => (r.followup === true) === (kind === 'followup'));
}
// 同じ項目なら昇降を切り替え、別の項目を選んだ最初の操作は昇順にする。
export function nextSort(sort: RowSort, field: string) {
  if (!sortFields.includes(field)) return validSort(sort);
  const current = validSort(sort);
  if (current.field !== field) return { field, direction: 'asc' };
  return { field, direction: current.direction === 'asc' ? 'desc' : 'asc' };
}
export function validSort(sort: RowSort) {
  return sortFields.includes(sort?.field) &&
    ['asc', 'desc'].includes(sort?.direction)
    ? { field: sort.field, direction: sort.direction }
    : { ...defaultSort };
}
const text = (a: unknown, b: unknown) => {
  const x = String(a ?? ''),
    y = String(b ?? '');
  const lx = x.toLowerCase(),
    ly = y.toLowerCase();
  if (lx !== ly) return lx < ly ? -1 : 1;
  return x < y ? -1 : x > y ? 1 : 0;
};
const issueNumber = (row: SortableRow) => {
  const n = Number(row.number);
  return row.number !== null &&
    row.number !== undefined &&
    row.number !== '' &&
    Number.isSafeInteger(n) &&
    n > 0
    ? n
    : null;
};
const updatedTime = (row: SortableRow) => {
  if (typeof row.updatedAt !== 'string' && typeof row.updatedAt !== 'number')
    return null;
  const t = new Date(row.updatedAt).getTime();
  return Number.isFinite(t) ? t : null;
};
const statusRank = (row: SortableRow) => {
  const n = statusOrder.indexOf(row.status ?? '');
  return n < 0 ? null : n;
};
// 優先度の順位（小さいほど高い）。受付サービスが実行待ちの間だけ付ける priority（shared/src/published-state.mjs の
// priorityView）で決める。割り込み＋AIの判定「優先」→割り込み（併記の判定が通常・判定失敗・未判定）→
// 優先（AIの判定）→通常（利用者の指定・AIの判定・判定失敗）。priority の無い行（未判定・実装中以降）と
// 相談・クローズ確認の行（下書きの priority は起票時の指定の文字列で、実行待ちの値ではない）は null。
// 値ごとに指定元も確かめる。「優先」は利用者の指定（割り込み）かAIの判定だけで、判定失敗は「通常」にしかならない。
// それ以外の組み合わせ（想定外の値）はAIの「優先」などと取り違えず、優先度なしと同じ扱いにする。
const prioritySources = {
  high: ['user', 'ai'],
  normal: ['user', 'ai', 'failed'],
};
export const priorityRank = (row: SortableRow) => {
  const p = row.kind === 'draft' || row.kind === 'close' ? null : row.priority;
  if (
    !p ||
    typeof p !== 'object' ||
    (p.level !== 'high' && p.level !== 'normal')
  )
    return null;
  if (
    typeof p.source !== 'string' ||
    !prioritySources[p.level].includes(p.source)
  )
    return null;
  if (p.source === 'user' && p.level === 'high')
    return p.judged?.level === 'high' ? 0 : 1;
  return p.level === 'high' ? 2 : 3;
};
// 欠けた値（番号のない相談・不正な日時・未知の状態）は、昇順でも降順でも末尾に置く。
function missingLast(x: number | null, y: number | null, sign: number) {
  if (x === null && y === null) return 0;
  if (x === null) return 1;
  if (y === null) return -1;
  return x === y ? 0 : (x < y ? -1 : 1) * sign;
}
// 主比較が同じ行は、プロジェクト名・Issue番号・相談IDの昇順で固定する。取得順が変わっても揺れない。
function tieBreak(a: SortableRow, b: SortableRow) {
  return (
    text(a.project, b.project) ||
    missingLast(issueNumber(a), issueNumber(b), 1) ||
    text(a.id ?? a.intakeId, b.id ?? b.intakeId) ||
    text(a.kind, b.kind)
  );
}
export function sortRows<T extends SortableRow>(rows: T[], sort: RowSort) {
  const { field, direction } = validSort(sort);
  const sign = direction === 'asc' ? 1 : -1;
  const primary =
    field === 'project'
      ? (a: SortableRow, b: SortableRow) =>
          text(a.project, b.project) * sign ||
          missingLast(issueNumber(a), issueNumber(b), sign)
      : field === 'updated'
        ? (a: SortableRow, b: SortableRow) =>
            missingLast(updatedTime(a), updatedTime(b), sign)
        : field === 'status'
          ? (a: SortableRow, b: SortableRow) =>
              missingLast(statusRank(a), statusRank(b), sign)
          : // 昇順（最初の操作）が「高い順」。順位は小さいほど高い。
            (a: SortableRow, b: SortableRow) =>
              missingLast(priorityRank(a), priorityRank(b), sign);
  return [...rows].sort((a, b) => primary(a, b) || tieBreak(a, b));
}
