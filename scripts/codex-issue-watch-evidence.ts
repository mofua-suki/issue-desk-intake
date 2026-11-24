import type { WatchState } from './codex-issue-watch-state.ts';

type Identity = Pick<
  WatchState,
  'sendId' | 'conversationId' | 'targetId' | 'notificationId'
>;
type Response = WatchState['response'];

// The caller supplies a fresh, successfully read detail and the evidence tied to
// the delivered notification. Revisions and decision IDs must come from that
// detail; replyId is the ID actually submitted to POST /api/comment.
type Evidence = {
  identity: Identity;
  detail?: {
    targetId: string;
    project: string;
    number: number;
    revision?: string;
    evidenceIds?: string[];
    decisionIds?: string[];
  } | null;
  investigation?: { evidenceId: string; revision?: string } | null;
  operation?: {
    replyId: string;
    comment?: {
      id: string;
      project: string;
      number: number;
      status: string;
      result?: unknown;
    } | null;
  } | null;
  decisionId?: string | null;
};

const identifier = (value: unknown): value is string =>
  typeof value === 'string' &&
  /^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,255}$/.test(value);
const identityKeys = [
  'sendId',
  'conversationId',
  'targetId',
  'notificationId',
] as const;

/** Returns only a response accepted by saveWatchState; never copies API content. */
export function classifyWatchEvidence(
  identity: Identity,
  evidence: Evidence | null,
): Response {
  const pending: Response = { kind: 'pending' };
  if (
    !identity ||
    !evidence?.identity ||
    !identityKeys.every(
      (key) =>
        identifier(identity[key]) && identity[key] === evidence.identity[key],
    )
  )
    return pending;
  const detail = evidence.detail;
  if (
    !detail ||
    detail.targetId !== identity.targetId ||
    !identifier(detail.project) ||
    !Number.isSafeInteger(detail.number) ||
    detail.number < 1
  )
    return pending;

  const operation = evidence.operation;
  const comment = operation?.comment;
  if (
    operation &&
    identifier(operation.replyId) &&
    comment &&
    comment.id === operation.replyId &&
    comment.project === detail.project &&
    comment.number === detail.number &&
    comment.status === 'done' &&
    comment.result !== null &&
    typeof comment.result === 'object' &&
    !Array.isArray(comment.result)
  ) {
    return { kind: 'operation', evidenceId: operation.replyId };
  }
  if (
    identifier(evidence.decisionId) &&
    Array.isArray(detail.decisionIds) &&
    detail.decisionIds.includes(evidence.decisionId)
  ) {
    return { kind: 'waiting', decision: evidence.decisionId };
  }
  const investigation = evidence.investigation;
  if (
    investigation &&
    identifier(investigation.evidenceId) &&
    ((identifier(detail.revision) &&
      investigation.revision === detail.revision) ||
      (Array.isArray(detail.evidenceIds) &&
        detail.evidenceIds.includes(investigation.evidenceId)))
  ) {
    return { kind: 'investigated', evidenceId: investigation.evidenceId };
  }
  return pending;
}
