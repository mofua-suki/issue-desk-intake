import { parseCommands } from './chat-shell-parser.ts';

// Compare visible classified questions, including their choices; examples are not questions.
function questionBlocks(text: string, skipBlank = false): string[][] {
  let fence = '';
  const lines: string[] = [];
  for (const line of text.split(/\r?\n/)) {
    const delimiter = line.trimStart().match(/^(`{3,}|~{3,})/);
    if (delimiter) {
      if (!fence) fence = delimiter[1][0];
      else if (fence === delimiter[1][0]) fence = '';
      continue;
    }
    if (!fence && !/^\s*>/.test(line) && !(skipBlank && !line.trim()))
      lines.push(line.replace(/`+[^`]*`+/g, '').trim());
  }
  const questions: string[][] = [];
  for (let i = 0; i < lines.length; i++) {
    if (!/【質問分類：[^】]+】/.test(lines[i])) continue;
    const parts = [lines[i]];
    while (
      i + 1 < lines.length &&
      /^(?:[-*•]\s*)?(?:\*\*)?[0-9０-９]*-?[A-ZＡ-Ｚ](?:（おすすめ）)?(?:\*\*)?\s*[:：.．)）]/.test(
        lines[i + 1],
      )
    )
      parts.push(lines[++i]);
    questions.push(parts);
  }
  return questions;
}

function visibleQuestions(text: string): string[] {
  return questionBlocks(text).map((parts) =>
    parts.join('\n').replace(/\s+/g, ''),
  );
}

/** A classified question followed by its choices: the reader can answer from this text alone. */
export function statesQuestionWithChoices(text: string): boolean {
  return questionBlocks(text, true).some((parts) => parts.length > 1);
}

export function repeatsVisibleQuestion(
  previous: string,
  current: string,
): boolean {
  const old = new Set(visibleQuestions(previous));
  return visibleQuestions(current).some((question) => old.has(question));
}

// Only the first continuation after a blocked answer can be blocked again.
// The transcript, rather than process-local state, bounds retries across hook processes.
export function repeatedQuestionOnFirstContinuation(
  texts: string[],
  indexes: number[],
  inspect: (index: number) => { blocked: boolean; questionInvalid: boolean },
): boolean {
  const last = indexes.at(-1);
  if (last === undefined) return false;
  const distinct = [...new Set(indexes)];
  const blockedIndex = distinct.find(
    (index) => index < last && inspect(index).blocked,
  );
  if (
    blockedIndex === undefined ||
    distinct.indexOf(last) !== distinct.indexOf(blockedIndex) + 1
  )
    return false;
  return texts.some(
    (text, i) =>
      indexes[i] <= blockedIndex &&
      !inspect(indexes[i]).questionInvalid &&
      repeatsVisibleQuestion(text, texts.at(-1)!),
  );
}

// Stop フックと一緒に配布する互換の公開入口。
export function commandBasename(s: string | undefined): string {
  return (s ?? '').split(/[\\/]/).at(-1) ?? '';
}

// Recognize only shell options whose argument boundaries are known.
function shellInvocation(
  words: string[],
): { args: string[]; command: boolean } | null {
  if (!/^(?:sh|bash)$/.test(commandBasename(words[0]))) return null;
  let i = 1;
  let interactive = false;
  while (i < words.length) {
    const option = words[i];
    if (option === '--')
      return interactive ? null : { args: words.slice(i + 1), command: false };
    if (
      (option === '-o' ||
        option === '+o' ||
        option === '-O' ||
        option === '+O') &&
      words[i + 1]
    ) {
      i += 2;
      continue;
    }
    if (/^-[a-zA-Z]+$/.test(option) && /[oOs]/.test(option.slice(1)))
      return null;
    if (option === '-c' || /^-[a-zA-Z]*c[a-zA-Z]*$/.test(option))
      return words[i + 1] === undefined
        ? null
        : { args: [words[i + 1]], command: true };
    if (/^-[a-zA-Z]+$/.test(option)) {
      interactive ||= option.includes('i');
      i++;
      continue;
    }
    if (option.startsWith('-') || option.startsWith('+')) return null;
    return interactive ? null : { args: words.slice(i), command: false };
  }
  return null;
}

function shellScriptArgs(
  shell: ReturnType<typeof shellInvocation>,
): string[] | null {
  return shell && !shell.command ? shell.args : null;
}

export function shellCommandString(words: string[]): string | null {
  const args = normalizeDeskArgs(words, false);
  const shell = shellInvocation(args);
  return shell?.command ? shell.args[0] : null;
}

