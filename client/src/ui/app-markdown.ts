// 受付画面の本文の簡易Markdown表示。client/src/ui/app.js の一部（Issue #456）で、単独のモジュールではない。受付は
// client/scripts/build.ts の順に app.js とつないだ1つのモジュールとして /app.js を配信する。
// --- 本文の簡易Markdown（解析は request-list.js の markdownBlocks・inlineTokens） -------------------------
// 要素の組み立てと textContent だけで表示し、HTML文字列を解釈させない（本文中のHTMLは文字のまま出る）。
// 長いID・コミットハッシュは先頭12文字に畳み、押すと全文を出す（開いた全文は選択・コピーできる）。
function longId(full: string) {
  const box = el('span', { class: 'long-id' });
  const show = (open: boolean): void =>
    box.replaceChildren(
      ...(open ? [el('span', { class: 'long-id-full', text: full })] : []),
      el('button', {
        type: 'button',
        class: 'long-id-toggle',
        title: open ? '短く表示' : full,
        'aria-expanded': open ? 'true' : 'false',
        text: open ? '短く表示' : `${full.slice(0, 12)}…`,
        onClick: () => show(!open),
      }),
    );
  show(false);
  return box;
}
function inlineNode(t: InlineToken): DOMChild {
  if (t.type === 'text') return t.text;
  if (t.type === 'id') return longId(t.text);
  if (t.type === 'code')
    return el('code', {}, longIdParts(t.text).map(inlineNode));
  if (t.type === 'strong') return el('strong', {}, t.children.map(inlineNode));
  // リンクは別タブで開き、参照元を送らない。表示が短くなる裸のURLも title で全文を確かめられる。
  return el('a', {
    href: t.href,
    target: '_blank',
    rel: 'noreferrer',
    title: t.href,
    text: t.type === 'url' ? t.label : t.text,
  });
}
const inline = (text: string) => inlineTokens(text).map(inlineNode);
const listNode = (b: MarkdownList): HTMLElement =>
  el(
    b.ordered ? 'ol' : 'ul',
    { start: b.ordered && b.start !== 1 ? String(b.start) : undefined },
    b.items.map((item) =>
      el('li', {}, inline(item.text), item.children.map(listNode)),
    ),
  );
function blockNode(b: MarkdownBlock): HTMLElement {
  // 画面の見出し（h1・h2）と区別するため、本文の見出しは h3 から始める。
  if (b.type === 'heading')
    return el(
      `h${Math.min(b.level + 2, 6)}` as keyof HTMLElementTagNameMap,
      { class: `md-heading md-h${b.level}` },
      inline(b.text),
    );
  if (b.type === 'list') return listNode(b);
  // 表とコードは横スクロールの枠に入れ、ページ全体を横に広げない。
  if (b.type === 'code')
    return el(
      'div',
      { class: 'md-scroll' },
      el('pre', {}, el('code', { text: b.text })),
    );
  if (b.type === 'table')
    return el(
      'div',
      { class: 'md-scroll' },
      el(
        'table',
        {},
        el(
          'thead',
          {},
          el(
            'tr',
            {},
            b.header.map((c) => el('th', {}, inline(c))),
          ),
        ),
        el(
          'tbody',
          {},
          b.rows.map((row) =>
            el(
              'tr',
              {},
              row.map((c) => el('td', {}, inline(c))),
            ),
          ),
        ),
      ),
    );
  if (b.type === 'quote') return el('blockquote', {}, b.blocks.map(blockNode));
  if (b.type === 'rule') return el('hr');
  return el('p', {}, inline(b.text));
}
const markdown = (text: string) =>
  el('div', { class: 'md' }, markdownBlocks(text).map(blockNode));
