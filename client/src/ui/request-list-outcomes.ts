import {
  shownProject,
  shownRepositoryReference,
} from './repository-display.js';
import type {
  FollowupPeriod,
  FollowupRow,
  TierOutcomeRow,
} from './stats-report-types.js';
import type {
  ModelOverride,
  ModelSetting,
  ModelDefaults,
} from './tier-types.js';
// 稼働実績の相談の難易度と結末・フォローアップの実績と、モデルの一時的な切り替えの表の文言と行。Issue #1489 で
// client/src/ui/request-list.js から分けた（既存の import は client/src/ui/request-list.js の再エクスポートで保つ）。難易度の表示名は
// 相談画面の難易度の行（client/src/ui/request-list-rows.js）も使うのでここに置く。import するのは client/src/ui/request-list-stats.js だけ。
import { jstTime } from './request-list-stats.js';

// --- 相談の難易度（server/src/intake-tiers.mjs。Issue #430） ----------------------------------------------------------
// 表示名は server/src/intake-tiers.mjs の tierNames・tierSourceNames と同じ。
export const tierNames: Record<string, string> = {
  light: '低',
  standard: '中',
  heavy: '高',
  fixed: '固定値',
};
// classifier・default は始めの判定を廃止する前に保存した記録だけにある。
export const tierSourceNames: Record<string, string> = {
  user: '依頼者の指定',
  classifier: '始めの判定（廃止前の記録）',
  default: '既定（廃止前の記録）',
  fixed: '固定値',
};
export const tierName = (level?: string) =>
  tierNames[level ?? ''] || level || '不明';
// 始めの判定を廃止する前に保存し、判定を待っていた記録（pending: true）の表示。判定はもう走らず、記録どおり難易度「高」で続ける。
export const pendingTierLevelText = '高（始めの判定の廃止前に保存した相談）';
export const pendingTierSettingText = '難易度「高」の設定で続けます';
export const pendingTierReasonText =
  '始めの判定を廃止したため、難易度「高」で続けます';
