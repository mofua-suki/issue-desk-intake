// 受付画面の新しいプロジェクト（相談の一覧・相談と定義・リポジトリの作成・分割案の起票・依頼の相談からの引き継ぎ）。
// client/src/ui/app.js の一部（Issue #456）で、単独のモジュールではない。受付は client/scripts/build.ts の順に app.js と
// つないだ1つのモジュールとして /app.js を配信する。
// --- 新しいプロジェクト（server/src/project-intake.mjs。docs/project-creation.md） ---------------------------------
const projectsHash = '#/projects';
const projectHash = (id: string) => `#/projects/${id}`;
const projectBadge = (s: string | number) =>
  el('span', { class: `badge project-${s}`, text: projectStatusNames[s] || s });
const projectName = (p: ProjectConsultation) =>
  p.definition?.name ||
  p.title ||
  p.messages
    ?.find((m: { role: string }) => m.role === 'user')
    ?.text.slice(0, 60) ||
  '新しいプロジェクト';
// NAS 配置の作成の段階（server/src/project-creation-nas.mjs の nasCreationSteps と同じ順）と、起票の前の NAS の監視の担当待ち。
const nasCreationSteps = [
  ['created', 'リポジトリの作成'],
  ['committed', '初期コミット'],
  ['collaborators', '権限の付与'],
  ['pull', 'issue-desk へのPRの作成'],
  ['merged', 'PRのマージ'],
  ['assignments', '受付への担当表の反映'],
  ['registered', '受付への登録'],
];
// 作成中の待ち（creation.waiting）の段階と理由。
const nasWaitSteps: Record<string, string> = {
  merge: 'merged',
  assignments: 'assignments',
};
const nasWaitTexts: Record<string, string> = {
  merge:
    'issue-desk へのPRのマージを待っています。A9 MAX の監視が issue-desk の作業の合間にマージします（手動PRのマージの依頼）。',
  assignments:
    '受付への担当表の反映待ちです。マージした assignments.json は、issue-desk の自動デプロイ（手動マージの反映）で受付が再起動したときに読み込まれ、続きから受付への登録へ進みます。',
};
const nasDefinition = (p: ProjectConsultation) =>
  p.definition?.placement === 'nas';
// 登録の後の監視への合図の結果（creation.monitor。server/src/project-webhook.mjs の monitorNotice と同じ文言）。
const monitorNotice = (monitor: string | undefined) =>
  monitor === 'notified'
    ? '監視へ即時の確認を求めました'
    : '監視は次回の定期確認（最大 pollSeconds）で読み込みます';
// 自動デプロイを設定した定義（A9 MAX 配置で autoDeploy）の登録の後の案内（server/src/project-creation.mjs の firstDeployNotice と同じ文言）。
const firstDeployNotice = (p: ProjectConsultation) =>
  p.definition?.autoDeploy === true && p.definition?.placement === 'a9max'
    ? '自動デプロイは反映先PCの hostd.json の apps に配置先・compose・healthUrl を登録してから行います。受付は実行手順を配りません。'
    : '';
// 作成処理を動かせない受付（応答の creationUnavailable）での案内（server/src/intake-actions.mjs の creationUnavailableReason と同じ文言）。
const creationUnavailableText =
  'この受付ではリポジトリを作成できません（登録先の設定ファイルが分かりません）。';
