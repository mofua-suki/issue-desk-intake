import type { ScopeDeclaration, ParsedScope } from './types.ts';

const scopeMarker = '<!-- issue-desk-scope:';
const scopeVersion = 1;
const scopeLimits = { paths: 40, path: 200, reason: 200 };
const scopeHeading = '## 変更範囲（受付の宣言）';
// 受付の区切り線（shared/src/proposal-body.mjs の intakeTrailer の先頭）。宣言の節はこの直前に付く。
const trailerStart = '\n\n---\n受付: ';

// 大文字小文字の比較キー。Windows（NTFS）が同じ名前とみなす組に近づけるため、1文字ずつ単純な大文字へそろえる
// （toLowerCase では揃わない ſ と s、ς と σ も同じになる）。大文字が複数文字になる文字（ß など）はそのまま。
const caseKey = (text: string) =>
  Array.from(String(text), (ch) => {
    const upper = ch.toUpperCase();
    return Array.from(upper).length === 1 ? upper : ch;
  }).join('');

// Windowsの予約デバイス名（拡張子付き・末尾の空白を含む。CON.txt も CON と同じ扱いになる）。
const reservedName =
  /^(CON|PRN|AUX|NUL|COM[0-9¹²³]|LPT[0-9¹²³]|CONIN\$|CONOUT\$)$/;
const reserved = (part) =>
  reservedName.test(caseKey(part.split('.')[0].trimEnd()));

