// 受付の /app.js に結合する許可の付与の一覧・詳細（docs/permission-grants.md『Grant Screen』）。
const permissionGrantStatusLabels: Record<string, string> = {
  active: '有効',
  expired: '期限切れ',
  revoked: '取り消し済み',
};
const permissionGrantKindLabels: Record<string, string> = {
  'nas-claude-rule': 'NASのClaude規則',
  'hostd-repair': 'hostdの修理',
  'host-ssh': 'ホスト作業用SSH',
  'db-read': 'DBの参照',
};
const permissionGrantExpiry = (grant: PermissionGrant) =>
  grant.permanent ? '恒久' : grant.expiresAt ? date(grant.expiresAt) : '未記録';
async function permissionGrantsView(
  grantId: string | null = null,
  navigate: Navigation = true,
) {
  const hash = grantId
    ? `#/permission-grants/${grantId}`
    : '#/permission-grants';
  if (navigate !== false) remember(hash, navigate === 'replace');
  const id = reset();
  current = { type: 'permission-grants' };
  const content = el(
    'section',
    {},
    el('p', { text: '許可の付与を読み込んでいます…' }),
  );
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    head(
      'PERMISSION GRANTS',
      grantId ? '許可の付与' : '許可の付与一覧',
      'チャットのAIが利用者の許可を根拠に登録した付与です。',
    ),
    content,
  );
  async function refresh() {
    const data = await api<PermissionGrant | { grants: PermissionGrant[] }>(
      grantId
        ? `/api/permission-grants/${grantId}`
        : '/api/permission-grants?status=all',
    );
    if (!isCurrent(id)) return;
    if (grantId && 'id' in data)
      content.replaceChildren(permissionGrantDetail(id, data, refresh));
    else if ('grants' in data) {
      const grants = [...data.grants].sort((a, b) =>
        String(b.createdAt).localeCompare(String(a.createdAt)),
      );
      content.replaceChildren(
        ...(grants.length
          ? grants.map((grant) =>
              el(
                'p',
                {},
                el('a', {
                  href: `#/permission-grants/${grant.id}`,
                  text: `${permissionGrantStatusLabels[grant.status] ?? grant.status} · ${grant.kind} ${grant.target} · 期限: ${permissionGrantExpiry(grant)}`,
                }),
              ),
            )
          : [el('p', { text: '許可の付与はありません。' })]),
      );
    }
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
        text: '許可の付与を読み込めませんでした。接続状態を確認してください。',
      }),
    );
    poll(refresh, 10000);
  }
}
function permissionGrantDetail(
  id: number,
  grant: PermissionGrant,
  refresh: () => Promise<void>,
) {
  const stated = grant.evidence?.statedAt;
  const fields = [
    [
      '種類',
      permissionGrantKindLabels[grant.kind]
        ? `${permissionGrantKindLabels[grant.kind]}（${grant.kind}）`
        : grant.kind,
    ],
    ['対象', grant.target],
    ['期限', permissionGrantExpiry(grant)],
    ['登録者', grant.createdBy],
    ['根拠の引用', grant.evidence?.quote],
    ['発言時刻', stated ? date(stated) : '未記録'],
    ['状態', permissionGrantStatusLabels[grant.status] ?? grant.status],
    ['反映済み時刻', grant.appliedAt ? date(grant.appliedAt) : '未記録'],
  ];
  const revoke = el('button', { type: 'button', text: '取り消す' });
  revoke.onclick = () => {
    if (!isCurrent(id) || revoke.disabled) return;
    revoke.disabled = true;
    return action(revoke, async () => {
      try {
        await api(`/api/permission-grants/${grant.id}/revoke`, {});
      } finally {
        // 競合や応答切断でも現在の状態を読み直し、古い取り消しボタンを残さない。
        try {
          if (isCurrent(id)) await refresh();
        } finally {
          revoke.disabled = false;
        }
      }
    });
  };
  return el(
    'div',
    {},
    el('a', { href: '#/permission-grants', text: '← 許可の付与一覧' }),
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
    grant.status === 'active' ? el('div', {}, revoke) : null,
  );
}
