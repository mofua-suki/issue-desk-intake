import type { DraftIssue, DraftMessage } from './draft-types.js';
interface ProjectPlan {
  title: string;
  purpose: string;
  acceptance: string[];
  dependsOn: number[];
  changeScope?: { known: boolean; paths: string[]; reason?: string };
}
export interface ProjectDefinition {
  name: string;
  description: string;
  technology: string;
  placement: string;
  testCommand: string[];
  setupCommand?: string[];
  autoMerge: boolean;
  autoDeploy: boolean;
  splitIssues: ProjectPlan[];
}
interface CreationStep {
  skipped?: boolean;
  number?: number;
  accounts?: { result: string; login: string }[];
}
export interface ProjectCreation {
  steps: Record<string, CreationStep>;
  waiting?: string;
  error?: string;
  reason?: string;
  monitor?: string;
}
interface ProjectFiling {
  status?: string;
  issues?: DraftIssue[];
  waiting?: string;
  error?: string;
  host?: string;
}
export interface ProjectConsultation {
  id: string;
  status: string;
  title?: string;
  updatedAt?: string;
  body?: string;
  messages: DraftMessage[];
  actions?: string[];
  sourceDraft?: string;
  handoff?: { unassigned?: boolean; project?: string | null; summary?: string };
  definition?: ProjectDefinition;
  creation?: ProjectCreation;
  filing?: ProjectFiling;
  fingerprint?: string;
  approval?: { approvedAt: string };
  creationLocked?: boolean;
  creationUnavailable?: boolean;
  queued?: boolean;
  questions?: string[];
  error?: string;
  finishingTurn?: boolean;
}
