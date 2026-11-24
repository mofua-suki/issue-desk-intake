/** Select only explicitly received targets from a classified work response. */
export type WatchTarget =
  | { type: 'draft'; id: string; received: boolean }
  | {
      type: 'issue';
      desk: number;
      project?: string;
      number?: number;
      received: boolean;
    }
  | {
      type: 'issue';
      desk?: number;
      project: string;
      number: number;
      received: boolean;
    };

type Work = {
  state: string;
  reason: string;
  nextActor: string;
  nextAction: string;
  waitCondition?: string;
  resumeBy?: string;
};
type Row = {
  kind: string;
  id?: string;
  project?: string;
  number?: number;
  desk?: number | null;
  work: Work;
};
type Group = 'attention' | 'running' | 'waiting' | 'stopped';
type WorkResponse = {
  partial: boolean;
  unavailable: string[];
  unreadProjects?: { project: string; reason: string }[];
  groups: Record<Group, Row[]>;
};
type WatchResult =
  | { target: WatchTarget; status: 'confirmed'; group: Group; work: Work }
  | { target: WatchTarget; status: 'unconfirmed'; reason: string };

const groups: Group[] = ['attention', 'running', 'waiting', 'stopped'];
const positive = (n: unknown): n is number =>
  Number.isSafeInteger(n) && (n as number) > 0;

function matches(target: WatchTarget, row: Row): boolean {
  if (target.type === 'draft')
    return row.kind === 'draft' && row.id === target.id;
  if (row.kind !== 'issue') return false;
  return (
    (target.desk === undefined || row.desk === target.desk) &&
    (target.project === undefined || row.project === target.project) &&
    (target.number === undefined || row.number === target.number)
  );
}

/** Missing or ambiguous evidence never implies completion. No input is modified. */
export function classifyWatchTargets(
  targets: readonly WatchTarget[],
  response: WorkResponse,
): WatchResult[] {
  const complete =
    response?.partial === false &&
    Array.isArray(response.unavailable) &&
    response.unavailable.length === 0 &&
    groups.every((group) => Array.isArray(response.groups?.[group]));
  return targets.map((target): WatchResult => {
    const unknown = (reason: string): WatchResult => ({
      target,
      status: 'unconfirmed',
      reason,
    });
    if (!target.received) return unknown('not_received');
    if (
      target.type === 'draft'
        ? typeof target.id !== 'string' || !target.id.trim()
        : !(
            (positive(target.desk) &&
              (target.project === undefined || !!target.project.trim()) &&
              (target.number === undefined || positive(target.number))) ||
            (target.desk === undefined &&
              typeof target.project === 'string' &&
              !!target.project.trim() &&
              positive(target.number))
          )
    ) {
      return unknown('invalid_target');
    }
    if (!complete) return unknown('incomplete_response');
    if (
      target.type === 'issue' &&
      response.unreadProjects?.some(
        (p) => !target.project || p.project === target.project,
      )
    ) {
      return unknown('unread_project');
    }
    const found = groups.flatMap((group) =>
      response.groups[group]
        .filter((row) => matches(target, row))
        .map((row) => ({ group, row })),
    );
    if (found.length !== 1)
      return unknown(found.length ? 'ambiguous' : 'not_found');
    const { group, row } = found[0];
    const work = row.work;
    if (
      !work ||
      work.state !== group ||
      typeof work.reason !== 'string' ||
      typeof work.nextActor !== 'string' ||
      typeof work.nextAction !== 'string' ||
      (group === 'waiting' &&
        (typeof work.waitCondition !== 'string' ||
          typeof work.resumeBy !== 'string'))
    ) {
      return unknown('invalid_work');
    }
    return { target, status: 'confirmed', group, work: { ...work } };
  });
}
