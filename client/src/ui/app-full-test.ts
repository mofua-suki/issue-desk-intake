// 要対応の「main の全部のテストが落ちています」（Issue #790。受付の GET /api/full-test/alerts、docs/test-selection.md の
// 「main での全部のテスト（2/3）」）。client/src/ui/app.js の一部で、単独のモジュールではない（client/scripts/build.ts の順に app.js と
// つなぐ）。依頼一覧の分類（client/src/ui/request-list.js の requestGroups）の後に要対応へ足す。知らせるだけで確認済みの操作は無く、
// 後の全部の実行が合格すると受付が返さなくなり、行が消える。
const fullTestGuidance =
  'main で全部のテストを流したところ落ちました。固定テストの選び方で省いたテストの漏れの可能性があります。直す依頼は受付が通常の優先度で自動で起票します（前の直す依頼が開いている間と、同じ失敗の繰り返しでは重ねて起票しません）。巻き戻さず、ほかのPRのマージも止めません。後の全部の実行が合格するとこの行は消えます。';
// 受付の応答（配列でないもの・形の違う項目は出さない）を要対応の行にする。
function fullTestAlertRows(alerts: RequestRow[] | null | undefined) {
  return fullTestRows(alerts).map((a) => ({
    ...a,
    guidance: fullTestGuidance,
  }));
}
// 起票した直す依頼の表示（共通番号。起票前・起票の失敗はその状態）。
function fullTestFixText(fix: RequestRow['fix']) {
  if (!fix)
    return '直す依頼: まだ起票していません（前の直す依頼が開いている、同じ失敗の繰り返し、または受付が確かめている途中です）';
  if (fix.issue)
    return `直す依頼: ${deskLabel(fix.issue)}（${shownProject(fix.issue.project)}）`;
  if (fix.status === 'removed') return '直す依頼: 下書きが削除されました';
  return `直す依頼: ${states[fix.status ?? ''] || fix.status}`;
}
// 要対応の行を開いた画面。失敗した版・失敗したテストファイル・失敗の抜粋と、直す依頼へのリンク。
function fullTestAlertView(alert: RequestRow) {
  reset();
  current = { type: 'full-test' };
  const fix = alert.fix?.issue;
  $('#app').replaceChildren(
    el('button', { class: 'back', onClick: () => home(), text: '← 依頼一覧' }),
    head('FULL TEST', alert.title ?? '', fullTestGuidance),
    el('p', {
      class: 'full-test-version',
      text: `失敗した版: ${alert.sha}（${alert.finishedAt ? date(alert.finishedAt) : '時刻不明'}・${alert.seconds ?? '?'}秒・${alert.files ?? '?'}ファイル）`,
    }),
    el('h2', { text: '失敗したテストファイル' }),
    (alert.failedFiles || []).length
      ? el(
          'ul',
          { class: 'full-test-files' },
          (alert.failedFiles ?? []).map((f) => el('li', { text: f })),
        )
      : el('p', {
          text: '出力から特定できませんでした。下の抜粋を確かめてください。',
        }),
    el('h2', { text: '失敗の抜粋' }),
    el('pre', {
      class: 'full-test-excerpt',
      text: alert.excerpt || '（抜粋はありません）',
    }),
    el('p', { class: 'full-test-fix', text: fullTestFixText(alert.fix) }),
    ...(fix
      ? [
          el(
            'div',
            { class: 'actions' },
            el('button', {
              type: 'button',
              onClick: () =>
                issueView(fix.project ?? null, fix.number, true, fix.desk),
              text: '直す依頼を開く',
            }),
          ),
        ]
      : []),
  );
}