// --- 相談の難易度と結末（Issue #433。組み立ては server/src/intake-tiers.mjs の tierReport） ------------------------------
// 画面と docs/intake.md『Consultation plus implementation totals』で同じ定義を示す。表示名は server/src/intake-tiers.mjs の tierOutcomeNames と同じ。
// 「結末」「履歴が無いときの扱い」の文は docs/intake-tiers.md『Raising the tier during a consultation』にも同じ文で載せ、固定テスト
// （server/test/tier-outcome-notes.test.mjs）で照合する（Issue #438）。
export const tierOutcomeNames: Record<string, string> = {
  passed: 'そのまま通った',
  review_held: 'レビュー保留',
  user_revised: '依頼者の修正',
  failed: '失敗',
  in_progress: '進行中',
  closed: 'クローズ済み',
};
export const tierOutcomeNotes = [
  [
    '相談の難易度と結末の対象',
    '難易度の記録（tier）がある相談のうち、Issueを起票したものを新しい順に、起票したIssueごとに1行ずつ並べます（分割起票は案ごと、調査Issueも含みます）。難易度の記録が無い導入前の相談と、まだ起票していない相談は出しません。期間の指定は使わず、プロジェクトの指定はIssueのリポジトリで絞ります。',
  ],
  [
    '結末',
    'そのまま通った: 完了で、受付から依頼者の追加指示（返信・再試行・本文付きの再開／実行）を投稿していないもの。依頼者の修正: 完了で、追加指示を投稿したもの。レビュー保留・失敗・クローズ済み: 今の状態。進行中: それ以外の状態（実行待ち・作業中・回答待ち・停止など）。依頼者の修正を区別するのは完了のときだけで、完了以外（進行中・失敗など）に追加指示があっても今の状態の区分に件数を添えるだけです。',
  ],
  [
    '履歴が無いときの扱い',
    '受付はIssueの過去の状態の履歴を持たないため、結末は今の状態（依頼一覧と同じ取得元）で分けます。再試行は失敗・レビュー保留の後にしか送れないので、受付から再試行した完了は「依頼者の修正」になります。Giteaへ直接書いた指示・再試行は受付の記録に無いため数えません。状態を取得できないIssueは「状態不明」で、依頼一覧に無いIssue（削除済み・許可外の起票者の転記など）はその理由を添えます。',
  ],
  [
    '往復／費用の目安',
    '往復は受付AIの発言の数です（相談の記録が無ければ「記録なし」）。費用の目安は、このPCの runtime/usage/ の受付の記録のうちその相談（下書き）の行（受付の相談・難易度の判定）の費用の目安の合計（米ドル）で、括弧内はAIの呼び出しの件数です。Codex の行は公開単価でトークン数から換算した額で、単価が決まらない行（単価表に無いモデル・要求の大きさが不明か272Kを超える要求・通常以外の処理区分・途中までの記録・トークン数の欠けなど）は金額に入れません。換算できる行が無い相談と、使用量の記録が無い・読めない相談も「—（費用の目安なし）」で件数を出しません（ゼロとは数えません）。分割起票の相談は案ごとの行に同じ値を出します。',
  ],
  [
    '相談＋実装の合計',
    'その相談の受付の行（受付の相談・難易度の判定）と、このPCの監視の行のうちそのIssueの行（実装・競合解消・独立レビュー・優先度の判定・調査など）を合わせた件数・費用の目安・入力・出力・キャッシュ読み・キャッシュ書きの合計です。費用は使用量の表と同じ補正を通します。分割起票の案ごとの行・調査Issueの行も、それぞれにその相談の分と実行先を含めます（Issueごとに比べるため。同じ相談から起票したIssueが複数あれば「相談の分は…N件のIssueに共通」と添え、行を足し合わせると相談の分が重なります）。相談の記録が無い行は「相談の記録なし」です。2行目は監視側の工程ごとの件数です。値の無い項目は「不明（N件）」、一部だけ不明なら「ほかに不明 N件」、記録が無ければ「記録なし」で、ゼロとは表示しません。',
  ],
  [
    '通った割合・安く済んで通った割合',
    '前後の期間ごとに、相談の日時がその期間に入る行を、相談の難易度（出発点）・相談のモデル・実装の実行先×モデル（監視の工程「実装」の最後の行。無ければ「記録なし」）で分けて数えます。そのまま通った割合は件数に対する結末「そのまま通った」の割合で、状態不明は通ったに数えません（母数には含みます）。安く済んで通った割合は、判定量のあるIssueのうち「そのまま通った」かつ判定量が中央値以下の割合です。判定量は、相談＋実装の行がすべて Claude なら費用の目安、Codex を含むなら入力＋出力トークンで、中央値は同じ期間・同じプロジェクトの指定で同じ判定量を使うIssueどうしで取ります。値の無い行がある・実行先が分からないIssueは判定量なしとして母数から外し、件数を「判定量なし」に出します。',
  ],
];
export const tierOutcomeHeaders = [
  '相談の日時',
  'プロジェクト',
  '難易度の出発点と理由',
  '引き上げ（時点と理由）',
  'モデル／推論レベル',
  '起票したIssue',
  '結末',
  '往復／費用の目安',
];
// 表の1行（server/src/intake-tiers.mjs の tierReport の rows の1件）。
export function tierOutcomeCells(
  r: TierOutcomeRow,
  projects: readonly unknown[] = [],
) {
  const start = r.start || {};
  const history = Array.isArray(r.history) ? r.history : [];
  const i: Partial<NonNullable<TierOutcomeRow['issue']>> = r.issue || {};
  const outcome = tierOutcomeNames[r.outcome] || '状態不明';
  // 切り替えで動いた記録（Issue #442 の overrides）がある行だけ、モデルの欄に併記する。
  const used = overrideUseList(r.overrides);
  return [
    r.createdAt ? jstTime(r.createdAt) : '不明',
    shownProject(r.project, projects) || '不明',
    // 判定待ち（start.pending。Issue #467）は記録の難易度・理由が判定前の保険の値なので、相談画面と同じく判定待ちと出す。
    start.pending === true
      ? `${pendingTierLevelText}（${pendingTierReasonText}）`
      : `${tierName(start.level)}（${tierSourceNames[start.source ?? ''] || start.source || '不明'}：${start.reason || '記録なし'}）`,
    history.length
      ? history
          .map(
            (h) =>
              `${h.at ? jstTime(h.at) : '時刻不明'} ${tierName(h.from)}→${tierName(h.to)}：${h.reason || '理由なし'}`,
          )
          .join('\n')
      : 'なし',
    `難易度「${tierName(r.level)}」：${r.model || '不明'} ／ ${r.effort || '不明'}${used.length ? `\nモデルの切り替えで実行：\n${used.map(overrideUseText).join('\n')}` : ''}`,
    [
      `${i.desk ? `#${i.desk}` : '共通番号未確定'}（${shownProject(i.project, projects)} の Gitea番号 ${i.number}）`,
      i.plan ? `分割案 ${i.plan}` : '',
      i.investigation ? '調査Issue' : '',
      i.title ? `「${i.title}」` : '',
    ]
      .filter(Boolean)
      .join(' '),
    // 一覧に無いIssueの状態不明には理由（statusReason。Issue #438）を添える。
    `${outcome}${r.statusReason ? `（${r.statusReason}）` : ''}${r.instructions ? `（依頼者の追加指示 ${r.instructions}件）` : ''}`,
    `${Number.isSafeInteger(r.turns) ? `${r.turns}回` : '記録なし'} ／ ${
      typeof r.usage?.costUsd === 'number'
        ? `$${r.usage.costUsd.toFixed(2)}`
        : '—（費用の目安なし）'
    }${Number.isSafeInteger(r.usage?.count) ? `（${r.usage?.count}件）` : ''}`,
  ];
}
// --- フォローアップの実績（Issue #486。集計は server/src/followup-stats.mjs） ----------------------------------------
// 画面と docs/followup-proposals.md『Follow-up Statistics』で同じ定義を示す。結末の区分と表示名は
// 相談の難易度と結末（tierOutcomeNames）と同じ。種類の表示名は shared/src/proposal-comment.ts の proposalKinds と同じ。
export const followupKindNames: Record<string, string> = {
  followup: 'レビュー由来のフォローアップ',
  improvements: '細かい改善のまとめ',
};
export const followupOutcomeOrder = [
  'passed',
  'user_revised',
  'review_held',
  'failed',
  'closed',
  'in_progress',
];
export const followupStatsNotes = [
  [
    'フォローアップの実績の対象',
    '受付が後続Issueの案（元Issueへのコメント）から起票したIssueのうち、種類がレビュー由来のフォローアップ（独立レビューの followups があった回。起票の基準に該当したもの）と細かい改善のまとめ（保留した細かい改善をまとめたもの）です。実装由来の分割の案と、受付の記録（proposals）の無い起票は数えません。',
  ],
  [
    'フォローアップの期間',
    '受付が案を受け付けた日時（日本時間の0時区切り、終了日を含む）の属する期間に入れます。プロジェクトの指定は起票したIssueのリポジトリで絞ります。行の一覧は前後どちらかの期間に入るものを受付の新しい順に最大200件です。',
  ],
  [
    'フォローアップの結末',
    '相談の難易度と結末と同じ区分で、今の状態から分けます。状態を取得できないIssueは「状態不明」として別に数え、どの区分にも入れません。',
  ],
  [
    '細かい改善の保留',
    'このPCの監視の runner.log にある「改善を保留 <repo>#<番号> <件数>件」の行を、行の時刻の期間とプロジェクトごとに合計します（件数は改善の数、回数は行の数）。期間に行が1つも無ければ0ではなく「記録なし」です（その期間に保留が無かったのか記録が無いのかは区別できません）。まとめて起票して保留から外した分は差し引きません。',
  ],
];
// 結末の1区分の件数。状態不明の行があれば添え、全行が状態不明なら件数を出さない（ゼロと表示しない）。
export function followupOutcomeText(
  p: FollowupPeriod | null | undefined,
  key: string,
) {
  const o: Record<string, number> = p?.outcomes || {};
  const unknown = o.unknown || 0;
  const total = p?.counts?.total || 0;
  if (total && unknown === total) return `状態不明（${unknown}件）`;
  return `${o[key] ?? 0}件${unknown ? `（ほかに状態不明 ${unknown}件）` : ''}`;
}
// 保留の件数。行の無い期間は「記録なし」。
export function followupPendingText(p: FollowupPeriod | null | undefined) {
  const x = p?.pending;
  if (!x) return '記録なし';
  return `${x.items}件（${x.records}回）`;
}
export function followupPendingProjects(
  p: FollowupPeriod | null | undefined,
  projects: readonly unknown[] = [],
) {
  const x = p?.pending;
  if (!x) return '記録なし';
  return x.projects
    .map(
      (y) =>
        `${shownProject(y.project, projects)}: ${y.items}件（${y.records}回）`,
    )
    .join('\n');
}
// 前後の比較の表の行（項目と1つの期間の値）。集計できなかった期間（null）は「記録なし」。
export function followupStatsRows(
  p: FollowupPeriod | null | undefined,
  projects: readonly unknown[] = [],
) {
  const labels = [
    ...Object.values(followupKindNames).map((name) => `起票: ${name}`),
    '起票: 合計',
    ...followupOutcomeOrder.map((key) => `結末: ${tierOutcomeNames[key]}`),
    '結末: 状態不明',
    '細かい改善の保留',
    '細かい改善の保留（プロジェクト別）',
  ];
  if (!p) return labels.map((label) => [label, '記録なし']);
  return [
    ...Object.keys(followupKindNames).map((k) => [
      `起票: ${followupKindNames[k]}`,
      `${p.counts?.[k] ?? 0}件`,
    ]),
    ['起票: 合計', `${p.counts?.total ?? 0}件`],
    ...followupOutcomeOrder.map((key) => [
      `結末: ${tierOutcomeNames[key]}`,
      followupOutcomeText(p, key),
    ]),
    ['結末: 状態不明', `${p.outcomes?.unknown ?? 0}件`],
    ['細かい改善の保留', followupPendingText(p)],
    [
      '細かい改善の保留（プロジェクト別）',
      followupPendingProjects(p, projects),
    ],
  ];
}
export const followupRowHeaders = [
  '案の受付日時',
  '起票したIssue',
  '種類',
  '元Issue',
  '期間',
  '結末',
];
// 行の一覧の1行（server/src/followup-stats.mjs の rows の1件）。
export function followupRowCells(
  r: FollowupRow,
  projects: readonly unknown[] = [],
) {
  const periods = Array.isArray(r.periods) ? r.periods : [];
  return [
    r.acceptedAt ? jstTime(r.acceptedAt) : '不明',
    [
      `${r.desk ? `#${r.desk}` : '共通番号未確定'}（${shownProject(r.project, projects)} の Gitea番号 ${r.number}）`,
      r.title ? `「${r.title}」` : '',
    ]
      .filter(Boolean)
      .join(' '),
    followupKindNames[r.kind] || r.kind || '不明',
    shownRepositoryReference(r.source, projects) || '不明',
    periods.map((x: string) => (x === 'before' ? '前' : '後')).join('・') ||
      '—',
    `${tierOutcomeNames[r.outcome] || '状態不明'}${r.instructions ? `（依頼者の追加指示 ${r.instructions}件）` : ''}`,
  ];
}
// --- AIのモデルの一時的な切り替え（API は server/internal/httpapi/model_routes.go。Issue #442・#443） ----------------------
// 表示名は監視（issue-desk-worker）の shared/src/model-overrides.mjs の overrideStepNames・overrideScopeNote と同じ。
export const overrideStepNames: Record<string, string> = {
  intake: '受付の相談',
  review: '独立レビュー',
  priority: '優先度の判定',
};
export const overrideProviderNames: Record<string, string> = {
  codex: 'Codex',
  claude: 'Claude',
};
// 受付が返す note が無い古い受付のときだけ使う。
export const overrideScopeNote =
  '受付が公開し、各PCの監視が定期確認で写して使う（受付を持たないPCの監視へ届くまで最大1周期）';
