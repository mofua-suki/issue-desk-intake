export function parseNotice(
  positional: string[],
  flags: Record<string, string>,
  fail: (message: string) => Error,
) {
  const kind = positional[0];
  if (kind === 'boot-check') {
    const items = (flags.items ?? '').split(',');
    if (
      Object.keys(flags).some((key) => !['signature', 'items'].includes(key)) ||
      !/^[a-fA-F0-9]{8,64}$/.test(flags.signature ?? '') ||
      /[\r\n]/.test(flags.signature ?? '') ||
      items.length > 30 ||
      !items.every(
        (name) => /^[A-Za-z0-9_.@:-]{1,80}$/.test(name) && !/[\r\n]/.test(name),
      )
    )
      throw fail('notice boot-check の署名と対象名を確認してください。');
    return { command: 'notice', kind, signature: flags.signature, items };
  }
  const instances =
    flags.instances === '' ? [] : (flags.instances ?? '').split(',');
  if (
    kind !== 'remote-control' ||
    !/^[a-fA-F0-9]{8,64}$/.test(flags.signature ?? '') ||
    /[\r\n]/.test(flags.signature ?? '') ||
    flags.instances === undefined ||
    instances.length > 20 ||
    !instances.every(
      (name) => /^[A-Za-z0-9_.@-]{1,64}$/.test(name) && !/[\r\n]/.test(name),
    )
  )
    throw fail(
      'notice remote-control の署名とインスタンス名を確認してください。',
    );
  if (flags.items !== undefined)
    throw fail('notice remote-control には --items を付けません。');
  return { command: 'notice', kind, signature: flags.signature, instances };
}

export async function sendNotice(
  send: (method: string, route: string, payload: any) => Promise<any>,
  args: any,
  log: (message: string) => void,
) {
  const value = await send('POST', '/api/v1/notices', {
    kind: args.kind,
    signature: args.signature,
    ...(args.kind === 'boot-check'
      ? { items: args.items }
      : { instances: args.instances }),
  });
  log(
    `${args.kind === 'boot-check' ? 'NASの起動後の点検の失敗' : 'remote-controlの配布失敗'}を知らせました: ${value.id}`,
  );
  return 0;
}
