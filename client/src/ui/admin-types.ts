import type { DraftIssue } from './draft-types.js';
export interface PCToken {
  id: string;
  name: string;
  delegate: boolean;
  issuedAt: string;
  lastUsedAt?: string;
}
export interface PermissionGrant {
  id: string;
  kind: string;
  target: string;
  status: string;
  permanent?: boolean;
  expiresAt?: string;
  createdAt: string;
  createdBy?: string;
  appliedAt?: string;
  evidence?: { statedAt?: string; quote?: string };
}
export interface SSHTicket {
  id: string;
  host: string;
  operation: string;
  reason: string;
  evidence?: string;
  requester: string;
  status: string;
  emergency?: boolean;
  approver?: string;
  requested_at: string;
  decided_at?: string;
  issued_at?: string;
  expires_at?: string;
  reported_at?: string;
  connection_result?: string;
  exit_code?: number;
  summary?: string;
}
export interface CloseReference {
  source?: string;
  found?: boolean;
  ref: string;
  note?: string;
  kind?: string;
  title?: string;
  merged?: boolean;
  base?: string;
  state?: string;
  message?: string;
  desk?: number;
}
export interface CloseMaterial {
  references?: CloseReference[];
  ambiguous?: number[];
  defaultBranch?: string | null;
  successors?: { found: DraftIssue[]; broken: DraftIssue[] };
  unavailable?: string[];
}
export interface CloseRequest {
  id: string;
  status: string;
  project: string;
  number: number;
  desk?: number;
  pc: string;
  reason: string;
  description?: string;
  comment?: string;
  fingerprint?: string;
  actions?: string[];
  target?: { title?: string; body?: string };
  message?: string;
  review?: { source?: string; reason?: string; evidence?: string[] };
  material?: CloseMaterial;
  url?: string;
}
export interface ModelOverrideView {
  overrides: import('./tier-types.js').ModelOverride[];
  defaults: import('./tier-types.js').ModelDefaults;
  steps: { id: string; name: string }[];
  providers: string[];
  efforts: string[];
  maxDays?: number;
  locked?: boolean;
  note?: string;
  problem?: string;
  cancelled?: boolean;
}
export interface ModelOverrideForm {
  controls: Record<string, HTMLInputElement | HTMLSelectElement>;
  initial: Record<string, string>;
  mark(): void;
}
export interface ProviderDisableView {
  disables: { provider: string; until: string }[];
  defaultUntil?: string;
}
export interface ProviderDisableForm {
  until: HTMLInputElement;
  reason: HTMLInputElement;
  initialUntil: string;
  initialReason: string;
  button: HTMLButtonElement;
  note: HTMLElement;
}
