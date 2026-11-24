import type { DatePeriod, UsageTotal } from './stats-types.js';
import type { DraftIssue } from './draft-types.js';
import type { ReturnRate } from './comparison-stats-types.js';
export interface ReworkTotal extends UsageTotal {
  perIssue?: UsageTotal;
  phase?: string;
  trigger?: string;
}
interface ReworkUsage {
  issues: number;
  total: ReworkTotal;
  phases: ReworkTotal[];
  triggers: ReworkTotal[];
  kinds: ReworkTotal[];
  autoReturns: number;
}
export interface ReworkPeriod extends DatePeriod {
  usage?: ReworkUsage;
  returns?: ReturnRate;
}
export interface ReworkReport {
  before?: ReworkPeriod;
  after?: ReworkPeriod;
  unavailable?: string[];
}
interface ImplementationPeriod {
  runs: number;
  noImplementationRecord: number;
  reasons: Record<string, number>;
  unknownGenerations: number;
  days: { day: string; reasons: Record<string, number> }[];
  distribution: { implementations: number; issues: number }[];
  unknownIssues: number;
  skipped: number;
  uc1Distribution?: { implementations: number; issues: number }[];
  uc1UnknownIssues?: number;
  implementationMismatchIssues?: number;
}
export interface ImplementationReport {
  before: ImplementationPeriod;
  after: ImplementationPeriod;
}
export interface AverageMetric {
  average: number | null;
  count: number;
  unknown: number;
}
export interface ReviewNotesUsage {
  costUsd: AverageMetric;
  tokens: AverageMetric;
  noImplementation?: number;
  noRecord?: number;
  uncorrectable: number;
}
export interface ReviewNotesPeriod extends DatePeriod {
  reason?: string;
  reviewed: number;
  otherHost?: number;
  since?: string;
  local: number;
  returnedPrs: number;
  rate: number;
  returns: number;
  usage?: ReviewNotesUsage;
  time?: {
    averageMs: number | null;
    count: number;
    missing?: number;
    noImplementation?: number;
    noRecord?: number;
  };
  notes: { bytes: number; tokens: number; lines: number; unreadable?: number };
}
export interface ReviewNotesReport {
  before?: ReviewNotesPeriod;
  after?: ReviewNotesPeriod;
  unavailable?: string[];
  skipped?: number;
  perReturn?: {
    returns: number;
    tokens: AverageMetric;
    costUsd: AverageMetric;
  };
  net?: {
    tokens: number | null;
    reason?: string;
    perReturnTokens: number | null;
    reducedReturns: number;
    savedTokens: number;
    noteTokens: number;
  };
}
export interface SelectionDuration {
  count: number;
  averageMs: number;
  medianMs: number;
  reason?: string;
  since?: string;
}
export interface SelectionFixed extends SelectionDuration {
  missing?: number;
  running?: number;
  chosen: SelectionDuration;
  whole: SelectionDuration;
  unlabeled: number;
}
interface SelectionChoice {
  reason?: string;
  since?: string;
  reasons: { reason: string; count: number }[];
  chosen: number;
  whole: number;
  notRun?: number;
  selectedShare: number;
  runShare: number;
}
interface SelectionReuse {
  reason?: string;
  since?: string;
  reasons: { reason: string; count: number }[];
  reused: number;
  rechecked: number;
  notReused: number;
  savedMs: number;
}
interface SelectionFull {
  reason?: string;
  since?: string;
  runs: number;
  passed: number;
  failed: number;
  interrupted: number;
  unrunnable: number;
  failedFiles: number;
  cutFiles: number;
  unknownFiles: number;
}
export interface SelectionLeakItem {
  version: string;
  file: string;
  issues: DraftIssue[];
}
export interface SelectionLeaks {
  reason?: string;
  since?: string;
  count: number;
  failedRuns: number;
  merged: number;
  unknownSelections?: number;
  unknownFiles?: number;
  cutRuns?: number;
  noPrevious?: number;
  items?: SelectionLeakItem[];
  omitted?: number;
}
export interface SelectionPeriod extends DatePeriod {
  fixed?: SelectionFixed;
  selection?: SelectionChoice;
  followReuse?: SelectionReuse;
  full?: SelectionFull;
  leaks?: SelectionLeaks;
}
export interface SelectionReport {
  before?: SelectionPeriod;
  after?: SelectionPeriod;
  reason?: string;
}
export interface StallDay {
  date: string;
  measured: boolean;
  stalls: number;
  merged: number;
  ratio: number | null;
}
export interface StallPeriod extends DatePeriod {
  measured?: { start: string; end: string };
  reason?: string;
  stalls: {
    total: number;
    suspected: number;
    kinds: Record<string, number>;
    stalledMs: number;
    unresolved: number;
  };
  unrecordedMs?: number;
  marked: number;
  released: number;
  fixes?: number;
  fixesReason?: string;
  parallel: {
    ratio: number | null;
    measuredMs: number;
    parallelMs: number;
    activeMs: number;
    maxHosts: number;
    hosts?: Record<string, number>;
  };
  mergedPerDay: number;
  merged: number;
  days: number;
  daily?: StallDay[];
}
export interface StallReport {
  before?: StallPeriod;
  after?: StallPeriod;
  reason?: string;
  unavailable?: string[];
  coverage?: { start: string; end: string };
}
