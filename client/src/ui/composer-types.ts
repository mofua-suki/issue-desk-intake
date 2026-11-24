import type { el } from './dom.js';
import type { ImageDescriptor } from './issue-types.js';

export interface ComposerScope {
  project?: string | null;
  draft?: string;
  todo?: string;
  number?: number;
}
export interface ComposerImage {
  id: string;
  name: string;
  blob: Blob;
  uploaded: boolean;
  uploadAttempted?: boolean;
  removePending?: boolean;
}
export interface ComposerMessage {
  id: string;
  messageId: string;
  text: string;
  images: string[];
  action?: string;
  notDuplicate?: string;
  expectedRevision?: number;
}
export interface AcceptedMessage {
  id: string;
  text: string;
  images?: ImageDescriptor[];
}
export interface AcceptedDraft {
  status: string;
  project?: string | null;
  issue?: { project?: string; number: number; desk?: number };
  messages?: AcceptedMessage[];
}
export interface TodoCheckEdit {
  problem: string;
  resolved: string;
  methods: string;
  dueDate: string;
}
export interface ComposerEdit {
  text?: string;
  removed?: string[];
  provider?: string;
  todoCheck?: TodoCheckEdit;
}
interface TodoCheck {
  problem: string;
  resolved: string;
  methods: string[];
}
export interface ComposerTodo {
  requestId?: string;
  body: string;
  revision?: string;
  images?: ImageDescriptor[];
  dueAt?: string;
  check?: TodoCheck;
}
export interface ComposerCreation {
  id: string;
  project: string | null;
  unassigned?: boolean;
  provider: string;
  consultationProvider?: string;
  priority?: string;
  tier?: string;
  implementationTier?: string;
}
export interface ComposerState {
  messageId: string;
  id: string;
  text: string;
  images: ComposerImage[];
  version?: string;
  baseRevision?: string;
  controls?: Record<string, string | { id: string; value: string }>;
  scope?: ComposerScope;
  destination?: ComposerScope;
  pending?: ComposerMessage;
  action?: string;
  edit?: ComposerEdit;
  creation?: ComposerCreation;
  todoPending?: { check: TodoCheckEdit; dueAtOmitted: boolean };
}
export interface ComposerOptions {
  key: string;
  text: HTMLTextAreaElement;
  el: typeof el;
  api<T = unknown>(url: string, data?: unknown, method?: string): Promise<T>;
  error(value: unknown): void;
  locked?: boolean;
  onDirty?: () => void;
  isCurrent?: () => boolean;
}
