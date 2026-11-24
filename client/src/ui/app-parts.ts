// 受付画面の共通部品（el・状態バッジ・日時・見出し・入力欄の枠）。client/src/ui/app.js の一部（Issue #456）で、単独の
// モジュールではない。受付は client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。
const states: Record<string, string> = {
  saved_todo: 'TODO',
  dependency_waiting: '先行PRのマージ待ち',
  project_waiting: 'プロジェクト排他待ち',
  pending: '検知待ち',
  draft: '起票前の相談',
  intake_waiting: '受付の回答待ち',
  auto_ready: '起票待ち',
  submit_error: '起票失敗',
  submit_unknown: '起票結果の確認が必要',
  thinking: 'AIが整理中',
  ai_queued: 'AIの順番待ち',
  ai_recovering: 'AIの自動復旧待ち',
  ai_stopped: '相談が止まっている',
  error: '相談を再試行',
  stalled: '停止中',
  submitting: '起票中',
  submitted: '送信済み',
  untracked: '未着手',
  queued: '受付済み',
  running: '作業中',
  waiting: '実装の回答待ち',
  reply_pending: '反映待ち',
  retry_pending: '再試行待ち',
  publishing: 'PR作成中',
  prerequisite_waiting: '前提待ち（実装回数外）',
  scope_waiting: '公開待ち（先行Issueのマージ待ちが長い）',
  releasing: 'レビュー・公開中',
  review_held: 'レビュー保留',
  deploying: '反映・起動確認中',
  completed: '完了',
  closed: 'クローズ済み',
  failed: '失敗',
  stopped: '停止中',
  remote: '担当PC待ち',
  close_confirm: '閉じてよいか確認',
  close_reviewing: 'クローズ依頼の確認中',
  close_closing: 'クローズの照合中',
  // 調査Issueを起票した相談（Issue #409）。
  investigation_waiting: '調査結果待ち',
  investigation_reported: '調査結果が届きました',
  investigation_ended: '調査が報告なしで終了',
  // 利用量の見直しで元が取れていない回（Issue #591）。
  usage_review_alert: '見直しの費用超過',
  // main の全部のテストが落ちた（Issue #790。client/src/ui/app-full-test.js）。
  full_test_alert: '全部のテストの失敗',
  // 監視が検知した実装の止まり・受付の無応答（Issue #678）。
  stalled_pipeline: '実装の止まり',
  // 受付と同じPCのホームの監視の heartbeat が15分更新されていない（Issue #982）。
  monitor_stopped: '監視の停止',
  // 監視の見張りが監視を自動で起動し直した（Issue #1568。24時間出す）。
  monitor_restarted: '監視の再起動',
  // 受付側で修正確認ができた止まり・自動修正を止めた止まり（Issue #1052）。
  intake_error_fixed: '受付の止まりが直りました',
  intake_error_stopped: '受付の自動修正を停止',
  // 起票前確認の判定が上限に達した相談・資料不足が重なったプロジェクト（server/internal/filingcheck/recovery.go）。
  filing_check_stopped: '起票前の確認を停止',
  // 新しいプロジェクトの相談へ引き継いだ相談（Issue #506）。
  project_handoff: '新しいプロジェクトへ引き継ぎ',
};
// 状態バッジは状態名だけを出す。担当が他PCでも担当ホスト名は付けない（どこで止まっているかは利用者に不要）。
// label: 表示名の上書き（調査Issueの完了の「報告済み」。request-list.js の statusLabel）。
const badge = (s: string, label?: string | null) =>
  el('span', { class: `badge ${s}`, text: label || states[s] || s });
const hint = (t: unknown) => el('small', { class: 'hint', text: t });
// 受付の「#番号」は全プロジェクト共通の共通番号。対応が確定するまではGitea番号で代用しない。
const deskLabel = (i: { desk?: number } | null | undefined) =>
  i?.desk ? `#${i.desk}` : '共通番号未確定';
const giteaNumber = (n: unknown) => `Gitea番号 ${n}`;
// 登録済みの完全名で同名衝突を判定する。metaの再取得後も最新の一覧を使う。
const repositoryProjects = () => meta.projects.map((project) => project.id);
const shownProject = (p: string | null | undefined) =>
  repositoryDisplay(p, repositoryProjects());
// 表示名が設定されていても、登録済みの同名衝突では完全名で区別する。
const projectOptionName = (p: MetaResponse['projects'][number]) =>
  shownProject(p.id) === p.id ? p.id : shownProject(p.name);
// 相談で書いた「#番号」を受付がどのIssueと解決したか。AIへ渡した参照と同じものを見せる。
const referenceNote = (r: ReferenceNote) =>
  r.resolution === 'resolved'
    ? `#${r.desk} → ${shownProject(r.project ?? '')}（${giteaNumber(r.number)}）「${r.title}」`
    : r.resolution
      ? `#${r.desk} → ${r.reason}`
      : `#${r.desk} → 確認中`;
