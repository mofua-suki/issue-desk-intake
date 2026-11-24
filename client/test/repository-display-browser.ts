// 画面の版のディレクトリ（配信時に __UI__ を版へ置き換える）。型検査は配信先のモジュールを解決しない。
const ui = '/v/__UI__/';
const pause = () => new Promise((resolve) => setTimeout(resolve, 10));
const check = (value, message) => {
  if (!value) throw Error(message);
};
const wait = async (ready) => {
  const deadline = performance.now() + 30000;
  while (!ready()) {
    if (
      performance.now() > deadline ||
      document.querySelector<HTMLElement>('#notice')?.hidden === false
    )
      throw Error(
        '画面の待機に失敗: ' +
          location.hash +
          ' ' +
          document.querySelector<HTMLElement>('#notice')?.textContent +
          ' ' +
          document.querySelector('h1')?.textContent,
      );
    await pause();
  }
};
const id = '11111111-1111-4111-8111-111111111111';
const projects = [
  'first/alpha',
  'second/dependency',
  'third/successor',
  'fourth/holder',
  'fifth/usage',
  'sixth/inventory',
  'seventh/priority',
  'eighth/followup',
  'ninth/detail',
  'first/shared',
  'second/shared',
  'creator/created',
  'former/old',
  'mofua-suki/issue-desk-intake',
  'mofua-suki/issue-desk-worker',
  'other/issue-desk-intake',
];
// 本文・会話と確認資料の原文は利用者のデータなので書き換えない。
// 担当表の受付/Workerの合算は専用表示を維持。同名衝突は完全名が必要。
const evidence = '.md, .request-body, .issue-excerpt';
const collisions = ['first/shared', 'second/shared'];
function leaks(root) {
  const found = [];
  const inspect = (text, node) => {
    if (node.closest(evidence)) return;
    for (const project of projects) {
      if (!collisions.includes(project) && text.includes(project))
        found.push(project + ': ' + text);
    }
  };
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  while (walker.nextNode())
    inspect(walker.currentNode.textContent, walker.currentNode.parentElement);
  for (const node of [root, ...root.querySelectorAll('*')]) {
    for (const attr of ['aria-label', 'title', 'alt'])
      if (node.hasAttribute(attr)) inspect(node.getAttribute(attr), node);
  }
  return found;
}
const clean = () =>
  check(
    leaks(document.querySelector('#app')).length === 0,
    '完全名の直接表示: ' + leaks(document.querySelector('#app')).join('\n'),
  );
const visit = async (hash, ready) => {
  location.hash = hash;
  await wait(ready);
  check(
    document.querySelector<HTMLElement>('#notice')?.hidden !== false,
    '画面エラー: ' +
      document.querySelector<HTMLElement>('#notice')?.textContent,
  );
  clean();
};
const button = (text) =>
  [...document.querySelectorAll<HTMLButtonElement>('#app button')].find(
    (node) => node.textContent === text,
  );
const textHas = (selector, text) =>
  [...document.querySelectorAll(selector)].some((node) =>
    node.textContent.includes(text),
  );
