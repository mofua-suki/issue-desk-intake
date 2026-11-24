// 稼働実績の「開いている依頼の棚卸し」（Issue #755。集計は server/src/inventory-stats.mjs、定義は docs/inventory.md『Statistics』）。
// client/src/ui/app.js の一部で、単独のモジュールではない（client/scripts/build.ts の順に app.js とつなぐ）。inventory の無い応答
// （更新前の受付）では出さない。報告を処理していない行は件数を「—」にする（ゼロとは出さない）。
const inventoryStatsHeaders = [
  '回',
  '週（月曜）',
  'プロジェクト',
  '調査Issue',
  '状態',
  '調べた',
  '閉じた（対応済み）',
  '閉じた（重複）',
  '要対応に回した',
  '残した',
  '未判定',
];
const inventoryStatsNotes = [
  [
    '開いている依頼の棚卸しの件数',
    '保存済みの棚卸しの回ごと・プロジェクトごとに、調べた件数（その回の一覧に載せた依頼の数）と、報告の処理の結果を数えます。新しい回は始めません（自動監視からの新規起票は停止されています）。閉じたのは受付が根拠などを確かめて閉じた依頼（理由別）、要対応に回したのは確かめられなかった・自動で閉じる対象でないプロジェクト・判断が要る分類の依頼、残したのは「残す」の依頼、未判定は報告に正しい行が無かった依頼です。報告がまだ届いていない回は件数を「—」にします。',
  ],
  [
    '開いている依頼の棚卸しで自動で閉じる条件',
    '保存済みの回が使う inventoryAutoCloseProjects の当初の初期値は issue-desk でした。自動で閉じたのは、その回の対象プロジェクトの「対応済み」「重複」だけで、根拠（既定ブランチへマージ済みのPR・既定ブランチに含まれるコミット・同じリポジトリの別の依頼）、処理中でないこと、後続が無いこと、一覧に載せた後に更新されていないことをすべて確かめられたときだけです。',
  ],
];
const inventoryStatusNames: Record<string, string> = {
  waiting: '報告待ち',
  processing: '処理中',
  done: '処理済み',
  closed: '報告を処理せずに終了',
};
const inventoryCount = (v: number | undefined) =>
  typeof v === 'number' ? `${v}件` : '—';
function inventoryStatsCells(round: InventoryRound, p: InventoryProject) {
  return [
    `第${round.round}回`,
    round.week || '?',
    shownProject(p.project),
    p.investigationIssue
      ? `${shownProject(p.investigationIssue.project)} の Gitea番号 ${p.investigationIssue.number}`
      : '未確定',
    `${inventoryStatusNames[p.status] || p.status}${p.pending ? `（確かめ中 ${p.pending}件）` : ''}`,
    inventoryCount(p.examined),
    inventoryCount(p.closed?.resolved),
    inventoryCount(p.closed?.duplicate),
    inventoryCount(p.escalated),
    inventoryCount(p.kept),
    inventoryCount(p.undetermined),
  ];
}
function inventoryStatsResult(r: StatsReport) {
  if (!r.inventory) return [];
  const v = r.inventory;
  const rounds = v.rounds || [];
  return [
    el('h2', { text: '開いている依頼の棚卸し' }),
    ...(v.reason ? [el('div', { class: 'error-box', text: v.reason })] : []),
    ...(rounds.length
      ? [
          statsTable(
            inventoryStatsHeaders,
            rounds.flatMap((x) =>
              (x.projects || []).map((p) => inventoryStatsCells(x, p)),
            ),
            'inventory-stats',
          ),
        ]
      : v.reason
        ? []
        : [
            el('p', {
              class: 'stats-coverage',
              text: '開いている依頼の棚卸しの回はまだありません。',
            }),
          ]),
  ];
}
