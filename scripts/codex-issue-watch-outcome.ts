/** A read-only decision for a single received issue, without performing any operation. */
type OutcomeTarget = { desk: number; project: string; number: number };
/** The caller supplies a saved controller verification for this particular PR. */
type PullVerification = {
  project: string;
  number: number;
  merged: boolean;
};
type WatchOutcome = 'merged' | 'withdrawn' | 'continue' | 'unconfirmed';

type RecordValue = Record<string, unknown>;
const object = (value: unknown): RecordValue | undefined =>
  value !== null && typeof value === 'object' && !Array.isArray(value)
    ? (value as RecordValue)
    : undefined;
const positive = (value: unknown): value is number =>
  typeof value === 'number' && Number.isSafeInteger(value) && value > 0;

/** detail is the complete GET /api/issue response; null represents a failed read.
 * verification is a controller's saved PR lookup result, not a job result or prose.
 */
export function classifyWatchOutcome(
  target: OutcomeTarget,
  detail: unknown,
  verification?: PullVerification,
): WatchOutcome {
  const issue = object(detail);
  if (
    !positive(target?.desk) ||
    typeof target.project !== 'string' ||
    !target.project.trim() ||
    !positive(target.number) ||
    !issue ||
    issue.desk !== target.desk ||
    issue.project !== target.project ||
    issue.number !== target.number ||
    issue.view === 'summary' ||
    issue.partial === true ||
    typeof issue.body !== 'string' ||
    !Array.isArray(issue.comments)
  )
    return 'unconfirmed';
  if (issue.state === 'open') return 'continue';
  if (issue.state !== 'closed') return 'unconfirmed';

  const pr = object(issue.pr);
  const reason = object(issue.closeReason);
  const route = object(reason?.route);
  // A direct PR route is optional, but when present its identity must be known.
  if (
    reason?.route != null &&
    (!route ||
      typeof route.repo !== 'string' ||
      !route.repo.trim() ||
      !positive(route.pr))
  )
    return 'unconfirmed';
  const repo = route ? route.repo : target.project;
  const number = route ? route.pr : pr?.number;
  if (
    route &&
    (repo !== target.project || (positive(pr?.number) && number !== pr.number))
  )
    return 'unconfirmed';
  const checked =
    verification &&
    verification.project === repo &&
    verification.number === number &&
    typeof verification.merged === 'boolean'
      ? verification.merged
      : undefined;
  const lookup =
    !route && positive(pr?.number) && typeof pr?.merged === 'boolean'
      ? pr.merged
      : undefined;
  if (checked !== undefined && lookup !== undefined && checked !== lookup)
    return 'unconfirmed';
  const merged = checked ?? lookup;
  if (positive(number) && merged === true) return 'merged';
  const noPull =
    !route &&
    !verification &&
    (issue.pr == null || (pr && Object.keys(pr).length === 0));
  if (merged !== false && !noPull) return 'unconfirmed';
  if (
    reason?.reason === 'voluntary' &&
    (positive(pr?.number) ||
      (typeof issue.closedAt === 'string' &&
        Number.isFinite(Date.parse(issue.closedAt)))) &&
    (reason.source === 'manual' || reason.source === 'token')
  )
    return 'withdrawn';
  return 'unconfirmed';
}
