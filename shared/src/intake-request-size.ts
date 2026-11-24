// 出す側と受付が共有する相談1件の目安（IN2）。
export const intakeRequestSize = Object.freeze({
  files: 4,
  materialBytes: 240000,
  nearLimit: 0.8,
});
export const intakeRequestGuidance = `依頼1件の目安: 対象ファイル${intakeRequestSize.files}件・読む資料${intakeRequestSize.materialBytes} bytes。対象を名指しし、資料は該当節・行範囲だけ読む。目安超えでも通常の相談を続ける。自動分割は資料を縮めても入力上限に収まらないときだけ。分割禁止は共通の出典規則（「## 利用者の判断と根拠」の該当判断を支持する利用者の「」付き原文引用と日時）で裏付けた場合だけ。引用のない「分割しない」「分けないで」「分けずに」「1件で」は判断材料。子が目安・元の範囲に合わなければ元の相談を続ける。`;

export function namedRequestFiles(text: string) {
  // URL・メール・版番号をファイルと数えず、通常の単語には拡張子か既知の名前を求める。
  const source = text.replace(/https?:\/\/[^\s`<>「」]+/gi, ' ');
  const tokens = source.match(/[A-Za-z0-9_@.+/-]+/g) || [];
  return [
    ...new Set(
      tokens
        .map((token) => token.replace(/(?<!\.)\.+$/, '').replace(/^\.\//, ''))
        .filter((token) => {
          if (!token || token.startsWith('/') || token.endsWith('/'))
            return false;
          if (token.includes('@') && !token.includes('/')) return false;
          const parts = token.replace(/^\.\//, '').split('/');
          if (parts.some((part) => !part || part === '.' || part === '..'))
            return false;
          const dot = token.lastIndexOf('.');
          return (
            parts.length > 1 ||
            /^\.[A-Za-z][\w.-]*$/.test(token) ||
            /^(?:Dockerfile|Makefile|GNUmakefile|Containerfile|Procfile|LICENSE|LICENCE|COPYING|NOTICE|README)(?:[.-][\w.-]+)?$/.test(
              token,
            ) ||
            (dot > 0 && /^[A-Za-z][\w-]*$/.test(token.slice(dot + 1)))
          );
        }),
    ),
  ];
}

// フェンス内の行と見出しを共通に判定する。閉じフェンスは同じ文字・同じ長さ以上。
export function markdownLines(text) {
  let fenceChar = '';
  let fenceLength = 0;
  return String(text)
    .split(/\r?\n/)
    .map((line) => {
      const fence = /^ {0,3}(`{3,}|~{3,})([\s\S]*)$/.exec(
        line.replace(/\r$/, ''),
      );
      if (fenceChar) {
        if (
          fence &&
          fence[1][0] === fenceChar &&
          fence[1].length >= fenceLength &&
          /^[ \t]*$/.test(fence[2])
        ) {
          fenceChar = '';
          fenceLength = 0;
        }
        return { line, heading: null, fenced: true };
      }
      if (fence && (fence[1][0] !== '`' || !fence[2].includes('`'))) {
        fenceChar = fence[1][0];
        fenceLength = fence[1].length;
        return { line, heading: null, fenced: true };
      }
      const match = /^(#{1,6})\s+(.+?)\s*#*\s*$/.exec(line);
      return {
        line,
        heading: match ? { depth: match[1].length, title: match[2] } : null,
        fenced: false,
      };
    });
}

// 同名の節をすべて集め、下位見出しを含める。空の節も存在として扱う。
function sections(text, title) {
  const found = [];
  let current = null;
  for (const row of markdownLines(text)) {
    if (row.heading && row.heading.depth <= 2) {
      current = null;
      if (row.heading.depth === 2 && row.heading.title === title) {
        current = [];
        found.push(current);
      }
    } else if (current) current.push(row);
  }
  return found;
}

function sizeFiles(text) {
  const scopes = sections(text, '変更範囲');
  return namedRequestFiles(
    scopes.length
      ? scopes
          .flat()
          .map((row) => row.line)
          .join('\n')
      : text,
  );
}

export function requestSizeWarning(text: string) {
  const count = sizeFiles(text).length;
  return count > intakeRequestSize.files
    ? `警告: 名指ししたファイルが${count}件あります。${intakeRequestGuidance} 送信は続けます。`
    : null;
}

/**
 * 受付は優先度によらず、起票に変更・追加・削除するファイルの宣言を求める。
 * 依頼文にファイルが1件も無いと、受付が止めて質問を返す往復になるので、送る前に知らせる。
 */
export function missingChangeFilesWarning(text: string) {
  return namedRequestFiles(text).length === 0
    ? '警告: 依頼文に変更するファイルが1件もありません。受付は優先度（割り込み・通常・自動）によらず、変更・追加・削除するファイルの宣言を求めます。repo-search などで特定したリポジトリのルート相対パスを本文に書いてください。送信は続けます。'
    : null;
}
