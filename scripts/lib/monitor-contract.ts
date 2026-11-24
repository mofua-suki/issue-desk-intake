// Session tools remain owned by the calling AI; this contract grants no execution authority.
export type Capability = {
  tool: string;
  specification: string;
  checkedAt: number;
  available: boolean;
  kind: string;
  destination: string;
};
export type MonitorConfig = {
  targets: { id: string; conditions: string[] }[];
  predicate: 'all';
  mode: 'periodic' | 'conditional';
  observeMs: number;
  reportMs: number;
  observer: Capability;
  reporter: Capability;
  end: { description: string; onSatisfied: boolean; at?: number };
  cancellation: string;
  continuity: {
    scope: 'session';
    constraint: string;
    registration?: {
      id: string;
      tool: string;
      evidence: string;
      accepted: boolean;
    };
  };
  action?: {
    description: string;
    authorization: string;
    capability: Capability;
  };
  handler?: Capability;
};
type Blocked = {
  actor: 'user' | 'ai' | 'system';
  reason: string;
  action: string;
};
type Sample = {
  target: string;
  observedAt: number;
  values: Record<string, boolean | null>;
  expectedTargets?: string[];
  blocked?: Blocked;
};
export type Observation = { samples: Sample[]; error?: string };
export type Request = {
  id: string;
  kind: 'observe' | 'report' | 'action' | 'handle';
  tool: string;
  destination: string;
  issuedAt: number;
  payload: unknown;
};
export type Receipt = {
  requestId: string;
  tool: string;
  destination: string;
  callId: string;
  evidence: string;
  outcome: 'failed' | 'accepted' | 'confirmed';
  observation?: Observation;
};
export type MonitorState = {
  version: 1;
  runId: string;
  config: MonitorConfig;
  status: 'active' | 'unavailable' | 'cancelled' | 'completed';
  limitations: string[];
  sequence: number;
  pending?: Request;
  nextCheckAt: number;
  nextReportAt: number;
  evaluation: 'unobserved' | 'pending' | 'satisfied' | 'read-error';
  observation?: Observation;
  reportDue: boolean;
  notification: 'unsent' | 'failed' | 'accepted' | 'confirmed';
  action: 'unstarted' | 'accepted' | 'failed' | 'confirmed';
  history: { request: Request; receipt: Receipt }[];
  stoppedReason?: string;
  blockedQueue?: {
    target: string;
    blocked: Blocked;
    evidence: string;
    key: string;
  }[];
  blockedNotified?: Record<string, number>;
  blockedReports?: string[];
};

const text = (value: unknown) =>
  typeof value === 'string' && value.trim().length > 0;
const positive = (value: unknown) =>
  Number.isSafeInteger(value) && Number(value) > 0;

export function capabilityProblems(
  c: Capability,
  now: number,
  report = false,
): string[] {
  if (
    !c ||
    !text(c.tool) ||
    !text(c.specification) ||
    !text(c.destination) ||
    !Number.isFinite(c.checkedAt) ||
    c.checkedAt > now ||
    now - c.checkedAt > 600_000 ||
    c.available !== true
  )
    return [
      'tool availability/specification/destination not verified in this session',
    ];
  if (report && !['user-conversation', 'user-notification'].includes(c.kind))
    return [
      `${c.kind}: no user-facing delivery route (stdout/files/attention/internal messages are insufficient)`,
    ];
  return [];
}

export function monitorConfig(input: MonitorConfig): MonitorConfig {
  const c = structuredClone(input);
  if (
    !c ||
    !Array.isArray(c.targets) ||
    !c.targets.length ||
    c.predicate !== 'all' ||
    !['periodic', 'conditional'].includes(c.mode) ||
    c.targets.some(
      (t) =>
        !text(t.id) ||
        !Array.isArray(t.conditions) ||
        !t.conditions.length ||
        t.conditions.some((k) => !text(k)) ||
        new Set(t.conditions).size !== t.conditions.length,
    ) ||
    new Set(c.targets.map((t) => t.id)).size !== c.targets.length
  )
    throw Error(
      'Explicit expected targets, conditions and the all predicate are required',
    );
  c.observeMs ??= 120_000;
  c.reportMs ??= 600_000;
  if (
    !positive(c.observeMs) ||
    !positive(c.reportMs) ||
    !text(c.end?.description) ||
    typeof c.end.onSatisfied !== 'boolean' ||
    (c.end.at !== undefined && !Number.isFinite(c.end.at)) ||
    !text(c.cancellation) ||
    c.continuity?.scope !== 'session' ||
    !text(c.continuity.constraint)
  )
    throw Error(
      'Invalid cadence/end/cancellation; only current-session continuity is implemented',
    );
  if (
    c.action &&
    (!text(c.action.description) || !text(c.action.authorization))
  )
    throw Error(
      'An action requires an existing explicit authorization and description',
    );
  return c;
}

export function evaluate(
  c: MonitorConfig,
  observation: Observation,
  now: number,
  previous: Observation[] = [],
): MonitorState['evaluation'] {
  if (observation?.error || !Array.isArray(observation?.samples))
    return 'read-error';
  const samples = observation.samples;
  if (
    samples.some((s) => !s || !text(s.target)) ||
    new Set(samples.map((s) => s.target)).size !== samples.length
  )
    return 'read-error';
  // An observation may discover additional expected children, but cannot shrink the declared manifest.
  const expected = new Set(c.targets.map((t) => t.id));
  // Valid discoveries remain binding even when a later observation omits its child manifest.
  for (const prior of previous) {
    for (const sample of Array.isArray(prior?.samples) ? prior.samples : []) {
      for (const id of Array.isArray(sample?.expectedTargets)
        ? sample.expectedTargets
        : []) {
        if (text(id)) expected.add(id);
      }
    }
  }
  for (const s of samples) {
    if (
      s.blocked !== undefined &&
      (!s.blocked ||
        typeof s.blocked !== 'object' ||
        Array.isArray(s.blocked) ||
        !['user', 'ai', 'system'].includes(s.blocked.actor) ||
        !text(s.blocked.reason) ||
        !text(s.blocked.action))
    )
      return 'read-error';
    if (
      s.expectedTargets !== undefined &&
      (!Array.isArray(s.expectedTargets) ||
        s.expectedTargets.some((id) => !text(id)))
    )
      return 'read-error';
    for (const id of s.expectedTargets ?? []) expected.add(id);
  }
  if ([...expected].some((id) => !c.targets.some((t) => t.id === id)))
    return 'read-error';
  let satisfied = true;
  for (const target of c.targets) {
    const s = samples.find((s) => s.target === target.id);
    if (
      !s ||
      !Number.isFinite(s.observedAt) ||
      s.observedAt > now ||
      now - s.observedAt >= c.observeMs ||
      !s.values ||
      target.conditions.some((k) => typeof s.values[k] !== 'boolean')
    )
      return 'read-error';
    if (s.blocked || target.conditions.some((k) => s.values[k] !== true))
      satisfied = false;
  }
  return satisfied ? 'satisfied' : 'pending';
}
