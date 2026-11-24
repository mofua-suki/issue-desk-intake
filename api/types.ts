// Wire types shared with the browser. Server internals and credentials are never
// imported here. Arbitrary JSON fields remain unknown until a route validates them.
export const API_CONTRACT_VERSION =
  '4cd4625bbadfe9da0aa2a3f0af5ead6a3c8b12da3ef27b3d1f751c2ca9759213';

export interface Recovery {
  action: string;
  guidance: string;
}

export interface ErrorEnvelope {
  error: string;
  code: string;
  recovery: Recovery;
  contractVersion: string;
  retryable: 'same-request' | 'new-request';
  safeToChange: boolean;
  detail?: string;
}

export interface Lease {
  epoch: number;
  expiresAt: string;
}

export interface DependencyMerge {
  repo: string;
  number: number;
  status: 'merged' | 'closed';
  prNumber: number | null;
  mergeCommitSha: string | null;
}

export type JobStatus =
  | 'queued'
  | 'running'
  | 'held'
  | 'failed'
  | 'stopped'
  | 'merged'
  | 'closed';

// This identity view lists guaranteed fields only. Route-specific payloads must
// be validated separately; it does not describe an entire job response.
export interface JobIdentity {
  id: number;
  repo: string;
  issue: number;
  status: JobStatus;
  leaseHost: string | null;
  leaseEpoch: number;
  leaseExpiresAt: string | null;
}

// Stop検査の発動記録と統計表のwire形式。
export interface StopCheckFire {
  id: string;
  checkId: string;
  result: 'block' | 'warn';
  at: string;
  project: string;
}

export interface StopCheckStatsRow {
  id: string;
  kind: 'hook' | 'guard';
  purpose: string;
  introducedAt: string;
  nextReviewAt: string;
  overdue: boolean;
  total: number | null;
  last30Days: number | null;
  blocked: number | null;
  warned: number | null;
  lastFiredAt: string | null;
  reviewResult: '残す' | '直す' | '外す' | null;
}

export interface StopCheckStats {
  rows: StopCheckStatsRow[];
  unknownCheckIds: string[];
}
