import {
  draftPageLimit,
  readAllDraftPages,
} from '../../shared/src/desk-alert-reader.ts';

const failure = (category, stage, detail = '') =>
  Object.assign(new Error(), {
    duplicateFailure: true,
    category,
    stage,
    detail,
  });
const shapeMessage = 'duplicate-search: invalid page shape';
const changedMessage =
  '受付の一覧を読む途中でデータが変わり続け、安定して読めませんでした。';

function safeId(id) {
  if (id === undefined || id === null || id === '') return 'id欠落';
  if (
    (typeof id !== 'string' && typeof id !== 'number') ||
    (typeof id === 'number' && (!Number.isSafeInteger(id) || id < 0)) ||
    (typeof id === 'string' && !id.trim())
  )
    return 'id不正';
  const value = String(id);
  if (/bearer|:\/\/|@/i.test(value)) return 'id非表示';
  return `id=${JSON.stringify(value).slice(1, -1).slice(0, 80)}`;
}
function readFailure(error) {
  if (
    Number.isInteger(error?.status) &&
    error.status >= 100 &&
    error.status <= 599
  )
    return `HTTP ${error.status}`;
  if (
    error?.unreachable === true ||
    ['ECONNREFUSED', 'ECONNRESET', 'ENOTFOUND', 'ETIMEDOUT'].includes(
      error?.code,
    )
  )
    return '接続不能';
  if (
    error?.code === 'ERR_CHILD_PROCESS_STDIO_MAXBUFFER' ||
    /^(?:出力上限超過|応答サイズ上限超過|応答.*bytes で上限 \d+ bytes を超えました)/.test(
      error?.message || '',
    )
  )
    return 'サイズ上限';
  return '';
}

// 一覧を途中までしか読めない場合は、空の一覧ではなく未確認として返す。
export async function searchDuplicates(call, project, query) {
  const matching = (title) =>
    title
      .normalize('NFKC')
      .toLowerCase()
      .includes(query.normalize('NFKC').toLowerCase());
  const result = {};
  for (const [kind, route] of [
    ['issues', '/api/issues/search'],
    ['drafts', '/api/drafts'],
    ['todos', '/api/todos'],
  ]) {
    const read = async (url) => {
      try {
        return await call('GET', url);
      } catch (error) {
        if (error?.nasHandoff) throw error;
        throw failure('読取の失敗', '一覧読取', readFailure(error));
      }
    };
    try {
      let rows;
      if (kind === 'issues') {
        rows = [];
        const seen = new Set();
        let total;
        for (let page = 1; ; page++) {
          const params = new URLSearchParams({
            project,
            state: 'open',
            limit: '100',
            page: String(page),
          });
          const value = await read(`${route}?${params}`);
          if (
            !Array.isArray(value?.issues) ||
            !Number.isInteger(value.total) ||
            value.total < 0 ||
            typeof value.hasMore !== 'boolean'
          )
            throw failure('応答の形の不正', '一覧読取');
          // 読めなかった候補のリポジトリがあり行がそろっていない応答（incomplete）は、照合済みとしない。
          if (value.incomplete === true) throw failure('途中欠落', '一覧読取');
          if (
            (value.hasMore && !value.issues.length) ||
            (total !== undefined && total !== value.total)
          )
            throw failure('途中欠落', '一覧読取');
          total = value.total;
          for (const row of value.issues) {
            if (row?.number != null && seen.has(row.number))
              throw failure('途中欠落', '一覧読取');
            if (row?.number != null) seen.add(row.number);
            rows.push(row);
          }
          if (rows.length > total || (value.hasMore && rows.length >= total))
            throw failure('途中欠落', '一覧読取');
          if (!value.hasMore) {
            if (rows.length !== total) throw failure('途中欠落', '一覧読取');
            break;
          }
        }
      } else {
        try {
          rows = await readAllDraftPages(async (page) => {
            const value = await read(
              `${route}?${new URLSearchParams({ project, page: String(page), limit: String(draftPageLimit), ...(kind === 'drafts' ? { excludeStatus: 'submitted', view: 'summary' } : {}) })}`,
            );
            if (
              Array.isArray(value?.items) &&
              value.hasMore === true &&
              value.items.length === 0
            )
              throw failure('途中欠落', '一覧読取');
            return value;
          }, shapeMessage);
        } catch (error) {
          if (error?.message === shapeMessage)
            throw failure('応答の形の不正', '一覧読取');
          if (error?.message === changedMessage)
            throw failure('途中欠落', '一覧読取');
          throw error;
        }
      }
      const candidates = [];
      let skipped = 0;
      for (const row of rows) {
        if (
          kind === 'drafts' &&
          row?.project === null &&
          row.unassigned === true
        )
          continue;
        const id = kind === 'issues' ? row?.number : row?.id;
        const invalid = (field) => {
          throw failure('項目の不正', '項目検査', `${field} ${safeId(id)}`);
        };
        if (safeId(id) === 'id欠落' || safeId(id) === 'id不正')
          invalid(kind === 'issues' ? 'number' : 'id');
        if (!row || typeof row.project !== 'string' || !row.project.trim())
          invalid('project');
        if (row.project !== project) continue;
        const state =
          kind === 'issues'
            ? row.state
            : kind === 'drafts'
              ? row.status
              : row.status === undefined
                ? row.removedAt
                  ? 'removed'
                  : row.draftId
                    ? 'started'
                    : 'todo'
                : row.status;
        if (typeof state !== 'string' || !state.trim())
          invalid(kind === 'issues' ? 'state' : 'status');
        let title;
        if (kind === 'todos') {
          if (typeof row.body !== 'string') invalid('body');
          title =
            row.body
              .split('\n')
              .map((line) => line.trim())
              .find(Boolean) || '';
        } else {
          if (
            row.shownTitle !== undefined &&
            typeof row.shownTitle !== 'string'
          )
            invalid('shownTitle');
          title = row.shownTitle || row.title;
          if (typeof title !== 'string') invalid('title');
        }
        if (!title.trim() && kind !== 'issues') {
          skipped++;
          continue;
        }
        if (!title.trim()) invalid('title');
        if (
          (kind === 'issues' && state !== 'open') ||
          (kind === 'drafts' && state === 'submitted')
        )
          continue;
        if (matching(title)) candidates.push({ id, title, state });
      }
      result[kind] = { confirmed: true, candidates, skipped };
    } catch (error) {
      if (error?.nasHandoff) throw error;
      const e = error?.duplicateFailure
        ? error
        : failure('読取の失敗', '一覧読取');
      result[kind] = {
        confirmed: false,
        candidates: [],
        reason: `${kind} ${e.stage}: ${e.category}${e.detail ? ` (${e.detail})` : ''}`,
      };
    }
  }
  return result;
}
