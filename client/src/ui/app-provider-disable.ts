// 実行先の期限付き無効。app.js と同じスコープで配信する。
let providerDisableView: ProviderDisableView | null = null,
  providerDisableBanner: HTMLElement | null = null,
  providerDisableForm: Map<string, ProviderDisableForm> | null = null,
  providerDisableScreen: string | null = null,
  providerDisablePending: { replace: boolean } | null = null,
  providerDisableSequence = 0,
  providerDisableTimer: number | null | undefined = null,
  providerDisableBusy = false,
  providerDisableReadAt = 0;
// 画面の切り替えごとに読み直すのは、直前の取得からこの時間が過ぎたときだけ。期限の到来・自分の操作・
// 設定画面（models）への移動では間隔に関係なく読み直す。
const providerDisableFreshMs = 60000;
const providerDisableNames: Record<string, string> = {
  codex: 'Codex',
  claude: 'Claude',
};
function providerDisableJst(value: string) {
  return new Date(Date.parse(value) + 9 * 60 * 60 * 1000)
    .toISOString()
    .slice(0, 19);
}
function providerDisableRoute({ clearForm = false, replace = false } = {}) {
  if (clearForm) providerDisableForm = null;
  if (!session?.authenticated) {
    // ログアウト中に読んだ値は次のログイン後に使い回さない。
    providerDisableReadAt = 0;
    return;
  }
  if (providerDisablePending) {
    providerDisablePending.replace &&= replace;
    return;
  }
  const pending = (providerDisablePending = { replace });
  // remember と reset は同じ画面遷移で呼ばれる。同期した画面の組み立てが済んでから1回だけ判定する。
  Promise.resolve()
    .then(() => {
      providerDisablePending = null;
      if (!session?.authenticated) return;
      const screen = JSON.stringify([current?.type || 'home', location.hash]);
      const changed = screen !== providerDisableScreen;
      providerDisableScreen = screen;
      // 一覧の初期タブやIssueの共通番号へのURLの正規化は、画面を切り替えず現在の識別子だけを更新する。
      if (pending.replace || !changed) return;
      const elapsed = Date.now() - providerDisableReadAt;
      // 端末の時計が戻ったとき（差が負）は読み直す。
      if (
        current?.type !== 'models' &&
        elapsed >= 0 &&
        elapsed < providerDisableFreshMs
      )
        return;
      return providerDisableRefresh();
    })
    .catch(error);
}
async function providerDisableRefresh() {
  const sequence = ++providerDisableSequence;
  const view = await api<ProviderDisableView>('/api/provider-disables');
  if (sequence !== providerDisableSequence) return;
  providerDisableReadAt = Date.now();
  providerDisableApply(view);
}
function providerDisableApply(view: ProviderDisableView) {
  providerDisableView = view;
  providerDisableDraw();
  cancelTimer(providerDisableTimer);
  providerDisableTimer = null;
  const times = (view.disables || [])
    .map((entry: { until: string }) => Date.parse(entry.until))
    .filter((t: number) => t > Date.now());
  if (times.length) {
    // 長い期限でも setTimeout の上限を超えないよう、同じ1回の取得まで分割して待つ。
    const due = Math.min(...times) + 1000;
    const wait = () => {
      const remaining = due - Date.now();
      if (remaining > 0)
        providerDisableTimer = setTimeout(
          wait,
          Math.min(remaining, 2 ** 31 - 1),
        );
      else providerDisableRefresh().catch(error);
    };
    wait();
  }
}
async function providerDisableSend(
  provider: string,
  data: { until?: string; reason?: string },
  cancel = false,
) {
  if (providerDisableBusy) return;
  providerDisableBusy = true;
  providerDisableSequence++;
  operations++;
  providerDisableDraw();
  try {
    const view = await api<ProviderDisableView>(
      `/api/provider-disables/${provider}${cancel ? '/cancel' : ''}`,
      data,
    );
    providerDisableSequence++;
    providerDisableApply(view);
    await providerDisableRefresh();
  } catch (caughtValue) {
    const e = uiError(caughtValue);
    error(e);
  } finally {
    operations--;
    providerDisableBusy = false;
    providerDisableDraw();
  }
}
function providerDisableDraw() {
  if (!providerDisableBanner) {
    providerDisableBanner = el('div', {
      class: 'provider-disable-banner',
      role: 'status',
      'aria-live': 'polite',
    });
    $('#app').parentNode!.insertBefore(providerDisableBanner, $('#app'));
  }
  const entries = providerDisableView?.disables || [];
  providerDisableBanner.hidden = !entries.length;
  providerDisableBanner.replaceChildren(
    ...entries.map((entry: { provider: string; until: string }) => {
      const other = entry.provider === 'codex' ? 'claude' : 'codex';
      const cancel = el('button', { type: 'button', text: '取り消す' });
      cancel.disabled = providerDisableBusy;
      cancel.onclick = () => providerDisableSend(entry.provider, {}, true);
      return el(
        'div',
        {},
        el('span', {
          text: `${providerDisableNames[entry.provider]} を無効にしています（期限 ${providerDisableJst(entry.until).replace('T', ' ')} 日本時間まで ${providerDisableNames[other]} で動きます）`,
        }),
        cancel,
      );
    }),
  );
  if (!providerDisableForm) return;
  for (const [provider, form] of providerDisableForm) {
    const other = provider === 'codex' ? 'claude' : 'codex';
    const blocked = entries.some(
      (entry: { provider: string }) => entry.provider === other,
    );
    form.button.disabled =
      providerDisableBusy || !providerDisableView || blocked;
    form.note.textContent = blocked
      ? `${providerDisableNames[other]} を無効にしているため、同時に無効にはできません。先に取り消してください。`
      : !providerDisableView
        ? '実行先の状態を読み込んでいます。'
        : '';
    if (
      form.until.value === form.initialUntil &&
      providerDisableView?.defaultUntil
    ) {
      form.until.value = providerDisableJst(providerDisableView.defaultUntil);
      form.initialUntil = form.until.value;
    }
  }
}
function providerDisableSection() {
  providerDisableForm = new Map();
  const section = el(
    'section',
    { class: 'model-step', id: 'provider-disables' },
    el('h2', { text: '実行先の一時的な無効' }),
  );
  for (const provider of ['codex', 'claude']) {
    const name = providerDisableNames[provider];
    const until = el('input', {
      type: 'datetime-local',
      step: '1',
      'aria-label': `${name}を無効にする期限（日本時間）`,
    });
    const reason = el('input', {
      'aria-label': `${name}を無効にする理由（任意）`,
    });
    const button = el('button', {
      type: 'button',
      class: 'primary',
      text: `${name} を無効にする`,
    });
    const note = el('p', { class: 'model-note' });
    button.onclick = () => {
      if (button.disabled) return;
      const deadline = jstDeadline(until.value);
      if (!deadline) {
        error(Error('期限を日本時間の日時で入力してください。'));
        return;
      }
      return providerDisableSend(provider, {
        until: deadline,
        reason: reason.value,
      });
    };
    providerDisableForm.set(provider, {
      until,
      reason,
      button,
      note,
      initialUntil: '',
      initialReason: reason.value,
    });
    section.append(
      el(
        'div',
        { class: 'model-form' },
        field('期限（日本時間）', until),
        field('理由（任意）', reason),
        button,
        note,
      ),
    );
  }
  providerDisableDraw();
  return section;
}
