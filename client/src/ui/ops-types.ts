interface OpsImplementation {
  host?: string;
  project?: string;
  completed: number | null;
}
interface OpsTier {
  tier?: string;
  count: number | null;
}
interface OpsTokenStep {
  step?: string;
  weighted: number | null;
  percent: number | null;
  unknown: number | null;
}
interface OpsModel {
  step?: string;
  provider?: string;
  model?: string;
  effort?: string;
  count: number | null;
  percent: number | null;
}
interface OpsDuration {
  averageMs: number | null;
  medianMs: number | null;
  count: number | null;
  missing: number | null;
  excluded?: number | null;
}
interface OpsDaily {
  date: string;
  completed: number | null;
  failed: number | null;
  tokens?: number | null;
}
export interface OpsReport {
  unavailable?: string[];
  implementations?: {
    byHost?: OpsImplementation[] | null;
    byProject?: OpsImplementation[] | null;
  } | null;
  implementationTiers?: OpsTier[] | null;
  consultTiers?: OpsTier[] | null;
  tokens?: {
    steps: OpsTokenStep[];
    weighted: number | null;
    unknown: number | null;
  } | null;
  models?: OpsModel[] | null;
  roundTrips?: {
    zero: number | null;
    distribution: { rounds: number; count: number | null }[];
    average: number | null;
    threshold: number;
    many: number | null;
    missing: number | null;
  } | null;
  durations?: Record<string, OpsDuration | null> | null;
  daily?: OpsDaily[] | null;
}
