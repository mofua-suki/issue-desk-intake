interface MarkdownListItem {
  text: string;
  children: MarkdownList[];
}
export interface MarkdownList {
  type: 'list';
  ordered: boolean;
  start: number;
  items: MarkdownListItem[];
}
export type MarkdownBlock =
  | MarkdownList
  | { type: 'code'; lang: string; text: string }
  | { type: 'heading'; level: number; text: string }
  | { type: 'rule' }
  | { type: 'quote'; blocks: MarkdownBlock[] }
  | { type: 'table'; header: string[]; rows: string[][] }
  | { type: 'paragraph'; text: string };
export type InlineToken =
  | { type: 'text'; text: string }
  | { type: 'id'; text: string }
  | { type: 'code'; text: string }
  | { type: 'strong'; children: InlineToken[] }
  | { type: 'link'; text: string; href: string }
  | { type: 'url'; href: string; label: string };
// 一覧の更新日時の短い形と、本文の簡易Markdown・URLの表示の解析（Issue #1488 で client/src/ui/request-list.js から分けた）。
// client/src/ui/request-list.js が名前をそのまま再エクスポートする。このファイルは client/src/ui/request-list.js を import しない。

// 一覧の更新日時の短い形（スマホ幅）。24時間以内は相対、同じ年は「月/日」、それ以外は「年/月/日」。
// 端末の時計が少し進んでいる場合（1分以内の未来）も「たった今」とする。t は読める日時だけを渡す。
export function shortTime(
  t: string | number | Date | undefined,
  now = Date.now(),
) {
  const d = new Date(t ?? NaN);
  const ago = now - d.getTime();
  if (ago > -60000 && ago < 24 * 60 * 60 * 1000) {
    const minutes = Math.max(0, Math.floor(ago / 60000));
    if (!minutes) return 'たった今';
    return minutes < 60
      ? `${minutes}分前`
      : `${Math.floor(minutes / 60)}時間前`;
  }
  const day = `${d.getMonth() + 1}/${d.getDate()}`;
  return d.getFullYear() === new Date(now).getFullYear()
    ? day
    : `${d.getFullYear()}/${day}`;
}

