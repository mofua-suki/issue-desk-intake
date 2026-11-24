import { unquote } from './release-sql.ts';

const permissionFiles: readonly string[] = Object.freeze([
  'host-ssh.json',
  'scripts/ssh-development-host.sh',
  'scripts/ssh-development-runner.ts',
  'scripts/ssh-verify-host.sh',
  'scripts/ssh-verify-runner.ts',
  'server/src/ssh-tickets.ts',
  'server/src/ssh-ticket-routes.ts',
  'deploy/nas-host/ssh-sign.sh',
  'deploy/nas-host/setup-admin.sh',
  'deploy/client/ssh/setup-ssh.ps1',
  'deploy/client/hostd/ssh-repair.ps1',
  'deploy/nas/remote-control-settings.json',
  'deploy/nas/claude-rc@.service',
  'deploy/nas/permission-grant-rules.json',
  'deploy/nas/permission-grants-apply.ts',
  'shared/src/permission-guard.ts',
]);

export function permissionReason(
  rows: readonly (readonly string[])[],
): string[] {
  const changed = new Set<string>();
  for (const [state, ...paths] of rows) {
    if (!/^(?:[AMDT]|[RC]\d*)$/.test(state)) continue;
    for (const path of paths) {
      const name = unquote(path);
      if (permissionFiles.includes(name)) changed.add(name);
    }
  }
  return [...changed].map(
    (name) =>
      `権限の範囲を定めるファイルの変更があります（利用者の承認が必要）: ${name}`,
  );
}
