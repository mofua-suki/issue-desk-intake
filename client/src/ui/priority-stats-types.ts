import type { DraftIssue } from './draft-types.js';
export interface PriorityCondition {
  result?: boolean;
  reason?: string;
}
export interface PriorityPopulation {
  high?: number;
  population?: number;
  ratio?: number;
  unjudged?: number;
  interrupt?: number;
  userNormal?: number;
}
export interface PriorityChanges {
  count?: number;
  up?: number;
  down?: number;
  same?: number;
}
interface PriorityOutcome {
  count?: number;
  userSpecified?: number;
  otherVersion?: number;
  reasons?: Record<string, string>;
  toMerge?: { medianMs?: number };
  returns?: { perJob?: number };
  instructions?: { perJob?: number };
}
export interface PriorityDaily {
  date?: string;
  version?: string | number;
  error?: string;
  population?: {
    total?: PriorityPopulation;
    repositories?: Record<string, PriorityPopulation>;
  };
  highRatio?: { min?: number; max?: number };
  changes?: {
    yesterday?: PriorityChanges;
    recent?: PriorityChanges;
    days?: number;
  };
  reasons?: {
    categories?: Record<string, number>;
    unknown?: number;
    count?: number;
    top?: { name?: string; category: string; count?: number; ratio?: number };
  };
  conditions?: Record<string, PriorityCondition>;
  outcomes?: { groups?: Record<string, PriorityOutcome> };
}
export interface PriorityReviewRound {
  round: number;
  date?: string;
  status: string;
  noAction?: boolean;
  autoTurnAt?: string;
  investigationIssue?: DraftIssue;
  filedIssues?: DraftIssue[];
  held?: number;
  startReasons?: string[];
  closedAt?: string;
  closeReason?: string;
}
export interface PriorityReport {
  daily?: PriorityDaily;
  rounds?: PriorityReviewRound[];
  categoryNames?: Record<string, string>;
  reason?: string;
  dailyNote?: string;
}
