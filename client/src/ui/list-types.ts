import type { ExecutionProgress } from './execution-progress.js';
import type { AlertDetail as importAlertDetail } from './attention-types.js';
import type {
  DraftIssue,
  DraftMessage,
  SplitPlan,
  FamilyProgress,
} from './draft-types.js';
import type { PublishedPriority } from './sort-types.js';
import type { ProjectWait, ImageDescriptor } from './issue-types.js';
import type { ListTab } from './models.js';
export interface RequestRow extends importAlertDetail {
  // 候補のリポジトリを読めず、前回読めた内容を表示している行。
  readFailed?: boolean;
  executionProgress?: ExecutionProgress;
  executionActive?: boolean;
  executionScheduled?: boolean;
  executionUntil?: string;
  work?: WorkState;
  relatedAlerts?: RequestRow[];
  requiresUser?: boolean;
  handoff?: boolean;
  recoveryState?: string;
  causeKey?: string;
  generation?: number;
  nextActor?: string;
  nextAction?: string;
  attentionClass?: 'decision' | 'handoff';
  attentionMembers?: RequestRow[];
  permissionApproval?: { head?: string; unavailable?: string };
  deletionApproval?: { head?: string };
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
  familyProgress?: FamilyProgress;
  cleanup?: unknown;
  shownTitle?: string;
  projectDefinition?: boolean;
  pc?: string;
  sourceKind?: string;
  proposalId?: string;
  proposalStatus?: string;
  postedAt?: string;
  checkedAt?: string;
  reason?: string;
  status: string;
  id?: string;
  project?: string | null;
  number?: number;
  desk?: number;
  kind?: string;
  state?: string;
  title?: string;
  body?: string;
  createdAt?: string;
  updatedAt?: string;
  startedAt?: string;
  startDate?: string;
  investigationIssue?: DraftIssue;
  detectedAt?: string;
  finishedAt?: string;
  repo?: string;
  followup?: boolean;
  intakeId?: string;
  proposal?: { kind: string };
  interruptDelay?: {
    minutes: number;
    behind?: string[];
    unchangedMinutes?: number;
  };
  turnStopped?: boolean;
  retryAt?: string;
  queued?: boolean;
  parentSplit?: string;
  parentConsultation?: string;
  splitChildren?: string[];
  consultationChildren?: string[];
  hasChildren?: boolean;
  questions?: string[];
  handoffRow?: boolean;
  projectHandoff?: { id?: string; status?: string; filing?: string } | null;
  investigations?: DraftIssue[];
  issue?: DraftIssue;
  issues?: DraftIssue[];
  unassigned?: boolean;
  splitIssues?: SplitPlan[];
  messages?: DraftMessage[];
  usageReviewRound?: number;
  priorityReviewRound?: number;
  inventoryRound?: number;
  investigation?: boolean | DraftIssue;
  target?: { followup?: boolean; title?: string };
  round?: number;
  detail?: string;
  stallKind?: string;
  fix?: { issue?: DraftIssue; status?: string };
  key?: string;
  url?: string;
  stateLabel?: string;
  caseKind?: string;
  issueTitle?: string;
  guidance?: string;
  error?: string | null;
  continuationLimited?: boolean;
  investigationNotice?: string;
  dependencyReason?: string;
  projectWait?: ProjectWait;
  scopeWait?: { note?: string };
  origin?: { sources: { desk?: number | null }[] };
  priority?: PublishedPriority | string | null;
  sha?: string;
  seconds?: number;
  files?: number;
  failedFiles?: string[];
  excerpt?: string;
  images?: ImageDescriptor[];
}
export interface WorkState {
  state: 'attention' | 'running' | 'waiting' | 'stopped';
  reason: string;
  nextActor: string;
  nextAction: string;
  waitCondition?: string;
  resumeBy?: string;
}
export type RequestGroups = Record<ListTab, RequestRow[]> & {
  monitoring: RequestRow[];
};
export type ListEntityType = 'issue' | 'draft' | 'todo' | 'closeRequest';
export interface ListData {
  at: number;
  scope?: 'active' | 'full';
  cursor?: string | null;
  issues: RequestRow[];
  drafts: RequestRow[];
  todos: RequestRow[];
  closes: RequestRow[];
  reviews: RequestRow[];
  fullTests: RequestRow[];
  stalls?: RequestRow[];
  attentionAlerts: RequestRow[];
  // 読めなかった候補のリポジトリ（GET /api/list の unreadProjects）。その行は前回の内容のまま残し、readFailed を付ける。
  unreadProjects?: ListUnreadProject[];
}
export interface ListUnreadProject {
  project: string;
  reason: string;
}
export interface ListScreen {
  id: number;
  failed: boolean;
  show(): void;
  apply(data: ListData): void;
  refresh(full?: boolean, waited?: boolean): Promise<void>;
  hostsShown?: unknown;
  tabNodes?: Map<
    ListTab,
    { button: HTMLButtonElement; number: HTMLSpanElement; shown: string | null }
  >;
  rowNodes?: Map<
    string,
    {
      row: RequestRow;
      sig: string | null;
      button: HTMLButtonElement;
      related?: HTMLButtonElement[];
    }
  >;
  emptyNode?: { sig: string; node: HTMLElement };
}
export interface ListEntry {
  type: ListEntityType;
  key: string;
  item: RequestRow;
}
export interface ListResponse {
  scope?: 'active' | 'full';
  full?: boolean;
  cursor?: string | null;
  rows?: ListEntry[];
  added?: ListEntry[];
  updated?: ListEntry[];
  removed?: string[];
  aux?: Partial<
    Pick<ListData, 'reviews' | 'fullTests' | 'stalls' | 'attentionAlerts'>
  >;
  unavailable?: string[];
  unreadProjects?: ListUnreadProject[];
  hosts?: unknown;
}
export interface ListNotice {
  type: string;
  project?: string;
  number?: number;
  id?: string;
}
export type ListTarget = (
  | { type: 'issue'; project: string; number: number }
  | { type: Exclude<ListEntityType, 'issue'>; id: string }
) & { key: string; tries?: number };
