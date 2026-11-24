export interface TierChange {
  at?: string;
  from?: string;
  to?: string;
  reason?: string;
}
export interface TierRecord {
  level?: string;
  source?: string;
  model?: string;
  effort?: string;
  reason?: string;
  pending?: boolean;
  attempts?: number;
  decidedAt?: string;
  history?: TierChange[];
}
export interface ModelSetting {
  provider?: string;
  model?: string;
  effort?: string;
}
export interface ModelOverride extends ModelSetting {
  step: string;
  until?: string;
  setAt?: string;
  firstUsedAt?: string;
  lastUsedAt?: string;
}
interface DefaultModelSetting extends ModelSetting {
  mode?: string;
  opposite?: Record<string, ModelSetting>;
  tiers?: Record<string, ModelSetting>;
}
export type ModelDefaults = Record<string, DefaultModelSetting>;