/** A shell script operand is not itself a direct command. */
export function shellScript(words: string[]): string | null {
  return (
    shellScriptArgs(shellInvocation(normalizeDeskArgs(words, false)))?.[0] ??
    null
  );
}

export function normalizeDeskArgs(
  words: string[],
  unwrapShell = true,
  unwrapTimeout = false,
): string[] {
  let args = words;
  let timeout = false;
  while (true) {
    const executable = commandBasename(args[0]);
    if (executable === 'env') {
      args = args.slice(1);
      while (args.length) {
        if (args[0] === '--') {
          args = args.slice(1);
          break;
        }
        if (
          args[0] === '-i' ||
          args[0] === '--ignore-environment' ||
          /^[A-Za-z_][A-Za-z0-9_]*=/.test(args[0])
        ) {
          args = args.slice(1);
          continue;
        }
        if ((args[0] === '-u' || args[0] === '--unset') && args[1]) {
          args = args.slice(2);
          continue;
        }
        if (args[0].startsWith('--unset=')) {
          args = args.slice(1);
          continue;
        }
        break;
      }
    } else if (unwrapShell && (executable === 'sh' || executable === 'bash')) {
      const shell = shellInvocation(args);
      if (!shell) break;
      const scriptArgs = shellScriptArgs(shell);
      if (scriptArgs) {
        // desk and desk-pc are shipped as shell scripts; other operands (including
        // JavaScript files and commands such as head) are not direct commands.
        if (!/^(?:desk|desk-pc)$/.test(commandBasename(scriptArgs[0]))) break;
        args = scriptArgs;
        continue;
      }
      const segments = parseCommands(shell.args[0]);
      if (!segments || segments.length !== 1) break;
      args = segments[0].words;
    } else if (unwrapTimeout && !timeout && executable === 'timeout') {
      const inner = timeoutCommand(['timeout', ...args.slice(1)]);
      if (!inner) return ['timeout'];
      timeout = true;
      args = inner;
    } else break;
  }
  const normalized = [commandBasename(args[0]), ...args.slice(1)];
  if (
    /^(?:node|issue-desk-node|desk|desk-pc)$/.test(normalized[0]) &&
    isDeskGetScript(commandBasename(normalized[1]))
  ) {
    const result = [commandBasename(normalized[1]), ...normalized.slice(2)];
    return timeout ? ['timeout', ...result] : result;
  }
  return timeout ? ['timeout', ...normalized] : normalized;
}

/** 互換用の公開入口として残す。正規化済みの timeout から一段だけ取り出し、不明な構文は認識しない。 */
export function timeoutCommand(args: string[]): string[] | null {
  if (args[0] !== 'timeout') return null;
  let i = 1;
  while (i < args.length) {
    const arg = args[i];
    if (arg === '--') {
      i++;
      break;
    }
    if (
      arg === '--foreground' ||
      arg === '--preserve-status' ||
      arg === '-v' ||
      arg === '--verbose'
    ) {
      i++;
      continue;
    }
    if (
      arg === '-k' ||
      arg === '--kill-after' ||
      arg === '-s' ||
      arg === '--signal'
    ) {
      if (!args[i + 1]) return null;
      i += 2;
      continue;
    }
    if (/^(?:--kill-after|--signal)=.+$/.test(arg) || /^-[ks].+/.test(arg)) {
      i++;
      continue;
    }
    break;
  }
  if (!/^(?:\d+(?:\.\d*)?|\.\d+)[smhd]?$/.test(args[i] ?? '') || !args[i + 1])
    return null;
  const inner = args.slice(i + 1);
  return [commandBasename(inner[0]), ...inner.slice(1)];
}

/** 受付スクリプトの読取確認オプションを識別する。 */
export function hasCheckOption(
  args: string[],
  { includeOnly = true }: { includeOnly?: boolean } = {},
): boolean {
  return args.some((arg) =>
    (includeOnly ? /^--check(?:-only)?(?:=|$)/ : /^--check(?:=|$)/).test(arg),
  );
}

/** 窓口の操作と読取を、経路とは独立して識別する。 */
export function deskCommand(words: string[]): 'operation' | 'get' | null {
  return normalizedDeskCommand(normalizeDeskArgs(words));
}

/** 正規化済みの窓口引数を判定する。 */
export function normalizedDeskCommand(
  args: string[],
): 'operation' | 'get' | null {
  if (
    /^(?:desk|desk-pc)$/.test(args[0]) &&
    (args[1] === 'operation' || args[1] === 'get')
  )
    return args[1];
  return isDeskGetScript(args[0]) && args[1] === 'GET' ? 'get' : null;
}

