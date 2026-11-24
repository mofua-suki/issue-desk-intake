import type { DatePeriod, UsageTotal } from './stats-types.js';
import type { DraftIssue } from './draft-types.js';
export interface PerIssueMetric {
  total?: number | null;
  perIssue?: number | null;
  known?: number;
  unknown?: number;
}
export interface ReturnRate {
  reason?: string;
  since?: string;
  reviewed: number;
  returned: number;
  rate: number;
}
export interface AntigravityGroup {
  count: number;
  oneShot: { known: number; passed: number; rate: number; unknown: number };
  returns: PerIssueMetric;
  switches: number;
  testFailures: PerIssueMetric;
  tokens?: PerIssueMetric;
}
export interface AntigravityReport {
  days: number;
  from: string;
  to: string;
  reason?: string;
  unavailable?: string[];
  groups?: { antigravity: AntigravityGroup; claude: AntigravityGroup };
  excluded?: { other: number };
}
interface Distribution {
  median: number;
  p90: number;
  max: number;
}
export interface IssueBodyGroup {
  count: number;
  reason?: string;
  bodyChars: Distribution;
  passedChars?: Distribution;
  referenceChars?: Distribution;
  usage?: Record<string, Record<string, PerIssueMetric>>;
  returns?: ReturnRate;
  intakeOutput?: PerIssueMetric;
}
export interface IssueBodyReport {
  days: number;
  from: string;
  to: string;
  reason?: string;
  unavailable?: string[];
  notes?: string[];
  groups?: { before: IssueBodyGroup; after: IssueBodyGroup };
}
export interface ScopeGroup {
  state: string;
  route: string;
  reason?: string;
  stateName?: string;
  routeName?: string;
  count: number;
}
interface ScopeFiled {
  reason?: string;
  since?: string;
  total: number;
  states: Record<string, number>;
  groups?: ScopeGroup[];
}
export interface ScopeCheck {
  reason?: string;
  since?: string;
  checks: number;
  withOutside?: number;
  outside?: number;
  unreadable?: number;
}
export interface ScopePeriod extends DatePeriod {
  filed?: ScopeFiled;
  checks?: Record<string, ScopeCheck>;
}
export interface ScopeReport {
  before: ScopePeriod;
  after: ScopePeriod;
  skipped?: { filed?: number };
}
export interface WorkTypeRate {
  rate: number | null;
  count: number;
  denominator: number;
  unknown: number;
}
export interface WorkTypeGroup {
  typeName?: string;
  sizeName?: string;
  count: number | null;
  usageIssues: number | null;
  missingUsage: number | null;
  unknownTriggers: number | null;
  reviewReturns?: WorkTypeRate;
  retries?: WorkTypeRate;
  retests?: WorkTypeRate;
  perIssue?: UsageTotal;
}
export interface WorkTypePeriod extends DatePeriod {
  reason?: string;
  groups: WorkTypeGroup[];
  types: WorkTypeGroup[];
  sizes: WorkTypeGroup[];
}
export interface WorkTypeReport {
  before?: WorkTypePeriod;
  after?: WorkTypePeriod;
  unavailable?: string[];
  skipped?: { jobs: number; usage: number };
}
export interface InventoryProject {
  project: string;
  investigationIssue?: DraftIssue;
  status: string;
  pending?: number;
  examined?: number;
  closed?: { resolved?: number; duplicate?: number };
  escalated?: number;
  kept?: number;
  undetermined?: number;
}
export interface InventoryRound {
  round: number;
  week?: string;
  projects?: InventoryProject[];
}
export interface InventoryReport {
  rounds?: InventoryRound[];
  reason?: string;
}
