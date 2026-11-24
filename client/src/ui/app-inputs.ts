// 受付画面の入力欄の部品（入力と画像の部品・実行先・優先度・難易度の選択と表示・依頼先の付け替え）。client/src/ui/app.js の
// 一部（Issue #456）で、単独のモジュールではない。受付は client/scripts/build.ts の順に app.js とつないで配信する。
let activeComposer: ComposerController | null = null;
const gallery = (
  images: ImageDescriptor[] | null | undefined,
  scope?: ComposerScope,
) => imageGallery(el, images, scope);
function composer(
  key: string,
  text: HTMLTextAreaElement,
  locked = false,
  onDirty = () => {},
) {
  const id = routeId;
  activeComposer?.dispose();
  return (activeComposer = imageComposer({
    key,
    text,
    el,
    api,
    isCurrent: () => isCurrent(id),
    error: (e: unknown) => {
      if (isCurrent(id)) error(e);
    },
    locked,
    onDirty,
  }));
}
// 1件のTODOが持てる画像は1メッセージ分まで。アップロードの前に確かめ、保存済みの画像を消さずに止める。
function checkTodoImages(
  kept: readonly string[],
  composer: ComposerController,
) {
  if (kept.length + composer.imageCount() > 4)
    throw Error(
      '画像は1件のTODOに4枚までです。保存済みの画像は「削除」を選んで保存してから追加してください。',
    );
}
// 依頼先の名前が変わると、この端末に残った未送信入力は古い名前を持ったままになり、保存も送信も断られる。
// 入力を捨てずに新しい依頼先へ移すための回復操作。利用者が押したときだけ付け替え、別の依頼へは移さない。
function retargetNotice(
  input: ComposerController,
  target: () => ComposerScope,
  done = () => {},
) {
  const box = el('div', { class: 'error-box retarget-notice', hidden: true });
  function update() {
    if (!input.valid()) return;
    const scope = target();
    const from = input.renamedFrom(scope);
    box.hidden = !from;
    box.replaceChildren();
    if (!from) return;
    const move = el('button', {
      type: 'button',
      class: 'quiet',
      text: '入力を新しい依頼先へ移す',
    });
    move.onclick = () =>
      action(move, async () => {
        await input.retarget(target());
        if (!input.valid()) return;
        update();
        done();
      });
    box.append(
      el('p', {
        text: `この端末に残っている未送信の入力は、以前の依頼先「${shownProject(from)}」のものです。名前が「${shownProject(scope.project)}」へ変わった場合は、入力と画像を保持したまま移せます。`,
      }),
      move,
    );
  }
  input.ready.then(update).catch(() => {});
  return { node: box, update };
}
// この端末の前回の選択だけを覚える。本文・資格情報はここへ入れず、サーバーにも既定値を持たせない。
const defaultsKey = 'issue-desk:new-request';
function savedDefaults() {
  try {
    const saved = JSON.parse(localStorage.getItem(defaultsKey) || 'null');
    if (!saved || typeof saved !== 'object') return {};
    return {
      project: (meta.projects || []).some((p) => p.id === saved.project)
        ? saved.project
        : '',
      provider: (meta.providers || []).some((p) => p.id === saved.provider)
        ? saved.provider
        : 'codex',
    };
  } catch {
    return {};
  }
}
function rememberDefaults(project: string | null, provider: string) {
  try {
    localStorage.setItem(defaultsKey, JSON.stringify({ project, provider }));
  } catch {}
}
// The requester picks who implements; model and reasoning effort are fixed per provider on the server.
function providerField(
  provider: string,
  label = '3. どのAIに実装させますか？',
  onChange = () => {},
) {
  const providers = el(
    'select',
    { 'aria-label': '実行先' },
    meta.providers.map((p) =>
      el('option', { value: p.id, text: p.name, selected: p.id === provider }),
    ),
  );
  const warning = el('div', { class: 'error-box', hidden: true });
  function update() {
    // 選択肢に残っていても実行できない実行先は警告を出す。自動では切り替えない。
    const p = meta.providers.find((x) => x.id === providers.value);
    const blocked = !!p && !p.ready;
    warning.textContent = blocked
      ? `${p.name}は現在実行できません。このまま依頼すると失敗として通知されます（他の実行先へは切り替えません）。
${p.reason}`
      : '';
    warning.hidden = !blocked;
  }
  update();
  providers.onchange = () => {
    update();
    onChange();
  };
  return {
    node: el(
      'div',
      {},
      field(
        label,
        providers,
        'モデルと推論レベルは自動で決まります。相談先と実装先は別に選べます。起票・クローズは相談先、レビューは実装と反対側のAIが担当します。無効なAIは使いません。',
      ),
      warning,
    ),
    value: () => ({ provider: providers.value }),
    // 復元した編集状態の実行先を、選び直したときと同じように表示へ反映する（選択は変えられるまま）。
    set(id: unknown) {
      if (!meta.providers.some((p) => p.id === id)) return;
      selectOption(providers, id);
      update();
    },
    // 保存済みの再試行は記憶した既定値より優先する。表示と実際の実行先をずらさない。
    fix(id: unknown) {
      selectOption(providers, id);
      providers.disabled = true;
      update();
    },
  };
}
// 利用者が選べる優先度。「自動」は監視のAIが判定する（既定）。割り込みの乱発を防ぐため、端末に既定値を覚えない。
// 利用者の指定 high は「割り込み」、AIの判定の high は「優先」と表記する（送る値はどちらも high のまま）。
const userPriorityNames: Record<string, string> = {
  high: '割り込み',
  normal: '通常',
};
function priorityField() {
  const select = el(
    'select',
    { 'aria-label': '優先度' },
    el('option', { value: '', text: '自動（既定）', selected: true }),
    el('option', { value: 'high', text: '割り込み' }),
    el('option', { value: 'normal', text: '通常' }),
  );
  return {
    node: field(
      '優先度',
      select,
      '急ぎの依頼だけ「割り込み」を選びます。実行中の作業は中断せず、AIの判定やフォローアップの区分より先に着手します。「自動」は監視のAIが判定します。起票後も実行待ちの間は依頼の画面から変更できます。',
    ),
    value: () => (select.value ? { priority: select.value } : {}),
    // 保存済みの再試行は、そのときの選択のまま送る。
    fix(value: string | undefined) {
      selectOption(select, value || '');
      select.disabled = true;
    },
  };
}
// 「相談の難易度」（Issue #430）。受付の相談を管理設定（intake.tiers）のその難易度で行う。依頼元が依頼の難しさで必ず選ぶ
// （受付は難易度を判定しない）。選ばないと相談を始められない。優先度と同じく端末に既定値を覚えない。
function tierField() {
  const select = el(
    'select',
    { 'aria-label': '相談の難易度' },
    el('option', { value: '', text: '選んでください', selected: true }),
    el('option', { value: 'light', text: '低' }),
    el('option', { value: 'standard', text: '中' }),
    el('option', { value: 'heavy', text: '高' }),
  );
  return {
    node: field(
      '相談の難易度',
      select,
      '依頼の難しさで選びます。受付の相談をその難易度のモデル・推論レベルで行います（管理側で難易度が設定されているとき）。迷ったら「中」、要件の確定が難しい依頼は「高」。実装のモデルは変わりません。「TODOとして保存」では残らず、TODOから始めるときに選びます。',
    ),
    value: () => (select.value ? { tier: select.value } : {}),
    // 相談を始める前に呼ぶ。未選択なら理由を示して止める。
    require() {
      if (!select.value)
        throw Error('相談の難易度（低・中・高）を選んでください。');
    },
    // 保存済みの再試行は、そのときの選択のまま送る。難易度を必須にする前に保存した入力（値なし）は選び直せるよう残す。
    fix(value: string | undefined) {
      if (!value) return;
      selectOption(select, value);
      select.disabled = true;
    },
  };
}
// 「実装の難易度」（Issue #487）。実装のモデル・推論レベルを管理設定（implementation.tiers）の難易度にする。「指定なし」
// （既定）は何も送らず、受付が起票時に判定する。優先度と同じく端末に既定値を覚えない。
function implementationTierField() {
  const select = el(
    'select',
    { 'aria-label': '実装の難易度' },
    el('option', { value: '', text: '指定なし（受付が判定）', selected: true }),
    el('option', { value: 'light', text: '低' }),
    el('option', { value: 'standard', text: '中' }),
    el('option', { value: 'heavy', text: '高' }),
  );
  return {
    node: field(
      '実装の難易度',
      select,
      '実装に使うモデル・推論レベルの難易度です（管理側で難易度が設定されているとき）。「指定なし」は受付が変更範囲と受入条件の数から決めます。受付の相談の難易度とは別です。',
    ),
    value: () => (select.value ? { implementationTier: select.value } : {}),
    // 保存済みの再試行は、そのときの選択のまま送る。
    fix(value: string | undefined) {
      selectOption(select, value || '');
      select.disabled = true;
    },
  };
}
// 相談画面の難易度の記録（client/src/ui/request-list.js の tierRows）。記録の無い導入前の下書きには出さない。
// どのモデルの切り替えで動いたか（下書きの modelOverrides。Issue #442）があれば併記する。難易度の記録が無く切り替えの
// 記録だけがある下書きは、見出しを「相談のモデルの切り替え」にする（Issue #469）。
function tierRecord(tier?: TierRecord, overrides?: ModelOverride[]) {
  const rows = tierRows(tier, overrides);
  if (!rows) return null;
  return fieldBlock(
    tier && typeof tier === 'object'
      ? '相談の難易度'
      : '相談のモデルの切り替え',
    el(
      'dl',
      { class: 'tier-record' },
      rows.flatMap(([label, value]) => [
        el('dt', { text: label }),
        el('dd', { text: value }),
      ]),
    ),
  );
}
