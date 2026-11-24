import type {
  UsageLimits,
  FixedTestReport,
  UsageArrival,
} from './usage-detail-types.js';
import type {
  ReworkReport,
  ImplementationReport,
  ReviewNotesReport,
  SelectionReport,
  StallReport,
} from './execution-stats-types.js';
import type {
  AntigravityReport,
  IssueBodyReport,
  ScopeReport,
  WorkTypeReport,
  InventoryReport,
} from './comparison-stats-types.js';
import type { PriorityReport } from './priority-stats-types.js';
import type {
  WorkPeriod,
  UsagePeriod,
  UsageTotal,
  DatePeriod,
} from './stats-types.js';
import type { DraftIssue } from './draft-types.js';
import type { TierRecord, ModelOverride } from './tier-types.js';
export interface FollowupPeriod extends DatePeriod {
  counts?: Record<string, number>;
  outcomes?: Record<string, number>;
  pending?: {
    items: number;
    records: number;
    projects: { project: string; items: number; records: number }[];
  };
}
export interface FollowupRow extends DraftIssue {
  acceptedAt?: string;
  kind: string;
  source?: string;
  periods?: string[];
  outcome: string;
  instructions?: number;
}
export interface TierOutcomeRow extends TierRecord {
  createdAt?: string;
  project?: string;
  start?: TierRecord;
  overrides?: ModelOverride[];
  issue?: DraftIssue & { plan?: number; investigation?: boolean };
  outcome: string;
  statusReason?: string;
  instructions?: number;
  turns?: number;
  usage?: UsageTotal;
  combined?: UsageTotal & {
    includesConsultation?: boolean;
    consultationShared: number;
  };
  issueUsage?: { steps: UsageTotal[] };
  implementation?: TierRecord;
  implementationDecision?: TierRecord;
}
export interface TierRateGroup {
  count: number;
  passed: number;
  passedRate: number;
  outcomeUnknown: number;
  judged: number;
  cheapPassed: number;
  cheapRate: number;
  unknown: number;
  level?: string;
  model?: string;
  provider?: string;
}
export interface TierRatePeriod extends DatePeriod {
  medians: { cost?: number; tokens?: number };
  total: TierRateGroup;
  levels?: TierRateGroup[];
  consultModels?: TierRateGroup[];
  implementations?: TierRateGroup[];
}
export interface TierReport {
  rows?: TierOutcomeRow[];
  total: number;
  omitted?: number;
  unavailable?: string[];
  rates?: { before?: TierRatePeriod; after?: TierRatePeriod };
}
export interface ReviewMeasure {
  count: number;
  value?: number;
  unknown: number;
}
interface ReviewComparisonPeriod {
  from: string;
  to: string;
  days: number;
}
interface ReviewVerdict {
  result: string;
  reason: string;
  providers?: Record<string, ReviewVerdict>;
}
export interface ReviewExtra {
  result: string;
  reason?: string;
  unavailable?: string[];
  before?: Record<string, number | null>;
  after?: Record<string, number | null>;
}
export interface UsageReviewRound {
  round: number;
  startDate?: string;
  investigationIssue?: DraftIssue;
  treatmentIssues: (DraftIssue & { mergedAt?: string; closedAt?: string })[];
  cost?: Record<string, ReviewMeasure>;
  verdict: ReviewVerdict;
  acknowledgedAt?: string;
  note?: string;
  comparison?: {
    before: ReviewComparisonPeriod;
    after: ReviewComparisonPeriod;
    providers?: Record<
      string,
      {
        before: { perDay: number; count: number };
        after: { perDay: number; count: number };
        diffPerDay: number;
        saved: number;
      }
    >;
  };
  speed?: ReviewExtra;
  tiers?: ReviewExtra;
}
export interface StatsReport {
  usageLimits?: UsageLimits;
  fixedTestUsage?: FixedTestReport;
  rework?: ReworkReport;
  implementation?: ImplementationReport | null;
  reviewNotes?: ReviewNotesReport;
  testSelection?: SelectionReport;
  stalls?: StallReport;
  antigravity?: AntigravityReport;
  issueBody?: IssueBodyReport;
  scope?: ScopeReport;
  workTypes?: WorkTypeReport;
  inventory?: InventoryReport;
  priorityReview?: PriorityReport;
  project: string;
  before: WorkPeriod;
  after: WorkPeriod;
  coverage?: { start: string; end: string };
  worker?: boolean;
  stoppedSince?: string;
  unavailable: string[];
  usage?: {
    before?: UsagePeriod;
    after?: UsagePeriod;
    reason?: string;
    skipped?: number;
    coverage?: UsageArrival;
  };
  followups?: {
    before?: FollowupPeriod | null;
    after?: FollowupPeriod | null;
    unavailable?: string[];
    rows?: FollowupRow[];
    total: number;
    omitted?: number;
  };
  tiers?: TierReport;
  usageReview?: { rounds?: UsageReviewRound[]; reason?: string };
}