function nasCreationRows(p: ProjectConsultation) {
  const steps = p.creation?.steps || {};
  const waiting =
    p.status === 'creating' ? nasWaitSteps[p.creation?.waiting ?? ''] : null;
  const rows = nasCreationSteps.map(([key, label]) => {
    const done = steps[key];
    if (!done) return [label, key === waiting ? '待ち' : '未'];
    if (key === 'pull')
      return [
        label,
        done.skipped
          ? '済み（issue-desk に同じ内容があるためPRは不要）'
          : `済み（PR #${done.number}）`,
      ];
    const missing =
      key === 'collaborators'
        ? (done.accounts || []).filter(
            (a: { result: string }) => a.result === 'missing',
          )
        : [];
    return [
      label,
      `済み${missing.length ? `（Giteaに無いため未付与: ${missing.map((a) => a.login).join('、')}）` : ''}`,
    ];
  });
  const filing = p.filing || {};
  rows.push([
    'NAS の監視の担当',
    filing.host ? '済み' : filing.status === 'host_waiting' ? '待ち' : '未',
  ]);
  return rows;
}
// 新しいプロジェクトの相談の一覧（#/projects）。
async function projectsView(navigate: Navigation = true) {
  if (navigate !== false) remember(projectsHash, navigate === 'replace');
  const id = reset();
  current = { type: 'projects' };
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    head(
      'NEW PROJECT',
      '新しいプロジェクト',
      '新しいシステムの定義をAIと相談し、確定した定義を確認してからリポジトリの作成に同意します。',
      el('button', {
        class: 'primary',
        onClick: () => newDraft({ preset: newProjectValue }),
        text: '＋ 新しいプロジェクトを相談',
      }),
    ),
    el(
      'div',
      { id: 'items', class: 'list' },
      el('div', { class: 'loading', text: '読み込んでいます…' }),
    ),
  );
  const seq = ++projectsFetches;
  const list = await api<ProjectConsultation[]>('/api/projects').catch((e) => {
    if (isCurrent(id)) routeError(e, projectMissing);
    return null;
  });
  if (!list || !isCurrent(id)) return;
  projectRows(list, seq);
}
// 新しいプロジェクトの一覧の取得の通し番号と、描いた取得の番号。後から始めた取得の結果を古い取得で上書きしない。
let projectsFetches = 0,
  projectsShown = 0;
