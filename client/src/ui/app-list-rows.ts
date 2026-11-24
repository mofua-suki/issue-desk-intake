// 受付画面の依頼一覧の行とタブの描画（Issue #2417、docs/web-ui.md）。client/src/ui/app.js の一部で、単独のモジュールでは
// ない（client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する）。
// 表示中の行は種類と識別子から作るキーで照合し、変わらない行・タブのボタンのDOMは作り直さない。必要な行だけを挿入・削除・
// 更新・並べ替え、スクロール位置は表示中の行を基準に補正する（フォーカス・選択・押下中の行を更新で失わない。動かした行の
// フォーカスは戻す）。
// 行のキー。Issueは project#number、ほかは種類と id（要対応の行の id は種類ごとに一意）。同じキーが重なったときは番号を足す。
const listRowKey = (i: RequestRow) =>
  i.kind === 'issue' ? `issue:${i.project}#${i.number}` : `${i.kind}:${i.id}`;
function listRowOpen(i: RequestRow) {
  if (i.kind === 'todo' && i.id) return todoView(i.id);
  if (i.kind === 'draft' && i.id) return draftView(i.id);
  if (i.kind === 'close' && i.id) return closeRequestView(i.id);
  if (i.kind === 'usage_review') return usageReviewAlertView(i);
  if (i.kind === 'full_test') return fullTestAlertView(i);
  if (i.kind === 'attention') return attentionAlertView(i);
  if (i.kind === 'stall') return stallAlertOpen(i);
  return issueView(i.project ?? null, i.number ?? null, true, i.desk);
}
const listRowTitle = (i: RequestRow) =>
  i.shownTitle ||
  i.title ||
  i.messages
    ?.find((x: { role: string }) => x.role === 'user')
    ?.text.slice(0, 60) ||
  '新しい相談';
// 行の2行目（プロジェクトと番号）。新しいプロジェクトの定義から起票したIssueは、定義の画面と同じく共通番号とGitea番号を並べる。
const listRowSub = (i: RequestRow) =>
  `${shownProject(i.project)}${i.kind === 'issue' ? ` · ${i.projectDefinition ? projectIssueLabel(i) : deskLabel(i)}` : i.kind === 'close' ? ` · ${deskLabel(i)} · ${giteaNumber(i.number)} · 依頼元PC ${i.pc}` : ''}`;
const scopeLimitRowMessage = (i: RequestRow) =>
  i.kind === 'issue' &&
  i.status === 'stopped' &&
  i.error?.startsWith('変更範囲が目安を超えました（')
    ? i.error
    : null;
const attentionProposalSummary = (i: RequestRow) =>
  i.kind === 'attention' && i.caseKind === 'unfiled_proposal'
    ? `元${i.sourceKind === 'pulls' ? 'PR' : 'Issue'} #${i.number} · 案ID ${i.proposalId || '不明'} · ${i.proposalStatus || i.stateLabel} · 投稿 ${i.postedAt || '不明'} · 確認 ${i.checkedAt || '不明'} · 理由 ${i.reason || '理由の記録なし'}`
    : null;