// 1つのメッセージの上限を超えて解決しなかった番号は、まとめて未解決と示す。
const referenceNotes = (refs: ReferenceNote[]) => {
  const omitted = refs.filter((r) => r.resolution === 'omitted');
  const notes = refs
    .filter((r) => r.resolution !== 'omitted')
    .map(referenceNote);
  if (omitted.length)
    notes.push(
      `${omitted.map((r) => `#${r.desk}`).join('、')} → ${omitted[0].reason}`,
    );
  return notes.join(' / ');
};
const date = (t: string | number | Date | undefined) =>
  new Date(t ?? NaN).toLocaleString('ja-JP', {
    month: 'numeric',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
// 一覧の行の日時。年が分かるよう西暦4桁を付ける（PC・スマホ共通。一覧以外の日時は date() のまま）。
const listDate = (t: string | number | Date | undefined) =>
  new Date(t ?? NaN).toLocaleString('ja-JP', {
    year: 'numeric',
    month: 'numeric',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
// 欠けた値・壊れた値は「Invalid Date」と出さずに不明と示す。本文はPC幅の年付きの表示で、スマホ幅は
// data-short の短い形（3時間前・9/23）を style.css で出す。
const rowTime = (t: string | number | Date | undefined) =>
  (typeof t === 'string' || typeof t === 'number') &&
  Number.isFinite(new Date(t ?? NaN).getTime())
    ? el('time', {
        text: listDate(t),
        datetime: new Date(t ?? NaN).toISOString(),
        'data-short': shortTime(t),
      })
    : el('time', { text: '日時不明', 'data-short': '日時不明' });
// 応答待ちの起点。旧形式の相談は受理日時の記録が無く、保存済みの最終更新日時で代用している。
const waitStartNote = (
  t: string | number | Date,
  source: string | undefined,
) =>
  source === 'legacy'
    ? `待機開始日時: ${date(t)}（旧形式の相談のため、受理日時の記録が無く最終更新日時で代用）`
    : `待機開始日時: ${date(t)}${source === 'scope_retry' ? '（変更範囲を再探索中）' : source === 'resumed' ? '（再開した時点）' : source === 'continued' ? '（了承後に受付AIが具体化を続けた時点）' : ''}`;
// className はスマホ幅だけ見出しの並びを変える画面の目印（依頼一覧・Issue画面）。
// mark は「プロジェクトと番号」の行に添える目印（Issue画面のフォローアップ）。同じ行に収めるため eyebrow の中に置く。
function head(
  eyebrow: string,
  title: string,
  description?: string | null,
  button?: DOMChild,
  className?: string,
  mark?: DOMChild,
) {
  return el(
    'div',
    { class: className ? `page-heading ${className}` : 'page-heading' },
    el(
      'div',
      {},
      el('span', { class: 'eyebrow', text: eyebrow }, mark),
      el('h1', { text: title }),
      description ? el('p', { text: description }) : null,
    ),
    button,
  );
}
const providerName = (id: string | undefined) =>
  meta.providers.find((p) => p.id === (id || 'codex'))?.name || id;
function field(label: string, node: HTMLElement, help?: string) {
  return el(
    'label',
    { class: 'field' },
    el('span', { text: label }),
    node,
    help ? hint(help) : null,
  );
}
// ボタンを含む内容（Markdownの長いIDの開閉など）の見出し付きの欄。labelで包むと、中のどこを押しても
// 最初のボタンが押されるので div にする。
const fieldBlock = (label: string, node: HTMLElement) =>
  el('div', { class: 'field' }, el('span', { text: label }), node);
// スマホ幅（style.css の @media (max-width: 760px) と同じ境界）。判定できない環境ではPC幅として扱う。
const narrow = () =>
  typeof matchMedia === 'function' && matchMedia('(max-width: 760px)').matches;
// スマホ幅の固定した入力欄は2行から始め、入力に合わせて8行まで伸ばす（超えたら入力欄の中でスクロール）。
// インラインのstyleはCSPで使えないので行数で調整する。PC幅では何もしない（従来の高さのまま）。
function fitRows(textarea: {
  rows: number;
  scrollHeight: number;
  clientHeight: number;
}) {
  if (!narrow()) return;
  textarea.rows = 2;
  while (textarea.rows < 8 && textarea.scrollHeight > textarea.clientHeight)
    textarea.rows++;
}
// 相談・Issue画面の入力欄。スマホ幅では入力欄・添付・送信だけを画面下に固定し（style.css の .docked）、
// 案内文は固定しない位置へ別に出す。PC幅では従来どおり送信ボタンの横の案内文だけを表示する。
function dock(
  text: HTMLElement,
  images: ComposerController,
  note: HTMLElement,
  send: HTMLElement,
) {
  return [
    el('small', { class: 'hint dock-note', text: note.textContent }),
    el(
      'div',
      { class: 'composer-dock' },
      text,
      images.node,
      el('div', { class: 'composer-actions' }, note, send),
    ),
  ];
}
function selectOption(select: HTMLSelectElement, value: unknown) {
  for (const option of select.querySelectorAll('option')) {
    if (option.value === value) option.selected = true;
    else if (option.selected !== undefined)
      delete (option as Partial<HTMLOptionElement>).selected;
  }
  select.value = String(value ?? '');
}
