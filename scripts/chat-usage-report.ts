// Match the CLI's report arguments before treating the command as a read.
function dateMillis(value: string): number | null {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value);
  if (!match) return null;
  const [, year, month, day] = match;
  const date = new Date(0);
  date.setUTCHours(0, 0, 0, 0);
  date.setUTCFullYear(Number(year), Number(month) - 1, Number(day));
  return date.getUTCFullYear() === Number(year) &&
    date.getUTCMonth() === Number(month) - 1 &&
    date.getUTCDate() === Number(day)
    ? date.getTime()
    : null;
}

export function isUsageReport(args: string[]): boolean {
  if (!/^(?:.*[\\/])?intake$/.test(args[0] ?? '') || args[1] !== 'admin')
    return false;
  let i = 2;
  while (
    i < args.length &&
    /^-{1,2}(?:config|application-dir)(?:=|$)/.test(args[i])
  ) {
    const option = args[i++];
    if (option.includes('=')) {
      if (!option.slice(option.indexOf('=') + 1)) return false;
    } else {
      if (!args[i] || args[i].startsWith('-')) return false;
      i++;
    }
  }
  if (args[i++] !== 'usage-per-issue' || args[i++] !== 'report') return false;
  const values = new Map<string, string>();
  while (i < args.length) {
    const token = args[i++];
    const equals = token.indexOf('=');
    const option = equals < 0 ? token : token.slice(0, equals);
    if (!/^-{1,2}(?:from|to|repo|format|population)$/.test(option))
      return false;
    const name = option.replace(/^-{1,2}/, '');
    if (values.has(name)) return false;
    const value = equals < 0 ? args[i++] : token.slice(equals + 1);
    if (value === undefined || (equals < 0 && value.startsWith('-')))
      return false;
    values.set(name, value);
  }
  const from = dateMillis(values.get('from') ?? '');
  const to = dateMillis(values.get('to') ?? '');
  if (from === null || to === null || to < from || to - from >= 92 * 86400000)
    return false;
  const repo = values.get('repo') ?? '';
  return (
    (!repo || /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo)) &&
    ['table', 'json', 'both'].includes(values.get('format') ?? 'both') &&
    ['merged', 'all'].includes(values.get('population') ?? 'merged')
  );
}
