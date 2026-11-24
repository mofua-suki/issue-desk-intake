// 相談が受け付ける範囲の公開仕様を、正本（shared/src/desk-contract.ts・assignments.json）から作る。
// node scripts/generate-consultation-spec.ts --write で docs/consultation-spec.md と
// shared/src/consultation-spec.generated.ts を更新する。引数なしは差分があれば失敗する。
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { deskContract } from '../shared/src/desk-contract.ts';

const root = path.resolve(import.meta.dirname, '..');
const markdownFile = path.join(root, 'docs/consultation-spec.md');
const moduleFile = path.join(root, 'shared/src/consultation-spec.generated.ts');
/** server/internal/ai/process.go の IdleTimeout。test/consultation-spec.test.ts が一致を確かめる。 */
const idleTimeoutSeconds = 240;

export function buildSpec(assignments: Record<string, unknown>) {
  const { size, input, limits, proposals } = deskContract;
  return {
    request: {
      files: size.files,
      materialBytes: size.materialBytes,
      title: limits.title,
      body: limits.body,
    },
    consultation: {
      readFiles: limits.readFiles,
      questions: limits.questions,
      rounds: limits.rounds,
      roundsAfterRaise: limits.roundsAfterRaise,
      chatText: limits.chatText,
      conversation: limits.conversation,
      proposals,
      inputChars: input.chars,
      inputBytes: input.bytes,
      idleTimeoutSeconds,
    },
    implementation: {
      runLimit: assignments.implementationRunLimit,
      usageLimit: assignments.implementationUsageLimit,
      reviewRounds: assignments.reviewRoundLimit,
      fixedTestRepairRounds: assignments.fixedTestRepairRounds,
      sizeFixRounds: assignments.sizeFixRounds,
      prSizeFixRounds: assignments.prSizeFixRounds,
    },
  };
}

