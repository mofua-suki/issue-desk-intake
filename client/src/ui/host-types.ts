interface HostFailure {
  at?: string;
  project?: string;
  issue?: number;
  phase?: string;
  reason?: string;
}
interface HostClaim {
  project?: string;
  issue?: number;
  progressAt?: string;
  openPhase?: string;
  failures?: number;
  failure?: HostFailure;
}
export interface HostHold {
  reason: string;
  since?: string;
  at?: string;
  code?: string;
  syscall?: string;
  path?: string;
}
interface HostActivity {
  phase?: { name?: string; project?: string; issue?: number; at?: string };
  skip?: { reason?: string; at?: string };
  claims?: HostClaim[];
  failure?: HostFailure;
  cliVersions?: {
    cli: string;
    current?: string;
    expected?: string;
    reason?: string;
  }[];
}
export interface Host {
  name: string;
  role?: 'implementation' | 'test-only';
  testRepositories?: string[] | null;
  repositories?: string[] | null;
  hostLoad?: { implementing: number; publishing: number } | null;
  repositoryLoads?:
    | {
        repository: string;
        max: number | null;
        implementing: number | null;
        publishing: number | null;
      }[]
    | null;
  alive?: boolean;
  restartSafe?: boolean;
  updatedAt?: string;
  maxImplementations?: number;
  capacity?: number | null;
  implementing?: number;
  publishing?: number;
  claimedTotal?: number;
  protocol?: number;
  config?: string;
  drain?: { until?: string; indefinite?: boolean };
  environmentHold?: HostHold;
  driftHold?: { current?: string; desired?: string; since?: string };
  activity?: HostActivity;
  load?: {
    cpuPercent?: number;
    freeMemoryMB?: number;
    freeDiskMB?: number;
    gpuPercent?: number;
    freeVramMB?: number;
    usedVramMB?: number;
  };
}
export interface HostsResponse {
  hosts: Host[];
  changed?: boolean;
  reason?: string;
  checkedAt?: string;
  leaseMinutes?: number;
  source?: { repo: string; issue: number };
  homeMonitor?: { stopped?: boolean; host?: string; lastHeartbeatAt?: string };
}
