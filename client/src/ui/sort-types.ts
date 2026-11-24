export interface PublishedPriority {
  level: string;
  source: string;
  judged?: { level?: string; source?: string } | null;
}
export interface SortableRow {
  project?: string | null;
  number?: number | string | null;
  id?: string | number;
  intakeId?: string;
  kind?: string;
  status?: string;
  updatedAt?: string | number | null;
  followup?: boolean;
  priority?: PublishedPriority | string | null;
}
export interface RowSort {
  field: string;
  direction: string;
}