export function specMarkdown(spec: ReturnType<typeof buildSpec>): string {
  const { request: r, consultation: c, implementation: i } = spec;
  return `# 相談が受け付ける範囲

起票する前に読む。\`create-issue.ts --spec\` でも同じ内容を表示する。生成物なので直接編集しない（\`node scripts/generate-consultation-spec.ts --write\`）。

## 依頼1件の大きさ

| 項目 | 値 | 超えたとき |
| --- | --- | --- |
| 変更対象のファイル | ${r.files}件 | 目安。超過だけを理由に通常の相談を止めたり強制分割したりしない。送信時は警告して続ける |
| 読む資料 | ${r.materialBytes} bytes | 資料を圧縮しても入力上限に収まらないときだけ、受付が子の相談へ分ける |
| 題名 | ${r.title}文字 | 受付が断る |
| 本文 | ${r.body}文字 | 受付が断る |

送信前の件数検査は、本文に「## 変更範囲」があればその節の変更対象を重複除外して数える。複数の節は合算し、下位見出しを含め、同階層以上の見出しで節を終える。\`\`\` または ~~~ で囲まれた範囲の見出しは節の境界に使わない。空の節があっても本文全体を数え直さない。節がなければ従来どおり本文全体を数える。節がある場合、節外で根拠として名指ししたファイルが多いだけでは停止しない。

閉じフェンスがない場合は、開きフェンスから本文末尾までをフェンス内として扱う。

資料を圧縮しても下記の1回の送信の全体入力上限に収まらない場合、受付は子の相談への自動分割を試みる。子は2〜25件で、各本文のUTF-8バイト数は元の会話全体（各発言の役割・区切りと、会話に含まれない現在の本文を含む）のUTF-8バイト数より小さく、各本文はUTF-16コード単位で8000以下である必要があり、検査に合わなければ分割を進めず元の入力超過エラーを返す。子の${r.files}件以内の検査は現在の実装にはない。

対象の追加・漏れの検査は本文中の特定のファイル名に限られる。例えば \`server/a.go\` や \`README.md\` は検査されるが、ディレクトリや拡張子のない対象は検査されないため、範囲全体の保持は保証されない。利用者は子の内容を確認する。

入力超過時は、次の出典規則で分割禁止を確認できる場合に元の入力超過エラーを返す。最初に受け取った依頼文では、出典節に記した利用者の原文と日時を確認する。保存本文や後の発言の出典節は、別に保存された利用者発言の原文と日時と照合する。合成された子の依頼文は証跡に使わず、後から撤回された判断は引き継がない。

分割禁止を利用者の判断として扱うには「## 利用者の判断と根拠」にその判断を支持する利用者の「」付き原文引用と日時が必要である。無関係な引用や否定された指定は裏付けにならず、引用のない指定は判断材料として扱う。これは分割判断の出典規則であり、目安を超える通常の相談を続けるための個別許可ではない。分けられない理由（heavyReason）にも同じ出典規則を求める。受入条件が2項目以上ある通常起票の本文または分割案に、裏付けのない heavyReason がある回答では、受付が一度だけ分割案を求め、それでも裏付けが無ければ heavyReason を外して起票する（実装の難易度は通常の判定になり、heavyReason によるPRの量の差し戻しの例外にもならない）。受入条件が1項目なら分けられないため外さない。\`--over-spec <理由>\` は互換性のため受け付ける。

## 相談AI

| 項目 | 値 |
| --- | --- |
| 1回の送信の上限（固定指示・会話・資料・出力スキーマの全体） | ${c.inputChars}文字・${c.inputBytes} bytes |
| 1発言 | ${c.chatText}文字 |
| 会話全体 | ${c.conversation}文字（UTF-16コード単位） |
| 1回で読み足せるファイル | ${c.readFiles}件 |
| 1回の質問 | ${c.questions}件 |
| 往復 | ${c.rounds}回（引き上げ後は${c.roundsAfterRaise}回を追加） |
| 分割案 | ${c.proposals}件 |
| 無出力の時間切れ | ${c.idleTimeoutSeconds}秒 |

通常相談の会話全体の上限は、実際にAIへ送る会話投影（発言と参照、および省略位置）のJSONをUTF-16コード単位で計数する。上限を超えるときは古い発言の参照から1件ずつ省略し、元の発言・参照の位置と未確認であることをAIに伝える。保存された発言と参照は変更しない。省略しても収まらない場合は停止する。送信受付時も新しい発言を含む投影で判定する。

プロジェクト相談の送信受付上限は、保存する発言一覧のJSONをUTF-16コード単位で計数する。

通常の相談では変更範囲を確認し、${r.files}件は目安として扱う。件数だけを理由に停止や強制分割はしない。未特定や広いディレクトリは起票前に資料と照合する。

プロジェクト相談の最初の分割案は新規プロジェクトの定義・承認の経路で、通常の相談AIの確定・修復経路とは別である。この4件検査の対象外であり、5件以上でも定義を承認できる。起票前に運用者が各案の範囲を確認する。

## 実装

| 項目 | 値 |
| --- | --- |
| 実装の実行回数 | ${i.runLimit}回 |
| 重み付き使用量 | ${i.usageLimit} |
| 独立レビューの保留 | ${i.reviewRounds}回 |
| 固定テストの修復 | ${i.fixedTestRepairRounds}回 |
| 差分の大きさの修正 | ${i.sizeFixRounds}回（PR全体は${i.prSizeFixRounds}回） |

実行回数・使用量の上限に達した依頼は止まる。範囲の広い依頼は、資料の上限に収まっても実装で止まるので、起票の時点で分ける。

使用量上限で停止した依頼の受付画面からの再試行は、利用者本人の操作を再開の承認として扱う。運用代行トークンからの再試行には、利用者の具体的な承認発言が必要となる。

値は生成時の既定で、受付の管理設定で変わることがある。「${r.files}件」は実装の完了を保証する値ではなく、実測からの導出も未確認。
`;
}

export function specModule(spec: ReturnType<typeof buildSpec>): string {
  return `// scripts/generate-consultation-spec.ts から生成。直接編集しない。\nexport const consultationSpecText = ${JSON.stringify(specMarkdown(spec))};\n`;
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href
) {
  const spec = buildSpec(
    JSON.parse(fs.readFileSync(path.join(root, 'assignments.json'), 'utf8')),
  );
  const outputs: [string, string][] = [
    [markdownFile, specMarkdown(spec)],
    [moduleFile, specModule(spec)],
  ];
  if (process.argv[2] === '--write')
    for (const [file, text] of outputs) fs.writeFileSync(file, text);
  else if (
    outputs.some(
      ([file, text]) =>
        !fs.existsSync(file) || fs.readFileSync(file, 'utf8') !== text,
    )
  ) {
    console.error(
      '相談の仕様が正本と一致しません。node scripts/generate-consultation-spec.ts --write で更新してください。',
    );
    process.exitCode = 1;
  }
}
