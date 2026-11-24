import type { ComposerTodo } from './composer-types.js';

export interface Todo extends ComposerTodo {
  id: string;
  project: string;
  revision: string;
  provider?: string;
  actions?: string[];
  updatedAt: string;
  createdAt?: string;
  dueAt?: string;
  savedBy?: string;
  url?: string;
  review?: {
    revision: string;
    dueAt: string;
    verdict: string;
    reason: string;
    evidence?: string[];
    at: string;
  } | null;
}
