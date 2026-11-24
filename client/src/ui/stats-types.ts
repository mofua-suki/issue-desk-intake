import type { UsageDetails as importUsageDetails } from './usage-detail-types.js';
export type MetricKey =
  | 'inputTokens'
  | 'outputTokens'
  | 'cacheReadTokens'
  | 'cacheWriteTokens'
  | 'costUsd'
  | 'uncachedInputTokens'
  | 'normalizedCacheWriteTokens'
  | 'weighted';
type UsageMetrics = Partial<Record<MetricKey, number | null>>;
export interface UsageTotal extends UsageMetrics {
  count: number;
  unknown?: Partial<Record<MetricKey, number>>;
  cacheWriteNotApplicable?: number;
  step?: string;
  provider?: string;
  model?: string;
  kind?: string;
  repo?: string;
  number?: number;
  desk?: number;
  host?: string;
  bucket?: string;
  calls?: number;
  promptTotalBytes?: number;
  promptAverageBytes?: number;
  contextMaxTokens?: number;
  contextTotalTokens?: number;
  detailUnknown?: Partial<
    Record<
      'calls' | 'promptTotalBytes' | 'contextMaxTokens' | 'contextTotalTokens',
      number
    >
  >;
}
export interface DatePeriod {
  fromDate: string;
  toDate: string;
}
export interface WorkDay {
  date: string;
  measured?: { start: string | number; end: string | number } | null;
  ratio: number | null;
  measuredMs: number;
  completed: number;
  unconfirmed: number;
  parallelMs: number;
  gapMs: number;
  stoppedMs: number;
  leadMs?: number | null;
  waitMs?: number | null;
  waitCount: number;
  waitUnconfirmed: number;
}
export interface WorkPeriod extends WorkDay, DatePeriod {
  gaps: number;
  activeMs: number;
  totalMs: number;
  maxConcurrent: number;
  runs: number;
  interrupted: number;
  unended: number;
  days: WorkDay[];
}
export interface CacheAggregate {
  step: string;
  count: number;
  cacheReadTokens?: number | null;
  cacheWriteTokens?: number | null;
  readRatio?: number | null;
  unknown: number;
}
export interface UsagePeriod extends DatePeriod, importUsageDetails {
  repos?: (UsageTotal & { steps?: UsageTotal[] })[];
  total: UsageTotal;
  steps: UsageTotal[];
  models: UsageTotal[];
  kinds: UsageTotal[];
  issues: UsageTotal[];
  hosts?: UsageTotal[];
  omittedIssues?: number;
  costCorrection?: {
    corrected: number;
    uncorrectable: number;
    from?: string;
    to?: string;
  };
  intakeCache?: { steps: CacheAggregate[] };
}

export type UsageAxis = 'steps' | 'models' | 'kinds' | 'issues' | 'hosts';
