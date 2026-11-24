// 受付画面の端末連携（スマホで開く・接続コードの入力）と、見出しの接続状態の表示。client/src/ui/app.js の一部（Issue #457）で、
// 単独のモジュールではない。受付は client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。
async function phone() {
  const box = $('#phone-content');
  if (!session.publicUrl) {
    box.replaceChildren(
      el('p', {
        class: 'instruction',
        text: '受付アプリは動いています。スマホから開くためのTailscale接続を設定中です。接続ができると、ここに専用URLと接続コードが表示されます。',
      }),
    );
    $('#phone-dialog').showModal();
    return;
  }
  // 発行を待つ間も自動更新を待たせる。表示する前に読み込み直すと、発行したコードを見られない。
  // 表示した後はダイアログが開いている間の保留へ引き継ぐ。
  operations++;
  try {
    const result = await api<{ code: string }>('/api/pair-code', {});
    box.replaceChildren(
      el('p', {
        class: 'instruction',
        text: 'iPhoneでTailscaleを接続し、Safariで次のURLを開いてください。',
      }),
      el('a', {
        class: 'phone-url',
        href: session.publicUrl,
        text: session.publicUrl,
      }),
      el('div', {
        class: 'code',
        text: result.code.slice(0, 4) + ' ' + result.code.slice(4),
      }),
      hint(
        'このコードをiPhoneの受付画面に入力してください。10分間有効・1回限りです。',
      ),
    );
    $('#phone-dialog').showModal();
  } finally {
    operations--;
  }
}
function pairing() {
  reset();
  current = { type: 'pairing' };
  const code = el('input', {
    inputmode: 'numeric',
    autocomplete: 'one-time-code',
    placeholder: '8桁の接続コード',
    maxlength: 9,
    required: true,
    'aria-label': '接続コード',
  });
  // 入力途中の接続コードを自動更新で消さない。
  holdReason = () => (code.value ? '接続コードを入力中です' : null);
  const button = el('button', {
    class: 'primary',
    type: 'submit',
    text: '接続する',
  });
  const form = el(
    'form',
    { class: 'panel-body' },
    el('span', { class: 'eyebrow', text: 'WELCOME' }),
    el('h1', { text: '開発受付へようこそ' }),
    el('p', {
      class: 'instruction',
      text: '初めて使う端末では、接続済みの端末の受付画面で「スマホで開く」を押して表示されたコード（またはNASの受付のコンテナで発行したコード）を入力します。',
    }),
    field('接続コード', code),
    button,
  );
  form.onsubmit = (e: { preventDefault: () => void }) => {
    e.preventDefault();
    action(button, async () => {
      await api('/api/pair', { code: code.value });
      location.reload();
    });
  };
  $('#app').replaceChildren(el('section', { class: 'panel pair-form' }, form));
}
// 受付がGiteaへ書き込むアカウント（/api/meta の giteaAccount）。見出しには短く、全文は title に出す。
function giteaAccountText(account: GiteaAccount | null | undefined) {
  if (!account?.login) return null;
  return account.dedicated
    ? {
        short: `Gitea ${account.login}（受付専用トークン）`,
        full: `受付のGitea書き込み: ${account.login}（受付専用トークン）`,
      }
    : {
        short: `Gitea ${account.login}（従来のトークン）`,
        full: `受付のGitea書き込み: ${account.login}（受付専用トークン未設定。従来のトークンで動作）`,
      };
}
// スマホ幅の上部は「● 稼働中」などの短い表示だけを出し、押すと全文（#connection の表示と title）を開く。
// PC幅は #connection をそのまま出す（切り替えは style.css）。
function connectionToggle(label: string, off: boolean) {
  const toggle = $('#connection-toggle');
  toggle.textContent = label;
  toggle.classList.toggle('off', off);
  toggle.onclick = () => {
    const open = $('#connection').dataset.open !== 'true';
    $('#connection').dataset.open = String(open);
    toggle.setAttribute('aria-expanded', String(open));
  };
}
