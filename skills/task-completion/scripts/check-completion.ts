import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

type Deliverable = {
  id: string;
  description: string;
  status: 'pending' | 'running' | 'verified' | 'blocked';
  evidence: string[];
  blocker?: {
    kind: 'user-only' | 'external';
    detail: string;
    evidence: string[];
    nextAction: string;
  };
};

type Ledger = {
  version: 1;
  objective: string;
  originalRequirements: string[];
  deliverables: Deliverable[];
  scopeChanges?: { removedIds: string[]; userQuote: string; source: string }[];
  agents: {
    id: string;
    assigned: string[];
    status: 'running' | 'completed' | 'failed';
    collected: boolean;
  }[];
  // open-ended: 改善・リファクタリング・見直しなど、完了の基準が依頼に無い依頼。
  kind?: 'open-ended';
  // 最後に行った、差分を知らない独立した見直しの結果。
  finalReview?: {
    evidence: string[];
    findings: string[];
    kept?: { finding: string; reason: string }[];
  };
};

type Result = {
  decision: 'complete' | 'continue' | 'needs-user';
  unfinished: string[];
  reasons: string[];
};

const nonempty = (value: unknown): value is string =>
  typeof value === 'string' && value.trim().length > 0;
const strings = (value: unknown): value is string[] =>
  Array.isArray(value) && value.every(nonempty);
const object = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value);

export function checkCompletion(input: unknown): Result {
  const reasons: string[] = [];
  const invalid = (reason: string): Result => ({
    decision: 'continue',
    unfinished: [],
    reasons: [reason],
  });
  if (!object(input) || input.version !== 1 || !nonempty(input.objective))
    return invalid('依頼全体の目的がありません。');
  if (
    !strings(input.originalRequirements) ||
    !input.originalRequirements.length
  )
    return invalid('最初の必須項目がありません。');
  if (
    new Set(input.originalRequirements).size !==
    input.originalRequirements.length
  )
    return invalid('最初の必須項目IDが重複しています。');
  if (!Array.isArray(input.deliverables) || !Array.isArray(input.agents))
    return invalid('作業項目か担当の記録がありません。');
  const deliverables: Deliverable[] = [];
  const ids = new Set<string>();
  for (const row of input.deliverables) {
    if (
      !object(row) ||
      !nonempty(row.id) ||
      !nonempty(row.description) ||
      !strings(row.evidence) ||
      !['pending', 'running', 'verified', 'blocked'].includes(
        String(row.status),
      )
    )
      return invalid('作業項目の形が不正です。');
    if (ids.has(row.id)) return invalid('作業項目IDが重複しています。');
    ids.add(row.id);
    deliverables.push(row as unknown as Deliverable);
  }
  const removed = new Set<string>();
  if (input.scopeChanges !== undefined && !Array.isArray(input.scopeChanges))
    return invalid('範囲変更の記録が不正です。');
  for (const change of (input.scopeChanges ?? []) as unknown[]) {
    if (
      !object(change) ||
      !strings(change.removedIds) ||
      !change.removedIds.length ||
      !nonempty(change.userQuote) ||
      !nonempty(change.source)
    )
      return invalid('範囲変更に利用者の原文と出典が必要です。');
    for (const id of change.removedIds) {
      if (!input.originalRequirements.includes(id))
        return invalid('範囲変更が最初の必須項目と一致しません。');
      removed.add(id);
    }
  }
  for (const id of input.originalRequirements)
    if (!ids.has(id) && !removed.has(id))
      reasons.push(`必須項目が消えています: ${id}`);
  for (const row of deliverables)
    if (row.status === 'verified' && !row.evidence.length)
      reasons.push(`完了の証跡がありません: ${row.id}`);
  const agents: Ledger['agents'] = [];
  const agentIDs = new Set<string>();
  for (const row of input.agents) {
    if (
      !object(row) ||
      !nonempty(row.id) ||
      !strings(row.assigned) ||
      typeof row.collected !== 'boolean' ||
      !['running', 'completed', 'failed'].includes(String(row.status))
    )
      return invalid('担当の記録が不正です。');
    if (agentIDs.has(row.id)) return invalid('担当IDが重複しています。');
    agentIDs.add(row.id);
    if (row.assigned.some((id) => !ids.has(id)))
      reasons.push(`担当の項目が作業台帳にありません: ${row.id}`);
    agents.push(row as unknown as Ledger['agents'][number]);
    if (row.status === 'running' || !row.collected)
      reasons.push(`担当の結果が未回収です: ${row.id}`);
    if (row.status === 'failed') {
      for (const id of row.assigned) {
        const task = deliverables.find((item) => item.id === id);
        if (task?.status === 'verified' && !task.evidence.length)
          reasons.push(`失敗した担当の項目を未検証で完了にしています: ${id}`);
      }
    }
  }
  if (input.kind !== undefined && input.kind !== 'open-ended')
    return invalid('依頼の種類が不正です。');
  if (input.kind === undefined && input.finalReview !== undefined)
    return invalid(
      '見直しの記録がある台帳には kind: "open-ended" を付けてください。',
    );
  if (input.kind === 'open-ended') {
    const review = input.finalReview;
    if (
      !object(review) ||
      !strings(review.evidence) ||
      !review.evidence.length ||
      !strings(review.findings)
    )
      reasons.push(
        '完了の基準が無い依頼で、最後の独立した見直しの記録がありません。',
      );
    else {
      if (review.findings.length)
        reasons.push(
          `最後の見直しで直すべき指摘が残っています: ${review.findings.join(' / ')}`,
        );
      if (
        review.kept !== undefined &&
        (!Array.isArray(review.kept) ||
          !review.kept.every(
            (row) =>
              object(row) && nonempty(row.finding) && nonempty(row.reason),
          ))
      )
        reasons.push('残すと決めた指摘には、指摘と理由が要ります。');
    }
  }
  const active = deliverables.filter((row) => !removed.has(row.id));
  const pending = active.filter((row) => row.status !== 'verified');
  const unfinished = pending.map((row) => row.id);
  if (reasons.length) return { decision: 'continue', unfinished, reasons };
  if (!pending.length) return { decision: 'complete', unfinished, reasons: [] };
  const needsUser = pending.every(
    (row) =>
      row.status === 'blocked' &&
      object(row.blocker) &&
      ['user-only', 'external'].includes(String(row.blocker.kind)) &&
      nonempty(row.blocker.detail) &&
      strings(row.blocker.evidence) &&
      row.blocker.evidence.length > 0 &&
      nonempty(row.blocker.nextAction),
  );
  if (needsUser)
    return {
      decision: 'needs-user',
      unfinished,
      reasons: ['残件は実証済みの阻害要因だけです。全体完了ではありません。'],
    };
  return {
    decision: 'continue',
    unfinished,
    reasons: ['未完了の許可済み作業を続けてください。'],
  };
}

function main(argv: string[]): number {
  if (argv.length !== 2 || argv[0] !== '--file' || !path.isAbsolute(argv[1])) {
    console.error(
      '使い方: node skills/task-completion/scripts/check-completion.ts --file <台帳の絶対パス>',
    );
    return 1;
  }
  let input: unknown;
  try {
    input = JSON.parse(fs.readFileSync(argv[1], 'utf8'));
  } catch {
    console.error('作業台帳を読み取れません。未完了として扱います。');
    return 1;
  }
  const result = checkCompletion(input);
  console.log(JSON.stringify(result));
  return result.decision === 'complete'
    ? 0
    : result.decision === 'needs-user'
      ? 2
      : 1;
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
)
  process.exitCode = main(process.argv.slice(2));