/** 起票・返信スクリプトを識別する。改名前の .mjs が残る作業ツリーでも送れるので、送信の見逃しを避けて両方の拡張子を数える。 */
export function isIssueScript(script: string): boolean {
  return /(?:^|[\\/])(?:create-issue|reply-issue)\.(?:mjs|ts)$/.test(script);
}

/** 汎用GETの入口（desk-get）を識別する。 */
function isDeskGetScript(script: string | undefined): boolean {
  return /^desk-get\.ts$/.test(script ?? '');
}

/** 実行する窓口と GET の対象経路を共に確認する。 */
export function deskIssueRoute(words: string[]): 'read' | 'search' | null {
  const args = normalizeDeskArgs(words);
  const executable = args[0];
  let route: string | undefined;
  if (
    (executable === 'desk' || executable === 'desk-pc') &&
    args[1] === 'get'
  ) {
    const options = args.slice(2);
    const index = options.indexOf('--path');
    route =
      index >= 0
        ? options[index + 1]
        : options.find((s) => s.startsWith('--path='))?.slice(7);
  } else {
    if (isDeskGetScript(executable) && args[1] === 'GET') route = args[2];
  }
  const pathname = route?.split('?')[0];
  return pathname === '/api/issues/search'
    ? 'search'
    : pathname === '/api/issue'
      ? 'read'
      : null;
}
export {
  authorizedQuestionReason,
  irreversibleQuestionReason,
  isQuestionOnly,
  sendsToIntake,
  heavyForeground,
  startsDetachedWatcher,
  unaskedWorkReason,
  followAttentionReason,
  changesWork,
  leftAloneReason,
  unaskedReason,
  answerFirstReason,
  silentWatchReason,
  foregroundWarning,
  plainText,
  turnChecks,
} from './chat-stop-hook.ts';
export type { TurnChecks } from './chat-stop-hook.ts';

// 指示と通常操作の対応を追加できる。保護対象はこの表では承認しない。
const authorizations = [
  {
    instruction: /直接\s*PR/i,
    operations: [
      /マージ/i,
      /\bpush\b|プッシュ/i,
      /PR[^。！？?\n]{0,20}(?:作成|作る|出す|作って)/i,
      /(?:Issue|依頼)[^。！？?\n]{0,20}(?:クローズ|閉じ)/i,
    ],
  },
];
const protectedOperation =
  /削除|権限|SSH|force\s*push|強制(?:push|プッシュ)|履歴(?:の)?(?:書き換え|改変)|保護対象|対象SHA|先頭(?:コミット|SHA).{0,20}(?:変|違)|範囲外|別(?:の)?(?:対象|リポジトリ)|(?:検証|テスト|確認).{0,20}(?:失敗|不合格|通ら|未完了|未実施)|(?:失敗|未完了).{0,20}(?:検証|テスト)|新たな|新しい|追加(?:の|で)|条件(?:が|を|変更)|前提(?:が|を|変更)/i;
const negation =
  /(?:しない|しなく|しません|するな|さない|せず|しちゃだめ|禁止|取消|取り消|撤回|やめ|不要|ではない|じゃない|不可|待って|保留|確認(?:して|が必要)|許可しない)/;
const instructionVerb =
  /直して|修正して|対応して|実装して|進めて|お願い|任せ|作って|してください|やって|で行う|で進め/;