// 行の見た目を決める値。同じなら行のDOMに触れない（更新日時の短い表示は時間とともに変わるので含める）。
function listRowSignature(
  i: RequestRow,
  tabName: string | null,
  filtered: boolean,
) {
  return JSON.stringify([
    i.status,
    statusLabel(i),
    listRowTitle(i),
    listRowSub(i),
    attentionProposalSummary(i),
    scopeLimitRowMessage(i),
    i.familyProgress,
    i.relatedAlerts,
    i.attentionMembers,
    i.kind === 'issue' ? originLabel(i) : null,
    i.kind === 'draft' ? i.id : null,
    filtered && i.followup,
    i.readFailed === true,
    tabName === 'attention',
    i.guidance,
    i.updatedAt,
    shortTime(i.updatedAt),
  ]);
}
function listRowChildren(
  i: RequestRow,
  tabName: string | null,
  filtered: boolean,
) {
  return [
    badge(i.status, statusLabel(i)),
    el(
      'div',
      {},
      el('h3', { text: listRowTitle(i) }),
      el('p', { text: listRowSub(i) }),
      i.kind === 'draft' && i.familyProgress
        ? el('p', { text: familyProgressText(i.familyProgress) })
        : null,
      scopeLimitRowMessage(i)
        ? el('p', {
            class: 'scope-limit-message',
            style: 'overflow-wrap: anywhere',
            text: scopeLimitRowMessage(i),
          })
        : null,
      attentionProposalSummary(i)
        ? el('p', {
            class: 'attention-proposal-summary',
            text: attentionProposalSummary(i),
          })
        : null,
      // 仕組みが作った依頼の元（題名にIssue番号を入れないので、題名と別に示す。Issue #1976）。
      i.kind === 'issue' && originLabel(i)
        ? el('p', { class: 'hint origin', text: originLabel(i) })
        : null,
      // 相談を指し示すときはこのIDをそのまま伝える。URLは #/drafts/<下書きID>。
      i.kind === 'draft'
        ? el('code', { class: 'draft-id', text: `下書きID ${i.id}` })
        : null,
      // 完了・クローズ済みタブでは通常の依頼と混ざるので、フォローアップに目印を付ける。
      filtered && i.followup
        ? el('span', { class: 'kind-mark', text: 'フォローアップ' })
        : null,
      // リポジトリを読めなかった行は前回の内容。最新ではないことを示す。
      i.readFailed
        ? el('span', {
            class: 'kind-mark read-failed',
            text: '読取失敗（前回の内容）',
          })
        : null,
    ),
    // 案内文はスマホ幅でも4分類の担当・次の行動を出す。
    el('small', {
      class: ['attention', 'issues', 'waiting', 'stopped'].includes(
        tabName || '',
      )
        ? 'hint row-guidance'
        : 'hint row-guidance narrow-hidden',
      text: i.guidance,
    }),
    rowTime(i.updatedAt),
  ];
}
// タブのボタン。最初の描画で作り、以後は件数と見た目だけを更新する。
function listTabsRender(screen: ListScreen, groups: RequestGroups) {
  const defs: [ListTab, string, number][] = [
    ['attention', '要対応', groups.attention.length],
    ['issues', '実行中', groups.issues.length],
    ['waiting', '待機中', groups.waiting.length],
    ['stopped', '停止中', groups.stopped.length],
    // コントローラー生成のフォローアップのうち未完了のもの（完了・クローズ済みは後ろの2つのタブ）。失敗しても要対応へは数えない。
    ['followups', 'フォローアップ', groups.followups.length],
    ['todos', 'TODO', groups.todos.length],
    ['completed', '完了', groups.completed.length],
    ['closed', 'クローズ済み', groups.closed.length],
  ];
  const nodes = (screen.tabNodes ??= new Map());
  for (const [t, label, count] of defs) {
    let node = nodes.get(t);
    if (!node) {
      const number = el('span', { class: 'count' });
      const button = el(
        'button',
        {
          // 一覧を作り直さないのでページの位置は残る。押したタブの一覧は先頭から見せる（戻る/進むはブラウザーに任せる）。
          onClick: () => {
            tab = t;
            home();
            window.scrollTo(0, 0);
          },
        },
        label,
        number,
      );
      node = { button, number, shown: null };
      nodes.set(t, node);
    }
    // 件数0のタブはスマホ幅で薄く表示する（押せるまま）。
    const unknown =
      listCache?.scope !== 'full' &&
      !['attention', 'issues', 'waiting', 'stopped'].includes(t);
    const shown = JSON.stringify([
      tab === t,
      count,
      unknown,
      t === 'attention' && count > 0,
    ]);
    if (node.shown !== shown) {
      node.shown = shown;
      node.button.className = [
        'tab',
        tab === t ? 'active' : '',
        count || unknown ? '' : 'tab-empty',
      ]
        .filter(Boolean)
        .join(' ');
      node.number.className = `count ${t === 'attention' && count ? 'alert' : ''}`;
      node.number.textContent = unknown ? '—' : String(count);
    }
  }
  const box = $('#tabs');
  const wanted = defs.map(([t]) => nodes.get(t)!.button);
  if (
    box.children.length !== wanted.length ||
    wanted.some((b, n) => box.children[n] !== b)
  )
    box.replaceChildren(...wanted);
}
// container の子を wanted の並びにする。要らない子だけを外し、位置が違う子だけを動かす（そのままの子には触れない）。
function listPlace(container: Element, wanted: Element[]) {
  const keep = new Set(wanted);
  for (const child of [...container.children])
    if (!keep.has(child)) container.removeChild(child);
  wanted.forEach((node, n) => {
    const at = container.children[n];
    if (at !== node) container.insertBefore(node, at ?? null);
  });
}
// 表示中の行（画面の上端より下に下端がある行）の位置を、先頭から数行ぶん記録する。更新で上の行が増減・移動したときの補正に使う。
// 先頭の行が更新で一覧から外れても、残った次の行で補正できるよう複数行を覚える。
const LIST_ANCHOR_ROWS = 8;
function listAnchor(container: Element) {
  if (
    typeof window.scrollBy !== 'function' &&
    typeof window.scrollTo !== 'function'
  )
    return null;
  if ((window.scrollY ?? window.pageYOffset ?? 0) <= 0) return null;
  const found = [];
  for (const child of container.children) {
    if (typeof child.getBoundingClientRect !== 'function') return null;
    const rect = child.getBoundingClientRect();
    if (rect.bottom <= 0) continue;
    found.push({ node: child, top: rect.top });
    if (found.length >= LIST_ANCHOR_ROWS) break;
  }
  return found.length ? found : null;
}
// 更新後もコンテナに残っている最初の行を基準に補正する。残った基準が無ければ（切り離された要素では）補正しない。
function listAnchorRestore(
  container: Element,
  anchor: ReturnType<typeof listAnchor>,
) {
  if (!anchor) return;
  const now = [...container.children];
  const base = anchor.find(
    (a) =>
      now.includes(a.node) &&
      typeof a.node.getBoundingClientRect === 'function',
  );
  if (!base) return;
  const moved = base.node.getBoundingClientRect().top - base.top;
  if (moved)
    window.scrollTo(window.scrollX ?? 0, (window.scrollY ?? 0) + moved);
}
// 行を挿入・削除・更新・並べ替えて、空の表示を添える。empty は rows が無いときに添える要素（{ sig, build }）。
function listRowsRender(
  screen: ListScreen,
  rows: RequestRow[],
  {
    tabName,
    filtered,
    empty,
  }: {
    tabName: string | null;
    filtered: boolean;
    empty: { sig: string; build: () => HTMLElement };
  },
) {
  const container = $('#items');
  const nodes = (screen.rowNodes ??= new Map());
  const anchor = listAnchor(container);
  const seen = new Map<string, number>();
  const used = new Set();
  const wanted = [];
  for (const i of rows) {
    const base = listRowKey(i);
    const n = seen.get(base) ?? 0;
    seen.set(base, n + 1);
    const key = n ? `${base}~${n}` : base;
    let node = nodes.get(key);
    if (!node) {
      const button = el('button', {
        class: 'issue-row',
        onClick: () => {
          const currentRow = nodes.get(key);
          if (currentRow) listRowOpen(currentRow.row);
        },
      });
      node = { row: i, sig: null, button };
      nodes.set(key, node);
    }
    node.row = i;
    const sig = listRowSignature(i, tabName, filtered);
    if (node.sig !== sig) {
      node.sig = sig;
      node.button.replaceChildren(...listRowChildren(i, tabName, filtered));
      const notices =
        i.relatedAlerts ||
        (i.attentionMembers?.length && i.attentionMembers.length > 1
          ? i.attentionMembers.filter((a) => listRowKey(a) !== listRowKey(i))
          : []);
      node.related = notices.map((a) =>
        el('button', {
          class: 'related-alert quiet',
          text: `関連通知を開く: ${a.work?.nextAction || a.title || a.reason || a.caseKind || '詳細'}`,
          onClick: () => listRowOpen(a),
        }),
      );
    }
    used.add(key);
    wanted.push(node.button, ...(node.related || []));
  }
  for (const key of [...nodes.keys()]) if (!used.has(key)) nodes.delete(key);
  // 読み込み表示・空の表示は行ではないので、行が定まったら外す。空の表示は内容が同じなら作り直さない。
  let extra = null;
  if (!rows.length && empty) {
    if (screen.emptyNode?.sig !== empty.sig)
      screen.emptyNode = { sig: empty.sig, node: empty.build() };
    extra = screen.emptyNode.node;
  }
  // 行を動かす（insertBefore）とフォーカス中のボタンも一度外れてフォーカスを失うので、動かす前に覚えて、残った行へ
  // 画面を動かさずに戻す。
  const focused = document.activeElement;
  const hadFocus =
    !!focused &&
    typeof container.contains === 'function' &&
    container.contains(focused);
  listPlace(container, extra ? [...wanted, extra] : wanted);
  if (
    hadFocus &&
    document.activeElement !== focused &&
    container.contains(focused) &&
    focused instanceof HTMLElement
  )
    focused.focus({ preventScroll: true });
  listAnchorRestore(container, anchor);
}
