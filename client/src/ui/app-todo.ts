// 受付画面のTODO（相談前のアイディア。開く・編集・保存・相談を始める・削除）。client/src/ui/app.js の一部（Issue #457）で、
// 単独のモジュールではない。受付は client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。
// TODO（相談前のアイディア）。開く・編集する・削除するだけではAIを呼ばず、Giteaへも何も送らない。
// 個別URL（#/todos/<id>）で開く。navigate は issueView と同じ（true / 'replace' / false）。
async function todoView(
  todoId: string,
  notice: unknown = null,
  navigate: Navigation = true,
): Promise<void> {
  if (navigate !== false) remember(todoHash(todoId), navigate === 'replace');
  const id = reset();
  current = { type: 'todo', id: todoId };
  let t: Todo;
  try {
    t = await api<Todo>(`/api/todos/${todoId}`);
  } catch (caughtValue) {
    const e = uiError(caughtValue);
    if (!isCurrent(id)) return;
    // 相談を始めたTODOは下書きへ移っている（同じID）。消えたのではなく、行き先が変わっただけ。
    // 置き換えで移り、戻る操作がTODOのURLと相談の間で循環しないようにする。ほかの理由の409では移らない（Issue #514。
    // 受付の識別子は server/src/intake-draft-rules.mjs の startedCode）。
    if (e.status === 409 && e.code === 'todo_started')
      return draftView(
        todoId,
        Error('このTODOは相談を開始済みです。相談画面へ移りました。'),
        'replace',
      );
    todoTab();
    error(e);
    $('#app').replaceChildren(
      el('button', {
        class: 'back',
        onClick: () => home(),
        text: '← 依頼一覧',
      }),
      el('div', {
        class: 'empty',
        text:
          e.status === 404
            ? 'このTODOは見つかりません。削除済みか、URLのTODO IDが誤っています。'
            : 'このTODOを開けませんでした。一覧から開き直してください。',
      }),
    );
    return;
  }
  if (!isCurrent(id)) return;
  todoTab();
  renderTodo(t);
  if (notice) error(notice);
}
// 戻り先の一覧タブ。一覧から開いたときはそのタブのまま（強制しない）。URLから直接開いて
// 戻り先が決まっていないときだけ「TODO」タブにする（「← 依頼一覧」・削除後の一覧がTODOの並ぶタブになる）。
function todoTab() {
  if (!isListTab(tab)) tab = 'todos';
}