// 1つのパスを正規化する。読めない指定は null（呼び出し側が宣言全体を「不明」にする）。
// 区切りは / に統一し、先頭の ./・重複した / ・途中の . を取り除く。末尾の / はディレクトリの印として残す。
// 絶対パス・ドライブ名・..・.git・制御文字・ワイルドカード・山括弧・引用符・リポジトリ全体（空）は受け付けない。
// 末尾が空白・ピリオドの要素もWindowsでは別名になりうるため受け付けない。途中の :（代替データストリーム）、
// 全角の区切り（／ ＼。Windowsでは区切りではなく別の文字なので、取り違えを避けて読まない）、予約デバイス名も
// 受け付けない。どれも宣言全体を「不明」（並列にしない）へ倒す。
function normalizeScopePath(raw: unknown): string | null {
  if (typeof raw !== 'string') return null;
  const text = raw.normalize('NFC').trim().replaceAll('\\', '/');
  if (!text || text.length > scopeLimits.path) return null;
  // eslint-disable-next-line no-control-regex -- 入力や表示から制御文字を除外する安全性検証。
  if (/[\u0000-\u001f\u007f*?[\]{}<>|"`:／＼]/.test(text)) return null;
  if (text.startsWith('/') || /^[A-Za-z]:/.test(text)) return null;
  const directory = text.endsWith('/');
  const parts = text.split('/').filter((p) => p && p !== '.');
  if (!parts.length) return null;
  if (
    parts.some(
      (p) =>
        p === '..' ||
        caseKey(p) === '.GIT' ||
        p.endsWith(' ') ||
        p.endsWith('.') ||
        reserved(p),
    )
  )
    return null;
  return parts.join('/') + (directory ? '/' : '');
}

const cleanReason = (text) =>
  String(text ?? '')
    .replace(/[\s<>]+/g, ' ')
    .trim()
    .slice(0, scopeLimits.reason);

// ディレクトリ（末尾 /）の宣言か。
const scopeDirectory = (p: string) => p.endsWith('/');

// 受付AIの出力（{known, paths, reason}）を宣言へ直す。欠落は null（宣言なし）。不正・解釈できない指定は
// 「不明」として残し、推測で範囲を補わない。同じパス（大文字小文字違いを含む）はまとめる。known=true の reason は、
// ディレクトリを含む宣言のときだけ残す（ディレクトリで宣言した理由。Issue #768）。ファイルだけの宣言では空文字。
function normalizeScope(value: {
  known?: boolean;
  paths?: unknown[];
  reason?: unknown;
}): ScopeDeclaration | null {
  if (value === undefined || value === null) return null;
  const unknown = (reason) => ({ known: false, paths: [], reason });
  if (
    typeof value !== 'object' ||
    Array.isArray(value) ||
    typeof value.known !== 'boolean'
  )
    return unknown('宣言の形式を読めません');
  if (!value.known)
    return unknown(
      cleanReason(value.reason) || '受付AIが範囲を特定できないと判断しました',
    );
  if (
    !Array.isArray(value.paths) ||
    !value.paths.length ||
    value.paths.length > scopeLimits.paths
  )
    return unknown(`宣言のパスが空か${scopeLimits.paths}件を超えています`);
  const paths = [];
  const seen = new Set();
  for (const raw of value.paths) {
    const p = normalizeScopePath(raw);
    if (!p) return unknown('読めないパスを含みます');
    const key = caseKey(p);
    if (seen.has(key)) continue;
    seen.add(key);
    paths.push(p);
  }
  return {
    known: true,
    paths,
    reason: paths.some(scopeDirectory) ? cleanReason(value.reason) : '',
  };
}

// 最後のマーカーが、受付が付けた位置（「## 変更範囲（受付の宣言）」見出しと箇条書きの直後）にあるか。
const inDeclarationPlace = (before) => {
  const heading = before.lastIndexOf(`\n\n${scopeHeading}\n`);
  if (heading < 0) return false;
  const lines = before.slice(heading + scopeHeading.length + 3).split('\n');
  return (
    lines.length > 1 &&
    lines.at(-1) === '' &&
    lines.slice(0, -1).every((line) => line.startsWith('- '))
  );
};

// Issue本文から宣言を読む。本文の最後のマーカーだけを使い、受付が付けた位置にあるときだけ宣言とみなす:
// 「## 変更範囲（受付の宣言）」見出しと箇条書きの直後で、閉じた直後に受付の区切り線（---）が続くこと。
// 作者（受付専用アカウントの起票か）は本文からは分からないので、呼び出し側が確かめる（shared/src/parallel.mjs の jobScope）。
// absent: マーカーが無い、または宣言の位置に無い（宣言の無い既存Issue、背景資料・画像の確認文・人の書いた
// 文章に紛れたもの）。unknown: 不明の宣言、または宣言の位置にあるが読めないマーカー。
// declared: 正規化済みのパス。マーカーに「後から付けた」印（backfilled: true）があれば backfilled: true を添える
// （Issue #769。true 以外の値は印の無い宣言として読む）。読む側は absent・unknown を他と並列にしない。
export function parseScope(body: string = ''): ParsedScope {
  // Giteaの画面で編集された本文は改行が CRLF になりうるので、位置の確認の前に LF へそろえる。
  const text = String(body).replaceAll('\r\n', '\n');
  const start = text.lastIndexOf(scopeMarker);
  if (start < 0 || !inDeclarationPlace(text.slice(0, start)))
    return { state: 'absent' };
  const rest = text.slice(start + scopeMarker.length);
  const end = rest.indexOf(' -->');
  const closed = end >= 0 && rest.startsWith(trailerStart, end + 4);
  let value;
  try {
    value = closed ? JSON.parse(rest.slice(0, end)) : null;
  } catch {
    value = null;
  }
  const broken: ParsedScope = {
    state: 'unknown',
    reason: '変更範囲のマーカーを読めません',
  };
  if (
    !value ||
    typeof value !== 'object' ||
    Array.isArray(value) ||
    value.v !== scopeVersion
  )
    return broken;
  // 変更なし（調査Issue）の宣言。パスも不明の印も混ざらないときだけ読む。
  if (value.none !== undefined)
    return value.none === true &&
      value.paths === undefined &&
      value.unknown === undefined
      ? { state: 'declared', paths: [], none: true }
      : broken;
  // 不明の印とパスが混ざった記録は、どちらとも決められないので読めないものとして扱う。
  if (value.unknown !== undefined)
    return value.unknown === true && value.paths === undefined
      ? { state: 'unknown', reason: '不明と宣言されています' }
      : broken;
  const scope = normalizeScope({ known: true, paths: value.paths });
  if (!scope.known || scope.paths.length !== value.paths.length) return broken;
  return {
    state: 'declared',
    paths: scope.paths,
    ...(value.backfilled === true ? { backfilled: true } : {}),
  };
}
