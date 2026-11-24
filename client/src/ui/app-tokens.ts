// 受付画面の「PC別トークン」（Issue #1120。docs/intake-client.md『Operations delegation』）。client/src/ui/app.js の一部で、
// 単独のモジュールではない。受付は client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。
// PC別トークンの一覧（名前・発行日時・最終使用日時・運用の代行の有無）と、トークンごとの「運用の代行を許す／やめる」。
// トークンの発行・失効は管理側の scripts/intake-token.mjs だけで、この画面ではできない。URLは #/tokens。
async function tokensView(navigate: Navigation = true) {
  if (navigate !== false) remember('#/tokens', navigate === 'replace');
  const id = reset();
  current = { type: 'tokens' };
  const list = el(
    'div',
    { class: 'token-list' },
    el('div', { class: 'loading', text: 'PC別トークンを読み込んでいます…' }),
  );
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    head(
      'TOKENS',
      'PC別トークン',
      '他のPCのAIが受付へ接続するトークンの一覧です。「運用の代行を許す」と、そのトークンから要対応の読み取り・全部の相談への回答と再試行・モデルの切り替え・Issueの再試行と返信・別のPCが担当するIssueのクローズ依頼ができるようになります。Issueの停止・再開・優先度の変更もできます。削除・権限・最終承認、相談の続行、代理回答の取消、新しいプロジェクトの作成への同意は、利用者がチャットで対象を特定して承認した発言を添えたときだけ受け付け、発言と送信元のPCを記録します。相談の優先度と編集はトークンではできません。トークンの発行・失効と運用の代行の切り替えはできません。トークンの発行・失効は管理側の scripts/intake-token.mjs で行います。',
    ),
    list,
  );
  async function refresh() {
    let tokens;
    try {
      tokens = await api<PCToken[]>('/api/tokens');
    } catch (caughtValue) {
      const e = uiError(caughtValue);
      if (!isCurrent(id)) return;
      error(e);
      list.replaceChildren(
        el('div', {
          class: 'empty',
          text: 'PC別トークンを読み込めませんでした。接続状態を確かめて開き直してください。',
        }),
      );
      return;
    }
    if (!isCurrent(id)) return;
    list.replaceChildren(
      ...(tokens.length
        ? tokens.map((t) => tokenRow(id, t, refresh))
        : [
            el('div', {
              class: 'empty',
              text: '発行済みのPC別トークンはありません。',
            }),
          ]),
    );
  }
  await refresh();
}
// 1件のトークン。許可の付け外しは、そのトークンの内部IDへ送る（失効して発行し直した同じ名前のトークンへは届かない）。
function tokenRow(id: number, t: PCToken, reload: () => Promise<void>) {
  const toggle = el('button', {
    class: t.delegate ? 'quiet' : 'primary',
    type: 'button',
    text: t.delegate ? '運用の代行をやめる' : '運用の代行を許す',
  });
  toggle.onclick = () =>
    action(toggle, async () => {
      try {
        await api(`/api/tokens/${t.id}/delegate`, { delegate: !t.delegate });
      } catch (caughtValue) {
        const e = uiError(caughtValue);
        // 失効・発行し直されたトークンは一覧を読み直す（古い行のまま押し直させない）。
        if (isCurrent(id) && e.status === 404) await reload();
        throw e;
      }
      if (!isCurrent(id)) return;
      toast(
        t.delegate
          ? `${t.name} の運用の代行をやめました。`
          : `${t.name} に運用の代行を許しました。`,
      );
      await reload();
    });
  return el(
    'div',
    { class: 'token-row', 'data-token': t.name },
    el('div', { class: 'token-name', text: t.name }),
    el('small', { class: 'hint token-meta', text: tokenMetaText(t) }),
    el('div', { class: 'token-actions' }, toggle),
  );
}
const tokenMetaText = (t: PCToken) =>
  `運用の代行: ${t.delegate ? '許可中' : 'なし'} · 発行 ${date(t.issuedAt)} · 最終使用 ${t.lastUsedAt ? date(t.lastUsedAt) : '未使用'}`;