// 即時通知での読み直し（client/src/ui/app-live.js の liveNotice）。画面を『読み込んでいます…』から作り直さず、行だけを入れ替える。
// 読めなければ今の行を残して知らせる（次の通知か開き直しで読み直す）。
async function projectsRefresh(id: number) {
  const seq = ++projectsFetches;
  let list;
  try {
    list = await api<ProjectConsultation[]>('/api/projects');
  } catch (caughtValue) {
    const e = uiError(caughtValue);
    if (isCurrent(id) && current?.type === 'projects') error(e);
    return;
  }
  if (isCurrent(id) && current?.type === 'projects') projectRows(list, seq);
}
function projectRows(list: ProjectConsultation[], seq: number) {
  // 読めずに案内へ置き換えた画面（行の枠が無い）と、後から始めた取得を描いた後の古い結果は描かない。
  if (!$('#items') || seq < projectsShown) return;
  projectsShown = seq;
  $('#items').replaceChildren(
    ...list.map((p) =>
      el(
        'button',
        { class: 'issue-row', onClick: () => projectView(p.id) },
        projectBadge(p.status),
        el(
          'div',
          {},
          el('h3', { text: projectName(p) }),
          el('code', { class: 'draft-id', text: `相談ID ${p.id}` }),
        ),
        el('small', {
          class: 'hint row-guidance',
          text: projectGuidance[p.status] || '',
        }),
        rowTime(p.updatedAt),
      ),
    ),
  );
  if (!list.length)
    $('#items').append(
      el(
        'div',
        { class: 'empty' },
        el('strong', { text: '新しいプロジェクトの相談はまだありません' }),
        '「＋ 新しいプロジェクトを相談」から、作りたいものを普段の言葉で伝えてください。',
      ),
    );
}
// 新しいプロジェクトの相談・確定した定義・「リポジトリを作成する」（#/projects/<相談ID>）。
async function projectView(projectId: string, navigate: Navigation = true) {
  if (navigate !== false)
    remember(projectHash(projectId), navigate === 'replace');
  const id = reset();
  current = { type: 'project', id: projectId };
  const first = await api<ProjectConsultation>(
    `/api/projects/${projectId}`,
  ).catch((e) => {
    if (isCurrent(id)) routeError(e, projectMissing);
    return null;
  });
  if (!first || !isCurrent(id)) return;
  const owner = meta.projectCreation?.owner || '';
  // 返信欄は描き直しても同じ要素を使い、定期更新で入力中の文を消さない。送信IDは受理されるまで同じものを使う。
  const reply = el('textarea', {
    rows: 3,
    'aria-label': '返信',
    placeholder: '回答や、変えたい点を書いてください',
  });
  let pending: { id: string; text: string } | null = null;
  holdReason = () => (reply.value.trim() ? '返信が未送信です' : null);
  const show = (next: ProjectConsultation) => {
    if (isCurrent(id)) render(next);
  };
  // 操作の出し分けは受付の応答の actions だけで決める（server/src/intake-actions.mjs。状態は案内・表示にだけ使う）。
  const offered = (p: ProjectConsultation, a: string) =>
    (p.actions || []).includes(a);
  // 登録の後の分割案の起票（server/src/project-creation.mjs）。起票した各Issueへは「開く」で移動する。
  function filingBlock(p: ProjectConsultation) {
    const filing = p.filing || {};
    const title = el('h3', {
      text: `最初の分割案の起票${filing.status ? `（${projectFilingNames[filing.status] || filing.status}）` : ''}`,
    });
    const retry = (label: string) => {
      if (!offered(p, 'file')) return null;
      const b = el('button', { class: 'primary', type: 'button', text: label });
      b.onclick = () =>
        action(b, async () => {
          const next = await api<ProjectConsultation>(
            `/api/projects/${projectId}/file`,
            {},
          );
          toast('分割案の起票を始めました');
          show(next);
        });
      return b;
    };
    if (filing.status === 'filed')
      return [
        title,
        el(
          'div',
          { class: 'field' },
          (filing.issues || []).map((i: DraftIssue, n: number) => {
            const b = el('button', {
              type: 'button',
              class: 'quiet',
              text: `${n + 1}. ${p.definition?.splitIssues[n]?.title || ''} ${projectIssueLabel(i)}を開く`,
            });
            b.onclick = () =>
              issueView(i.project ?? null, i.number, true, i.desk);
            return b;
          }),
        ),
        hint(
          '各Issueは既存の監視が依存の順に実装・PR・レビュー・マージへ進めます（監視が登録を読み込んだ後）。',
        ),
      ];
    if (filing.status === 'failed')
      return [
        title,
        el(
          'div',
          { class: 'error-box' },
          filing.error || '起票が途中で止まりました。',
          retry('起票を再試行'),
        ),
      ];
    // NAS 配置の担当待ち。受付が確かめ直して起票するので、起票の操作は出さない（actions に file が無い）。
    if (filing.status === 'host_waiting')
      return [
        title,
        el('div', {
          class: 'loading',
          text: `NAS の監視の担当待ちです（${filing.waiting || '確認中'}）。NAS の監視が初回スキャンを済ませたら、受付が分割案を起票します。`,
        }),
      ];
    // 起票中でも、受付の停止で切れた起票は続きから動かせる（そのときだけ actions に file がある）。
    if (filing.status === 'filing')
      return [
        title,
        el('div', {
          class: 'loading',
          text: '受付が分割案を依存の順に起票しています…',
        }),
        retry('起票を再試行'),
      ];
    return [
      title,
      hint('分割案はまだ起票していません。'),
      retry('起票を始める'),
    ];
  }
  // 依頼の相談から引き継いだ相談（Issue #506）。元の相談への案内と、受付AIがまとめた「引き継いだ内容」。
  // #app の replaceChildren へ展開して渡すので、無いときは空の配列（null は文字として出てしまう）。
  function handoffBlock(p: ProjectConsultation) {
    if (!p.sourceDraft) return [];
    const note = el(
      'div',
      { class: 'handoff-note' },
      el('p', {
        class: 'instruction',
        // プロジェクト未定で始めた相談（Issue #689）からの引き継ぎは handoff.project が無く、対象を「なし（プロジェクト未定）」と出す。
        text: `依頼の相談（相談ID ${p.sourceDraft}、対象 ${p.handoff?.unassigned ? `なし（${unassignedText}）` : shownProject(p.handoff?.project) || '不明'}）から引き継ぎました。`,
      }),
      el('button', {
        type: 'button',
        class: 'quiet',
        text: '元の相談を開く',
        onClick: () => draftView(p.sourceDraft!),
      }),
      p.handoff?.summary
        ? fieldBlock(
            '引き継いだ内容',
            el('div', { class: 'issue-body' }, markdown(p.handoff.summary)),
          )
        : null,
    );
    return [note];
  }
  function definitionBlock(p: ProjectConsultation) {
    const heading = [
      el('span', { class: 'eyebrow', text: 'DEFINITION' }),
      el('h2', { text: 'プロジェクト定義' }),
    ];
    if (!p.definition)
      return [
        ...heading,
        hint(
          '相談で定義が確定すると、ここに全文と「リポジトリを作成する」ボタンが出ます。確定するまでリポジトリは作りません。',
        ),
      ];
    const rows = projectDefinitionRows(
      p.definition,
      owner,
      repositoryProjects(),
    );
    const approve = el('button', {
      class: 'primary',
      type: 'button',
      text: 'リポジトリを作成する',
    });
    approve.onclick = () =>
      action(approve, async () => {
        // 表示している定義の指紋を送る。相談で定義が変わっていれば受付が断る（同意は表示した定義にだけ結び付く）。
        const next = await api<ProjectConsultation>(
          `/api/projects/${projectId}/approve`,
          { fingerprint: p.fingerprint },
        );
        toast('リポジトリの作成に同意しました');
        show(next);
      });
    // 同意の後の作成（server/src/project-creation.mjs）。作成を始める・続きから再開する・もう一度確かめるは同じ操作。
    const startButton = (label: string) => {
      // 作成処理を動かせない受付では、ボタンの代わりに理由を出す（Issue #496）。
      if (!offered(p, 'create'))
        return p.creationUnavailable ? hint(creationUnavailableText) : null;
      const b = el('button', { class: 'primary', type: 'button', text: label });
      b.onclick = () =>
        action(b, async () => {
          const next = await api<ProjectConsultation>(
            `/api/projects/${projectId}/create`,
            {},
          );
          toast('リポジトリの作成を始めました');
          show(next);
        });
      return b;
    };
    const progress = p.creation
      ? el(
          'dl',
          { class: 'project-definition' },
          (nasDefinition(p)
            ? nasCreationRows(p)
            : projectCreationRows(p.creation)
          ).flatMap(([label, value]) => [
            el('dt', { text: label }),
            el('dd', { text: value }),
          ]),
        )
      : null;
    const creationBlock =
      p.status === 'creating'
        ? [
            el('h3', { text: 'リポジトリの作成' }),
            progress,
            el('div', {
              class: 'loading',
              text:
                nasWaitTexts[p.creation?.waiting ?? ''] ||
                '受付がリポジトリを作成しています…',
            }),
            // 受付の停止で切れた作成は続きから動かせる（そのときだけ actions に create がある）。担当表の反映待ちは
            // 同じ操作で確かめ直す。
            startButton(
              p.creation?.waiting ? 'もう一度確かめる' : '作成を再試行',
            ),
          ]
        : p.status === 'creation_failed'
          ? [
              el('h3', { text: 'リポジトリの作成' }),
              progress,
              el(
                'div',
                { class: 'error-box' },
                p.creation?.error || '作成が途中で止まりました。',
                startButton('作成を再試行'),
              ),
            ]
          : p.status === 'creation_blocked'
            ? [
                el('h3', { text: 'リポジトリの作成（要対応）' }),
                el(
                  'div',
                  { class: 'error-box' },
                  p.creation?.reason || 'このままでは作成できません。',
                  startButton('もう一度確かめる'),
                ),
              ]
            : p.status === 'registered'
              ? [
                  el('h3', { text: 'リポジトリの作成' }),
                  progress,
                  el('p', {
                    class: 'instruction',
                    text: nasDefinition(p)
                      ? `${shownProject(`${owner}/${p.definition.name}`)} を作成し、NAS の担当として受付の対象に登録しました。「新しい依頼」のプロジェクトに出ます。NAS の監視は更新（issue-desk の登録一覧）で読み込み、初回スキャンを済ませてから最初の分割案を起票します。`
                      : `${shownProject(`${owner}/${p.definition.name}`)} を作成し、受付の対象に登録しました。「新しい依頼」のプロジェクトに出ます。${monitorNotice(p.creation?.monitor)}。${firstDeployNotice(p)}`,
                  }),
                  ...filingBlock(p),
                ]
              : [];
    // 同意済みで作成を始めていない定義（作成の各段階の表示が無く、作成を始められる）。作成処理を動かせない受付では
    // 同意の記録だけを示し、ボタンの代わりに理由を出す。
    const approved =
      !creationBlock.length &&
      (offered(p, 'create') ||
        (p.creationUnavailable &&
          p.status === 'create_approved' &&
          !!p.approval));
    return [
      ...heading,
      el(
        'dl',
        { class: 'project-definition' },
        rows.flatMap(([label, value]) => [
          el('dt', { text: label }),
          el('dd', { text: value }),
        ]),
      ),
      ...creationBlock,
      approved
        ? el(
            'div',
            { class: 'project-approve' },
            el('p', {
              class: 'instruction',
              text: `${date(p.approval?.approvedAt)} にこの定義でのリポジトリ作成に同意しました。まだリポジトリは作られていません。`,
            }),
            startButton('作成を始める'),
          )
        : creationBlock.length
          ? null
          : offered(p, 'approve')
            ? el(
                'div',
                { class: 'project-approve' },
                p.approval
                  ? hint(
                      '定義が変わったため、前回の同意は無効になりました。内容を確認して、もう一度押してください。',
                    )
                  : null,
                hint(
                  `この定義で Gitea に ${shownProject(`${owner}/${p.definition.name}`)} を作ります。内容を確認してから押してください。`,
                ),
                approve,
              )
            : hint(
                '相談中です。定義が確定すると「リポジトリを作成する」を押せます。',
              ),
    ];
  }
  function render(p: ProjectConsultation) {
    const thinking = p.status === 'thinking';
    // 返信を送れるか（actions の chat）。返信欄ごと出さないのは、作成で相談が固まった（受付の応答の creationLocked。
    // 作成の要求を送った後・作成中・登録済み）ときだけ。それ以外で送れない間（AIの整理中・ターンの後片付け中）は
    // 返信欄を残して送信だけを止める（Issue #496）。
    const canChat = offered(p, 'chat');
    const locked = !!p.creationLocked;
    const send = el('button', {
      class: 'primary',
      type: 'button',
      text: '送信 →',
    });
    send.disabled = !canChat;
    send.onclick = () =>
      action(send, async () => {
        if (!canChat)
          throw Error('今は返信を送れません。画面を読み込み直してください。');
        const value = reply.value.trim();
        if (!value) throw Error('返信を入力してください。');
        if (pending?.text !== value)
          pending = { id: crypto.randomUUID(), text: value };
        const next = await api<ProjectConsultation>(
          `/api/projects/${projectId}/chat`,
          pending,
        );
        pending = null;
        reply.value = '';
        show(next);
      });
    const retry = el('button', {
      class: 'quiet',
      type: 'button',
      text: '相談を再試行',
    });
    retry.onclick = () =>
      action(retry, async () =>
        show(
          await api<ProjectConsultation>(`/api/projects/${projectId}/chat`, {
            retry: true,
          }),
        ),
      );
    $('#app').replaceChildren(
      el('button', {
        class: 'back',
        onClick: () => projectsView(),
        text: '← 新しいプロジェクト',
      }),
      head(
        'NEW PROJECT',
        p.definition
          ? (shownProject(`${owner}/${p.definition.name}`) ?? '')
          : projectName(p),
        `状態: ${projectStatusNames[p.status] || p.status}`,
      ),
      ...handoffBlock(p),
      el(
        'div',
        { class: 'workspace' },
        el(
          'section',
          { class: 'panel' },
          el(
            'div',
            { class: 'panel-head' },
            el('h2', { text: '相談' }),
            projectBadge(p.status),
          ),
          el(
            'div',
            { class: 'panel-body' },
            el(
              'div',
              { class: 'conversation' },
              p.messages.map((m) =>
                el(
                  'div',
                  { class: `message ${m.role}` },
                  el('span', {
                    class: 'who',
                    text: m.role === 'user' ? 'あなた' : '受付AI',
                  }),
                  markdown(m.text),
                ),
              ),
            ),
            thinking
              ? el('div', {
                  class: 'loading',
                  text: p.queued ? 'AIの順番待ちです…' : 'AIが整理しています…',
                })
              : null,
            p.questions?.length
              ? el(
                  'div',
                  { class: 'questions' },
                  el('strong', { text: '確認したいこと' }),
                  el(
                    'ol',
                    {},
                    p.questions.map((q) => el('li', { text: q })),
                  ),
                )
              : null,
            p.status === 'error'
              ? el(
                  'div',
                  { class: 'error-box' },
                  p.error || '相談が止まりました。',
                  offered(p, 'retry') ? retry : null,
                )
              : null,
            locked
              ? hint('リポジトリの作成を始めたため、この相談は続けられません。')
              : field(
                  '返信',
                  reply,
                  '定義を変えたいときも、ここに書いて送ります。定義が変わると同意はやり直しです。',
                ),
            !locked && p.finishingTurn
              ? hint(
                  '前回の処理を終了中です。終わると返信できるようになります（画面は自動で更新します）。',
                )
              : null,
            locked ? null : el('div', { class: 'composer-actions' }, send),
          ),
        ),
        el(
          'aside',
          { class: 'panel project-panel' },
          el('div', { class: 'panel-body' }, definitionBlock(p)),
        ),
      ),
    );
    if (
      thinking ||
      p.finishingTurn ||
      p.status === 'creating' ||
      (p.status === 'registered' &&
        ['filing', 'host_waiting'].includes(p.filing?.status ?? ''))
    )
      poll(async () =>
        show(await api<ProjectConsultation>(`/api/projects/${projectId}`)),
      );
  }
  render(first);
}
