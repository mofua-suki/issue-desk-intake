export interface SessionResponse {
  authenticated: boolean;
  local?: boolean;
  csrf?: string;
  publicUrl?: string;
}

interface Provider {
  id: string;
  name: string;
  ready?: boolean;
  reason?: string;
}

interface ProjectSummary {
  id: string;
  name: string;
  host?: string;
}

export interface GiteaAccount {
  login: string;
  dedicated: boolean;
}

export interface MetaResponse {
  providers: Provider[];
  projects: ProjectSummary[];
  worker?: boolean;
  workerId?: string;
  hosts?: string[];
  giteaAccount?: GiteaAccount | null;
  projectCreation?: { owner: string } | null;
}

export type Navigation = boolean | 'replace';
export type ListTab =
  | 'attention'
  | 'issues'
  | 'waiting'
  | 'stopped'
  | 'completed'
  | 'closed'
  | 'todos'
  | 'followups';
export type ParsedRoute =
  | { type: 'home'; tab: string | null }
  | { type: 'new'; share: boolean }
  | {
      type:
        | 'ssh-ticket'
        | 'permission-grant'
        | 'draft'
        | 'todo'
        | 'project'
        | 'close';
      id: string;
    }
  | { type: 'issue'; desk: number; project?: never; number?: never }
  | { type: 'issue'; project: string; number: number; desk?: never }
  | { type: 'attention'; key: string }
  | { type: 'invalid'; scope?: string }
  | {
      type:
        | 'ssh-tickets'
        | 'permission-grants'
        | 'projects'
        | 'stats'
        | 'models'
        | 'tokens';
    };
export type CurrentScreen =
  | { type: 'new'; share: boolean }
  | {
      type: 'issue';
      project: string | null;
      number: number | null;
      early?: Set<string>;
    }
  | { type: 'draft' | 'todo' | 'project' | 'close'; id: string }
  | {
      type:
        | 'pairing'
        | 'models'
        | 'tokens'
        | 'ssh-tickets'
        | 'permission-grants'
        | 'projects'
        | 'full-test'
        | 'usage-review'
        | 'attention'
        | 'stats';
    };

export interface Polling {
  fn: () => void | Promise<void>;
  id: number;
  waiting: boolean;
  again: boolean;
  hurried: boolean;
}

export interface APIProblem {
  error?: string;
  detail?: string;
  retryable?: unknown;
  code?: string;
}

export interface UIError extends Error {
  status?: number;
  retryable?: unknown;
  code?: string;
  composerReported?: boolean;
}

export function uiError(value: unknown): UIError {
  if (value instanceof Error) return value;
  const message =
    value && typeof value === 'object' && 'message' in value
      ? String(value.message)
      : String(value);
  return new Error(message, { cause: value });
}
