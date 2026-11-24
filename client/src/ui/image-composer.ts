// IndexedDB retains the exact request and blobs across reloads and lost replies.
// No credentials or server tokens are stored here.
import { stored } from './composer-storage.js';
import { uiError } from './models.js';
import type { el as elementFactory } from './dom.js';
import type { ImageDescriptor } from './issue-types.js';
import type {
  ComposerState,
  ComposerOptions,
  ComposerImage,
  ComposerMessage,
  ComposerScope,
  ComposerEdit,
  ComposerCreation,
  ComposerTodo,
  TodoCheckEdit,
  AcceptedDraft,
  AcceptedMessage,
} from './composer-types.js';
const owners = new Map<string, symbol>();
const base64 = (blob: Blob) =>
  new Promise<string>((resolve, reject) => {
    const r = new FileReader();
    r.onload = () =>
      typeof r.result === 'string'
        ? resolve(r.result.split(',')[1])
        : reject(Error('画像を読み取れません。'));
    r.onerror = () => reject(r.error);
    r.readAsDataURL(blob);
  });
export function imageGallery(
  el: typeof elementFactory,
  images: ImageDescriptor[] | null | undefined,
  scope?: ComposerScope,
) {
  return el(
    'div',
    { class: 'image-gallery' },
    (images || []).map((i) => {
      const url = `/api/images/${i.id}?${new URLSearchParams(Object.entries(scope ?? {}).map(([key, value]) => [key, String(value)]))}`;
      return el(
        'figure',
        {},
        el(
          'a',
          { href: url, target: '_blank', rel: 'noreferrer' },
          el('img', { src: url, alt: `添付画像 ${i.id}`, loading: 'lazy' }),
        ),
        el('figcaption', { text: `画像 ${i.id.slice(0, 8)}` }),
      );
    }),
  );
}
export function imageComposer({
  key,
  text,
  el,
  api,
  error,
  locked = false,
  onDirty = () => {},
  isCurrent = () => true,
}: ComposerOptions) {
  let state: ComposerState = {
      messageId: crypto.randomUUID(),
      id: crypto.randomUUID(),
      text: '',
      images: [],
    },
    busy = true,
    failed = false,
    tail: Promise<unknown> = Promise.resolve();
  const generation = Symbol(key);
  owners.set(key, generation);
  let disposed = false,
    preparedVersion: string | undefined;
  // この入力欄が読み込んだ・書いた版。clear(true) はこれらの版だけを消す（Issue #514）。
  const owned = new Set<string>();
  const valid = () =>
    !disposed && owners.get(key) === generation && isCurrent();
  const check = () => {
    if (!valid()) throw Error('Composer is no longer active');
  };
  const live = async <T>(promise: Promise<T>): Promise<T> => {
    check();
    const result = await promise;
    check();
    return result;
  };
  const report = (value: unknown) => {
    const e = uiError(value);
    if (valid() && !e.composerReported) {
      error(e);
      e.composerReported = true;
    }
  };
  const urls: string[] = [];
  // 素のファイル選択欄は見た目だけ隠し、「画像を添付」ボタンから開く（DOMには残す）。
  const picker = el('input', {
    type: 'file',
    class: 'visually-hidden',
    accept: 'image/png,image/jpeg',
    multiple: true,
    'aria-label': 'PNG/JPEG画像を選択',
    disabled: true,
  });
  const gallery = el('div', { class: 'image-gallery' }),
    note = el('small', {
      class: 'hint',
      text: 'PNG/JPEG・1枚2MiB・1200万画素・縦横6000pxまで。1メッセージ4枚、1相談12枚。PCは入力欄への貼り付けも使えます。iPhoneは「画像を添付」から写真・ファイルを選びます。送信前の入力もこのブラウザーに保存します。',
    });
  const attach = el('button', {
    type: 'button',
    class: 'quiet image-attach',
    text: '画像を添付',
    disabled: true,
    onClick: () => {
      if (!attach.disabled) picker.click();
    },
  });
  // 制限の注意書きは畳んでおき、制限を超えた画像を選んだときだけ自動で開く。
  const limits = el(
    'details',
    { class: 'image-limits' },
    el('summary', { text: '画像の条件' }),
    note,
  );
  // 取り消しボタン（プレビュー内）が最初のボタンになるよう、添付ボタンはプレビューの後ろに置く。
  const node = el(
    'div',
    { class: 'image-composer' },
    picker,
    gallery,
    el('div', { class: 'image-attach-row' }, attach, limits),
  );
  function controls() {
    if (!valid()) return;
    text.disabled =
      picker.disabled =
      attach.disabled =
        locked || busy || failed;
    for (const b of gallery.querySelectorAll('button'))
      b.disabled = locked || busy || failed;
  }
  const persist = () => {
    check();
    state.version = crypto.randomUUID();
    owned.add(state.version);
    const snapshot = structuredClone(state);
    tail = stored(key, snapshot);
    return tail;
  };
  // Only a controller-confirmed rejection releases the send identity. The message
  // identity still owns unbound uploads, so corrected input can reuse those bytes.
  async function rejected(value: unknown) {
    const e = uiError(value);
    if (
      valid() &&
      e.retryable === 'new-request' &&
      (state.pending || state.action)
    ) {
      state.id = crypto.randomUUID();
      delete state.action;
      delete state.pending;
      await live(persist());
    }
  }
  async function adopt(d: AcceptedDraft) {
    await live(ready);
    if (d.status !== 'submitted' || !d.issue || state.destination) return;
    const accepted = (d.messages || []).some(
      (m) =>
        m.id === state.messageId &&
        m.text === state.text.trim() &&
        JSON.stringify((m.images || []).map((i) => i.id)) ===
          JSON.stringify(state.images.map((i) => i.id)),
    );
    if (accepted) {
      state.text = '';
      state.images = [];
      text.value = '';
    }
    state.id = crypto.randomUUID();
    state.messageId = crypto.randomUUID();
    state.images = state.images
      .filter((i) => !i.removePending)
      .map((i) => ({
        ...i,
        id: crypto.randomUUID(),
        uploaded: false,
        uploadAttempted: false,
      }));
    // 起票したリポジトリへ送る。分割案は案ごとに対象が違い、導入前の結果は相談のプロジェクト。
    state.destination = {
      project: d.issue.project || d.project,
      number: d.issue.number,
    };
    delete state.pending;
    delete state.action;
    preparedVersion = undefined;
    await live(persist());
    render();
  }
  // 保存の応答を失ったあと、受付が実際に受理した内容とこの端末の入力を突き合わせる。同じなら未保存入力を
  // 片付け、受理後に書き足していれば残りへ新しい送信ID・画像IDを与える。前回の結果を「保存できた」と
  // 取り違えて書き足しを消さないための照合で、相談側の adopt と同じ考え方。
  async function adoptTodo(todo: ComposerTodo) {
    await live(ready);
    if (!todo || !todo.requestId || todo.requestId !== state.id) return false;
    const savedIds = (todo.images || []).map((i) => i.id);
    const todoPending = state.todoPending;
    const dueMatches = todoPending?.dueAtOmitted
      ? true
      : !state.edit?.todoCheck ||
        state.edit.todoCheck.dueDate === (todo.dueAt || '').slice(0, 10);
    const accepted =
      todo.body === state.text.trim() &&
      JSON.stringify(savedIds) ===
        JSON.stringify(state.images.map((i) => i.id)) &&
      (!todoPending ||
        JSON.stringify(state.edit?.todoCheck) ===
          JSON.stringify(todoPending.check)) &&
      dueMatches &&
      (!state.edit?.todoCheck ||
        JSON.stringify({ ...state.edit.todoCheck, dueDate: '' }) ===
          JSON.stringify({
            problem: todo.check?.problem || '',
            resolved: todo.check?.resolved || '',
            methods: (todo.check?.methods || []).join('\n'),
            dueDate: '',
          }));
    if (accepted) {
      state.text = '';
      state.images = [];
      text.value = '';
      // 受理された編集はもう保存済みの内容そのもの。編集状態を残すと、保存済み本文を空のまま
      // 上書きしてしまうため、次に開いたときに保存内容から作り直させる。
      delete state.edit;
    } else {
      // 受理された画像は保存済みTODOの持ち物。同じIDで送り直さず、プレビューからも外す。
      state.images = state.images
        .filter((i) => !savedIds.includes(i.id) && !i.removePending)
        .map((i) => ({
          ...i,
          id: crypto.randomUUID(),
          uploaded: false,
          uploadAttempted: false,
        }));
    }
    state.id = crypto.randomUUID();
    state.messageId = crypto.randomUUID();
    state.baseRevision = todo.revision;
    delete state.pending;
    delete state.todoPending;
    delete state.action;
    preparedVersion = undefined;
    await live(persist());
    render();
    return accepted;
  }
  // 画面の編集状態（本文の下書き・保存済み画像の削除予約・実行先）を入力と一緒に保存する。本文だけを
  // 別扱いにすると、画像だけ足して開き直したときに空の本文が復元され、保存で保存済み本文を消してしまう。
  async function startEdit(
    initial: ComposerEdit & { text: string },
    revision?: string,
  ) {
    await live(ready);
    // 編集状態を持たない入力（この機能より前に保存されたもの）でも、本文が残っていればそれが編集中の
    // 内容。種を入れ直して消さず、編集状態だけを付け直す。
    const seeded = !state.edit && !state.text;
    if (!state.edit) {
      state.edit = { ...initial };
      delete state.edit.text;
      if (seeded) {
        state.text = initial.text || '';
        text.value = state.text;
        // 種を作った版が編集の元になる。
        if (revision !== undefined) state.baseRevision = revision;
      }
      // 開いただけの画面は端末へ書かない（保存済みの内容の写しを残さない）。種は最初の編集・画像の追加・
      // 削除予約・実行先の変更と一緒に保存されるので、開き直しても本文が空で戻ることはない。
      if (!seeded) await live(persist());
    }
    return { ...state.edit, seeded };
  }
  async function setEdit(patch: Partial<ComposerEdit>) {
    await live(ready);
    if (!state.edit) return;
    state.edit = { ...state.edit, ...patch };
    await live(persist());
  }
  // 依頼先（リポジトリ）の名前が変わると、この端末に残った未送信入力は古い名前を持ったままになる。
  // 同じ相談・同じTODOだと確認できるときだけ、名前の違いを改名として扱う。
  function renamedScope(
    saved: ComposerScope | undefined,
    scope: ComposerScope,
  ) {
    if (!saved || !scope?.project || saved.project === scope.project)
      return null;
    const keys = [
      ...new Set([...Object.keys(saved), ...Object.keys(scope)]),
    ].filter((k) => k !== 'project');
    return keys.every(
      (k) =>
        saved[k as keyof ComposerScope] === scope[k as keyof ComposerScope],
    )
      ? saved.project
      : null;
  }
  function renamedFrom(scope: ComposerScope) {
    if (!scope?.project) return null;
    if (state.scope) return renamedScope(state.scope, scope);
    // まだ一度も送っていない新しい依頼は、確保した作成内容だけが古い名前を持っている。
    if (state.creation && state.creation.project !== scope.project)
      return state.creation.project;
    return null;
  }
  async function retarget(scope: ComposerScope) {
    await live(ready);
    const project = scope.project;
    if (!project || !renamedFrom(scope))
      throw Error(
        'この入力は別の依頼へは移せません。元の依頼で続けてください。',
      );
    if (state.scope) state.scope = { ...state.scope, project };
    if (state.creation) state.creation = { ...state.creation, project };
    if (state.destination)
      state.destination = { ...state.destination, project };
    await live(persist());
  }
  async function remove(i: ComposerImage, scope?: ComposerScope) {
    try {
      await live(api(`/api/images/${i.id}/remove`, scope));
      return true;
    } catch (caughtValue) {
      const e = uiError(caughtValue);
      if (valid() && e.retryable === 'new-request') {
        delete i.removePending;
        await live(persist());
      }
      throw e;
    }
  }
  function render() {
    if (!valid()) return;
    for (const url of urls) URL.revokeObjectURL(url);
    urls.length = 0;
    gallery.replaceChildren(
      ...state.images.map((i) => {
        const url = URL.createObjectURL(i.blob);
        urls.push(url);
        return el(
          'figure',
          {},
          el('img', { src: url, alt: i.name }),
          el('figcaption', { text: i.name }),
          el('button', {
            type: 'button',
            class: 'quiet',
            text: '取り消し',
            onClick: async () => {
              if (!valid() || busy || locked) return;
              busy = true;
              controls();
              try {
                if (i.uploaded || i.uploadAttempted) {
                  i.removePending = true;
                  await live(persist());
                  await remove(i, state.destination || state.scope);
                }
                state.images = state.images.filter((x) => x.id !== i.id);
                await live(persist());
                onDirty();
                render();
              } catch (caughtValue) {
                const e = uiError(caughtValue);
                report(e);
              } finally {
                busy = false;
                controls();
              }
            },
          }),
        );
      }),
    );
    controls();
  }
  const ready = stored(key)
    .then((saved) => {
      if (!valid()) return;
      if (saved) {
        state = saved;
        if (saved.version) owned.add(saved.version);
      }
      text.value = state.text;
      busy = false;
      render();
      if (state.text || state.images.length) onDirty();
    })
    .catch((e: unknown) => {
      failed = true;
      report(e);
      controls();
      throw e;
    });
  ready.catch(() => {});
  async function add(files: File[]) {
    try {
      await live(ready);
      if (!valid() || busy || locked) return;
      busy = true;
      controls();
      for (const file of files) {
        const refused =
          !['image/png', 'image/jpeg'].includes(file.type) ||
          file.size > 2 * 1024 * 1024 ||
          !file.size
            ? 'PNG/JPEGの画像を1枚2MiB以内で選択してください。'
            : state.images.length >= 4
              ? '画像は1メッセージ4枚までです。'
              : null;
        if (refused) {
          limits.open = true;
          throw Error(refused);
        }
        state.images.push({
          id: crypto.randomUUID(),
          name: file.name || '貼り付け画像',
          blob: file,
          uploaded: false,
        });
        await live(persist());
        onDirty();
        render();
      }
    } catch (caughtValue) {
      const e = uiError(caughtValue);
      report(e);
    } finally {
      busy = false;
      picker.value = '';
      controls();
    }
  }
  picker.onchange = () => add([...(picker.files ?? [])]);
  text.addEventListener('paste', (e) => {
    const images = [...(e.clipboardData?.items || [])]
      .filter((i) => i.kind === 'file' && i.type.startsWith('image/'))
      .map((i) => i.getAsFile())
      .filter((file): file is File => file !== null);
    if (images.length) {
      e.preventDefault();
      add(images);
    }
  });
  text.addEventListener('input', () => {
    if (!valid()) return;
    state.text = text.value;
    onDirty();
    persist().catch((e) => {
      failed = true;
      controls();
      report(e);
    });
  });
  return {
    node,
    ready,
    valid,
    live,
    hasContent: () => busy || !!state.text || !!state.images.length,
    imageCount: () => state.images.length,
    dispose() {
      disposed = true;
      for (const url of urls) URL.revokeObjectURL(url);
      urls.length = 0;
    },
    requestId: () => state.id,
    // 保存先のTODOを、この入力と同じ識別子で一度だけ作るために使う（再試行でも同じIDになる）。
    messageId: () => state.messageId,
    adoptTodo,
    // 編集状態は本文もまとめて保存・復元する。startEdit は初めて開いたときだけ保存済みの内容を種にし、
    // 2回目以降はこの端末の編集をそのまま返す（seeded で区別できる）。
    edit: () => state.edit,
    startEdit,
    setEdit,
    async setTodoPending(check: TodoCheckEdit, dueAtOmitted: boolean) {
      await live(ready);
      state.todoPending = { check: structuredClone(check), dueAtOmitted };
      const saving = persist();
      // The TODO request is submitted after prepare(). Make this final request snapshot the
      // version clear() removes on success, while later edits keep their newer version.
      preparedVersion = state.version;
      await live(saving);
    },
    // 改名で行き先の名前だけが変わったとき、入力・画像・再送の識別子を保ったまま移すための回復操作。
    renamedFrom,
    retarget,
    // 未保存入力は、編集を始めた版と一緒に置いておく。開き直しただけで最新版へ付け替えると、その間に
    // 別の端末が保存した内容を警告なく上書きしてしまう。付け替えは利用者が明示的に選んだときだけ。
    baseRevision: () => state.baseRevision,
    async rebase(revision: string | undefined) {
      await live(ready);
      if (state.baseRevision === revision) return;
      state.baseRevision = revision;
      await live(persist());
    },
    creation: () => state.creation,
    async reserveCreation(value: Omit<ComposerCreation, 'id'>) {
      await live(ready);
      if (!state.creation) {
        state.creation = { id: state.id, ...value };
        await live(persist());
      }
      return state.creation;
    },
    // value: 同じ操作で値を選ぶもの（優先度の変更）。操作ごとに1つだけ持つIDが別の値のものなら、結果の分からない
    // 前回の送信は照合せず新しいIDで送る。同じ値の再送だけ同じIDで照合し、二重に投稿しない（値ごとにIDを残すと、
    // 応答を失った古いIDを後で同じ値に戻したときに再利用し、照合済みの古い投稿を返すだけで変更が送られない）。
    async controlRequest(action: string, value?: string) {
      await live(ready);
      state.controls ??= {};
      const previous = state.controls[action];
      if (value === undefined) state.controls[action] ??= crypto.randomUUID();
      else if (
        !previous ||
        typeof previous !== 'object' ||
        previous.value !== value
      )
        state.controls[action] = { id: crypto.randomUUID(), value };
      await live(persist());
      const held = state.controls[action];
      return {
        id: typeof held === 'string' ? held : held.id,
        action,
        text: '',
      };
    },
    async controlDone(action: string) {
      if (!valid()) return;
      delete state.controls?.[action];
      await live(persist());
    },
    async checkpoint() {
      await live(ready);
      await live(tail);
      await live(persist());
    },
    async reconcile(messages: (string | AcceptedMessage)[]) {
      await live(ready);
      const version = state.version;
      if (
        messages.some((m) =>
          typeof m === 'string'
            ? m === state.messageId
            : m.id === state.messageId &&
              m.text === state.text.trim() &&
              JSON.stringify((m.images || []).map((i) => i.id)) ===
                JSON.stringify(state.images.map((i) => i.id)),
        )
      ) {
        await live(tail);
        if (
          !version ||
          !(await live(stored(key, null, version))) ||
          state.version !== version
        )
          return;
        state = {
          messageId: crypto.randomUUID(),
          id: crypto.randomUUID(),
          text: '',
          images: [],
        };
        text.value = '';
        render();
      }
    },
    adopt,
    scope: () => state.scope,
    pendingAction: () => state.action,
    async prepare(scope: ComposerScope, action?: string) {
      await live(ready);
      await live(tail);
      if (failed || busy) throw Error('画像の保存完了を待ってください。');
      busy = true;
      controls();
      try {
        if (
          state.scope &&
          JSON.stringify(state.scope) !== JSON.stringify(scope)
        ) {
          const renamed = renamedScope(state.scope, scope);
          throw Error(
            renamed
              ? `この端末に残っている入力は、以前の依頼先「${renamed}」のものです。名前が変わった場合は「入力を新しい依頼先へ移す」を押してから、もう一度お試しください。`
              : '保存済みの送信先と一致しません。元の依頼で再試行してください。',
          );
        }
        state.scope = scope;
        state.text = text.value;
        // Replay the immutable original request before giving edited input a new identity.
        if (
          state.pending &&
          (state.pending.text !== state.text ||
            JSON.stringify(state.pending.images) !==
              JSON.stringify(state.images.map((i) => i.id)))
        ) {
          try {
            const replayScope = state.destination || scope;
            const result = await live(
              api<AcceptedDraft>(
                replayScope.draft
                  ? `/api/drafts/${replayScope.draft}/chat`
                  : '/api/comment',
                {
                  ...replayScope,
                  ...state.pending,
                },
              ),
            );
            if (result?.status === 'submitted') await adopt(result);
          } catch (caughtValue) {
            const e = uiError(caughtValue);
            if (e.retryable !== 'new-request') throw e;
            check();
          }
          state.id = crypto.randomUUID();
          state.messageId = crypto.randomUUID();
          state.images = state.images
            .filter((i) => !i.removePending)
            .map((i) => ({
              ...i,
              id: crypto.randomUUID(),
              uploaded: false,
              uploadAttempted: false,
            }));
          delete state.pending;
          delete state.action;
          await live(persist());
        }
        scope = state.destination || scope;
        if (state.destination) action = 'reply';
        // A lost cancellation reply is retried as a deletion, never as an upload.
        for (const i of [...state.images])
          if (i.removePending) {
            try {
              await remove(i, scope);
            } catch (caughtValue) {
              const e = uiError(caughtValue);
              if (e.retryable === 'new-request' && state.pending) continue;
              throw e;
            }
            state.images = state.images.filter((x) => x.id !== i.id);
            await live(persist());
            render();
          }
        if (action) state.action ??= action;
        await live(persist());
        for (const i of state.images)
          if (!i.uploaded) {
            i.uploadAttempted = true;
            await live(persist());
            await live(
              api('/api/images', {
                ...scope,
                id: i.id,
                messageId: state.messageId,
                data: await live(base64(i.blob)),
              }),
            );
            i.uploaded = true;
            await live(persist());
          }
        const message = {
          id: state.id,
          messageId: state.messageId,
          text: state.text,
          images: state.images.map((i) => i.id),
          ...(state.action ? { action: state.action } : {}),
        };
        preparedVersion = state.version;
        return message;
      } catch (caughtValue) {
        const e = uiError(caughtValue);
        if (!state.pending && e.retryable === 'new-request') await rejected(e);
        busy = false;
        controls();
        throw e;
      }
    },
    async send(scope: ComposerScope, message: ComposerMessage) {
      check();
      scope = state.destination || scope;
      state.pending = structuredClone(message);
      await live(persist());
      preparedVersion = state.version;
      try {
        return await live(
          api<AcceptedDraft>(
            scope.draft ? `/api/drafts/${scope.draft}/chat` : '/api/comment',
            { ...scope, ...message },
          ),
        );
      } catch (caughtValue) {
        const e = uiError(caughtValue);
        await rejected(e);
        throw e;
      }
    },
    rejected,
    release() {
      if (!valid()) return;
      busy = false;
      controls();
    },
    // force: 相談そのものが無くなった（TODOに戻した。Issue #514）ときの後片付け。最新の版に限らず、画面を離れていても
    // この入力欄が読み込んだ・書いた版の入力を消し、以後この入力欄からは書かない（同じIDで相談を始め直したときに古い
    // 入力を復元しない）。同じIDで始め直した相談の入力欄が保存した版は、この入力欄の版ではないので残す。
    // 戻り値は、消し終えたときにこの入力欄がまだ画面の持ち主か（呼び出し側が画面を移ってよいか）。
    async clear(force = false) {
      if (force) {
        const current = valid();
        disposed = true;
        for (const url of urls) URL.revokeObjectURL(url);
        urls.length = 0;
        // 書きかけの保存（tail）の後に消す。消す前の入力の保存が後から書き戻すことはない（同じキーの書き込みは順番に進む）。
        if (owned.size) await stored(key, null, owned);
        return current && owners.get(key) === generation && isCurrent();
      }
      if (!valid()) return;
      const version = preparedVersion || state.version;
      await live(tail);
      if (
        !version ||
        !(await live(stored(key, null, version))) ||
        state.version !== version
      )
        return;
      for (const url of urls) URL.revokeObjectURL(url);
      state = {
        messageId: crypto.randomUUID(),
        id: crypto.randomUUID(),
        text: '',
        images: [],
      };
    },
  };
}
