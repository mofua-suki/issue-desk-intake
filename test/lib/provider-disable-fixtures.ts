// Executable examples for the pre-migration state. Response checks belong to the
// example itself; the fake window never turns missing data into a domain result.
const common = [
  '4xx・5xx・通信失敗では再送しない。回り込まないで停止し報告する。403では権限を追加しない。トークンを切り替えない。',
  '4xx、5xx、接続失敗では送り直さない。迂回しないで中止して利用者に伝える。403では権限を増やさない。認証を変更しない。',
  '4xx/5xx と通信エラーは再実行しない。回避しないで操作を止めて知らせる。403では権限を付与しない。トークンを差し替えない。',
];
const list = [
  'disables の欠測、JSONでない本文、配列でない値は空一覧と扱わない。未確認とする。',
  'disables が欠落した応答やJSONの不正、配列以外の値を設定なしと判断しない。確認できていないと伝える。',
  'JSON を読めない、disables が欠測、配列以外の場合は無効設定なしとみなさない。確定できない。',
];
const set = [
  '結果が不明なときは一覧を読み直して provider・until・by・reason を照合する。照合できなければ未確認で停止する。自動で再送しない。until は送った値と照合する。期限省略時は setAt が送信の前後の時刻の範囲内、until が setAt＋24時間と確かめる。',
  '応答が不明なら一覧を再取得し provider・until・by・reason を突き合わせる。一致しなければ確定できないので中止する。自動で送り直さない。until を指定値と突き合わせる。期限を省略したら setAt は要求の開始から終了の範囲、until は setAt+24h と確認する。',
  '結果不明では再度一覧を読む。provider と until と by と reason の一致を確認する。確認が取れなければ確認できていないとして止める。自動で再実行しない。until は送信値との一致を確認する。期限省略なら setAt は送信前後の間、until は setAt + 24時間であることを確認する。',
];
const cancel = [
  'cancelled=true は取り消した、cancelled=false は設定がなかった。cancelled の欠測や真偽値でない値はどちらとも扱わない。未確認とする。結果が不明なら一覧を読み直して確かめる。',
  'cancelled = true は取消済み、cancelled = false は設定なし。cancelled が欠落、またはbooleanでなければいずれとも判断しない。確認できていないとする。応答不明では一覧を再取得する。',
  'cancelled=true なら取り消した、cancelled=false なら設定がなかった。cancelled の欠測や真偽値以外はどちらとも扱わない。確定できない。結果不明なら再度一覧を取得して確認する。',
];

export function guidedExamples(base, variant = 0) {
  let serial = 0;
  const source = base.replace('## NAS', common[variant] + '\n\n## NAS');
  const result = source.replace(
    /^```(?:sh|shell|bash)\s*\n([\s\S]*?)^```\s*$/gm,
    (_, code) => {
      const op = /\/cancel/.test(code)
        ? 'cancel'
        : /--method POST/.test(code)
          ? 'set'
          : 'list';
      const title = { list: '一覧', set: '設定', cancel: '取消' }[op];
      // Preserve the predecessor's multiline/file/stdin examples, but check the raw
      // response before presenting it as a result. No HTTP error is swallowed.
      const guard = `node -e 'const fs=require("fs"); let r; try { r=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); } catch { process.exit(1); } if (!r || !Array.isArray(r.disables)${op === 'cancel' ? ' || typeof r.cancelled!=="boolean"' : op === 'set' ? ' || !r.set || typeof r.set.provider!=="string"' : ''}) process.exit(1); console.log(JSON.stringify(r));' "$HOME/result.json"`;
      const guidance = { list, set, cancel }[op][variant];
      return `### ${title}${++serial}\n\n${variant === 2 ? guidance : '受付の応答を確認する。'}\n\n\`\`\`sh\n${variant === 1 ? '## Ubuntu (フェンス内のシェルコメント)\n' : ''}${code.trimEnd()} > "$HOME/result.json"\n${guard}\n\`\`\`\n\n${variant === 2 ? '' : guidance}\n`;
    },
  );
  return variant === 1
    ? result
        .replace(/^### /gm, '#### ')
        .replace(/^## (NAS|Ubuntu)$/gm, '## $1\n\n### 窓口の操作')
        .replace(/^```sh$/gm, '~~~bash')
        .replace(/^```$/gm, '~~~')
    : result;
}

// Mutations are placed beside exactly one fence. Other operations retain their
// valid guidance; the diagnostic must identify the changed example's own line.
export function besideExample(markdown, example, sentence) {
  const lines = markdown.split('\n');
  lines.splice(example.end + 1, 0, '', sentence);
  return lines.join('\n');
}

export const unsafeGuidance = [
  '欠測は無効設定なしとして扱う。',
  '結果不明の変更を再送する。',
  '拒否された後に認証を切り替える。',
  '拒否された後に権限を追加する。',
  '期限を省略した設定を自動で再送する。',
];
