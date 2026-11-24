import type { UsageTotal, DatePeriod } from './stats-types.js';
export interface DetailCoverage {
  count: number;
  unknown: number;
}
export interface UsageFile {
  repo?: string;
  path: string;
  count: number;
  bytes?: number;
  unknown: number;
}
export interface PromptPart {
  name?: string;
  total?: number;
  average?: number;
  unknown: number;
}
export interface PromptParts extends DetailCoverage {
  parts: (PromptPart & { name: string })[];
  promptTotalBytes: PromptPart;
  cacheReadTokens: PromptPart;
  cacheWriteTokens: PromptPart;
}
export interface LimitReset {
  resetsAt?: string;
  records: number;
  first: { at?: string; percent?: number };
  last: { at?: string; percent?: number };
  claude: UsageTotal;
}
export interface RateLimits extends DetailCoverage {
  windows: {
    name: string;
    latest: { percent?: number; resetsAt?: string; at?: string };
    resets: LimitReset[];
  }[];
}
export interface UsageDetails {
  tools?: DetailCoverage & { items: { name: string; count: number }[] };
  files: DetailCoverage & { items: UsageFile[]; omitted?: number };
  promptParts: PromptParts;
  rateLimits: RateLimits;
}
export interface UsageArrival {
  from: string;
  to: string;
  scope: string;
  caution: string;
  failures: string[];
  rows: {
    host: string;
    sender: string;
    source?: string;
    count: number;
    latest?: string;
    status: string;
    registeredHost?: boolean;
    registeredSender?: boolean;
  }[];
  registrations: {
    kind: string;
    name: string;
    count?: number;
    latest?: string;
    status: string;
  }[];
  imports?: {
    file: string;
    position: number;
    updatedAt?: string;
    mapping: string;
  }[];
  reconciliation: {
    recorded?: number;
    missing?: number;
    unmatchable?: number;
    reason: string;
  };
}
export interface UsageLimit {
  provider: string;
  limit?: string;
  percent?: number;
  windowMinutes?: number;
  resetsAt?: string;
  at?: string;
  host?: string;
}
export interface UsageLimits {
  rows: UsageLimit[];
  reason?: string;
}
interface FixedTestGroup {
  group: string;
  count: number | null;
  elapsedMsMedian?: number;
  elapsedMsP90?: number;
  percentMedian?: number;
  cutCount?: number;
  loadavg1Median?: number;
}
interface FixedTestRow {
  host: string;
  repo: string;
  scope: string;
  groups: FixedTestGroup[];
  unknownImplementing: number;
  zeroImplementing: number;
  unknownLoad: number;
}
export interface FixedTestPeriod extends DatePeriod {
  rows: FixedTestRow[];
}
export interface FixedTestReport {
  reason?: string;
  before?: FixedTestPeriod;
  after?: FixedTestPeriod;
}
