// NASの窓口から渡される開始時刻は子の再起動をまたいで同じ。
// 窓口の最初の子か。開始時刻は子の起動より前の秒単位の値なので、短い期限は起動時点で過ぎていることがある。
// 最初の子は期限に関わらず1回読み、再起動した子は元の期限を過ぎていれば読まない。
export function handoffFirstRun() {
  return (
    process.env.ISSUE_DESK_NAS_HANDOFF === 'read' &&
    !(Number(process.env.ISSUE_DESK_NAS_RESTARTS) > 0)
  );
}
export function handoffDeadline(duration, now = Date.now()) {
  const started = Number(process.env.ISSUE_DESK_NAS_STARTED) * 1000;
  return (
    (process.env.ISSUE_DESK_NAS_HANDOFF === 'read' && started > 0
      ? started
      : now) + duration
  );
}