function todoReviewView(t: Todo, now = Date.now()) {
  const r = t.review;
  const labels: Record<string, string> = {
    resolved: '削除候補',
    unresolved: '相談を始める候補',
    unknown: '利用者へ',
  };
  if (
    r &&
    r.revision === t.revision &&
    r.dueAt === t.dueAt &&
    labels[r.verdict]
  )
    return field(
      '期限後の見直し',
      el(
        'div',
        {},
        el('div', { text: labels[r.verdict] }),
        el('div', { class: 'issue-body', text: r.reason }),
        (r.evidence || []).map((line) =>
          el('div', { class: 'issue-body', text: line }),
        ),
        el('div', { text: `見直した日時（UTC）: ${r.at}` }),
        hint('候補の表示だけで、削除や相談の開始は自動では行いません。'),
      ),
    );
  return now >= Date.parse(t.dueAt ?? '')
    ? hint('期限を過ぎています（見直し待ち）')
    : null;
}
function renderTodo(t: Todo) {
  // 保存・相談を始める・削除のボタンは、受付が返した actions だけで出し分ける（docs/intake-api.md）。
  const can = (name: string) => (t.actions || []).includes(name);
  const scope = { project: t.project, todo: t.id };
  const savedIds = () => (t.images || []).map((i) => i.id);
  // 保存済み画像の削除予約。本文・実行先と同じ編集状態の一部として、この端末に保存して復元する。
  let removed = new Set<string>();
  // 編集状態を復元し終えるまでは、画面の入力を保存内容と比べても意味がなく、保存もさせない。
  let restored = false;
  const text = el('textarea', {
    rows: 6,
    placeholder:
      '例：スマホで見たときに、一覧の文字が小さいので読みやすくしたい',
    'aria-label': 'TODOの内容',
  });
  const problemInput = el('textarea', {
    rows: 2,
    maxlength: 2000,
    'aria-label': '問題',
    disabled: true,
  });
  const resolvedInput = el('textarea', {
    rows: 2,
    maxlength: 2000,
    'aria-label': '解決の条件',
    disabled: true,
  });
  const methodsInput = el('textarea', {
    rows: 4,
    'aria-label': '確かめ方',
    disabled: true,
  });
  const dueInput = el('input', {
    type: 'date',
    'aria-label': '期限',
    disabled: true,
  });
  const savedCheck = {
    problem: t.check?.problem || '',
    resolved: t.check?.resolved || '',
    methods: (t.check?.methods || []).join('\n'),
    dueDate: (t.dueAt || '').slice(0, 10),
  };
  const checkEdit = () => ({
    problem: problemInput.value,
    resolved: resolvedInput.value,
    methods: methodsInput.value,
    dueDate: dueInput.value,
  });
  const checkChanged = () =>
    JSON.stringify(checkEdit()) !== JSON.stringify(savedCheck);
  for (const input of [problemInput, resolvedInput, methodsInput, dueInput]) {
    input.oninput = () => {
      rememberEdit();
      updateSaveState();
    };
  }
  const saveState = el('div', { class: 'preview-note' });
  const settings = providerField(
    t.provider || 'codex',
    '相談を始めるときの実行先',
    () => {
      rememberEdit();
      updateSaveState();
    },
  );
  const inputImages = composer(`todo:${t.id}`, text, false, () =>
    updateSaveState(),
  );
  // 相談を始めるときに依頼元（利用者）が選ぶ。TODOには保存しない。
  const tier = tierField();
  // 改名で依頼先の名前が変わったときの出口。入力を保持したまま新しい名前へ移す。
  const retarget = retargetNotice(inputImages, () => scope, updateSaveState);
  // 編集状態は端末に残るが、保存するまでは自動更新を待たせる。編集中の入力部品は保存済みの本文も持つため、
  // 入力部品の有無ではなく保存済みの内容（この画面の t。保存すると描き直して新しい内容が基準になる）と比べる。
  // 保存済みの内容を表示しているだけでは待たせない。復元できなかった入力欄は本文を表示していないので比べない。
  let restoring = true;
  holdReason = () => {
    if (restoring) return 'TODOの入力を復元中です';
    if (!restored) return null;
    const body = text.value !== (t.body || '');
    if (inputImages.imageCount() > 0 || (body && text.value))
      return composerHold;
    if (body) return 'TODOの本文の変更が未保存です';
    if (savedIds().some((id) => removed.has(id)))
      return 'TODOの画像の削除予定が未保存です';
    if (chosenProvider() !== (t.provider || 'codex'))
      return 'TODOの実行先の選択が未保存です';
    if (checkChanged()) return 'TODOの確認条件・期限が未保存です';
    return null;
  };
  // この端末の未保存入力が、どの版を元にしているか。開き直しただけでは付け替えない。
  const editedFrom = () => inputImages.baseRevision() ?? t.revision;
  // 実行先を1件も選べない設定では、選択欄が空になる。保存済みの実行先のままとして扱う。
  const chosenProvider = () =>
    settings.value().provider || t.provider || 'codex';
  // 未保存かどうかは、本文だけでなく追加した画像・保存済み画像の削除予約・実行先もまとめて比べる。
  const changed = () =>
    text.value !== (t.body || '') ||
    inputImages.imageCount() > 0 ||
    savedIds().some((id) => removed.has(id)) ||
    chosenProvider() !== (t.provider || 'codex') ||
    checkChanged();
  const stale = () => changed() && editedFrom() !== t.revision;
  // 競合したときの出口。保存済みの内容を見たうえで、利用者が押したときだけ最新版へ付け替える。
  const rebaseSave = el('button', {
    class: 'quiet danger',
    type: 'button',
    text: '最新に合わせて保存',
    hidden: true,
  });
  function rememberEdit() {
    if (!restored || !inputImages.valid()) return;
    inputImages
      .setEdit({
        removed: [...removed],
        provider: chosenProvider(),
        todoCheck: checkEdit(),
      })
      .catch(() => {});
  }
  // 端末内だけの未保存入力と、受付側への保存完了を混同させない。
  function updateSaveState() {
    if (!inputImages.valid()) return;
    const conflict = restored && stale();
    saveState.textContent = !restored
      ? 'この端末に残っている編集内容を読み込んでいます…'
      : conflict
        ? 'この端末の未保存入力は、受付の今の保存内容より前の版を元にしています。右の「受付に保存済みの内容」を確かめ、この入力で置き換えてよければ「最新に合わせて保存」を押してください。'
        : changed()
          ? 'この端末にだけある未保存の入力です。「保存」すると受付に残り、認証済みの別の端末からも見えます。'
          : `受付に保存済みです（${date(t.updatedAt)}）。この端末に未保存の入力はありません。`;
    saveState.className = conflict ? 'preview-note error-box' : 'preview-note';
    rebaseSave.hidden = !conflict;
  }
  updateSaveState();
  inputImages.ready
    .then(async () => {
      if (!inputImages.valid()) return;
      // 応答を失った保存が実は受理されていた場合に備えて、受付の内容と突き合わせてから表示する。
      await inputImages.adoptTodo(t);
      if (!inputImages.valid()) return;
      // 本文・削除予約・実行先をまとめて復元する。初めて開いたときだけ、保存済みの内容を編集の種にする。
      const edit = await inputImages.startEdit(
        {
          text: t.body || '',
          removed: [],
          provider: t.provider || 'codex',
          todoCheck: savedCheck,
        },
        t.revision,
      );
      if (!inputImages.valid()) return;
      // 保存済みでなくなった画像の予約は持ち越さない（別の画像を巻き添えにしない）。
      removed = new Set(
        (edit.removed || []).filter((id) => savedIds().includes(id)),
      );
      settings.set(edit.provider || t.provider || 'codex');
      const check = edit.todoCheck || savedCheck;
      problemInput.value = check.problem;
      resolvedInput.value = check.resolved;
      methodsInput.value = check.methods;
      dueInput.value = check.dueDate;
      restored = true;
      for (const input of [problemInput, resolvedInput, methodsInput, dueInput])
        input.disabled = false;
      renderSaved();
      // 編集の元にした版は、種を作ったとき（startEdit が設定）と、受付の保存内容と違いが無いときだけ最新へ合わせる。
      if (!edit.seeded && !changed()) await inputImages.rebase(t.revision);
      if (!inputImages.valid()) return;
      retarget.update();
      updateSaveState();
    })
    .catch(() => {})
    // 復元に失敗した入力欄は操作できないので、本文の比較はせずに保留だけを解く。
    .finally(() => (restoring = false));
  const savedImages = el('div', { class: 'image-gallery' });
  function renderSaved() {
    if (!(t.images || []).length)
      return savedImages.replaceChildren(hint('保存済みの画像はありません。'));
    savedImages.replaceChildren(
      ...(t.images ?? []).map((i) => {
        const url = `/api/images/${i.id}?${new URLSearchParams(scope)}`;
        const gone = removed.has(i.id);
        return el(
          'figure',
          {},
          el(
            'a',
            { href: url, target: '_blank', rel: 'noreferrer' },
            el('img', { src: url, alt: `添付画像 ${i.id}`, loading: 'lazy' }),
          ),
          el('figcaption', {
            text: `画像 ${i.id.slice(0, 8)}${gone ? '（保存で削除）' : ''}`,
          }),
          el('button', {
            type: 'button',
            class: 'quiet',
            text: gone ? '削除をやめる' : '削除',
            // 復元前の予約は、直後の復元で上書きされてしまう。読み込みが終わるまでは受け付けない。
            disabled: !restored,
            onClick: () => {
              if (!restored) return;
              if (gone) removed.delete(i.id);
              else removed.add(i.id);
              renderSaved();
              rememberEdit();
              updateSaveState();
            },
          }),
        );
      }),
    );
  }
  renderSaved();
  // 保存は受付側の永続ストアへ書くだけ。AIも起票も呼ばない。
  async function persist() {
    // 復元前の空の編集欄で保存すると、保存済みの本文と画像を消してしまう。
    if (!restored)
      throw Error(
        'この端末に残っている編集内容を読み込んでいます。表示が切り替わってから保存してください。',
      );
    await inputImages.setEdit({ todoCheck: checkEdit() });
    await inputImages.checkpoint();
    const kept = (t.images || [])
      .filter((i) => !removed.has(i.id))
      .map((i) => i.id);
    checkTodoImages(kept, inputImages);
    try {
      const message = await inputImages.prepare(scope);
      const request = {
        id: message.id,
        // 編集を始めた版で送る。その間に別の端末が保存していれば受付が断り、黙って上書きしない。
        revision: editedFrom(),
        text: message.text,
        images: [...kept, ...message.images],
        provider: chosenProvider(),
        ...(problemInput.value || resolvedInput.value || methodsInput.value
          ? {
              check: {
                problem: problemInput.value,
                resolved: resolvedInput.value,
                methods: methodsInput.value
                  ? methodsInput.value.split(/\r?\n/)
                  : [],
              },
            }
          : t.check
            ? { check: null }
            : {}),
        ...(dueInput.value !== savedCheck.dueDate
          ? { dueAt: dueInput.value ? dueInput.value + 'T00:00:00.000Z' : null }
          : {}),
      };
      const checkSnapshot = checkEdit();
      await inputImages.setTodoPending(checkSnapshot, !('dueAt' in request));
      const stored = await inputImages.live(
        api<Todo>(`/api/todos/${t.id}/save`, request),
      );
      await inputImages.clear();
      return stored;
    } finally {
      inputImages.release();
    }
  }
  async function started() {
    try {
      await api(`/api/drafts/${t.id}`);
      return true;
    } catch {
      return false;
    }
  }
  const save = el('button', { class: 'primary', type: 'button', text: '保存' });
  async function saveAndShow() {
    const stored = await persist();
    if (!inputImages.valid()) return;
    Object.assign(t, stored);
    renderTodo(t);
  }
  save.onclick = () => action(save, saveAndShow);
  // 競合の解消は利用者の判断。押して初めて、この端末の入力を最新版の続きとして保存する。
  rebaseSave.onclick = () =>
    action(rebaseSave, async () => {
      await inputImages.rebase(t.revision);
      if (!inputImages.valid()) return;
      await saveAndShow();
    });
  const start = el('button', {
    class: 'primary',
    type: 'button',
    text: 'AIと相談を始める →',
  });
  start.onclick = () =>
    action(start, async () => {
      try {
        tier.require();
        // 最新の本文・画像・実行先を保存してから渡す。ここで初めてAIが動く。
        const latest = await persist();
        if (!inputImages.valid()) return;
        await inputImages.live(
          api(`/api/todos/${t.id}/start`, {
            revision: latest.revision,
            provider: chosenProvider(),
            ...tier.value(),
          }),
        );
      } catch (caughtValue) {
        const e = uiError(caughtValue);
        if (!inputImages.valid()) throw e;
        // 応答を失った再試行で二重に開始しない。開始済みならその相談を開く。
        // TODOのURLは相談へ置き換える（戻るで開始済みのTODOを経由せず、元の一覧へ戻れる）。
        if (await started()) {
          if (inputImages.valid()) await draftView(t.id, null, 'replace');
          return;
        }
        if (!inputImages.valid()) throw e;
        await todoView(t.id, e);
        return;
      }
      if (!inputImages.valid()) return;
      mobileView = 'chat';
      await draftView(t.id, null, 'replace');
    });
  const remove = el('button', {
    class: 'quiet danger',
    type: 'button',
    text: 'このTODOを削除',
  });
  remove.onclick = (e) => {
    const button = e.currentTarget as HTMLButtonElement;
    if (button.dataset.armed !== '1') {
      button.dataset.armed = '1';
      button.textContent = 'もう一度押すと削除します';
      return;
    }
    return action(button, async () => {
      await api(`/api/todos/${t.id}/remove`, { revision: t.revision });
      await inputImages.clear();
      if (!inputImages.valid()) return;
      tab = 'todos';
      await home();
    });
  };
  const editor = el(
    'section',
    { class: 'panel chat-panel' },
    el(
      'div',
      { class: 'panel-head' },
      el('h2', { text: 'TODOを編集' }),
      badge('saved_todo'),
    ),
    el(
      'div',
      { class: 'composer' },
      saveState,
      retarget.node,
      text,
      field('問題', problemInput),
      field('解決の条件', resolvedInput),
      field(
        '確かめ方',
        methodsInput,
        '1行に1つ、最大10行。ファイルあり/なし: パス、文字あり/なし: パス :: 語、Issue完了/未完了: 共通番号N、コマンド: 文（実行しません）。条件を消すときは3欄を空にしてください。',
      ),
      field(
        '期限（UTC）',
        dueInput,
        '保存時刻の1〜30日先。変更時は指定日のUTC 0時を送ります。初回は省略すると7日後になります。',
      ),
      inputImages.node,
      meta.providers.length > 1 ? settings.node : null,
      can('start') ? tier.node : null,
      el(
        'div',
        { class: 'composer-actions' },
        hint(
          '保存・編集・削除ではAIを呼ばず、Giteaへの起票も実装も始まりません。',
        ),
        can('save') ? rebaseSave : null,
        can('save') ? save : null,
        can('start') ? start : null,
      ),
    ),
  );
  const saved = el(
    'aside',
    { class: 'panel draft-panel' },
    el(
      'div',
      { class: 'panel-head' },
      el('h2', { text: '受付に保存済みの内容' }),
    ),
    el(
      'div',
      { class: 'panel-body' },
      hint(`${shownProject(t.project)} · 更新 ${date(t.updatedAt)}`),
      // 最後にPC別トークンから保存したときだけ、そのPC名を出す（画面・一時キーの保存で消える。Issue #1305）。
      // PC名は textContent で表示し、HTMLとして解釈しない。
      typeof t.savedBy === 'string' && t.savedBy
        ? hint(`PC別トークン ${t.savedBy} が保存`)
        : null,
      t.url
        ? field(
            'URL',
            // 下書きIDと同じ見た目（省略せず折り返し、選んでコピーできる）。
            el('code', { class: 'draft-id todo-url', text: t.url }),
            'このTODOを指し示すURLです。AIとの会話や別の端末へ貼ると同じTODOを開けます。開くには受付の認証とプロジェクトの許可が必要です。',
          )
        : null,
      field(
        '本文',
        el('div', { class: 'issue-body', text: t.body || '（未入力）' }),
      ),
      field(
        '問題',
        el('div', {
          class: 'issue-body',
          text: t.check?.problem || '（未設定）',
        }),
      ),
      field(
        '解決の条件',
        el('div', {
          class: 'issue-body',
          text: t.check?.resolved || '（未設定）',
        }),
      ),
      field(
        '確かめ方',
        el('div', {
          class: 'issue-body',
          text: (t.check?.methods || []).join('\n') || '（未設定）',
        }),
      ),
      field('期限（UTC）', el('div', { text: t.dueAt || '（未設定）' })),
      todoReviewView(t),
      field('添付画像', savedImages),
      hint(
        '相談を始めると、この内容と実行先を引き継ぎ、TODO一覧から相談へ移ります。',
      ),
      el('button', {
        class: 'quiet',
        type: 'button',
        text: '最新の内容を読み込む',
        onClick: () => todoView(t.id),
      }),
      can('remove') ? remove : null,
    ),
  );
  const workspace = el(
    'div',
    { class: 'workspace', 'data-view': mobileView },
    editor,
    saved,
  );
  const tabs = el(
    'div',
    { class: 'mobile-tabs' },
    ...[
      ['chat', '編集'],
      ['draft', '保存済み'],
    ].map(([v, name]) =>
      el('button', {
        class: mobileView === v ? 'active' : '',
        text: name,
        onClick: () => {
          mobileView = v;
          workspace.dataset.view = v;
          for (const b of tabs.children)
            b.classList.toggle('active', b.textContent === name);
        },
      }),
    ),
  );
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    head('TODO', todoHeadline(t.body), shownProject(t.project)),
    tabs,
    workspace,
  );
}
