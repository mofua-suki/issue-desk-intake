import { intakeRequestSize } from '../../shared/src/intake-request-size.ts';

const approvals = new Set(['A', '1-A', '承認します', 'この計画を承認します']);
const valid = (n) => Number.isSafeInteger(n) && n >= 0;

// 数値が明示された見込みだけを比較する。範囲は上限を使い、曖昧な記述は未確認とする。
function requestEstimate(text, unit) {
  if (typeof text !== 'string') return undefined;
  const pattern =
    unit === 'files'
      ? /(?:変更ファイル(?:数)?|ファイル(?:数)?)\s*[:：]?\s*(?:約\s*)?(\d+)(?:\s*[〜～~-]\s*(\d+))?\s*(?:件|本|ファイル)/g
      : /(?:変更行(?:数)?|行数)\s*[:：]?\s*(?:約\s*)?(\d+)(?:\s*[〜～~-]\s*(\d+))?\s*行/g;
  const reverse =
    unit === 'files'
      ? /(?:約\s*)?(\d+)(?:\s*[〜～~-]\s*(\d+))?\s*(?:件|本|ファイル)(?:の)?(?:変更)?ファイル/g
      : /(?:約\s*)?(\d+)(?:\s*[〜～~-]\s*(\d+))?\s*行/g;
  const values = [...text.matchAll(pattern), ...text.matchAll(reverse)]
    .map((m) => Number(m[2] ?? m[1]))
    .filter(valid);
  return values.length ? Math.max(...values) : undefined;
}

export async function showDraftEstimate(call, draft, text) {
  if (!approvals.has(text.trim())) return undefined;
  let current;
  try {
    current = await call('GET', `/api/drafts/${draft}`);
  } catch {
    console.error('受付の見込みと依頼文の見込みは読取失敗のため未確認です。');
    return undefined;
  }
  const original =
    Array.isArray(current?.messages) && current.messages[0]?.role === 'user'
      ? current.messages[0].text
      : undefined;
  const estimates = ['files', 'lines'].map((key) => {
    const value = current?.estimate?.[key];
    const requested = requestEstimate(original, key);
    return { key, value: valid(value) ? value : undefined, requested };
  });
  for (const { key, value, requested } of estimates) {
    const name = key === 'files' ? 'ファイル数' : '行数';
    console.error(
      `受付の見込み ${name}: ${value ?? '未確認'}、依頼文: ${requested ?? '未確認'}`,
    );
    if (
      value !== undefined &&
      ((key === 'files' && value > intakeRequestSize.files) ||
        (requested !== undefined && value > requested))
    )
      console.error(
        `警告: 受付の見込みの${name}が目安または依頼文を超えています。送信は続けます。`,
      );
    if (value === undefined || requested === undefined)
      console.error(`${name}の比較は未確認です。`);
  }
  return valid(current?.revision) ? current.revision : undefined;
}