export const overrideNoEffort = '指定なし（切り替えが無い場合と同じ）';
const overrideProvider = (p?: string) =>
  overrideProviderNames[p ?? ''] || p || '不明';
const overrideStep = (step: string) =>
  `${overrideStepNames[step] || step}（${step}）`;
const overrideTime = (t?: string) =>
  typeof t === 'string' && Number.isFinite(Date.parse(t))
    ? `${jstTime(t)}（日本時間）`
    : '不明';
// 下書きの記録のうち表示できる項目だけ（形の違う記録は外す）。相談画面の難易度の行（client/src/ui/request-list-rows.js）も使う。
export const overrideUseList = (list?: ModelOverride[]) =>
  Array.isArray(list) ? list.filter((u) => u && typeof u === 'object') : [];
// 実行先・モデル・推論レベルの1行（今の設定の値）。
export const overrideSettingText = (s?: ModelSetting) =>
  `${overrideProvider(s?.provider)} ${s?.model || '不明'} ／ 推論レベル ${s?.effort || '不明'}`;
// 今効いている切り替え1件（GET /api/model-overrides の overrides の1件）の見出しと本文の組。
export function overrideRows(o: ModelOverride) {
  return [
    ['工程', overrideStep(o.step)],
    ['実行先', overrideProvider(o.provider)],
    ['モデル', o.model || '不明'],
    ['推論レベル', o.effort || overrideNoEffort],
    ['期限', overrideTime(o.until)],
    ['設定時刻', overrideTime(o.setAt)],
  ];
}
// 切り替えの無い工程の今の設定（defaults）。受付の相談は、難易度（intake.tiers）があれば難易度ごとの値と固定値。
// 独立レビューが review.provider = "opposite"（Issue #576）のときは単一の実行先として示さず、「実装の反対側」と、
// 実装の実行先ごとのレビューの値（defaults.review.opposite）を並べる。
export function overrideDefaultRows(step: string, defaults: ModelDefaults) {
  const d = defaults?.[step];
  if (!d) return [['今の設定', '不明']];
  if (step === 'review' && d.mode === 'opposite')
    return [
      [
        '今の設定',
        '実装の反対側（Issueごとに、実装した実行先と異なる実行先でレビュー）',
      ],
      ['Claudeで実装したIssue', overrideSettingText(d.opposite?.claude)],
      ['Codexで実装したIssue', overrideSettingText(d.opposite?.codex)],
    ];
  if (step !== 'intake' || !d.tiers)
    return [['今の設定', overrideSettingText(d)]];
  return [
    ...['light', 'standard', 'heavy']
      .filter((level) => d.tiers?.[level])
      .map((level) => [
        `今の設定（難易度「${tierName(level)}」）`,
        overrideSettingText(d.tiers?.[level]),
      ]),
    ['今の設定（固定値。クローズ依頼の確認）', overrideSettingText(d)],
  ];
}
// 下書きの記録（modelOverrides）の1件。相談画面の「相談の難易度」と稼働実績の「相談の難易度と結末」に併記する。
export const overrideUseText = (u: ModelOverride) =>
  `${overrideStep(u.step)}：${overrideProvider(u.provider)} ${u.model || '不明'} ／ 推論レベル ${u.effort || overrideNoEffort} ／ 期限 ${overrideTime(u.until)} ／ 使用 ${overrideTime(u.firstUsedAt)}〜${overrideTime(u.lastUsedAt)}`;
