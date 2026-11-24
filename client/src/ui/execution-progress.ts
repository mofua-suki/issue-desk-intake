import { el } from './dom.js';
export interface ExecutionProgress {
  unavailable?: string;
  planSHA?: string;
  head?: string;
  generation?: number;
  pending?: number;
  checks?: {
    id: string;
    criterion: string;
    owner: string;
    phase: string;
    status: string;
    reason: string;
    method?: string;
    expected?: string;
    evidenceKind?: string;
    capabilities?: string[];
    inputs?: string[];
  }[];
}
export function executionProgressView(progress?: ExecutionProgress) {
  const box = el('section', { 'aria-label': '受入条件の完了状況' });
  if (!progress) return box;
  if (progress.unavailable) {
    box.append(el('p', { text: progress.unavailable }));
    return box;
  }
  box.append(el('h3', { text: '受入条件の完了状況' }));
  const owners: Record<string, string> = {
    implementer: '実装担当',
    controller: 'コントローラー',
    reporter: '報告担当',
  };
  for (const check of progress.checks ?? []) {
    const state =
      check.status === 'passed'
        ? '確認済み'
        : check.status === 'failed'
          ? '不合格'
          : '未確認';
    box.append(
      el('p', {
        text: `${state}：${check.criterion}（${owners[check.owner] ?? '担当未確認'}）${check.reason ? `／${check.reason}` : ''}`,
      }),
    );
    if (check.method)
      box.append(
        el('p', {
          text: `確認：${check.method} ／ 期待：${check.expected ?? ''}`,
        }),
      );
  }
  return box;
}
