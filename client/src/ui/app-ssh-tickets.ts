// 受付の /app.js に結合する修理申請の一覧・詳細。
async function sshTicketsView(
  ticketId: string | null = null,
  navigate: Navigation = true,
) {
  const hash = ticketId ? `#/ssh-tickets/${ticketId}` : '#/ssh-tickets';
  if (navigate !== false) remember(hash, navigate === 'replace');
  const id = reset();
  current = { type: 'ssh-tickets' };
  const content = el(
    'section',
    {},
    el('p', { text: 'ホスト作業用SSHの申請を読み込んでいます…' }),
  );
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    head(
      'HOST SSH',
      ticketId ? 'ホスト作業用SSHの申請' : 'ホスト作業用SSHの申請一覧',
      '承認だけでは操作は実行されません。',
    ),
    content,
  );
  async function refresh() {
    const data = await api<SSHTicket | { tickets: SSHTicket[] }>(
      ticketId ? `/api/ssh-tickets/${ticketId}` : '/api/ssh-tickets',
    );
    if (!isCurrent(id)) return;
    if (ticketId && 'id' in data)
      content.replaceChildren(sshTicketDetail(id, data, refresh));
    else if ('tickets' in data)
      content.replaceChildren(
        ...(data.tickets.length
          ? data.tickets.map((ticket) =>
              el(
                'p',
                {},
                el('a', {
                  href: `#/ssh-tickets/${ticket.id}`,
                  text: `${ticket.host} ${ticket.operation} · ${ticket.status} · ${date(ticket.requested_at)}`,
                }),
              ),
            )
          : [el('p', { text: 'ホスト作業用SSHの申請はありません。' })]),
      );
    poll(refresh, 15000);
  }
  try {
    await refresh();
  } catch (caughtValue) {
    const e = uiError(caughtValue);
    if (!isCurrent(id)) return;
    error(e);
    content.replaceChildren(
      el('p', {
        text: '申請を読み込めませんでした。接続状態を確認してください。',
      }),
    );
    poll(refresh, 10000);
  }
}
function sshTicketDetail(
  id: number,
  ticket: SSHTicket,
  refresh: () => Promise<void>,
) {
  const fields = [
    ['対象PC', ticket.host],
    ['操作', ticket.operation],
    ['何が止まっているか', ticket.reason],
    ['根拠', ticket.evidence],
    ['申請元', ticket.requester],
    ['状態', ticket.status],
    ['有事の申請', ticket.emergency ? 'はい' : 'いいえ'],
    ['承認者', ticket.approver],
    ...(
      [
        ['申請時刻', 'requested_at'],
        ['判断時刻', 'decided_at'],
        ['発券時刻', 'issued_at'],
        ['有効期限', 'expires_at'],
        ['結果報告時刻', 'reported_at'],
      ] as const
    ).map(([label, key]) => [
      label,
      ticket[key] ? date(ticket[key]) : '未記録',
    ]),
    ['接続結果', ticket.connection_result],
    ['終了コード', ticket.exit_code],
    ['結果の要旨', ticket.summary],
  ];
  const decisions = ['approve', 'deny'].map((decision) => {
    const button = el('button', {
      type: 'button',
      text: decision === 'approve' ? '承認する' : '却下する',
    });
    button.onclick = () => {
      if (!isCurrent(id) || decisions.some((b) => b.disabled)) return;
      for (const other of decisions) other.disabled = true;
      return action(button, async () => {
        try {
          await api(`/api/ssh-tickets/${ticket.id}/${decision}`, {});
        } finally {
          // 競合や応答切断でも現在の状態を読み直し、古い判断ボタンを残さない。
          try {
            if (isCurrent(id)) await refresh();
          } finally {
            for (const other of decisions) other.disabled = false;
          }
        }
      });
    };
    return button;
  });
  return el(
    'div',
    {},
    el('a', { href: '#/ssh-tickets', text: '← ホスト作業用SSHの申請一覧' }),
    el(
      'dl',
      {},
      ...fields.flatMap(([label, value]) => [
        el('dt', { text: label }),
        el('dd', {
          text: String(value ?? '未記録'),
          style: 'white-space: pre-wrap',
        }),
      ]),
    ),
    ticket.status === 'requested' ? el('div', {}, ...decisions) : null,
  );
}
