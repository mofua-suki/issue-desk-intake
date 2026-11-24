import type { ExecutionProgress } from './execution-progress.js';
import type { PublishedPriority } from './sort-types.js';
import type { UIError } from './models.js';

export interface ImageDescriptor {
  id: string;
  name?: string;
  mime?: string;
  width?: number;
  height?: number;
  size?: number;
  url?: string;
}

export interface UIMessage {
  id?: string | number;
  kind: string;
  body: string;
  createdAt: string;
  mine?: boolean;
  author?: string;
  images?: ImageDescriptor[];
}

export interface IssueScope {
  project: string;
  number: number;
}
export interface PriorityRequest {
  reason?: string;
  pc?: string;
  via?: string;
}
export interface ProjectWait {
  reason: string;
  condition: string;
  holders: {
    project: string;
    issue: number;
    host: string;
    pr?: number;
    note?: string;
  }[];
}

interface PermissionApprovalTarget {
  unavailable?: string | null;
  head?: string;
  pr?: number;
  files: string[];
}

interface DeletionItem {
  kind: string;
  from?: string;
  path: string;
  identical?: boolean;
}

interface DeletionApproval {
  head: string;
  items: DeletionItem[];
  total: number;
}

interface OverLimitApproval {
  statement: string;
  at: string;
  sourcePc: string;
  id: string;
  implementationRuns: number;
  limit: number;
  kind?: 'usage' | 'run_and_usage';
  usageLimit?: number;
  usageReason?: string;
  grantId?: string;
  grantQuote?: string;
  pass?: number;
}

export interface Issue extends IssueScope {
  executionProgress?: ExecutionProgress;
  origin?: { sources: { desk?: number | null }[] };
  desk?: number;
  title: string;
  body: string;
  status: string;
  createdAt: string;
  updatedAt: string;
  actions?: string[];
  images?: ImageDescriptor[];
  comments: UIMessage[];
  latestAsk?: UIMessage | null;
  commentCount?: number;
  contentRevision?: string | null;
  bodyExcerpt?: string;
  shownBody?: string;
  shownTitle?: string;
  closeReason?: { reason: string; source: string } | null;
  closedAt?: string | null;
  finalApprovalTransfers?: {
    id: string;
    fromHead: string;
    toHead: string;
    diffFingerprint: string;
    at: string;
  }[];
  pendingFinalApprovals?: {
    id: string;
    headSha: string;
    reason: string;
    fingerprint: string;
    target?: string;
    operation?: string;
    condition?: string;
    scope?: string[];
  }[];
  finalApprovalsUnavailable?: string;
  permissionApproval?: PermissionApprovalTarget | null;
  deletionApproval?: DeletionApproval | null;
  dependencyReason?: string;
  projectWait?: ProjectWait | null;
  priority?: PublishedPriority | null;
  priorityRequest?: PriorityRequest | null;
  passedOver?: boolean;
  followup?: boolean;
  investigation?: boolean;
  error?: string | null;
  hasResult?: boolean;
  host?: string | null;
  intakeId?: string;
  overLimitApprovals?: OverLimitApproval[];
  pr?: { number: number; url: string } | null;
  transferPending?: boolean;
  url: string;
  workTypeLabel?: string;
}

export interface IssueFullState {
  key: string | null;
  loading: boolean;
  error: UIError | null;
  detail: Issue | null;
  commentCount: number | null;
  status: string;
  revision: string | null;
}
