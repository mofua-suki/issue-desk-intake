// 利用者の承認発言として受け付ける内容。空・短すぎる・定型語だけの発言は断る（画面・スクリプト・受付で同じ判定）。
const boilerplateStatements = new Set([
  '承認',
  '承認します',
  'approve',
  'approved',
  'yes',
  'ok',
  '了解',
  '了解しました',
  'お願いします',
  'よろしくお願いします',
  'どうぞよろしくお願いします',
  'お願いいたします',
  'よろしくお願いいたします',
  'どうぞよろしくお願いいたします',
]);
export const approvalStatementMinLength = 8;
export function meaningfulApprovalStatement(value: unknown): boolean {
  if (typeof value !== 'string') return false;
  const text = value.trim();
  const normalized = text
    .normalize('NFKC')
    .toLocaleLowerCase()
    .replace(/[\s\p{P}\p{S}]/gu, '');
  return (
    text.length >= approvalStatementMinLength &&
    !boilerplateStatements.has(normalized)
  );
}

const approvalChoice = /^[0-9０-９]*[-‐ー]?[A-Za-zＡ-Ｚａ-ｚ]$|^はい$/;
// 利用者が質問に記号だけで答えたときの承認の文。質問文に対象の番号が無ければ断る（どの質問への回答か特定できないため）。
export function choiceApprovalStatement(
  question: unknown,
  answer: unknown,
): string {
  const text = typeof question === 'string' ? question.trim() : '';
  const choice = typeof answer === 'string' ? answer.trim() : '';
  if (!/[0-9０-９]/.test(text) || text.length > 400)
    throw Error(
      '--approval-question には対象の番号を含む、利用者に見せた質問文（400文字以内）をそのまま指定してください。',
    );
  if (!approvalChoice.test(choice))
    throw Error(
      '--approval-answer には利用者が答えた記号（A・1-A など）をそのまま指定してください。',
    );
  return `質問「${text}」への利用者の回答: ${choice}`;
}
// --approval-question と --approval-answer を1つの承認の文にまとめ、out[key] へ入れる。
export function applyChoiceApproval(
  out: Record<string, any>,
  key: string,
): void {
  const { 'approval-question': question, 'approval-answer': answer } = out;
  if (question === undefined && answer === undefined) return;
  if (out[key] !== undefined)
    throw Error(`--${key} と --approval-question は併用できません。`);
  out[key] = choiceApprovalStatement(question, answer);
  delete out['approval-question'];
  delete out['approval-answer'];
}
export const choiceApprovalGuidance =
  '--approval-question と --approval-answer は、利用者が記号だけで答えたときに使います。対象の番号と操作を書いた、利用者に見せた質問文と、答えの記号をそのまま指定します（発言の文を指定する引数の代わり）。AIの判断で記号を補いません。';

// 利用者がチャットで指示した日時（承認の証跡）。UTCオフセット付きISO 8601（秒まで）で、未来でなく7日以内に限る（受付と同じ判定）。
const instructionAtMaxAgeMs = 7 * 24 * 60 * 60 * 1000;
const instructionAtPattern =
  /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d{1,9})?(?:Z|[+-](\d{2}):(\d{2}))$/;
export function approvalInstructionAt(
  value: unknown,
  now = Date.now(),
): string {
  const match =
    typeof value === 'string' ? instructionAtPattern.exec(value) : null;
  if (!match)
    throw Error(
      '--instruction-at に、利用者がチャットで指示した日時をUTCオフセット付きISO 8601（例: 2026-11-11T11:58:00+09:00）で指定してください。',
    );
  const [, year, month, day, hour, minute, second, oh, om] = match;
  const time = Date.parse(value as string);
  const valid =
    Number(month) >= 1 &&
    Number(month) <= 12 &&
    Number(day) >= 1 &&
    Number(day) <=
      new Date(Date.UTC(Number(year), Number(month), 0)).getUTCDate() &&
    Number(hour) <= 23 &&
    Number(minute) <= 59 &&
    Number(second) <= 59 &&
    Number(oh || 0) <= 23 &&
    Number(om || 0) <= 59 &&
    Number.isFinite(time);
  if (!valid || time > now || time < now - instructionAtMaxAgeMs)
    throw Error(
      '--instruction-at の日時が不正か、未来か、7日より前です。利用者がチャットで指示した日時を指定してください。',
    );
  return value as string;
}
export const instructionAtGuidance =
  '--instruction-at には、利用者がチャットでその指示・回答をした日時を、会話の利用者の発言の時刻からUTCオフセット付きISO 8601（例: 2026-11-11T11:58:00+09:00）で指定します。未来・7日より前の日時は断ります。発言の原文と指示の日時は承認の証跡として受付に記録されます。';