// --- 本文の簡易Markdown（Issue #296） -----------------------------------------------------------------
// 文字列をブロック・インラインの配列にするだけで、HTMLは作らない。画面は textContent と要素の組み立てで
// 表示する（HTML文字列を解釈させないので、本文中のHTMLは常に文字のまま出る）。対応しない記法はそのまま文字。
const fencePattern = /^ {0,3}(`{3,}|~{3,})(.*)$/;
const headingPattern = /^ {0,3}(#{1,6})[ \t]+(.*)$/;
const rulePattern = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
const quotePattern = /^ {0,3}>[ \t]?(.*)$/;
const itemPattern = /^( *)([-*+]|\d{1,9}[.)])[ \t]+(.*)$/;
const separatorPattern =
  /^ {0,3}\|?[ \t]*:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)*\|?[ \t]*$/;
// 見出しの末尾の閉じの「#」（空白の後に続くものだけ）を外す。「# C#」の「#」は残す。
function headingText(text: string) {
  const t = text.trimEnd();
  let end = t.length;
  while (end > 0 && t[end - 1] === '#') end--;
  return end < t.length && (end === 0 || /[ \t]/.test(t[end - 1]))
    ? t.slice(0, end).trimEnd()
    : t;
}
function listItem(line: string) {
  const m = itemPattern.exec(line);
  if (!m) return null;
  const ordered = /\d/.test(m[2]);
  return {
    indent: m[1].length,
    ordered,
    start: ordered ? parseInt(m[2], 10) : 1,
    text: m[3],
  };
}
const tableStart = (lines: string[], i: number) =>
  lines[i].includes('|') &&
  i + 1 < lines.length &&
  lines[i + 1].includes('|') &&
  separatorPattern.test(lines[i + 1]);
// 段落や箇条書きの続きを打ち切る行（見出し・コード・水平線・引用）。
const startsBlock = (line: string) =>
  fencePattern.test(line) ||
  headingPattern.test(line) ||
  rulePattern.test(line) ||
  quotePattern.test(line);
// 表の1行をセルへ分ける。`\|` はセル内の縦線として残す。
function tableCells(line: string) {
  let s = line.trim();
  if (s.startsWith('|')) s = s.slice(1);
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);
  const cells = [''];
  for (let i = 0; i < s.length; i++) {
    if (s[i] === '\\' && s[i + 1] === '|') {
      cells[cells.length - 1] += '|';
      i++;
    } else if (s[i] === '|') cells.push('');
    else cells[cells.length - 1] += s[i];
  }
  return cells.map((c) => c.trim());
}
// 箇条書き。入れ子は1段まで（それより深い項目は1段目の入れ子へまとめる）。
function parseList(lines: string[], i: number): [MarkdownList, number] {
  const first = listItem(lines[i])!;
  const list: MarkdownList = {
    type: 'list',
    ordered: first.ordered,
    start: first.start,
    items: [],
  };
  let item: MarkdownListItem | null = null,
    nested: MarkdownList | null = null;
  for (; i < lines.length; i++) {
    const line = lines[i];
    if (!line.trim()) break;
    const m = listItem(line);
    if (m && m.indent >= first.indent + 2) {
      if (!nested || nested.ordered !== m.ordered) {
        nested = {
          type: 'list',
          ordered: m.ordered,
          start: m.start,
          items: [],
        };
        item!.children.push(nested);
      }
      nested.items.push({ text: m.text, children: [] });
      continue;
    }
    if (m && m.ordered !== list.ordered) break;
    if (m) {
      item = { text: m.text, children: [] };
      nested = null;
      list.items.push(item);
      continue;
    }
    if (startsBlock(line)) break;
    const target = nested ? nested.items.at(-1) : item;
    target!.text += `\n${line.trim()}`;
  }
  return [list, i];
}
export function markdownBlocks(text: string, depth = 0): MarkdownBlock[] {
  const lines = String(text ?? '')
    .replace(/\r\n?/g, '\n')
    .split('\n');
  const blocks: MarkdownBlock[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) {
      i++;
      continue;
    }
    const fence = fencePattern.exec(line);
    if (fence && !(fence[1][0] === '`' && fence[2].includes('`'))) {
      const mark = fence[1];
      const closes = (l: string) => {
        const t = l.trim();
        return t.length >= mark.length && t === mark[0].repeat(t.length);
      };
      const body = [];
      for (i++; i < lines.length && !closes(lines[i]); i++) body.push(lines[i]);
      i++;
      blocks.push({
        type: 'code',
        lang: fence[2].trim().split(/\s+/)[0],
        text: body.join('\n'),
      });
      continue;
    }
    const heading = headingPattern.exec(line);
    if (heading) {
      blocks.push({
        type: 'heading',
        level: heading[1].length,
        text: headingText(heading[2]),
      });
      i++;
      continue;
    }
    if (rulePattern.test(line)) {
      blocks.push({ type: 'rule' });
      i++;
      continue;
    }
    if (quotePattern.test(line)) {
      const body = [];
      for (; i < lines.length && quotePattern.test(lines[i]); i++)
        body.push(quotePattern.exec(lines[i])![1]);
      // 引用の中の引用は3段まで解釈し、それより深いものは文字のまま出す。
      blocks.push({
        type: 'quote',
        blocks:
          depth < 3
            ? markdownBlocks(body.join('\n'), depth + 1)
            : [{ type: 'paragraph', text: body.join('\n') }],
      });
      continue;
    }
    const item = listItem(line);
    if (item && item.indent < 4) {
      const [list, next] = parseList(lines, i);
      blocks.push(list);
      i = next;
      continue;
    }
    if (tableStart(lines, i)) {
      const header = tableCells(line);
      const rows = [];
      for (
        i += 2;
        i < lines.length && lines[i].trim() && lines[i].includes('|');
        i++
      )
        rows.push(tableCells(lines[i]));
      blocks.push({ type: 'table', header, rows });
      continue;
    }
    const paragraph = [line.trimEnd()];
    for (i++; i < lines.length; i++) {
      const next = lines[i];
      const nextItem = listItem(next);
      if (
        !next.trim() ||
        startsBlock(next) ||
        (nextItem && nextItem.indent < 4) ||
        tableStart(lines, i)
      )
        break;
      paragraph.push(next.trimEnd());
    }
    blocks.push({ type: 'paragraph', text: paragraph.join('\n') });
  }
  return blocks;
}
// 40文字以上続く英数字・ハイフンの列（コミットハッシュ・長いID）。画面では先頭12文字に畳む。
export function longIdParts(
  text: string,
): { type: 'text' | 'id'; text: string }[] {
  const parts: { type: 'text' | 'id'; text: string }[] = [];
  let last = 0;
  for (const m of text.matchAll(/[A-Za-z0-9-]{40,}/g)) {
    // ハイフンだけの列はIDではない。
    if (!/[A-Za-z0-9]/.test(m[0])) continue;
    if (m.index > last)
      parts.push({ type: 'text', text: text.slice(last, m.index) });
    parts.push({ type: 'id', text: m[0] });
    last = m.index + m[0].length;
  }
  if (last < text.length) parts.push({ type: 'text', text: text.slice(last) });
  return parts;
}
// リンクにしてよいのは http: と https: だけ。javascript: などや読めないURLは文字のまま出す。
function webUrl(href: string) {
  try {
    return ['http:', 'https:'].includes(new URL(href).protocol) ? href : null;
  } catch {
    return null;
  }
}
// 裸のURLの末尾の句読点・閉じ括弧（対応する開き括弧が無いもの）はURLに含めない。
function trimUrl(url: string): [string, string] {
  let href = url;
  for (;;) {
    const c = href.at(-1) ?? '';
    if (/[.,;:!?*_~\]]/.test(c)) href = href.slice(0, -1);
    else if (c === ')' && href.split('(').length < href.split(')').length)
      href = href.slice(0, -1);
    else break;
  }
  return [href, url.slice(href.length)];
}
// URLの短い表示。Giteaの PR・Issue は番号で、それ以外は「リンク」（全文は title に出す）。
export function urlLabel(href: string | URL) {
  const m = new URL(href).pathname.match(
    /\/[^/]+\/[^/]+\/(pulls|issues)\/(\d+)\/?$/,
  );
  return m ? `${m[1] === 'pulls' ? 'PR' : 'Issue'} #${m[2]}` : 'リンク';
}
const inlinePattern =
  /`([^`\n]+)`|\*\*(?=\S)([^\n]*?\S)\*\*|\[([^\]\n]+)\]\(([^()\s]+)\)|https?:\/\/[^\s<>"'`（）「」『』、。，．！？]+/g;
export function inlineTokens(text: string): InlineToken[] {
  const source = String(text ?? '');
  const tokens: InlineToken[] = [];
  const plain = (s: string) => tokens.push(...longIdParts(s));
  let last = 0;
  for (const m of source.matchAll(inlinePattern)) {
    plain(source.slice(last, m.index));
    last = m.index + m[0].length;
    if (m[1] !== undefined) tokens.push({ type: 'code', text: m[1] });
    else if (m[2] !== undefined)
      tokens.push({ type: 'strong', children: inlineTokens(m[2]) });
    else if (m[3] !== undefined) {
      const href = webUrl(m[4]);
      if (href) tokens.push({ type: 'link', text: m[3], href });
      else plain(m[0]);
    } else {
      const [url, rest] = trimUrl(m[0]);
      const href = webUrl(url);
      if (href) tokens.push({ type: 'url', href, label: urlLabel(href) });
      else plain(url);
      plain(rest);
    }
  }
  plain(source.slice(last));
  return tokens;
}
