// 受付画面のAIのモデルの一時的な切り替え（modelsView と工程ごとの欄）。client/src/ui/app.js の一部（Issue #457）で、単独の
// モジュールではない。受付は client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。
// AIのモデルの一時的な切り替え（Issue #443。API は server/internal/httpapi/model_routes.go・Issue #442）。工程ごとに
// 今効いている切り替えと期限、切り替えが無い工程は今の設定の値を示し、設定・取り消しを送る。期限は日本時間で入力し、
// オフセット付きのISO日時で送る。切り替えは人がここ（またはmodel-override SkillのAPI手順）で設定したときだけ効く。
async function modelsView(navigate: Navigation = true) {
  if (navigate !== false) remember('#/models', navigate === 'replace');
  const id = reset();
  current = { type: 'models' };
  const body = el(
    'div',
    { id: 'model-overrides' },
    el('div', { class: 'loading', text: '切り替えを読み込んでいます…' }),
  );
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    head(
      'MODELS',
      'モデルの一時的な切り替え',
      '受付の相談・独立レビュー・優先度の判定のAIのモデルを、期限付きで切り替えます。期限が来ると元の設定（管理設定）に戻ります。切り替えは人がここで設定したときだけ効き、自動では切り替えず、API課金へも切り替えません。',
    ),
    providerDisableSection(),
    body,
  );
  // 工程ごとの入力欄（forms）。描き直しても、利用者が入れかけた値は引き継ぐ（Issue #469）。done は設定・取り消しに
  // 成功した工程で、その工程だけは応答の内容で入れ直す。
  const forms = new Map<string, ModelOverrideForm>();
  holdReason = () => {
    for (const form of providerDisableForm?.values() || []) {
      if (
        form.until.value !== form.initialUntil ||
        form.reason.value !== form.initialReason
      )
        return '実行先の無効設定が未送信です';
    }
    return null;
  };
  let expiry: number | null | undefined = null,
    shown = 0;
  const show = (view: ModelOverrideView, done: string | null = null) => {
    if (!isCurrent(id)) return;
    shown++;
    cancelTimer(expiry);
    const drafts = modelOverrideDrafts(forms, done);
    forms.clear();
    body.replaceChildren(...modelOverrideSections(view, show, forms, drafts));
    // 開いた後に期限が来たら取り直す。取り直せなければ「期限切れの可能性」のまま示す。
    const wait = modelOverrideWait(view.overrides, Date.now());
    if (wait !== null) expiry = setTimeout(() => refresh(), wait);
  };
  async function refresh() {
    if (!isCurrent(id)) return;
    const at = shown;
    for (const form of forms.values()) form.mark();
    try {
      const view = await api<ModelOverrideView>('/api/model-overrides');
      // 待つ間に設定・取り消しで描き直した結果は、古い取り直しで上書きしない。
      if (at === shown) show(view);
    } catch {
      // 「期限切れの可能性」の表示を残す（開き直しを案内する）。
    }
  }
  try {
    show(await api<ModelOverrideView>('/api/model-overrides'));
  } catch (caughtValue) {
    const e = uiError(caughtValue);
    if (!isCurrent(id)) return;
    error(e);
    body.replaceChildren(
      el('div', {
        class: 'empty',
        text: 'モデルの切り替えを読み込めませんでした。接続状態を確かめて開き直してください。',
      }),
    );
  }
}
// 期限切れの可能性の表示（開いた後に期限が過ぎ、まだ取り直せていない切り替え）。
const modelOverrideExpiredText =
  '期限切れの可能性（期限を過ぎました。元の設定に戻っているはずです。開き直して確かめてください）';
const modelOverrideDue = (o: ModelOverride, now: number) =>
  Date.parse(o?.until ?? '') <= now;
