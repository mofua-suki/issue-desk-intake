import { shownProject } from './repository-display.js';
import type { MetricKey, UsageTotal, UsagePeriod } from './stats-types.js';
// 稼働実績のAIの使用量の表の文言と行。Issue #1489 で client/src/ui/request-list.js から分けた（既存の import は
// client/src/ui/request-list.js の再エクスポートで保つ）。このファイルは他のモジュールを import しない。

// --- AIの使用量（Issue #480。集計は shared/src/usage-stats.mjs） ------------------------------------------------
// 画面と docs/ai-usage.md『Aggregation』で同じ定義を示す。区分の表示名は shared/src/usage-stats.mjs の usageKindNames と同じ。
export const usageStatsNotes = [
  [
    '使用量の対象',
    'このPC（受付と同じPC）の runtime/usage/ の記録（監視と受付の日別の記録）。他のPCの監視の記録は、実装PCが引き渡しで渡した行だけを含みます。1件はAIの呼び出し1回（Claudeの実装は往復の1ターン）です。',
  ],
  [
    'PC別',
    '記録したPCのホスト名ごとの合計です。このPC以外の行は、Surface などの実装PCが実装・独立レビューした依頼をホームへ引き渡したときに添えた行を、このPCの runtime/usage/ へ取り込んだものです（同じ行は1回だけ数えます）。ホスト名の無い行は「不明」です。',
  ],
  [
    '使用量の不明',
    '記録に値が無い項目（CLIが報告しなかった・読めなかった）は合計に入れず「不明 N件」と示し、ゼロとは区別します。Codex のキャッシュ書きは非該当（0として数え、Codex だけの行は「非該当」）、費用の目安は公開単価で換算できない行だけ不明に数えます。Claude・Codex 以外の実行先の行と実行先の無い行は、キャッシュでない入力・キャッシュ書き・重み付きの量を不明に数えます。',
  ],
  [
    'トークン数と費用の目安',
    '記録の「入力トークン」は実行先で意味が違い、Codex はキャッシュ読みを含み、Claude はキャッシュの読み書きを含みません。表ではキャッシュでない入力（Claude は入力のまま、Codex は入力−キャッシュ読み）に揃えます。Codex で入力よりキャッシュ読みが大きい行は不明です。費用の目安は Claude の total_cost_usd と、Codex のトークン数を OpenAI の公開単価（通常区分・272K以下の単価）で換算した額（米ドル）で、どちらも契約での実際の請求額ではありません。',
  ],
  [
    '重み付きの量',
    'キャッシュでない入力×1 + キャッシュ読み×0.1 + キャッシュ書き×1.25 + 出力×5（既定の重み）。料金の比に合わせた共通の量で、単位はキャッシュでない入力トークン換算です。重みは設定 usageWeights で実行先・モデルごとに変えられます。内訳が1つでも不明な行は合計に入れず、不明の件数に数えます。',
  ],
  [
    '使用量の期間と区分',
    '記録した時刻（日本時間の0時区切り、終了日を含む）の属する期間に入れ、プロジェクトの指定は記録のリポジトリで絞ります。区分は通常・フォローアップ（監視の判定）・受付・不明です。',
  ],
  [
    '使用量のIssue上位',
    '重み付きの量（不明を除く）の多い順に10件（工程別・実行先×モデル別・PC別も同じ並び）。Gitea番号に受付の共通番号を添え、共通番号が無いものはGitea番号だけです。受付の相談・段の判定の記録は、その下書きが起票済みなら最初に起票したIssueへ寄せ、未起票なら「受付（Issueなし）」にまとめます。',
  ],
  [
    '費用の目安の補正',
    'Claude の実装・競合解消で再開したセッションの費用は、前のターンまでの累計との差です。修正前の記録（sessionCostUsd の無い行）の2ターン目以降は、同じ実行の1つ前のターンの費用を差し引いて集計します（記録は書き換えません）。前のターンが読んだ範囲に無い・不明・差が負になる行と、前の実行のセッションを再開したかもしれない2回目以降の実行の最初のターンは補正できず、値のまま合計に入れます。',
  ],
  // 呼び出しの中身・問いかけの構成・利用上限の表（Issue #571。client/src/ui/app-usage-detail-stats.js）。
  [
    '呼び出しと文脈の大きさ',
    '呼び出し回数は1件（CLIの呼び出し1回）の中のモデル呼び出しの回数の合計です（Codex は turn 数の近似）。文脈の大きさは呼び出しごとの入力＋キャッシュ読み＋キャッシュ書きで、最大と合計を示します（Codex は不明）。問いかけのバイト数は受付の相談の問いかけの合計（UTF-8）で、1件あたりは値のある行で割ります。値の無い行（項目の無い旧形式の行を含む）は合計に入れず「不明（N件）」です。',
  ],
  [
    '道具と読んだファイル',
    '道具は道具名（Codex は item 種別）ごとの回数の合計で、多い順です。ファイルはリポジトリとパスごとに、そのパスが載った行の数と読んだバイト数（Claude の Read の結果の大きさ。Codex は不明）の合計で、バイト数の多い順の上位20件です。記録の無い行の件数を表の上に示します。',
  ],
  [
    'プロンプトの構成',
    '受付の相談の問いかけを構成要素（手順書・指示・会話・前回の下書き・画像・環境資料・初期資料・追加で読んだ資料・確認資料の残り・スキーマ）ごとのバイト数に分けた記録の合計と1件あたりの平均に、同じ行のキャッシュ読み・書き（トークン）を並べます。構成の記録の無い受付の相談の件数は不明として示します。',
  ],
  [
    '利用上限',
    'Claude の行に記録した利用上限（rate_limit_event）を窓（five_hour・seven_day など出来事のまま）ごとに、リセット時刻が同じ記録をまとめて、最初と最後の記録の利用率と、その間（両端を含む）の Claude の行の件数と入力・出力・キャッシュ読み・キャッシュ書きの合計を示します。上限はアカウント単位なので、プロジェクトの指定に関わらず全リポジトリの行を数えます。他のPCの行は引き渡された分だけです。',
  ],
];
export const usageKindNames: Record<string, string> = {
  normal: '通常',
  improvement: '改善',
  followup: 'フォローアップ',
  intake: '受付',
  unknown: '不明',
};
const usageMetricKeys = [
  'inputTokens',
  'outputTokens',
  'cacheReadTokens',
  'cacheWriteTokens',
  'costUsd',
] as const;
export const usageHeaders = [
  '件数',
  '入力',
  '出力',
  'キャッシュ読み',
  'キャッシュ書き',
  '費用の目安',
];
// 1つの項目の値。値の無い項目は「不明（N件）」、一部だけ不明なら合計に添える。ゼロとは表示しない。
export function usageMetricText(
  t: UsageTotal | null | undefined,
  key: MetricKey,
) {
  const value = t?.[key];
  const unknown = t?.unknown?.[key] ?? 0;
  if (typeof value !== 'number') return `不明（${unknown}件）`;
  const shown =
    key === 'costUsd'
      ? `$${value.toFixed(value >= 1 ? 2 : 4)}`
      : Math.round(value).toLocaleString('ja-JP');
  return `${shown}${unknown ? `（ほかに不明 ${unknown}件）` : ''}`;
}
export function usageCells(label: string, t: UsageTotal) {
  return [
    label,
    `${t.count}件`,
    ...usageMetricKeys.map((k) => usageMetricText(t, k)),
  ];
}
const usageNone = (label: string) => [[label, '—', '—', '—', '—', '—', '—']];
export function usageIssueLabel(
  i: UsageTotal,
  projects: readonly unknown[] = [],
) {
  if (i.bucket === 'intake') return '受付（Issueなし）';
  if (i.bucket) return 'Issueの記録なし';
  const gitea = `${shownProject(i.repo, projects)} の Gitea番号 ${i.number}`;
  return i.desk ? `#${i.desk}（${gitea}）` : gitea;
}
// 1つの期間の各軸の表の行。記録の無い期間は「記録なし」の1行だけ。
export function usageTables(
  p: UsagePeriod | null | undefined,
  projects: readonly unknown[] = [],
) {
  if (!p?.total?.count)
    return {
      steps: usageNone('記録なし'),
      models: usageNone('記録なし'),
      kinds: usageNone('記録なし'),
      issues: usageNone('記録なし'),
      hosts: usageNone('記録なし'),
    };
  return {
    steps: p.steps.map((s) => usageCells(s.step ?? '不明', s)),
    models: p.models.map((m) =>
      usageCells(`${m.provider ?? '不明'} / ${m.model ?? '不明'}`, m),
    ),
    kinds: p.kinds.map((k) =>
      usageCells(usageKindNames[k.kind ?? ''] ?? '不明', k),
    ),
    issues: p.issues.map((i) => usageCells(usageIssueLabel(i, projects), i)),
    // PC別（Issue #876）。hosts の無い旧い応答では「記録なし」。
    hosts: Array.isArray(p.hosts)
      ? p.hosts.map((h) => usageCells(h.host ?? '不明', h))
      : usageNone('記録なし'),
  };
}
// 期間の費用の補正の1行（Issue #535）。補正した行も補正できない行も無ければ（costCorrection の無い更新前の応答も）null。
export function usageCorrectionText(p: UsagePeriod | null | undefined) {
  const c = p?.costCorrection;
  if (!c || (!c.corrected && !c.uncorrectable)) return null;
  const parts = [];
  if (c.corrected)
    parts.push(
      `修正前の記録 ${c.corrected}件は前のターンとの差に補正しました。`,
    );
  if (c.uncorrectable)
    parts.push(
      `${c.corrected ? '' : '修正前の記録 '}${c.uncorrectable}件（${c.from ?? '?'}〜${c.to ?? '?'}）は補正できず、前のターンまでの費用を含む可能性があります。`,
    );
  return `費用の目安: ${parts.join('')}`;
}
