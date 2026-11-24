interface ConsultationBudgetStop {
  calls: number;
  weighted: number;
  limits: { calls: number; weighted: number };
  questions: string[];
}
export interface AlertDetail {
  period?: { fromDate?: string; toDate?: string };
  draftId?: string;
  stopCount?: number;
  budgetStop?: ConsultationBudgetStop;
  artifacts?: { artifact: string; repo: string; issue: number }[];
  count?: number;
  reworkRate?: number;
  threshold?: number;
  kindName?: string;
  deadline?: string;
  history?: { at: string; event: string; reason?: string }[];
  stageWatch?: boolean;
  holder?: string;
  host?: string;
  heartbeatAt?: string;
  monitorObservation?: string;
  observation?: string;
  successObservation?: string;
  lastSuccessAt?: string;
  event?: string;
  exitCode?: number;
  outputs?: number;
  gist?: string;
  autoCount?: number;
  streak?: number;
  commentId?: number;
}
export interface ProposalDismissalCandidate {
  title?: string;
  purpose?: string;
  acceptance?: string[];
  body?: string;
  parseError?: string;
  recordedReason?: string;
  fingerprintKind?: string;
  fingerprint: string;
  project?: string;
  number?: number;
  proposalId?: string;
  commentId?: number;
}
export interface BudgetOperation {
  operationId: string;
  pending: boolean;
  button?: HTMLButtonElement;
}
