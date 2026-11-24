// 受付画面の新しい依頼（newDraft。共有シートからの貼り付け・TODOとしての保存・新しいプロジェクトの相談の開始）。
// client/src/ui/app.js の一部（Issue #456）で、単独のモジュールではない。受付は client/scripts/build.ts の順に app.js と
// つないだ1つのモジュールとして /app.js を配信する。
// クリップボードへのアクセスを1か所にまとめ、利用者の操作から直接読む。
const clipboardSource = () =>
  typeof navigator === 'undefined' ? null : navigator.clipboard;
// 通常の貼り付けと同じく選択範囲へ挿入し、選択していない本文は消さない。
function insertText(node: HTMLTextAreaElement, value: string) {
  const start =
    typeof node.selectionStart === 'number'
      ? node.selectionStart
      : node.value.length;
  const end =
    typeof node.selectionEnd === 'number'
      ? node.selectionEnd
      : node.value.length;
  node.value = node.value.slice(0, start) + value + node.value.slice(end);
  node.selectionStart = node.selectionEnd = start + value.length;
  node.focus?.();
}
const pasteHelp = '入力欄へ直接貼り付けるか、手入力してください。';
// プロジェクトの選択肢の「新しいプロジェクト」。登録済みの名前（owner/repo）とは重ならない値にする。
const newProjectValue = '__new_project__';
// プロジェクトの選択肢の「該当無し（プロジェクト未定）」（Issue #689）。登録済みの名前（owner/repo）・空文字（未選択）・
// 新しいプロジェクトの値と重ならない値にする。受付へは project を null にし unassigned: true を付けて送る。
const unassignedValue = '__unassigned__';
const unassignedText = 'プロジェクト未定';
// 相談がプロジェクト未定で始まったか（受付の下書きの unassigned。判定は server/src/intake-actions.mjs の unassignedDraft と同じ）。
const unassignedDraft = (
  d: Pick<Draft, 'unassigned' | 'project'> | null | undefined,
) => d?.unassigned === true && !d?.project;
// 相談画面の見出しのプロジェクト。プロジェクト未定の相談は、起票するまで「プロジェクト未定」、起票後は起票した
// リポジトリを並べる。
function draftProjectText(d: Draft) {
  if (!unassignedDraft(d)) return shownProject(d.project);
  const filed = [
    ...new Set(
      (d.issues || (d.issue ? [d.issue] : []))
        .map((i) => i.project)
        .filter(Boolean),
    ),
  ];
  return filed.length
    ? filed.map((p) => shownProject(p)).join('・')
    : unassignedText;
}
const projectChoiceHelp =
  '依頼するプロジェクトを選びます。どこに置くべきか分からない・複数のプロジェクトにまたがるときは「該当無し（プロジェクト未定）」を選んでください。該当無しでは、AIが登録済みのプロジェクト（受付の許可リスト）の中から対象を判断して理由・対象・変更範囲を示し、返信で了承するまで起票しません。新しいリポジトリが要るときは、新しいプロジェクトの相談へ引き継ぎます。プロジェクトを選んだ相談でも、別のプロジェクトにも変更が必要なときは、AIが理由と対象を説明し、返信で了承するまで起票しません。';