// 画面の期限の入力（datetime-local の「YYYY-MM-DDTHH:MM」。日本時間として読む）を、APIへ送るオフセット付きの
// ISO日時にする。空・読めない値・存在しない日時（2026-02-30・24:00 など）は null（送らない）。
export function jstDeadline(value: unknown) {
  const m = String(value ?? '')
    .trim()
    .match(/^(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2})(:\d{2})?$/);
  if (!m) return null;
  // Date.parse は存在しない日付を翌月へ繰り越して読むことがあるので、組み立て直した日時が同じ値に戻るかで確かめる
  // （Issue #469。受付へ送るISO日時の実在日付検査）。
  const parts = [
    ...m[1].split('-'),
    ...`${m[2]}${m[3] || ':00'}`.split(':'),
  ].map(Number);
  const t = new Date(
    Date.UTC(parts[0], parts[1] - 1, parts[2], parts[3], parts[4], parts[5]),
  );
  const back = [
    t.getUTCFullYear(),
    t.getUTCMonth() + 1,
    t.getUTCDate(),
    t.getUTCHours(),
    t.getUTCMinutes(),
    t.getUTCSeconds(),
  ];
  if (back.some((v, i) => v !== parts[i])) return null;
  const iso = `${m[1]}T${m[2]}${m[3] || ':00'}+09:00`;
  return Number.isFinite(Date.parse(iso)) ? iso : null;
}
