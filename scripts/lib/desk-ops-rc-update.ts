// notice rc-update は PC 別トークンで送る報告。運用の代行の許可は不要。
export function parseRcUpdate(argv: string[]) {
  if (
    argv.length !== 4 ||
    argv[0] !== 'notice' ||
    argv[1] !== 'rc-update' ||
    argv[2] !== '--timer' ||
    !['enabled', 'disabled'].includes(argv[3])
  )
    throw Error(
      'notice rc-update には --timer enabled|disabled を指定してください。',
    );
  return { command: 'notice', kind: 'rc-update', timer: argv[3] };
}

export async function rcUpdateNotice(call, args, log) {
  await call('POST', '/api/v1/notices', {
    kind: 'rc-update',
    timer: args.timer,
  });
  log('配布の報告を送りました');
  return 0;
}