try {
  await import(ui + 'app.js');
  await wait(() => textHas('.attention-lines', '共通関数'));
  check(
    textHas('.attention-lines', '依存先: dependency'),
    '依存先が短縮されません',
  );
  clean();
  await visit('#/attention/stage%3A1', () =>
    textHas('.attention-lines', 'heartbeat'),
  );
  check(
    textHas('.attention-lines', '対象: alpha・#1'),
    '監視の対象が短縮されません',
  );
  await visit('#/attention/proposal%3A1', () =>
    textHas('.attention-lines', '案ID'),
  );
  check(
    document.querySelector<HTMLAnchorElement>('a[target="_blank"]').href ===
      'https://example.invalid/first/alpha/pulls/1',
    'PRの完全名が変わりました',
  );
  await visit('#/close-requests/' + id, () =>
    document.querySelector('.close-request'),
  );
  check(
    textHas('.close-request', '後続: successor#3') &&
      textHas('.close-request', '読めない依存指定: dependency#2'),
    'クローズ資料の表示が違います',
  );
  check(
    textHas('.close-request', 'dependency#2（依頼者の申告）') &&
      textHas('.close-request', 'successor@abcdef'),
    '参照の識別子が短縮表示で失われました',
  );
  check(
    document.querySelector<HTMLAnchorElement>('.close-request a').href ===
      'https://example.invalid/first/alpha/issues/1',
    'Giteaへのリンクが変わりました',
  );
  button('閉じない').click();
  await wait(() => textHas('#app', '「閉じない」を選びました'));

  const { stored } = await import(ui + 'composer-storage.js');
  await stored('new-request', {
    id: 'unsent',
    messageId: 'unsent-message',
    text: '保持した本文',
    images: [],
    creation: { id, project: 'former/old', provider: 'codex' },
  });
  await visit('#/new', () => textHas('.error-box', 'いまの一覧にありません'));
  check(
    textHas('.error-box', '依頼先「old」'),
    '残った入力の旧名が短縮されません',
  );
  const select = document.querySelector<HTMLSelectElement>(
    'select[aria-label="プロジェクト"]',
  );
  check(
    [...select.options].find((option) => option.value === 'first/alpha')
      ?.textContent === 'alpha',
    '選択肢の表示または値が違います',
  );
  check(
    [...select.options].find((option) => option.value === 'ninth/detail')
      ?.textContent === '読取資料',
    '設定済みの表示名が変わりました',
  );
  check(
    [...select.options]
      .filter((option) => option.value.endsWith('/shared'))
      .every((option) => option.textContent === option.value),
    '同名の選択肢を識別できません',
  );
  select.value = 'first/alpha';
  select.dispatchEvent(new Event('change'));
  await wait(() => textHas('.retarget-notice', '名前が「alpha」'));
  clean();
  button('入力を新しい依頼先へ移す').click();
  await wait(
    () => document.querySelector<HTMLElement>('.retarget-notice').hidden,
  );
  const saved = await stored('new-request');
  check(
    saved.creation.project === 'first/alpha' && saved.text === '保持した本文',
    '保存先の完全名または入力が失われました',
  );
  // 難易度を必須にする前に保存した入力（creation に tier なし）は、選び直した難易度を足して送る。
  const tier = document.querySelector<HTMLSelectElement>(
    'select[aria-label="相談の難易度"]',
  );
  check(!tier.disabled, '保存済みの入力で相談の難易度を選べません');
  tier.value = 'standard';
  tier.dispatchEvent(new Event('change'));
  document
    .querySelector('form')
    .dispatchEvent(new Event('submit', { cancelable: true }));
  await wait(
    () =>
      location.hash === '#/drafts/' + id &&
      document.querySelector('.conversation'),
  );
  clean();
  check(
    textHas('.conversation', '調査の報告（dependency#2）'),
    '調査の参照元が短縮されません',
  );
  check(
    textHas('.conversation', '引用 first/alpha を保持'),
    '会話の原文が変わりました',
  );
  button('依頼内容').click();
  await wait(() => textHas('#app', '連携を直す'));
  check(
    textHas('#app', '・dependency: src/') &&
      textHas('#app', 'リポジトリ · dependency'),
    '相談の分割案と範囲が短縮されません',
  );
  clean();

  await visit('#/issues/first/alpha/1', () =>
    document.querySelector('.issue-content'),
  );
  check(
    textHas('.issue-status', '- holder #4') &&
      textHas('.issue-status', 'alpha#1 の承認'),
    '排他待ちまたは承認対象が短縮されません',
  );
  check(
    document.querySelector<HTMLAnchorElement>('a[target="_blank"]').href ===
      'https://example.invalid/first/alpha/issues/1',
    '依頼のリンクが変わりました',
  );
  check(location.hash === '#/issues/101', '共通番号のURLが変わりました');
  button('A: 了承する').click();
  await wait(() => textHas('.final-approvals', '了承を記録しました'));
  await visit(
    '#/stats',
    () =>
      document.querySelector('.inventory-stats') &&
      document.querySelector('.ops-results svg'),
  );
  for (const [selector, expected] of [
    ['.inventory-stats', 'inventory の Gitea番号 6'],
    ['.priority-review-daily', '「優先」の割合（priority）'],
    ['.priority-review-rounds', 'priority の Gitea番号 7'],
    ['.usage-repos', 'usage'],
    ['.fixed-test-usage', 'usage'],
    ['.tier-outcomes', 'alpha の Gitea番号 1'],
    ['.followup-rows', 'followup の Gitea番号 8'],
    ['.followup-rows', 'dependency#2'],
    ['.followup-stats', 'followup: 1件'],
    ['.usage-detail-table', '1件'],
    ['.ops-results', 'usage'],
  ])
    check(
      textHas(selector, expected),
      selector + ' に表示がありません: ' + expected,
    );
  check(
    textHas('.tier-outcomes', 'first/shared') &&
      textHas('.tier-outcomes', 'second/shared'),
    '独立モジュールへ衝突情報が届きません',
  );
  check(
    textHas('#stats-result', 'usage の Gitea番号 5') &&
      textHas('#stats-result', 'detail'),
    '使用量の見直しまたは読取ファイルの表示が違います',
  );
  clean();
  const statsProject = document.querySelector<HTMLSelectElement>(
    'select[aria-label="プロジェクト"]',
  );
  statsProject.value = 'first/alpha';
  button('集計する').click();
  await wait(() => document.querySelector('.inventory-stats'));

  await visit('#/projects/' + id, () =>
    document.querySelector('.project-definition'),
  );
  check(
    document.querySelector('h1').textContent === 'created' &&
      textHas('.project-definition', 'created'),
    '新しいプロジェクト定義が短縮されません',
  );
  check(
    textHas('.project-approve', 'Gitea に created を作ります'),
    '作成同意の表示が短縮されません',
  );
  for (const projectId of [
    '22222222-2222-4222-8222-222222222222',
    '33333333-3333-4333-8333-333333333333',
  ]) {
    await visit('#/projects/' + projectId, () =>
      textHas('.project-panel', 'created を作成し'),
    );
  }
  await visit(
    '#/list/waiting',
    () =>
      document.querySelector('.issue-row') &&
      document.querySelector('.hosts-assignment'),
  );
  check(textHas('.issue-row', 'alpha'), '一覧が短縮されません');
  const assignment = document.querySelector('.hosts-assignment');
  const headers = [...assignment.querySelectorAll('th')].map(
    (cell) => cell.textContent,
  );
  const row = assignment.querySelector<HTMLTableRowElement>('tbody tr');
  const assigned = row.cells[headers.indexOf('担当リポジトリ')].textContent;
  check(
    (assigned.match(/issue-desk/g) || []).length === 2 &&
      assigned.includes('issue-desk-intake') &&
      !/上限|実装中|公開中/.test(assigned),
    '担当名のグループまたは別ownerの区別が違います: ' + assigned,
  );
  check(
    ['総枠', '使用中', '空き']
      .map((name) => row.cells[headers.indexOf(name)].textContent)
      .join(',') === '4枠,3枠,1枠',
    'PC全体の枠が担当名と区別されません',
  );

  for (const [index, project] of collisions.entries()) {
    check(
      document.querySelectorAll('.issue-row').length === 4,
      '同番号の別ownerの行がキーで上書きされました',
    );
    [...document.querySelectorAll<HTMLElement>('.issue-row')]
      .find((row) => row.textContent.includes(project))
      .click();
    await wait(
      () =>
        location.hash === '#/issues/' + (102 + index) &&
        document.querySelector('.issue-content'),
    );
    clean();
    await visit(
      '#/list/waiting',
      () => document.querySelectorAll('.issue-row').length === 4,
    );
  }

  await visit('#/list/waiting', () =>
    textHas('.row-guidance', '待っている相手: holder#4'),
  );
  clean();

  // 製品のDOM関数で直接表示の検証例を描画する。href・value・キー・data属性は検査対象にしない。
  const { el } = await import(ui + 'dom.js');
  const { shownProject } = await import(ui + 'repository-display.js');
  const sample = { project: 'first/alpha', repo: 'second/dependency' };
  for (const render of [
    () => el('p', { text: `${sample.project}#1` }),
    () => el('td', {}, sample.repo),
    () => {
      const node = el('span');
      node.textContent = sample.project;
      return node;
    },
    () => el('span', { 'aria-label': sample.repo }),
  ]) {
    const node = render();
    document.querySelector('#app').append(node);
    check(leaks(node).length > 0, '表示への直接埋め込みを見逃しました');
    node.remove();
  }
  const node = el(
    'a',
    {
      href: 'https://example.invalid/' + sample.project,
      'data-project': sample.project,
      'data-key': 'issue:' + sample.project + '#1',
      text: shownProject(sample.project),
    },
    el('input', { value: sample.repo }),
  );
  document.querySelector('#app').append(node);
  check(
    leaks(node).length === 0 &&
      node.dataset.project === sample.project &&
      node.dataset.key === 'issue:first/alpha#1' &&
      node.querySelector('input').value === sample.repo,
    '表示以外の完全名が変わったか誤検出されました',
  );
  node.remove();
  const collision = el('p', { text: shownProject('first/shared', collisions) });
  check(
    leaks(collision).length === 0 && collision.textContent === 'first/shared',
    '同名衝突の例外を誤検出しました',
  );
  await fetch('/__result', {
    method: 'POST',
    body: JSON.stringify({ success: true }),
  });
} catch (error) {
  await fetch('/__result', {
    method: 'POST',
    body: JSON.stringify({ success: false, message: error.stack }),
  });
}