// 次に期限が来る切り替えまでの待ち（ミリ秒。期限の1秒後に取り直す）。未来の期限が無ければ null。既に期限を過ぎた
// 項目（受付と端末の時計のずれ）は取り直しを繰り返さないよう待ちに数えない。setTimeout の上限（約24.8日）で切る。
function modelOverrideWait(overrides: ModelOverride[], now: number) {
  const times = (overrides || [])
    .map((o) => Date.parse(o?.until ?? ''))
    .filter((t) => Number.isFinite(t) && t > now);
  if (!times.length) return null;
  return Math.min(Math.min(...times) - now + 1000, 2 ** 31 - 1);
}
// 描き直す前の入力欄のうち、利用者が変えた値（描いたときの値と違うもの）。done の工程は含めない。
function modelOverrideDrafts(
  forms: Map<string, ModelOverrideForm>,
  done: string | null,
) {
  const drafts = new Map<string, Record<string, string>>();
  for (const [step, form] of forms) {
    if (step === done) continue;
    const edited = Object.entries(form.controls).filter(
      ([key, c]) => c.value !== form.initial[key],
    );
    if (edited.length)
      drafts.set(
        step,
        Object.fromEntries(edited.map(([key, c]) => [key, c.value])),
      );
  }
  return drafts;
}
function modelOverrideSections(
  view: ModelOverrideView,
  show: (view: ModelOverrideView, done?: string | null) => void,
  forms = new Map<string, ModelOverrideForm>(),
  drafts = new Map<string, Record<string, string>>(),
) {
  if (view.locked)
    return [
      el('p', {
        class: 'model-note',
        text: '工程の実行先は固定規則に従います。相談・起票・クローズ・優先度判定は相談元、修正は実装担当、レビューは実装と反対側です。変更できるのは実行先の無効指定だけです。',
      }),
    ];
  const active = new Map((view.overrides || []).map((o) => [o.step, o]));
  // replaceChildren へ渡すので null を含めない（ブラウザーは null を文字「null」として挿入する）。
  return [
    el('p', { class: 'model-note', text: view.note || overrideScopeNote }),
    ...(view.problem
      ? [
          el('div', {
            class: 'error-box',
            text: `切り替えの設定を一部または全部使えません: ${view.problem}`,
          }),
        ]
      : []),
    ...(view.steps || []).map((step) =>
      modelOverrideStep(
        step,
        active.get(step.id) || null,
        view,
        show,
        forms,
        drafts.get(step.id),
      ),
    ),
  ];
}
// 1つの工程の欄。今効いている切り替え（無ければ今の設定の値）と、設定の入力・取り消しのボタン。
// forms へ入力欄を登録し、draft（描き直す前に利用者が入れかけた値）があれば入れ直す。
function modelOverrideStep(
  step: { id: string; name: string },
  entry: ModelOverride | null,
  view: ModelOverrideView,
  show: (view: ModelOverrideView, done?: string | null) => void,
  forms = new Map<string, ModelOverrideForm>(),
  draft?: Record<string, string>,
) {
  const record = (rows: string[][]) =>
    el(
      'dl',
      { class: 'tier-record' },
      rows.flatMap(([label, value]) => [
        el('dt', { text: label }),
        el('dd', { text: value }),
      ]),
    );
  const name = `${step.name}（${step.id}）`;
  const provider = el(
    'select',
    { 'aria-label': `${step.name}の実行先` },
    ...(view.providers || []).map((p: string | number) =>
      el('option', { value: p, text: overrideProviderNames[p] || p }),
    ),
  );
  selectOption(
    provider,
    entry?.provider ||
      view.defaults?.[step.id]?.provider ||
      provider.options[0]?.value ||
      '',
  );
  const model = el('input', {
    'aria-label': `${step.name}のモデル`,
    maxlength: '100',
    autocomplete: 'off',
    spellcheck: 'false',
  });
  model.value = entry?.model || '';
  const effort = el(
    'select',
    { 'aria-label': `${step.name}の推論レベル` },
    el('option', { value: '', text: overrideNoEffort }),
    ...(view.efforts || []).map((e) => el('option', { value: e, text: e })),
  );
  selectOption(effort, entry?.effort || '');
  const until = el('input', {
    type: 'datetime-local',
    'aria-label': `${step.name}の期限（日本時間）`,
  });
  const controls: Record<string, HTMLInputElement | HTMLSelectElement> = {
    provider,
    model,
    effort,
    until,
  };
  const initial = Object.fromEntries(
    Object.entries(controls).map(([key, c]) => [key, c.value]),
  );
  for (const [key, value] of Object.entries(draft || {})) {
    if (key === 'provider' || key === 'effort')
      selectOption(key === 'provider' ? provider : effort, value);
    else if (controls[key]) controls[key].value = value;
  }
  const state = el('p', {
    class: entry ? 'model-state active' : 'model-state',
    text: entry
      ? modelOverrideDue(entry, Date.now())
        ? modelOverrideExpiredText
        : '切り替え中'
      : '切り替えなし（今の設定で動いています）',
  });
  forms.set(step.id, {
    controls,
    initial,
    // 期限が来た切り替えを、取り直すまで「期限切れの可能性」と示す。
    mark() {
      if (entry && modelOverrideDue(entry, Date.now()))
        state.textContent = modelOverrideExpiredText;
    },
  });
  const save = el('button', {
    type: 'button',
    class: 'primary',
    text: entry ? '設定し直す' : '切り替える',
  });
  save.onclick = () =>
    action(save, async () => {
      const deadline = jstDeadline(until.value);
      if (!deadline) throw Error('期限を日本時間の日時で入力してください。');
      const data = {
        provider: provider.value,
        model: model.value.trim(),
        ...(effort.value ? { effort: effort.value } : {}),
        until: deadline,
      };
      show(
        await api<ModelOverrideView>(
          `/api/model-overrides/${encodeURIComponent(step.id)}`,
          data,
        ),
        step.id,
      );
      toast(`${step.name}のモデルの切り替えを設定しました。`);
    });
  const cancel = entry
    ? el('button', { type: 'button', class: 'quiet', text: '取り消す' })
    : null;
  if (cancel)
    cancel.onclick = () =>
      action(cancel, async () => {
        const result = await api<ModelOverrideView>(
          `/api/model-overrides/${encodeURIComponent(step.id)}/cancel`,
          {},
        );
        show(result, step.id);
        toast(
          result.cancelled
            ? `${step.name}のモデルの切り替えを取り消しました。元の設定で動きます。`
            : `${step.name}の切り替えは既にありませんでした。`,
        );
      });
  return el(
    'section',
    { class: 'model-step', 'data-step': step.id },
    el('h2', { text: name }),
    state,
    entry
      ? [record(overrideRows(entry)), cancel]
      : record(overrideDefaultRows(step.id, view.defaults)),
    el(
      'div',
      { class: 'model-form' },
      field('実行先', provider),
      field('モデル', model),
      field('推論レベル（任意）', effort),
      field(
        '期限（日本時間）',
        until,
        `設定時から${view.maxDays || 7}日以内。期限が来ると元の設定に戻ります。`,
      ),
      save,
    ),
  );
}