function prose(text: string): string {
  let fence = '';
  return text
    .split(/\r?\n/)
    .filter((line) => {
      const marker = line.trimStart().match(/^(`{3,}|~{3,})/);
      if (marker) {
        if (!fence) fence = marker[1][0];
        else if (fence === marker[1][0]) fence = '';
        return false;
      }
      return !fence && !/^\s*>/.test(line);
    })
    .map((line) =>
      line
        .replace(/`+[^`]*`+/g, '')
        .replace(/「[^」]*」|『[^』]*』|“[^”]*”|"[^"\n]*"/g, ''),
    )
    .join('\n');
}

function targets(text: string): string[] {
  return [
    ...text.matchAll(
      /(?:[\w.-]+\/[\w.-]+|(?:PR|Issue|依頼)\s*(?:番号)?\s*#?\s*\d+|#\d+)/gi,
    ),
  ].map((m) => m[0].replace(/\s/g, '').toLowerCase());
}
function compatible(instruction: string, question: string): boolean {
  const a = targets(instruction),
    b = targets(question);
  // Explicit identifiers of the same kind must agree. An unspecified PR number may be produced by the task.
  const repos = (xs: string[]) => xs.filter((x) => x.includes('/'));
  const nums = (xs: string[], kind: RegExp) =>
    xs.filter((x) => kind.test(x)).map((x) => x.match(/\d+$/)?.[0]);
  const mismatch = (kind: RegExp) => {
    const first = nums(a, kind),
      second = nums(b, kind);
    return (
      first.length > 0 &&
      second.length > 0 &&
      !first.some((x) => second.includes(x))
    );
  };
  return (
    !(
      repos(a).length &&
      repos(b).length &&
      !repos(a).some((x) => repos(b).includes(x))
    ) &&
    !mismatch(/^pr/) &&
    !mismatch(/^(?:issue|依頼)/) &&
    !mismatch(/^#/) &&
    !/(?:別|他|別件|ほか)(?:の|件|リポジトリ|PR|Issue)/i.test(question)
  );
}

function questions(text: string): string[] {
  // Preserve line positions so quoted text, code blocks and options cannot supply a reason.
  const raw = text.split(/\r?\n/);
  // prose removes fenced and quoted lines; clean each line in context while retaining gaps.
  let fence = '';
  const lines = raw.map((line) => {
    const marker = line.trimStart().match(/^(`{3,}|~{3,})/);
    if (marker) {
      if (!fence) fence = marker[1][0];
      else if (fence === marker[1][0]) fence = '';
      return '';
    }
    return fence || /^\s*>/.test(line) ? '' : prose(line);
  });
  return lines.flatMap((line, i) => {
    if (/^\s*(?:[-*•]\s*)?(?:\d+-)?[A-ZＡ-Ｚ][)）.．:：]/.test(line)) return [];
    const parts = line.split(/(?=【質問分類：)/);
    return parts.flatMap((part, index) => {
      if (
        !/【質問分類：/.test(part) ||
        !/[?？]|(?:ですか|ますか|でしょうか)/.test(part)
      )
        return [];
      return [
        index === parts.length - 1 &&
        /^\s*戻せない理由\s*[:：]/.test(lines[i + 1] ?? '')
          ? `${part} ${lines[i + 1]}`
          : part,
      ];
    });
  });
}

export function questionPolicy(
  text: string,
  asked: string[],
  humanTurns: string[],
): { authorized: boolean; missingReason: boolean } {
  const allQuestions = [...questions(text), ...asked.flatMap(questions)];
  const instructions: string[] = [];
  for (const turn of humanTurns) {
    const clean = prose(turn);
    // Cancellation or changed conditions invalidate earlier instructions, without creating a new authorization.
    if (negation.test(clean)) instructions.length = 0;
    else if (targets(clean).length) {
      for (let i = instructions.length - 1; i >= 0; i--)
        if (!compatible(instructions[i], clean)) instructions.splice(i, 1);
    }
    for (const sentence of clean.match(/[^。！？!?\n]+[。！？!?]?/g) ?? []) {
      if (
        !negation.test(sentence) &&
        instructionVerb.test(sentence) &&
        !/[?？]|(?:ですか|ますか|でしょうか)/.test(sentence)
      )
        instructions.push(sentence);
    }
  }
  let authorized = false,
    missingReason = false;
  for (const question of allQuestions) {
    const category = question.match(/【質問分類：([^】]+)】/)?.[1];
    if (category !== '破壊的な操作' && category !== '取り返しのつかない操作')
      continue;
    if (
      !protectedOperation.test(question) &&
      authorizations.some(
        (row) =>
          row.operations.some((op) => op.test(question)) &&
          instructions.some(
            (instruction) =>
              row.instruction.test(instruction) &&
              compatible(instruction, question),
          ),
      )
    )
      authorized = true;
    // Another question, sentence or option cannot supply this question's reason.
    const questionPart = question.replace(/([?？])[^?？]*[?？].*$/, '$1');
    const reasonMatch = questionPart.match(
      /戻せない理由\s*[:：]\s*([^。！？!?\n]*)/,
    );
    const beforeReason = reasonMatch
      ? questionPart.slice(0, reasonMatch.index)
      : '';
    const afterQuestion = beforeReason.split(/[?？]/);
    const reason =
      afterQuestion.length > 1 && afterQuestion.at(-1)!.trim()
        ? ''
        : (reasonMatch?.[1]?.trim() ?? '');
    // A concrete consequence needs both an affected object and an adverse outcome;
    // words such as 「保存」 alone, a negated outcome or a request are insufficient.
    const object =
      /(?:データ|記録|情報|履歴|ファイル|利用者|ユーザー|第三者|顧客|権限|アクセス|公開先|送信先|サービス|稼働|業務|システム|復旧|バックアップ|個人情報|機密|秘密|メール|通知|料金|費用|支払い|課金|契約|注文|成果物)/;
    const effect =
      /(?:失われ|失う|消え|消失|削除され|流出|漏洩|漏れ|破損|上書き|書き換わ|書き換えられ|残り|残る|保存され|複製され|拡散|流布|公開され|送信され|回収でき|復元でき|復旧でき|取り戻せ|戻せ|停止|中断|アクセスでき|利用でき|付与され|取り消せ|取消せ|撤回でき|返金でき|取り消しでき|請求され|課金され|送られ|届き|通知され|知られ|閲覧され|見られ|無効にな|利用不能|使えなく|実行され)/;
    const vague =
      /(?:不明|未定|わかりません|分かりません|確認してください|検討してください|ありません|起きません|発生しません|失われません|漏れません|残りません|保存されません|消えない|消えません|漏れない|残らない|失われない|停止しない|公開されない|送信されない)/;
    if (!object.test(reason) || !effect.test(reason) || vague.test(reason))
      missingReason = true;
  }
  return { authorized, missingReason };
}
/** Issue停止を不可逆な操作として確認する質問を検出する。 */
type Question = { question: string; options: string[] };

function stopQuestionProse(text: string): string[] {
  let fence = '';
  return text.split(/\r?\n/).filter((line) => {
    const mark = line.trimStart().match(/^(`{3,}|~{3,})/);
    if (mark) {
      if (!fence) fence = mark[1][0];
      else if (fence === mark[1][0]) fence = '';
      return false;
    }
    return !fence && !/^\s*>/.test(line);
  });
}

function visible(text: string): Question[] {
  const questions: Question[] = [];
  let current: Question | undefined;
  for (const line of stopQuestionProse(text)) {
    if (/【質問分類：/.test(line)) {
      // Multiple questions on one line must not share their classification or choices.
      for (const part of line.split(/(?=【質問分類：)/).filter(Boolean)) {
        current = { question: part, options: [] };
        questions.push(current);
      }
    } else if (
      current &&
      /^\s*(?:[-*•]\s*)?(?:\*\*)?[0-9０-９]*-?[A-ZＡ-Ｚ](?:（おすすめ）)?(?:\*\*)?\s*[:：.．)）]/.test(
        line,
      )
    ) {
      current.options.push(line);
    } else if (line.trim()) current = undefined;
  }
  return questions;
}

function instructed(userText: string, question: Question): boolean {
  const target = question.question
    .match(/(?:#|第|番号\s*)?([0-9０-９]+)/)?.[1]
    ?.replace(/[０-９]/g, (d) => String(d.charCodeAt(0) - 0xff10));
  if (!target) return false;
  for (const line of stopQuestionProse(userText)) {
    for (const sentence of line.match(/[^。！？!?\n]+[。！？!?]?/g) ?? []) {
      // Quoted instructions, questions and negative instructions are not authorization.
      const plain = sentence.replace(
        /「[^」]*」|『[^』]*』|“[^”]*”|"[^"]*"|`[^`]*`/g,
        '',
      );
      if (
        /[?？]|(?:ますか|ですか|でしょうか)|(?:止め|停止)(?:ない|ません|ず|るな|なく)|(?:止めないで|停止しないで)/.test(
          plain,
        )
      )
        continue;
      const numbers = [...plain.matchAll(/[0-9０-９]+/g)].map((m) =>
        m[0].replace(/[０-９]/g, (d) => String(d.charCodeAt(0) - 0xff10)),
      );
      if (
        numbers.length === 1 &&
        numbers[0] === target &&
        /(?:止めて|停止して|停止せよ|止めろ|停止してください|止めてください)/.test(
          plain,
        )
      )
        return true;
    }
  }
  return false;
}

export function stopQuestion(
  text: string,
  tools: Question[],
  userText: string,
): boolean {
  return [...visible(text), ...tools].some((item) => {
    if (
      !/【質問分類：(?:取り返しのつかない操作|破壊的な操作)】/.test(
        item.question,
      )
    )
      return false;
    const question = stopQuestionProse(item.question)
      .join('\n')
      .replace(/`+[^`]*`+/g, '');
    if (!question.includes('【質問分類：')) return false;
    const asksStop = /(?:止めますか|停止しますか)/.test(question);
    const choiceStop = item.options.some((option) =>
      /(?:^|[\s:：])(?:停止する|止める)(?:[\s。:：]|$)|"action"\s*:\s*"stop"/.test(
        option,
      ),
    );
    return (asksStop || choiceStop) && !instructed(userText, item);
  });
}
