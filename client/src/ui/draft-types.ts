import type { TierRecord, ModelOverride } from './tier-types.js';
import type { AcceptedMessage } from './composer-types.js';
import type { PriorityRequest, ImageDescriptor } from './issue-types.js';

export interface DraftIssue {
  project?: string;
  number: number;
  desk?: number;
  title?: string;
  status?: string;
  reason?: string;
}
export interface ReferenceNote {
  resolution?: string;
  desk?: number;
  project?: string;
  number?: number;
  title?: string;
  reason?: string;
}
export interface ProxyEvidence {
  kind: string;
  source?: string;
  location?: string;
  detail?: string;
}
interface ProxyAnswer {
  question: string;
  answer: string;
  evidence?: ProxyEvidence[];
}
export interface DraftMessage extends AcceptedMessage {
  role: string;
  completionProposal?: string;
  completionPlan?: Record<string, unknown>;
  answers?: ProxyAnswer[];
  references?: ReferenceNote[];
  source?: DraftIssue;
}
export interface SplitPlan {
  project?: string;
  title: string;
  purpose: string;
  acceptance: string[];
  dependsOn?: number[];
}
export interface FamilyProgress {
  total: number;
  completed: number;
  closed: number;
  remaining: number;
  unknown?: number;
  stopped: {
    project: string;
    number?: number;
    desk?: number | null;
    status: string;
    reason: string;
  }[];
}
export interface Draft {
  completionPlan?: Record<string, unknown> | null;
  completionApproval?: Record<string, unknown> | null;
  completionComparison?: Record<string, unknown> | null;
  completionReconfirmation?: string | null;
  completionReconfirmationAfter?: number | null;
  familyProgress?: FamilyProgress;
  splitChildren?: string[];
  parentSplit?: string;
  parentConsultation?: string;
  consultationChildren?: string[];
  hasChildren?: boolean;
  id: string;
  project: string | null;
  unassigned?: boolean;
  status: string;
  body: string;
  title?: string;
  revision?: string;
  messages: DraftMessage[];
  issue?: DraftIssue;
  issues?: DraftIssue[];
  actions?: string[];
  attempts?: number;
  retryAt?: string;
  queued?: boolean;
  // 受付AIのターンを実行中か、順番待ち・自動の再試行の予約中か（受付が応答のたびに決める）。
  executionActive?: boolean;
  executionScheduled?: boolean;
  turnStopped?: boolean;
  error?: string;
  // 旧記録はサーバーが読取時に判定し、判定不能なら省略する。再開時に消去される。
  stallCause?: 'explicit' | 'timeout';
  lastFailure?: {
    message: string;
    at: string;
    stage?: string;
    cause?: string;
    kind?: string;
  } | null;
  stalled?: {
    since: string;
    sinceSource?: string;
    stoppedAt: string;
    lastFailure?: { message: string; at: string } | null;
  } | null;
  stallHistory?: unknown[];
  waitStartedAt?: string;
  waitSource?: string;
  provider?: string;
  implementationTier?: TierRecord;
  priority?: string;
  priorityChangeable?: boolean;
  prioritySource?: PriorityRequest | null;
  questionReasons?: { question: string; reason: string }[];
  finishingTurn?: boolean;
  filingQueue?: {
    stuck: boolean;
    inactiveMinutes: number;
    ahead: number;
  } | null;
  projectHandoff?: { id: string; filing?: string } | null;
  consultationBudget?: { stopped?: unknown; stops?: unknown[] } | null;
  crossProposal?: {
    reason: string;
    approved?: boolean;
    scopes: { project: string; scope: string | string[] }[];
  } | null;
  splitIssues?: SplitPlan[];
  investigations?: DraftIssue[];
  imageReviews?: {
    id: string;
    screen: string;
    location: string;
    change: string;
    uncertainty?: string;
  }[];
  tier?: TierRecord;
  modelOverrides?: ModelOverride[];
  images?: ImageDescriptor[];
}