function newDraft({
  share = false,
  navigate = true,
  preset = null,
}: { share?: boolean; navigate?: Navigation; preset?: string | null } = {}) {
  if (navigate) remember(`#/list/${tab || 'attention'}`);
  const id = reset();
  // 一覧のボタンから開くとURLは一覧のまま。画面の自動更新で戻せるよう、表示中の画面として覚える。
  current = { type: 'new', share };
  const defaults = savedDefaults();
  // 管理側で projectCreation を設定したときだけ「新しいプロジェクト」を選べる（server/src/project-intake.mjs）。
  const creation = meta.projectCreation;
  const project = el(
    'select',
    { 'aria-label': 'プロジェクト', required: true },
    el('option', { value: '', text: '依頼するプロジェクトを選択' }),
    meta.projects.map((p) =>
      el('option', { value: p.id, text: projectOptionName(p) }),
    ),
    el('option', {
      value: unassignedValue,
      text: '該当無し（プロジェクト未定。AIが対象を判断）',
    }),
    creation
      ? el('option', {
          value: newProjectValue,
          text: '＋ 新しいプロジェクト（リポジトリを作る）',
        })
      : null,
  );
  if (defaults.project) selectOption(project, defaults.project);
  if (preset && creation) selectOption(project, preset);
  const settings = providerField(defaults.provider || 'codex');
  const consultation = providerField('codex', '相談するAI');
  const consultationNode = consultation.node;
  const priority = priorityField();
  const tier = tierField();
  const implementationTier = implementationTierField();
  // 開いたときの選択から変えたプロジェクト・実行先・優先度・難易度は、相談を始めるまで端末に残らない。自動更新を待たせる。
  const selection = () =>
    `${project.value}\n${settings.value().provider}\n${consultation.value().provider}`;
  let chosen = selection();
  let chosenPriority = priority.value().priority;
  let chosenTier = tier.value().tier;
  let chosenImplementationTier = implementationTier.value().implementationTier;
  holdReason = () => {
    if (selection() !== chosen) return 'プロジェクト・実行先の選択が未送信です';
    if (priority.value().priority !== chosenPriority)
      return '優先度の選択が未送信です';
    if (tier.value().tier !== chosenTier)
      return '「相談の難易度」の選択が未送信です';
    if (
      implementationTier.value().implementationTier !== chosenImplementationTier
    )
      return '「実装の難易度」の選択が未送信です';
    return null;
  };
  // 起票はこの受付だが実装は別のPCが行うプロジェクトがある。依頼を書く前に分かるようにする。
  const hostHint = hint('');
  function updateHostHint() {
    // 新しいプロジェクトの相談は難易度の指定を送らない（tiers があれば常に高。server/src/intake-tiers.mjs の projectSettings）。
    tier.node.hidden = !!creation && project.value === newProjectValue;
    if (creation && project.value === newProjectValue) {
      hostHint.textContent = `新しいリポジトリ（${creation.owner}/<名前>）の定義をAIと相談します。配置先は A9 MAX か NAS（NAS は Node.js だけで、自動デプロイなし）です。確定した定義を確認して「リポジトリを作成する」を押すまで、リポジトリは作りません。画像の添付とTODOへの保存はできません。`;
      hostHint.hidden = false;
      return;
    }
    if (project.value === unassignedValue) {
      hostHint.textContent =
        'プロジェクト未定で相談します。AIが登録済みのプロジェクトから対象を判断し、理由・対象・変更範囲を示します。返信で了承するまで起票しません。画像の添付とTODOへの保存はできません。';
      hostHint.hidden = false;
      return;
    }
    const p = meta.projects.find((x) => x.id === project.value);
    hostHint.textContent =
      p?.host && meta.workerId && p.host !== meta.workerId
        ? `このプロジェクトの実装は ${p.host} が担当します。`
        : '';
    hostHint.hidden = !hostHint.textContent;
  }
  updateHostHint();
  const text = el('textarea', {
    rows: 5,
    placeholder:
      '例：スマホで見たときに、一覧の文字が小さいので読みやすくしたい',
    required: true,
    'aria-label': '変更したいこと',
  });
  const inputImages = composer('new-request', text);
  // 保存済みの送信先が今の一覧から消えているときだけ出す案内。入力は残したまま選び直させる。
  const missing = el('div', { class: 'error-box', hidden: true });
  const retarget = retargetNotice(
    inputImages,
    // 該当無しはプロジェクトの改名の移し先にならない（project が無い）。
    () => ({
      ...(inputImages.scope() || {}),
      project: project.value === unassignedValue ? null : project.value,
    }),
    () => {
      missing.hidden = true;
      missing.textContent = '';
    },
  );
  project.onchange = () => {
    updateHostHint();
    retarget.update();
  };
  inputImages.ready
    .then(() => {
      if (!inputImages.valid()) return;
      const saved = inputImages.scope() || inputImages.creation();
      if (saved) {
        // 改名などで保存済みの依頼先が一覧から消えた場合は固定しない。固定すると選び直せず、
        // この端末に残った入力をどこへも送れなくなる。プロジェクト未定の相談の入力（project が null）は該当無しに固定する。
        if (saved.project === null) {
          selectOption(project, unassignedValue);
          project.disabled = true;
        } else if (meta.projects.some((p) => p.id === saved.project)) {
          selectOption(project, saved.project);
          project.disabled = true;
        } else {
          missing.textContent = `この端末に残っている入力の依頼先「${shownProject(saved.project)}」は、いまの一覧にありません。名前が変わった場合は、新しい依頼先を選んでから「入力を新しい依頼先へ移す」を押してください。`;
          missing.hidden = false;
        }
        updateHostHint();
        retarget.update();
      }
      const creation = inputImages.creation();
      if (creation) {
        settings.fix(creation.provider);
        consultation.fix(creation.consultationProvider ?? 'codex');
        settings.node.append(
          hint(`保存済みの実行先で再試行：${providerName(creation.provider)}`),
        );
        priority.fix(creation.priority);
        tier.fix(creation.tier);
        implementationTier.fix(creation.implementationTier);
      }
      // 保存済みの入力から戻した選択は利用者の変更ではない。
      if (saved || creation) {
        chosen = selection();
        chosenPriority = priority.value().priority;
        chosenTier = tier.value().tier;
        chosenImplementationTier =
          implementationTier.value().implementationTier;
      }
    })
    .catch(() => {});
  const pasteNote = el('small', { class: 'hint share-note', hidden: true });
  const pasteNotice = (message: string) => {
    pasteNote.textContent = message;
    pasteNote.hidden = !message;
  };
  const pasteHint = hint(
    '共有シートでコピーした本文を貼り付けます。送信はしません。',
  );
  const pasteButton = el('button', {
    type: 'button',
    class: 'quiet',
    text: 'クリップボードから貼り付け',
  });
  const sharePaste = share
    ? el('div', { class: 'share-paste' }, pasteButton, pasteHint, pasteNote)
    : null;
  // 自動読取はしない。利用者が押したときだけ読み、失敗してもフォームは使えるままにする。
  pasteButton.onclick = async () => {
    const owner = inputImages,
      here = () => isCurrent(id) && owner === activeComposer && owner.valid();
    const source = clipboardSource();
    if (!source || typeof source.readText !== 'function') {
      pasteNotice(
        `このブラウザーはクリップボードの読み取りに対応していません。${pasteHelp}`,
      );
      return;
    }
    let reading;
    try {
      reading = source.readText();
    } catch {
      pasteNotice(`クリップボードを読み取れませんでした。${pasteHelp}`);
      return;
    }
    pasteButton.disabled = true;
    operations++;
    try {
      const value = await reading;
      // 復元前・送信中の入力へは書き込まない。遅れて届いた結果で別画面や新しい入力も上書きしない。
      let restored = true;
      await owner.ready.catch(() => (restored = false));
      if (!here()) return;
      if (!restored || text.disabled) {
        pasteNotice(
          `いま入力欄へ書き込めません。復帰後にもう一度押すか、${pasteHelp}`,
        );
        return;
      }
      if (!value) {
        pasteNotice(`クリップボードが空でした。${pasteHelp}`);
        return;
      }
      insertText(text, value);
      text.dispatchEvent(new Event('input'));
      pasteNotice('');
    } catch {
      if (here())
        pasteNotice(`クリップボードを読み取れませんでした。${pasteHelp}`);
    } finally {
      operations--;
      if (here()) pasteButton.disabled = false;
    }
  };
  const start = el('button', {
    class: 'primary',
    type: 'submit',
    text: 'AIと相談を始める →',
  });
  // 「AIと相談を始める」とは別の出口。押してもAIは動かず、起票も実装も始まらない。
  const keep = el('button', {
    class: 'quiet',
    type: 'button',
    text: 'TODOとして保存',
  });
  // 保存後はその場で編集しないので一覧へ戻る。タブは開いたときのまま（直接開いたときは初期表示の規則）。
  // 呼ぶのは保存を確認でき、この画面がまだ表示中のときだけ。遅れて届いた結果で別画面を動かさない。
  // 難易度の指定はTODOに残らない（TODOから始めるときに選ぶ）。選んだまま保存したときは捨てたことを知らせる。
  const savedToList = () => {
    toast(
      tier.value().tier
        ? 'TODOを保存しました（相談の難易度は保存されません。TODOから相談を始めるときに選びます）'
        : 'TODOを保存しました',
    );
    return home('replace');
  };
  keep.onclick = () =>
    action(keep, async () => {
      if (!project.value) throw Error('プロジェクトを選んでください。');
      if (project.value === newProjectValue)
        throw Error(
          '新しいプロジェクトはTODOに保存できません。「AIと相談を始める」から相談してください。',
        );
      if (project.value === unassignedValue)
        throw Error('TODOはプロジェクトを1つ選んで保存します。');
      await inputImages.checkpoint();
      if (!inputImages.hasContent())
        throw Error('保存する内容を入力してください。');
      const saved = inputImages.scope();
      if (saved && !saved.todo)
        throw Error(
          'この入力は相談へ送る途中です。「AIと相談を始める」から続けてください。',
        );
      const todoId = saved?.todo || inputImages.messageId();
      const created = await inputImages.live(
        api<Todo>('/api/todos', {
          id: todoId,
          project: project.value,
          ...settings.value(),
          consultationProvider: consultation.value().provider,
        }),
      );
      // 前回の保存の応答を失っていた場合の突き合わせ。受理済みならこの入力は受付にあり、受理後に
      // 書き足した分だけを新しい送信ID・画像IDで保存する（前回の結果で書き足しを消さない）。
      if (await inputImages.adoptTodo(created)) {
        await inputImages.clear();
        if (!inputImages.valid()) return;
        return savedToList();
      }
      if (!inputImages.valid()) return;
      // 版を進めてよいのは、自分の保存が受理されたと確認できたとき（上の adoptTodo）だけ。初回はこの
      // 作成が返した版から書き始め、以後の再試行でもその版のまま送る。
      if (inputImages.baseRevision() === undefined)
        await inputImages.rebase(created.revision);
      if (!inputImages.valid()) return;
      const base = inputImages.baseRevision();
      // 書き始めた版より先へ進んでいれば、別の画面が保存している。画像を上げず、相手の画像も引き継がず、
      // 上書きもせずに知らせる（入力はこの画面に残す）。
      if (base !== created.revision)
        throw Error(
          'このTODOは別の画面から更新されています。TODOタブで最新の内容を確認し、この入力を反映するか決めてください。入力はこの画面に残しています。',
        );
      const kept = (created.images || []).map((i) => i.id);
      checkTodoImages(kept, inputImages);
      try {
        const message = await inputImages.prepare({
          project: project.value,
          todo: todoId,
        });
        await inputImages.live(
          api(`/api/todos/${todoId}/save`, {
            id: message.id,
            revision: base,
            text: message.text,
            images: [...kept, ...message.images],
            ...settings.value(),
            consultationProvider: consultation.value().provider,
          }),
        );
        await inputImages.clear();
        if (!inputImages.valid()) return;
        await savedToList();
      } finally {
        inputImages.release();
      }
    });
  // 重複の疑いで断られたときの案内の置き場（下の showDuplicate が中身を入れる）。
  const duplicateBox = el('div', {
    class: 'notice duplicate-box',
    role: 'alert',
  });
  const form = el(
    'form',
    { class: 'panel-body' },
    el(
      'div',
      {},
      field('1. どのプロジェクトの依頼ですか？', project, projectChoiceHelp),
      hostHint,
      missing,
      retarget.node,
    ),
    field(
      '2. 何を変えたいですか？',
      text,
      'まだ整理できていなくても大丈夫です。AIが必要なことを確認します。',
    ),
    sharePaste,
    inputImages.node,
    meta.providers.length > 1 ? settings.node : null,
    meta.providers.length > 1 ? consultationNode : null,
    priority.node,
    tier.node,
    implementationTier.node,
    duplicateBox,
    // スマホ幅では説明文・「AIと相談を始める」・「TODOとして保存」を縦に全幅で並べる（style.css）。
    el(
      'div',
      { class: 'composer-actions new-request-actions' },
      hint(
        '今すぐ相談しないアイディアは、AIを呼ばずにTODOとして保存できます。',
      ),
      keep,
      start,
    ),
  );
  // 新しいプロジェクトの相談を始める。本文だけを送り、依頼の下書き（/api/drafts）は作らない。送信IDはこの入力の
  // メッセージIDで、応答を失って押し直しても同じ相談を返す。
  async function startProject() {
    await inputImages.checkpoint();
    if (inputImages.scope() || inputImages.creation())
      throw Error(
        'この端末に既存の依頼先へ送る途中の入力があります。その依頼先で送ってから、新しいプロジェクトを始めてください。',
      );
    if (inputImages.imageCount())
      throw Error(
        '新しいプロジェクトの相談では画像を添付できません。画像を外してから始めてください。',
      );
    const value = text.value.trim();
    if (!value) throw Error('作りたいものを入力してください。');
    const p = await inputImages.live(
      api<{ id: string }>('/api/projects', {
        id: inputImages.messageId(),
        text: value,
        ...settings.value(),
        consultationProvider: consultation.value().provider,
      }),
    );
    await inputImages.clear();
    if (!inputImages.valid()) return;
    return projectView(p.id);
  }
  // 受付が最初の発言を重複の疑い（409・code duplicate_suspected）で断ったときの案内。入力は消さずに残し、利用者が
  // 「重複ではないので相談を続ける」を押したときだけ、同じ下書きへ notDuplicate を付けて送り直す（理由は固定）。
  const notDuplicateReason = '受付画面で依頼者が重複ではないと確認';
  let notDuplicateConfirmed = false;
  // 409 のときの本文。ボタンを押した時点で本文が変わっていたら、確かめていない本文に notDuplicate を付けず通常の送信で確かめ直す。
  let duplicateText = '';
  const showDuplicate = (e: UIError) => {
    duplicateText = text.value;
    duplicateBox.replaceChildren(
      ...String(e.message)
        .split('\n')
        .map((line) => el('p', { text: line })),
      el('button', {
        type: 'button',
        class: 'quiet',
        text: '重複ではないので相談を続ける',
        onClick: () => {
          notDuplicateConfirmed = text.value === duplicateText;
          duplicateBox.replaceChildren();
          submit();
        },
      }),
    );
  };
  const withTier = <T extends { tier?: string }>(saved: T): T =>
    saved.tier ? saved : { ...saved, ...tier.value() };
  const submit = () =>
    action(start, async () => {
      const notDuplicate = notDuplicateConfirmed;
      notDuplicateConfirmed = false;
      duplicateBox.replaceChildren();
      if (!project.value) throw Error('プロジェクトを選んでください。');
      if (project.value === newProjectValue) return startProject();
      // 該当無し（Issue #689）は project を null にし unassigned: true を付けて相談を作る。
      const unassigned = project.value === unassignedValue;
      if (!inputImages.creation()?.tier) tier.require();
      await inputImages.checkpoint();
      const savedScope = inputImages.scope();
      if (savedScope?.todo)
        throw Error(
          '保存の途中のTODOがあります。「TODOとして保存」を終えてから、TODOの画面で相談を始めてください。',
        );
      const d = savedScope
        ? await inputImages.live(api<Draft>(`/api/drafts/${savedScope.draft}`))
        : await inputImages.live(
            api<Draft>(
              '/api/drafts',
              // 難易度を必須にする前に保存した入力は、選んだ難易度を足して送る。
              withTier(
                await inputImages.reserveCreation({
                  ...(unassigned
                    ? { project: null, unassigned: true }
                    : { project: project.value }),
                  ...settings.value(),
                  consultationProvider: consultation.value().provider,
                  ...priority.value(),
                  ...tier.value(),
                  ...implementationTier.value(),
                }),
              ),
            ),
          );
      selectOption(project, d.project ?? unassignedValue);
      project.disabled = true;
      if (d.status === 'submitted') {
        await inputImages.adopt(d);
        if (!inputImages.hasContent()) {
          await inputImages.clear();
          return submittedLanding(d);
        }
      }
      try {
        const message = await inputImages.prepare({
          project: d.project,
          draft: d.id,
        });
        await inputImages.send(
          { project: d.project, draft: d.id },
          notDuplicate
            ? { ...message, notDuplicate: notDuplicateReason }
            : message,
        );
        // 開始できた選択だけを覚える。選び直しただけでは更新しない。
        rememberDefaults(d.project, d.provider || settings.value().provider);
        await inputImages.clear();
        if (!inputImages.valid()) return;
        mobileView = 'chat';
        await draftView(d.id);
      } catch (caughtValue) {
        const e = uiError(caughtValue);
        if (e.status !== 409 || e.code !== 'duplicate_suspected') throw e;
        if (inputImages.valid()) showDuplicate(e);
      } finally {
        inputImages.release();
      }
    });
  form.onsubmit = (e: { preventDefault: () => void }) => {
    e.preventDefault();
    submit();
  };
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    head(
      'NEW REQUEST',
      '新しい依頼',
      '確認が完了すると自動で起票し、実装へ進みます。依頼先と、やりたいことを教えてください。',
    ),
    el(
      'div',
      { class: 'workspace' },
      el(
        'section',
        { class: 'panel' },
        el('div', { class: 'panel-head' }, el('h2', { text: '相談を始める' })),
        form,
      ),
      el(
        'aside',
        { class: 'panel' },
        el(
          'div',
          { class: 'panel-body' },
          el('span', { class: 'eyebrow', text: 'HOW IT WORKS' }),
          el('h2', { text: '話して、確認して、依頼する。' }),
          el('p', {
            class: 'instruction',
            text: '01　プロジェクトを選ぶ\n02　AIと話しながら依頼を整理\n03　確認完了後、自動起票・実装へ\n04　実装・テストの結果を確認',
          }),
          hint(
            '不明点が出たら、依頼の画面に質問が届きます。そのまま返信すると作業を再開します。',
          ),
        ),
      ),
    ),
  );
}
