import { stopQuestion } from './chat-turn-checks.ts';
import { questionPolicy } from './chat-turn-checks.ts';

export const authorizedQuestionReason =
  '依頼で許可済みの操作は聞かずに実行し、判断の記録に書いてください';
export const irreversibleQuestionReason =
  '破壊的な操作・取り返しのつかない操作の質問には、同じ質問内に「戻せない理由：」と具体的な副作用を書いてください';
import { isUsageReport } from './chat-usage-report.ts';
import fs from 'node:fs';
import os from 'node:os';
import { recordFires } from './stop-check-record.ts';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  normalizeDeskArgs,
  normalizedDeskCommand,
  hasCheckOption,
  isIssueScript,
  shellCommandString,
  shellScript,
  repeatsVisibleQuestion,
  repeatedQuestionOnFirstContinuation,
  statesQuestionWithChoices,
} from './chat-turn-checks.ts';
import { parseCommands as parseShellCommands } from './chat-shell-parser.ts';

type Block = { decision: 'block'; reason: string };
type Part = {
  type: string;
  id?: string;
  tool_use_id?: string;
  content?: unknown;
  is_error?: boolean;
  text?: string;
  name?: string;
  input?: {
    command?: string;
    run_in_background?: boolean;
    [key: string]: unknown;
  };
};
type Inspection = {
  block: Block | null;
  warning: string | null;
  fires: { id: StopCheckId; result: 'block' | 'warn' }[];
};
type Segment = { words: string[]; background: boolean };
type Entry = { type: string; message?: { content?: unknown } };
type Parse = (command: string) => Segment[] | null;
export type TurnChecks = {
  blocks: string[];
  warnings: string[];
  worked: boolean;
};

const SUBSTANTIVE = 40;
const requestWords =
  /起票して|起票を|送って|送信して|直して|やって|やっといて|お願い|任せ|任す|返信して|返して|再試行|リトライ|閉じて|クローズ|進めて|実装して|対応して|修正して|作って|出して|保存して|止めて|再開して|してください|して$|して。|しろ|やれ|いいよ|良いよ|許可|承認/;
const questionWords =
  /[？?]|ですか|ますか|でしょうか|のか[。\s]*$|どう(?:なっ|し|で|思)|なぜ|なんで|どうして|何が|いつ|どれ|いくつ|教えて|知りたい|ってこと|じゃない|のでは|優先順位|おかしい|だめ|ダメ|遅い|放置|勝手/;
// A status check or complaint with no instruction to act: the AI must answer, not file or send.
export function isQuestionOnly(userText: string): boolean {
  const text = userText.trim();
  if (!text) return false;
  return (
    questionWords.test(text) && !requestWords.test(text.replace(/[？?]/g, ''))
  );
}

// 送信と重い前景は、改名前の .mjs が残る古い作業ツリーでも実際に送れるので、両方の拡張子で数える（見逃しを危険側とする）。
// 読取・見張りの扱いは .ts だけにする（数えないほうが厳しい判定になる）。
const sendScripts =
  /(?:^|[\\/])(?:create-issue|reply-issue|retry-issue|close-issue|save-todo|edit-todo|desk-ops|control-issue|handover-issue|create-pr|merge-pr|host-drain|model-override)\.(?:mjs|ts)$/;
const readScripts =
  /(?:^|[\\/])(?:list-issues|desk-alerts|usage-per-issue)\.ts$/;
const watchedSendScripts =
  /(?:^|[\\/])(?:create-issue|retry-issue|close-issue|desk-ops|reply-issue)\.(?:mjs|ts)$/;

function scriptOf(args: string[]): { script: string; args: string[] } | null {
  const wrapped = /^(?:node|desk|desk-pc|issue-desk-node)$/.test(args[0]);
  const script = args[wrapped ? 1 : 0];
  if (!script) return null;
  return { script, args: args.slice(wrapped ? 2 : 1) };
}

/** 正規化済みの引数（timeout の内側を含む）が読取か判定する。 */
function isReadCommand(args: string[]): boolean {
  if (normalizedDeskCommand(args) === 'get') return true;
  if (isUsageReport(args)) return true;
  const found = scriptOf(args);
  return (
    !!found &&
    (readScripts.test(found.script) ||
      (isIssueScript(found.script) && hasCheckOption(found.args)))
  );
}

/** 受付へ送る呼び出しの名前（スクリプト名、desk operation は 'operation'）。 */
function intakeSends(command: string, parse: Parse): string[] {
  return (parse(command) ?? []).flatMap(({ words }) => {
    const args = normalizeDeskArgs(words);
    if (normalizedDeskCommand(args) === 'operation') return ['operation'];
    const found = scriptOf(args);
    if (!found || !sendScripts.test(found.script)) return [];
    if (isIssueScript(found.script))
      return !hasCheckOption(found.args) || found.args.includes('--draft')
        ? [found.script]
        : [];
    if (/desk-ops\.(?:mjs|ts)$/.test(found.script))
      return found.args.some(
        (arg) => !/^-/.test(arg) && !/^(?:list|read|show|get)$/.test(arg),
      )
        ? [found.script]
        : [];
    return [found.script];
  });
}

/** A command that sends something to the intake (read-only --check / --check-only do not count). */
export function sendsToIntake(command: string, parse: Parse): boolean {
  return intakeSends(command, parse).length > 0;
}

/** 送信のうち新しい起票（create-issue.ts）だけのもの。不具合は質問の回でも聞かずに起票する。 */
export function onlyFilesIssue(command: string, parse: Parse): boolean {
  const sends = intakeSends(command, parse);
  return (
    sends.length > 0 &&
    sends.every((script) => /(?:^|[\\/])create-issue\.(?:mjs|ts)$/.test(script))
  );
}

function readsIntake(command: string, parse: Parse): boolean {
  return (parse(command) ?? []).some(({ words }) => {
    const args = normalizeDeskArgs(words, true, true);
    return isReadCommand(args[0] === 'timeout' ? args.slice(1) : args);
  });
}

/** A foreground call that blocks the chat for a long time (tests, create-issue, long sleep or wait loops). */
export function heavyForeground(
  part: Part,
  parse: Parse,
  normalize: typeof normalizeDeskArgs = normalizeDeskArgs,
): boolean {
  const input = part.input ?? {};
  if (
    part.name !== 'Bash' ||
    input.run_in_background === true ||
    typeof input.command !== 'string'
  )
    return false;
  if (typeof input.timeout === 'number' && input.timeout >= 300000) return true;
  return (parse(input.command) ?? []).some(({ words, background }) => {
    if (background) return false;
    const args = normalize(words, true, true);
    const head = args[0];
    if (/^(?:until|while)$/.test(head)) return true;
    if (head === 'sleep') return Number(args[1]) >= 30;
    if (head === 'timeout') {
      return !isReadCommand(args.slice(1));
    }
    if (
      head === 'verify.sh' ||
      /(?:^|[\\/])verify\.sh$/.test(shellScript(words) ?? '')
    )
      return true;
    if (/^(?:npm|pnpm|yarn)$/.test(head))
      return args.slice(1).some((word) => word === 'test');
    if (/^(?:go)$/.test(head)) return args[1] === 'test';
    if (/^(?:docker|docker-compose)$/.test(head))
      return args.some((word) => /^(?:build|up)$/.test(word));
    const found = scriptOf(args);
    if (args[0] === 'node' && args.includes('--test')) return true;
    if (found && /(?:^|[\\/])verify\.sh$/.test(found.script)) return true;
    return !!found && /(?:^|[\\/])create-issue\.(?:mjs|ts)$/.test(found.script);
  });
}

// A watcher started with setsid/nohup/disown or a trailing & only writes a file: nothing wakes the chat when it fires.
export function startsDetachedWatcher(command: string): boolean {
  const detached = /\b(?:setsid|nohup|disown)\b|&\s*\)?\s*(?:;|$)(?!&)/m.test(
    command.replace(/&&|2>&1|>&\d/g, ''),
  );
  return (
    detached &&
    /watch|monitor|見張|(?:until|while)\b[^\n]*\bsleep\b|\bsleep\b[^\n]*\bdone\b/i.test(
      command,
    )
  );
}

const stallReport =
  /保留|上限(?:で|に達|のため)|止まっ(?:て|た)|停止(?:中|して)|未起票|放置|詰まっ|滞留/;
const delegated =
  /任せ|面倒を見|見ておいて|やっておいて|進めておいて|対応しておいて|頼む|放置/;

export const unaskedWorkReason =
  '利用者は聞いているだけで、作業を頼んでいません。その回にファイルの変更・コミット・push・実装のサブエージェントの起動をしています。聞かれたことに答えて回を終えてください。見つけた問題は事実を伝えるだけにし、対処は見張りの通知の回か、利用者が頼んだ回に行います';
export const followAttentionReason =
  '見張りが対処の要る変化を知らせましたが、この回に原因の読取（汎用 get など）がありません。原因を読み、破壊的でなければ自分で対処して、何をどう判断したかを報告してください。利用者が聞くまで放置しません';

/**
 * Scratch space: /tmp and a cloned worktree's git-ignored .tmp/ directory. Paths that climb with `..`,
 * reach into .git/, sit directly under the home directory or inside the NAS deploy checkout are not scratch.
 */
export function scratchPath(file: unknown, home = os.homedir()): boolean {
  if (
    typeof file !== 'string' ||
    !file.startsWith('/') ||
    file.split('/').includes('..')
  )
    return false;
  if (file.startsWith('/tmp/')) return true;
  if (
    file.includes('/.git/') ||
    file.startsWith(`${home}/.tmp/`) ||
    file.startsWith(`${home}/Workspace/issue-desk-intake/`)
  )
    return false;
  return file.includes('/.tmp/');
}

/** Changing files or history outside the scratch space, or starting a sub-agent that may do so. */
export function changesWork(part: Part, parse: Parse): boolean {
  const name = part.name ?? '';
  const input = part.input ?? {};
  if (/^(?:Edit|Write|NotebookEdit|MultiEdit)$/.test(name))
    return !scratchPath(input.file_path);
  if (name === 'Agent' || name === 'Task')
    return !/^(?:Explore|Plan)$/.test(String(input.subagent_type ?? ''));
  if (name !== 'Bash' || typeof input.command !== 'string') return false;
  return (parse(input.command) ?? []).some(({ words }) => {
    const args = normalizeDeskArgs(words);
    if (args[0] === 'git')
      return args
        .slice(1)
        .some((word) =>
          /^(?:commit|push|merge|rebase|revert|reset|cherry-pick)$/.test(word),
        );
    return args[0] === 'sed' && args.some((word) => /^-[a-zA-Z]*i/.test(word));
  });
}

export const leftAloneReason =
  '止まっている・保留の依頼を報告していますが、同じ回に原因の読取（汎用 get など）と具体的な対処（再試行・返信・起票の確認質問・見張り）がありません。保留の理由を読み、再試行で何が変わるかを示して対処するか、対処を判断する質問（【質問分類】付き）を同じ回に添えてください';
export const unaskedReason =
  '利用者の発言は質問・状況確認・指摘で、起票や送信を求めていません。その回に受付への送信（reply-issue.ts・汎用 operation など）を実行しています。状況の確認・質問は送信の許可ではありません（不具合の新しい起票 create-issue.ts は除きます）。まず質問に答え、送ってよいかを番号と記号の質問で確認してください。送った内容と重複・取り消しの要否も報告してください';
export const answerFirstReason =
  '質問への回答より先に重い・長い前景の呼び出し（テスト・create-issue.ts・長い sleep や待ちループ）をしました。先に聞かれたことへ答え、調査や長い処理は run_in_background かサブエージェントに回してください';
export const silentWatchReason =
  '見張りを setsid・nohup・& で切り離して起動しましたが、状態が変わったときにチャットを呼び戻す仕組みがありません（ファイルへ書くだけでは誰も気づきません）。Monitor ツール（標準出力の各行が通知になる）、run_in_background の Bash（終了時に通知）、ScheduleWakeup・CronCreate、または scripts/monitor-watch.ts のどれかで見張り直してください。出力ファイルは通知に数えません';
export const foregroundWarning =
  '長い処理を前景で連続して走らせています。run_in_background かサブエージェントに回し、チャットは次の指示をいつでも受けられる状態にしてください';

/** Plain text of a user message (string or text parts). */
export function plainText(value: unknown): string {
  if (typeof value === 'string') return value;
  if (!Array.isArray(value)) return '';
  return value
    .map((part) =>
      part?.type === 'text' && typeof part.text === 'string' ? part.text : '',
    )
    .join('\n');
}

export function turnChecks(
  entries: Entry[],
  userText: string,
  parse: Parse,
  content: (v: unknown) => Part[],
  notification = '',
): TurnChecks {
  const blocks: string[] = [];
  const warnings: string[] = [];
  const texts: string[] = [];
  let answered = false;
  let heavyBeforeAnswer = false;
  let sent = false;
  let read = false;
  let acted = false;
  let foreground = 0;
  let changed = false;
  let detachedWatch = false;
  let callsBack = false;
  for (const entry of entries) {
    if (entry.type !== 'assistant') continue;
    for (const part of content(entry.message?.content)) {
      if (part.type === 'text') {
        texts.push(part.text!);
        if (part.text!.trim().length >= SUBSTANTIVE) answered = true;
        continue;
      }
      if (part.type !== 'tool_use') continue;
      const name = part.name ?? '';
      const background = part.input?.run_in_background === true;
      if (changesWork(part, parse)) changed = true;
      if (
        /^(?:Agent|Task|CronCreate|ScheduleWakeup|Monitor)$|__send_later$/.test(
          name,
        ) ||
        background
      )
        acted = true;
      if (/^(?:Monitor|ScheduleWakeup|CronCreate)$/.test(name) || background)
        callsBack = true;
      if (name === 'Bash' && typeof part.input?.command === 'string') {
        const command = part.input.command;
        if (sendsToIntake(command, parse)) {
          if (!onlyFilesIssue(command, parse)) sent = true;
          acted = true;
        }
        if (readsIntake(command, parse)) read = true;
        if (/stage-watch\.ts|monitor-watch\.ts/.test(command)) {
          acted = true;
          callsBack = true;
        }
        if (!background && startsDetachedWatcher(command)) detachedWatch = true;
      }
      if (heavyForeground(part, parse)) {
        foreground++;
        if (!answered) heavyBeforeAnswer = true;
      }
    }
  }
  const text = texts.join('\n');
  const question = isQuestionOnly(userText);
  if (question && sent) blocks.push(unaskedReason);
  if (question && changed) blocks.push(unaskedWorkReason);
  if (/【(?:要対処|停滞|読取失敗)】/.test(notification) && !read)
    blocks.push(followAttentionReason);
  if (question && heavyBeforeAnswer) blocks.push(answerFirstReason);
  if (foreground >= 2) warnings.push(foregroundWarning);
  if (detachedWatch && !callsBack) blocks.push(silentWatchReason);
  const finalText = texts.at(-1) ?? '';
  const asks = /[？?]/.test(finalText) && /【質問分類/.test(finalText);
  // A question is answered, not acted on: only a delegated turn is held to acting on a stall it reports.
  if (!question && stallReport.test(finalText) && delegated.test(userText)) {
    if (!read || !(acted || asks)) blocks.push(leftAloneReason);
  } else if (!text) {
    /* nothing to check */
  }
  // 利用者の依頼か通知で作業した回。質問だけの回はここまでに止めている。
  return { blocks, warnings, worked: !question && (changed || sent) };
}

/** Tool calls before the first substantive answer text that trigger the investigation warning. */
const HEAVY_INVESTIGATION_CALLS = 8;
/** A text part of at least this many characters counts as the first substantive answer. */
const SUBSTANTIVE_TEXT_LENGTH = 40;
const watchReason =
  '切り離して動かす長い処理（nohup・setsid・末尾の &）に見張りがありません。scripts/stage-watch.ts で包んで起動してください。短い処理なら run_in_background で動かしてください。見張りを付けない理由を返答に書かないでください';
const filingReason =
  '起票した依頼、または実装AIへ返信した依頼を見張らずに返答を終えようとしています。Monitor（timeout_ms 1800000）で ~/.issue-desk/desk follow-issues.ts --desk <共通番号> --state <絶対パス> を起動してから返答を終えてください（docs/chat-operations.md『起票後の見張り』）。ほかの裏の処理や予約は見張りの代わりになりません';
/**
 * 通知の回に、利用者の回答待ちを伝えるだけで、質問と選択肢の全文を置かない返答。質問が流れて、利用者が何を聞かれているか分からなくなる。
 * 文ごとに判定し、実装AI・受付AIなど利用者以外の回答待ちを伝える文は対象にしない。
 */
const waitingPhrase =
  /(?:回答|返答|返事|承認|判断|確認|指示|了承)(?:を|は|が)?お?(?:待ち|待って)/;
// 利用者へ出した質問を指す語（番号付きの質問・前に出した質問）。「質問」だけでは実装AI・受付AIの質問と区別できない。
const userQuestion =
  /質問\s*[0-9０-９]|(?:表示済みの|既出の|先ほどの|前回の|上記の)質問/;
const waitingOnUser =
  /(?:(?:利用者|ユーザー)(?:の|から)ご?|ご)(?:回答|返答|返事|承認|判断|確認|指示|了承)(?:を|は|が)?お?(?:待ち|待って)/;
const pointsAtQuestion =
  /(?:表示済みの|既出の|先ほどの|前回の|上記の)質問[^。\n]*有効なまま/;
// 待ちが今も続いていると述べる形だけを対象にする（「待ちは残っていません」「待ちにせず」「待ちで止めていた」は対象外）。
const waitingOngoing =
  /待ち(?:のまま|です|は変わりません|が続い|で止まってい(?:ます|る))|お待ちして|待っています/;
// 待ち相手として書かれる利用者以外。「監視」「Worker」は作業の主語にもなるので含めない。
const waitingOnOthers = /実装|受付|レビュ|相談|Codex|依頼先|こちらから送った/;
// 待ちが無い・終わった・待たなかったという報告。
const waitingResolved =
  /解消|待ち(?:の[^、。\n]{0,6})?は(?:あり|ござい)ません|待ちでは(?:なく|ありません)|待ちだった|待っていた|止まっていた|取り下げ|待たず|待って止まらず|回答を受け|戻りました|外れ|必要はありません/;
const waitingReferenceReason =
  '利用者の回答待ちを「表示済みの質問」「質問1の回答待ち」のように伝えるだけで、質問と選択肢の全文がありません。新しい変化を報告するなら、返答の最後に有効な質問（【質問分類：…】付き）と選択肢（A・B など）の全文を1回置き、この返答だけで何を聞かれているか分かるようにしてください。新しい変化が無いなら返答を書かないでください。利用者以外（実装AI・受付AIなど）の回答待ちなら、誰の回答待ちかを同じ文に書いてください';
function waitingOnly(text: string): boolean {
  if (statesQuestionWithChoices(text)) return false;
  // 共通番号に添えた題名や引用の語で判定しない。
  return stopProse(text)
    .replace(/[（(][^（）()\n]*[）)]|「[^「」\n]*」/g, '')
    .replace(/[（(][^（）()\n]*[）)]/g, '')
    .split(/[。\n]/)
    .some(
      (sentence) =>
        !waitingOnOthers.test(sentence) &&
        !waitingResolved.test(sentence) &&
        (pointsAtQuestion.test(sentence) ||
          (waitingOngoing.test(sentence) &&
            (waitingOnUser.test(sentence) ||
              (userQuestion.test(sentence) && waitingPhrase.test(sentence))))),
    );
}
const rearmReason =
  '見張り（follow-issues.ts）の時間が切れましたが張り直していません。同じコマンドで Monitor を起動し直してください';
const apology =
  /すみません|申し訳|失敗しました|確かめずに|守り損ね|確認せずに|私のミス|こちらのミス|判断ミス|ミスでした|誤りでした|報告の誤り|見落としました/g;
const reference =
  /(?:再発防止|防止)[^。\n]{0,40}(?:共通番号\s*[0-9０-９]+|\b[0-9a-f]{8}-[0-9a-f-]{27,}\b)|(?:共通番号\s*[0-9０-９]+|\b[0-9a-f]{8}-[0-9a-f-]{27,}\b)[^。\n]{0,40}(?:再発防止|防止)/i;
const reason =
  '依頼に関わる不具合と、利用者が原因・是正を尋ねた不具合は、既存の依頼を確認し、聞かずに修正依頼を起票して結果を報告してください。担当外の問題は既存の依頼の共通番号を示すか、事実を1文で伝えるだけにしてください。この指摘だけを理由に起票しないでください。記憶（memory）への保存は再発防止ではありません';

const duplicateReason =
  '起票・修正依頼を提案する前に、同じ内容の依頼が既にないかを scripts/list-issues.ts --duplicates --project <owner/repo> --query <語> で照合し、各種別の結果と読取失敗を確認してから提案してください。コマンドの実行だけでは結果の完全性や内容を検証できません。あれば新しい依頼ではなく既存の共通番号を示してください';
/** A sentence that asks the user whether to file (起票) or send a fix request. */
const filingProposal =
  /(?:起票|修正依頼を送)[^。\n]{0,30}(?:てよい|よいですか|ますか|でしょうか)/;

const filingQuestionReason =
  '不具合は起票の可否を利用者に聞かず、既存の依頼を確認して起票し、結果だけを報告してください（割り込みにするかも自分で決めます）。不具合とは言えない改善案だけは、質問文に「改善案」と書いて起票を提案できます';
/** Asking about these needs the user's approval, so a filing question that mentions them may stay. */
const approvalTerms =
  /直接\s*PR|上限(?:超え|超過|を超)|権限|マージ|削除|承認(?!なし|不要|せず)|許可の付与/;
/** 起票の可否を尋ねる言い方。「起票済みの依頼を確認しますか」のような起票の話題だけの質問は含めない。 */
const filingAsk =
  /起票(?:して(?:も)?(?:よい|いい|良い|おき)|しますか|しましょうか|するか|しておきますか)|修正依頼を送(?:って(?:も)?(?:よい|いい|良い)|りますか|りましょうか)/;

/** Bugs are filed without asking: stop a filing question unless it is marked as an improvement (改善案) or needs approval. */
export function filingQuestion(
  text: string,
  userText: string,
  asked: string[] = [],
): string | null {
  if (userText.includes('起票')) return null;
  const sentences = [stopProse(text), ...asked.map(stopProse)].flatMap(
    (prose) =>
      prose.replace(/【[^】]*】/g, '').match(/[^。！？!?\n]+[。！？!?]?/g) ??
      [],
  );
  for (const sentence of sentences) {
    const value = sentence.trim();
    if (
      filingAsk.test(value) &&
      !/改善/.test(value) &&
      !approvalTerms.test(value) &&
      /[？?]$|(?:ですか|ますか|でしょうか|ましょうか)[。]?\s*$/.test(value)
    )
      return filingQuestionReason;
  }
  return null;
}

const scopeReason =
  '担当外（依頼の範囲外・ほかのセッションや別の依頼の領分）に触れています。利用者はセッションを並行で動かしているので、同じ話を何度も受けることになります。この会話の依頼とその完了に必要なことだけを書き直してください。依頼が担当外の問題で止まるときは、止まっている事実を1文で伝えるだけにし、提案・質問・判断依頼はしません';
const scopeTerms =
  /担当外|依頼の範囲外|別件|ついでに|(?:別|他|ほか)のセッション|並行(?:の)?セッション/g;
const askSentence = /[？?]$|(?:ですか|ますか|でしょうか|ください)[。]?\s*$/;

// Prose used by the stop checks excludes quoted and code examples.
function stopProse(text: string): string {
  let fence = '';
  return text
    .split(/\r?\n/)
    .filter((line) => {
      const delimiter = line.trimStart().match(/^(`{3,}|~{3,})/);
      if (delimiter) {
        if (!fence) fence = delimiter[1][0];
        else if (delimiter[1][0] === fence) fence = '';
        return false;
      }
      return !fence && !/^\s*>/.test(line);
    })
    .map((line) => line.replace(/`+[^`]*`+/g, ''))
    .join('\n');
}

/** Wording right after 直接PR that refuses or withdraws it rather than asking for it. */
const directPRRefusal =
  /^(?:は|を|で|に|の|も)?[^。、\n]{0,10}?(?:しない|作らない|不要|いらない|要らない|必要(?:は)?ない|やめ|禁止|じゃな|ではな|頼んでいない|頼んでない|言っていない|言ってない)/;
/** An instruction to do the work through the direct PR, such as 直接PRで直して / 直接PRでやれ / 直接PRでマージして. */
const directPRInstruction =
  /^(?:で|に|にして|を)[^。、\n]{0,12}?(?:して|しろ|やれ|やって|作って|作れ|直せ|直して|入れ|進め|対応|マージ|是正|実装|修正)/;
const filingRequest =
  /起票(?:して(?!は|ほしくな|欲しくな|も(?:いい|よい)?(?:か|の)|ない)|しろ|しといて|しておいて)/;
/** Whether the latest relevant user turn asks for a direct PR. A later refusal of the direct PR, or a
 * request to file (起票して), ends it. Quoted text and Markdown quotes are not instructions. */
export function directPRActive(humanTurns: string[]): boolean {
  let active = false;
  for (const turnText of humanTurns) {
    let fence = false;
    const prose = turnText
      .split('\n')
      .filter((line) => {
        if (/^\s*(?:```|~~~)/.test(line)) {
          fence = !fence;
          return false;
        }
        return !fence && !/^\s*>/.test(line);
      })
      .join('\n')
      .replace(/「[^「」\n]*」|『[^『』\n]*』|"[^"\n]*"|`[^`\n]*`/g, '');
    for (const sentence of prose.match(/[^。！？!?\n]+/g) ?? []) {
      if (filingRequest.test(sentence)) active = false;
      else {
        const mention = sentence.match(/直接\s*PR(.*)$/i);
        if (mention && directPRRefusal.test(mention[1])) active = false;
        else if (mention && directPRInstruction.test(mention[1])) active = true;
      }
    }
  }
  return active;
}

function hasApology(text: string, userText: string): boolean {
  return [...stopProse(text).matchAll(apology)].some(
    (match) => !userText.includes(match[0]),
  );
}

function waitingForInstruction(text: string): boolean {
  const prose = stopProse(text).trim();
  if (/【質問分類：[^】]+】/.test(prose)) return false;
  const sentences = prose.match(/[^。！？!?\n]+[。！？!?]?/g) ?? [];
  const last = sentences.at(-1)?.trim() ?? '';
  return (
    /(?:と言っていただければ|と言ってもらえれば|指示があれば|ご指示いただければ)(?:[、\s]*(?:修正指示を送|この[^。！？!?]{0,30}着手|進め|対応|送|行|実施|開始|着手|確認|修正|起票|取りかか|取り掛か)[^。！？!?]*)?[。！？!?\s]*$/.test(
      last,
    ) ||
    /(?:と伝えてください|起票するなら言ってください)[。！？!?\s]*$/.test(last)
  );
}

/** A term the user used this turn is on topic: answering about it is not an out-of-scope mention. */
export function scopeMention(text: string, userText: string): string | null {
  let fence = '';
  for (const line of text.split(/\r?\n/)) {
    const delimiter = line.trimStart().match(/^(`{3,}|~{3,})/);
    if (delimiter) {
      if (!fence) fence = delimiter[1][0];
      else if (delimiter[1][0] === fence) fence = '';
      continue;
    }
    if (fence || /^\s*>/.test(line)) continue;
    const prose = line.replace(/`+[^`]*`+/g, '').replace(/【[^】]*】/g, '');
    for (const term of prose.match(scopeTerms) ?? [])
      if (!userText.includes(term)) return scopeReason;
    if (/PR/.test(userText)) continue;
    const sentences = prose.match(/[^。！？!?\n]+[。！？!?]?/g) ?? [];
    if (
      sentences.some(
        (sentence) =>
          /直接PR/.test(sentence) && askSentence.test(sentence.trim()),
      )
    )
      return scopeReason;
  }
  return null;
}

/** The only reasons to stop and ask. Everything else is decided by the AI and reported at the end. */
const askable =
  '破壊的な操作|取り返しのつかない操作|新しい権限|上限超えの再試行'.split('|');
/** 改善案の起票の提案だけは「範囲外の変更」で聞ける。何も待たせない提案で、答えが無ければ何もしない。 */
function askableCategory(
  category: string | undefined,
  sentence: string,
): boolean {
  return (
    category !== undefined &&
    (askable.includes(category) ||
      (category === '範囲外の変更' &&
        /改善案/.test(sentence) &&
        /起票|修正依頼|依頼を送/.test(sentence)))
  );
}
const decideReason =
  '破壊的な操作・取り返しのつかない操作・新しい権限・上限超えの再試行のほかは利用者に聞きません（範囲外の変更・判断の根拠の変化も聞きません。依頼の範囲内はおすすめ案で進め、範囲外へは広げません。「改善案」と書いた起票の提案だけは【質問分類：範囲外の変更】で聞けます）。自分で決めて進め、最後の返答の「判断の記録」に何をどう判断したかを書いてください。聞くときは質問と同じ文に【質問分類：破壊的な操作】の形で分類を付け、選択肢ごとの差とおすすめを添えてください';
/** 最後の返答の上限（コードブロックを除く文字数）。超える説明は聞かれたときに答える。 */
const REPORT_LIMIT = 800;
const reportReason = `報告が長すぎます。最後の返答を${REPORT_LIMIT}文字以内にしてください。結論を先頭に書き、利用者に関係ない内部の事情・経緯・見張りの説明は書かず、同じことを繰り返さないでください`;
const noWatchNoteReason =
  '「見張りを付けない理由」や「裏で動かしている処理はありません」を返答に書かないでください。利用者に関係ありません';
const questionLastReason =
  '質問が返答の途中にあります。質問と選択肢は返答の最後にまとめ、1回だけ書いてください。後ろに報告を続けず、同じ質問を繰り返さないでください';

/** Stopフックが止めるのは返答の表示後。書き直しで同じ内容を2回読ませない。 */
const noRepeatAfterBlock =
  '前の返答は利用者に表示済みです。全文を書き直さず、内訳・一覧も再掲せず、足りない点だけを短く書いてください。表示済みの質問が有効なままなら再掲しないでください。質問の内容を変えたとき、または指摘が質問自体に向いたときだけ、質問と選択肢を最後に1回置いてください。判断の記録が要る回は、その行だけを足してください';

/** 引用・コードブロックを除いた本文の行。 */
function proseLines(text: string): string[] {
  let fence = '';
  const lines: string[] = [];
  for (const line of text.split(/\r?\n/)) {
    const delimiter = line.trimStart().match(/^(`{3,}|~{3,})/);
    if (delimiter) {
      if (!fence) fence = delimiter[1][0];
      else if (delimiter[1][0] === fence) fence = '';
      continue;
    }
    if (!fence && !/^\s*>/.test(line)) lines.push(line);
  }
  return lines;
}

/** 読めない報告を止める。質問は最後に1回だけ、本文は上限まで、利用者に関係ない説明は書かない。 */
export function reportProblem(finalText: string): string | null {
  const lines = proseLines(finalText);
  const body = lines.join('\n');
  if (/見張りを付けない理由|裏で動かしている処理は(?:、)?ありません/.test(body))
    return noWatchNoteReason;
  if ([...body.replace(/\s/g, '')].length > REPORT_LIMIT) return reportReason;
  const kept = lines.filter((line) => line.trim());
  const asks = kept.flatMap((line, index) =>
    /【質問分類：/.test(line) ? [index] : [],
  );
  if (!asks.length) return null;
  // 最後の質問より後ろに置いてよいのは、選択肢とその答え方の説明だけ。
  const tail = kept.slice(asks.at(-1)! + 1);
  const optionOrHow =
    /^\s*(?:[-*•]\s*)?(?:\*\*)?[0-9０-９]*-?[A-ZＡ-Ｚ](?:（おすすめ）)?(?:\*\*)?\s*[:：.．)）]|番号と記号|だけで答え/;
  if (tail.some((line) => !optionOrHow.test(line))) return questionLastReason;
  // 同じ選択肢の記号を質問の外で繰り返さない。
  if (kept.slice(0, asks[0]).some((line) => /[0-9０-９]-[A-ZＡ-Ｚ]/.test(line)))
    return questionLastReason;
  return null;
}

const decisionLogReason =
  '作業をした回の最後の返答に「判断の記録」がありません。自分で決めたことのうち利用者が覆す可能性のあるものだけを「何を・どう判断したか・理由」で書いてください。該当が無ければ「判断の記録: なし」と書いてください';

/** A real question (not a request like 「確認してください」) that lacks an allowed category. */
function needlessQuestion(text: string, asked: string[]): boolean {
  for (const question of asked) {
    if (
      !askableCategory(question.match(/【質問分類：([^】]+)】/)?.[1], question)
    )
      return true;
  }
  let fence = '';
  for (const line of text.split(/\r?\n/)) {
    const delimiter = line.trimStart().match(/^(`{3,}|~{3,})/);
    if (delimiter) {
      if (!fence) fence = delimiter[1][0];
      else if (delimiter[1][0] === fence) fence = '';
      continue;
    }
    if (fence || /^\s*>/.test(line)) continue;
    const prose = line
      .replace(/`+[^`]*`+/g, '')
      .replace(/https?:\/\/[^\s。！？]+/g, '');
    for (const sentence of prose.match(/[^。！？!?\n]+[。！？!?]?/g) ?? []) {
      if (!/[？?]$|(?:ですか|ますか|でしょうか)[。]?\s*$/.test(sentence.trim()))
        continue;
      if (
        !askableCategory(
          sentence.match(/【質問分類：([^】]+)】/)?.[1],
          sentence,
        )
      )
        return true;
    }
  }
  return false;
}

// Check each question separately: a category on an earlier question never authorizes the next one.
function questionWarning(text: string): string | null {
  let fence = '';
  let missing = false;
  let forbidden = false;
  const numbers: (string | null)[] = [];
  for (const line of text.split(/\r?\n/)) {
    const delimiter = line.trimStart().match(/^(`{3,}|~{3,})/);
    if (delimiter) {
      if (!fence) fence = delimiter[1][0];
      else if (delimiter[1][0] === fence) fence = '';
      continue;
    }
    if (fence || /^\s*>/.test(line)) continue;
    const prose = line
      .replace(/`+[^`]*`+/g, '')
      .replace(/https?:\/\/[^\s。！？]+/g, '');
    const sentences = prose.match(/[^。！？!?\n]+[。！？!?]?/g) ?? [];
    for (const [index, sentence] of sentences.entries()) {
      if (
        !/[？?]$|(?:ですか|ますか|でしょうか|ください|教えて)[。]?\s*$/.test(
          sentence.trim(),
        )
      )
        continue;
      const category = sentence.match(/【質問分類：([^】]+)】/);
      if (!category) missing = true;
      else {
        if (!askableCategory(category[1], sentence)) forbidden = true;
        // Only a sentence that starts the line can carry the line-head number.
        const number =
          index === 0
            ? sentence.match(
                /^\s*(?:#{1,6}\s*)?(?:\*\*)?\s*([0-9０-９]+)\s*[.．)）]/,
              )
            : null;
        numbers.push(
          number
            ? number[1].replace(/[０-９]/g, (d) =>
                String(d.charCodeAt(0) - 0xff10),
              )
            : null,
        );
      }
    }
  }
  const unnumbered =
    numbers.length >= 2 &&
    (numbers.includes(null) || new Set(numbers).size < numbers.length);
  if (!missing && !forbidden && !unnumbered) return null;
  const messages = [
    missing && '分類のない質問があります',
    forbidden && '聞かない分類または不明な分類の質問があります',
    unnumbered &&
      '番号のない複数の質問があります。質問に「1.」「2.」、選択肢に「1-A」「1-B」などを付け、「1-A、2-B」のように答えられると添えてください',
    '質問ごとに【質問分類：破壊的な操作】等を付け、許可分類だけを選択肢の差とおすすめ付きで尋ねてください。それ以外はAIが決めて判断の記録に書いてください',
  ];
  return `質問基準: ${messages.filter(Boolean).join('。')}`;
}

// Shared fence/quote/sentence rules; keep inline code for permission identifiers.
function commandProse(text: string): { sentences: string[]; code: boolean } {
  let fence = '';
  let code = false;
  const sentences: string[] = [];
  for (const line of text.split(/\r?\n/)) {
    const delimiter = line.trimStart().match(/^(`{3,}|~{3,})/);
    if (delimiter) {
      if (!fence) fence = delimiter[1][0];
      else if (delimiter[1][0] === fence) fence = '';
      continue;
    }
    if (fence) {
      if (line.trim()) code = true;
      continue;
    }
    if (/^\s*>/.test(line)) continue;
    const prose = line.replace(/https?:\/\/[^\s。！？]+/g, '');
    sentences.push(...(prose.match(/[^。！？!?\n]+[。！？!?]?/g) ?? []));
  }
  return { sentences, code };
}

const permissionSettingsWarning =
  '利用者は許可を設定していません。許可ルールの追加を頼まず、何をなぜ行うかを示して承認を求め、承認後に自分で叩いてください。止められない経路か承認後も通らないときだけ、その場で叩く1行にしてください';

function permissionWarning(text: string): string | null {
  for (const sentence of commandProse(text).sentences) {
    if (
      !/permissions(?:\.allow)?|allow\s*ルール|許可(?:設定|ルール)|settings(?:\.local)?\.json/i.test(
        sentence,
      )
    )
      continue;
    if (
      !/(?:足して|追加して|追記して|加えて)(?:ください|下さい|もら|いただ|くれ|ほしい|欲しい|[。！？!?」』]?\s*$)/.test(
        sentence,
      )
    )
      continue;
    return permissionSettingsWarning;
  }
  return null;
}

const manualRequestWarning = 'AIが叩けないか、先に承認を求めて試してください';
const manualReason =
  '拒否された操作は、何をなぜ行うかを示して承認を求め、承認後に自分で再実行してください。利用者の手作業にしないでください';
function manualRequest(text: string): boolean {
  const { sentences, code } = commandProse(text);
  return (
    code &&
    sentences.some((sentence) =>
      /(?:(?:叩いて|実行して|貼って|貼り付けて|打って|打ち込んで|走らせて|入力して)(?:ください|下さい|もら(?!わ)|いただ(?!か)|くれ(?!な)|ほしい|欲しい|[。！？!?」』]?\s*$)|(?:実行|入力|貼り付け)をお願い)/.test(
        sentence,
      ),
    )
  );
}

function toolDenied(value: unknown): boolean {
  const text =
    typeof value === 'string'
      ? value
      : Array.isArray(value)
        ? value
            .filter(
              (part) => part?.type === 'text' && typeof part.text === 'string',
            )
            .map((part) => part.text)
            .join('\n')
        : '';
  const refusal =
    /Permission for this action was denied|doesn't want to proceed with this tool use|tool use was rejected/i;
  return (
    refusal.test(text) ||
    (/Permission to use/i.test(text) && /denied/i.test(text))
  );
}

// Expand shell -c strings so each command is inspected at its actual command position.
export function parseCommands(command: string, depth = 0): Segment[] | null {
  const segments = parseShellCommands(command);
  if (!segments || depth >= 16) return segments;
  return segments.flatMap((segment) => {
    const inner = shellCommandString(segment.words);
    if (inner === null) return [segment];
    const parsed = parseCommands(inner, depth + 1);
    return parsed
      ? parsed.map((item) => ({
          ...item,
          background: item.background || segment.background,
        }))
      : [segment];
  });
}

/** True when the command launches work in the background without being watched by stage-watch.ts. */
function unwatchedBackground(input: NonNullable<Part['input']>): boolean {
  const command = input.command!;
  if (command.includes('stage-watch.ts')) return false;
  // run_in_background itself reports the end to the conversation; only a launch that detaches from it is unwatched.
  return (parseCommands(command) ?? []).some(
    ({ words, background }) =>
      background || /^(?:nohup|setsid)$/.test(normalizeDeskArgs(words)[0]),
  );
}

/**
 * 背景で動かす見張りの中で、さらに `&`・nohup・setsid・disown で切り離すと、見張りが終わっても完了通知が会話に戻らない。
 * 切り離した見張りは、見張りとして数えない。
 */
export function selfDetaching(command: string): boolean {
  return (parseCommands(command) ?? []).some(
    ({ words, background }) =>
      background ||
      /^(?:nohup|setsid|disown)$/.test(normalizeDeskArgs(words)[0]),
  );
}

/** 重複照合入口の引数を確認する。結果の内容や完全性は判定できない。 */
function duplicateSearchCommand(command: string): boolean {
  return (parseCommands(command) ?? []).some(({ words }) => {
    const found = scriptOf(normalizeDeskArgs(words));
    if (!found || !/(?:^|[\\/])list-issues\.ts$/.test(found.script))
      return false;
    const args = found.args;
    const value = (name: string) => {
      const i = args.indexOf(name);
      return i >= 0 && !!args[i + 1]?.trim() && !args[i + 1].startsWith('--');
    };
    return (
      args.includes('--duplicates') && value('--project') && value('--query')
    );
  });
}

function issueCommand(command: string, filing = false): boolean {
  return (parseCommands(command) ?? []).some(({ words }) => {
    const found = scriptOf(normalizeDeskArgs(words));
    if (!found || !isIssueScript(found.script)) return false;
    const { args } = found;
    // 起票も実装AIへの返信も、結果を見張る仕組みが要る。
    if (filing) return !hasCheckOption(args);
    return (
      !hasCheckOption(args, { includeOnly: false }) || args.includes('--draft')
    );
  });
}

// A visible --text can be checked; opaque file/stdin bodies deliberately fail open.
function preventionCommand(command: string): boolean {
  return (parseCommands(command) ?? []).some(({ words }) => {
    const found = scriptOf(normalizeDeskArgs(words));
    if (!found || !isIssueScript(found.script)) return false;
    const { args } = found;
    if (args.some((arg) => /^--hold(?:=|$)/.test(arg))) return false;
    if (hasCheckOption(args) && !args.includes('--draft')) return false;
    const texts = args.flatMap((arg, i) =>
      arg === '--text'
        ? [args[i + 1] ?? '']
        : arg.startsWith('--text=')
          ? [arg.slice(7)]
          : [],
    );
    if (texts.length)
      return texts.some((value) =>
        /再発防止|防止|仕組み|フック|検査/.test(value),
      );
    return (
      args.some((arg) => /^--file(?:=|$)/.test(arg)) ||
      /(?:^|\s)(?:<|<<)/.test(command) ||
      !args.some((arg) => /^--text(?:=|$)/.test(arg))
    );
  });
}

/** A follow-issues.ts launch that keeps running after the reply ends (Monitor, or a background Bash). */
function followLaunch(part: Part): boolean {
  if (part.type !== 'tool_use' || !/^(?:Monitor|Bash)$/.test(part.name ?? ''))
    return false;
  if (
    part.name === 'Bash' &&
    (part.input?.run_in_background !== true ||
      selfDetaching(String(part.input?.command ?? '')))
  )
    return false;
  return (
    typeof part.input?.command === 'string' &&
    /(?:^|[\s/\\])follow-issues\.ts(?:\s|$)/.test(part.input.command)
  );
}

type TurnStart = { origin: 'human' | 'notification'; text: string };
/**
 * Where a turn begins. Tool results and meta entries (Skill bodies, reminders, peer messages) do not begin one.
 * A background task's notification does, and it is not the user's words, so a question asked earlier does not
 * govern the work it triggers. Text typed while the AI is working arrives as a queued_command attachment.
 */
function turnStart(entry: any): TurnStart | null {
  if (!entry || typeof entry !== 'object') return null;
  if (entry.type === 'attachment') {
    const attachment = entry.attachment;
    if (
      attachment?.type !== 'queued_command' ||
      typeof attachment.prompt !== 'string'
    )
      return null;
    if (attachment.commandMode === 'task-notification')
      return { origin: 'notification', text: attachment.prompt };
    return attachment.commandMode === 'prompt'
      ? { origin: 'human', text: attachment.prompt }
      : null;
  }
  if (entry.type !== 'user' || entry.isMeta === true) return null;
  const raw = entry.message?.content;
  if (Array.isArray(raw) && raw.some((part) => part?.type === 'tool_result'))
    return null;
  const text = plainText(raw);
  const notification =
    entry.origin?.kind === 'task-notification' ||
    text.trimStart().startsWith('<task-notification>');
  return { origin: notification ? 'notification' : 'human', text };
}

function content(value: unknown): Part[] {
  if (typeof value === 'string') return [{ type: 'text', text: value }];
  if (!Array.isArray(value)) throw new Error('unknown content');
  for (const part of value) {
    if (!part || typeof part !== 'object')
      throw new Error('unknown content block');
    if (part.type === 'text' && typeof part.text !== 'string')
      throw new Error('unknown text');
    if (
      part.type === 'tool_use' &&
      (typeof part.name !== 'string' ||
        !part.input ||
        typeof part.input !== 'object')
    )
      throw new Error('unknown tool');
    if (
      part.type === 'tool_use' &&
      part.name === 'Bash' &&
      typeof part.input.command !== 'string'
    )
      throw new Error('unknown Bash input');
  }
  // New block kinds are skipped, not treated as a reason to give up on every check of the turn.
  return value.filter((part) =>
    [
      'text',
      'tool_use',
      'tool_result',
      'thinking',
      'redacted_thinking',
      'image',
      'document',
    ].includes(part.type),
  );
}

const unreadOperationReason =
  '裏で送った受付の操作の結果を読まずに報告しています。出力を読んでから報告してください。読めないなら「送信中で、結果は未確認」とだけ書いてください';

// Track each launch separately, in transcript order; a later read cannot excuse an earlier claim.
// prettier-ignore
function unreadReport(entries: { type: string; message?: { content?: unknown }; toolUseResult?: unknown }[]): string | null {
  try {
  const pending: { id?: string; refs: string[]; request: string[]; read: boolean }[] = [];
  for (const entry of entries) {
    if (entry.type !== 'assistant' && entry.type !== 'user') continue;
    for (const part of content(entry.message?.content)) {
      if (entry.type === 'user' && part.type === 'tool_result') {
        const launch = pending.find((item) => item.id && item.id === part.tool_use_id);
        if (!launch) continue;
        content(part.content);
        const result = JSON.stringify({ content: part.content, result: entry.toolUseResult });
        for (const match of result.matchAll(/(?:"(?:bash_id|task_id|bashId|backgroundTaskId|output_file|outputFile)":"|ID: |Output is being written to: )([^"\s\\<]+)/g))
          launch.refs.push(match[1].replace(/[。.,]$/, ''));
        for (const match of result.matchAll(/(?:"(?:draftId|requestId|draft_id|request_id)":"|--check |--request )([a-zA-Z0-9_-]+)/g))
          launch.request.push(match[1]);
      }
      if (entry.type !== 'assistant') continue;
      if (part.type === 'text' && !/未確認|送信中/.test(part.text!) && /相談中|起票しました|起票しています|受け付けました|送りました|閉じました|番号は/.test(part.text!) && pending.some((item) => !item.read))
        return unreadOperationReason;
      if (part.type !== 'tool_use') continue;
      const input = part.input!;
      const commands = part.name === 'Bash' ? parseCommands(input.command!) ?? [] : [];
      for (const launch of pending) {
        if (/^(?:BashOutput|TaskOutput|Read)$/.test(part.name!) && Object.entries(input).some(([key, value]) => /^(?:bash_id|task_id|file_path|output_file)$/.test(key) && typeof value === 'string' && launch.refs.includes(value))) launch.read = true;
        for (const { words } of commands) {
          const normalized = normalizeDeskArgs(words);
          const readArgs = normalizeDeskArgs(words, false);
          if (/^(?:cat|tail|head|sed|less|more)$/.test(readArgs[0]) && readArgs.slice(1).some((word) => launch.refs.includes(word))) launch.read = true;
          const found = scriptOf(normalized);
          const script = found?.script;
          const args = found?.args ?? [];
          if (isIssueScript(script ?? '') && args.some((arg) => /^--check(?:=|$)|^--request(?:=|$)/.test(arg)) && args.some((arg, index) => /^(?:--draft|--request|--number|--id|--check)$/.test(arg) ? launch.request.includes(args[index + 1]) : launch.request.includes(arg.match(/^--(?:draft|request|number|id|check)=(.+)$/)?.[1] ?? ''))) launch.read = true;
        }
      }
      if (part.name !== 'Bash' || input.run_in_background !== true) continue;
      for (const { words } of commands) {
        const found = scriptOf(normalizeDeskArgs(words));
        if (!found || !watchedSendScripts.test(found.script)) continue;
        const { script, args } = found;
        if (isIssueScript(script) && args.some((arg) => /^--check(?:=|$)/.test(arg)) && !args.includes('--draft')) continue;
        const refs = words.flatMap((word, index) => /^\d*(?:>>?|&>)$/.test(word) ? [words[index + 1]] : word.match(/^\d*(?:>>?|&>)(.+)$/)?.slice(1) ?? []).filter((word): word is string => !!word);
        const request = args.flatMap((arg, index) => /^(?:--draft|--request|--number|--id)$/.test(arg) ? [args[index + 1]] : arg.match(/^--(?:draft|request|number|id)=(.+)$/)?.slice(1) ?? []).filter((word): word is string => !!word);
        pending.push({ id: part.id, refs, request, read: false });
      }
    }
  }
  return null;
  } catch {
    return null;
  }
}

/** 安定した検査IDと説明。固定文言は実際の出力と共有し、可変文言は要約を登録する。 */
export const stopCheckRegistry = {
  'unasked-send': { kind: 'fixed', text: unaskedReason },
  'unasked-work': { kind: 'fixed', text: unaskedWorkReason },
  'attention-unread': { kind: 'fixed', text: followAttentionReason },
  'answer-first': { kind: 'fixed', text: answerFirstReason },
  'silent-watch': { kind: 'fixed', text: silentWatchReason },
  'stall-unhandled': { kind: 'fixed', text: leftAloneReason },
  'foreground-repeat': { kind: 'fixed', text: foregroundWarning },
  'apology-prevention': { kind: 'fixed', text: reason },
  scope: { kind: 'fixed', text: scopeReason },
  'filing-question': { kind: 'fixed', text: filingQuestionReason },
  'duplicate-search': { kind: 'fixed', text: duplicateReason },
  'unwatched-background': { kind: 'fixed', text: watchReason },
  'unnecessary-question': { kind: 'fixed', text: decideReason },
  'report-length': { kind: 'fixed', text: reportReason },
  'report-watch-note': { kind: 'fixed', text: noWatchNoteReason },
  'report-question-position': { kind: 'fixed', text: questionLastReason },
  'decision-log': { kind: 'fixed', text: decisionLogReason },
  'filing-watch': { kind: 'fixed', text: filingReason },
  'watch-rearm': { kind: 'fixed', text: rearmReason },
  'waiting-reference': { kind: 'fixed', text: waitingReferenceReason },
  'denied-manual': { kind: 'fixed', text: manualReason },
  'unread-operation': { kind: 'fixed', text: unreadOperationReason },
  'instruction-wait': {
    kind: 'fixed',
    text: '許可済みなら自分で行う。判断が要るなら分類を付けて質問する',
  },
  'authorized-question': { kind: 'fixed', text: authorizedQuestionReason },
  'stop-question': {
    kind: 'fixed',
    text: '停止は再開で戻せる操作で、利用枠・時間の無駄を止める停止は聞かずに行い、結果を報告してください',
  },
  'irreversible-question': { kind: 'fixed', text: irreversibleQuestionReason },
  'question-format': { kind: 'summary', text: '質問基準' },
  'permission-settings': { kind: 'fixed', text: permissionSettingsWarning },
  'manual-request': { kind: 'fixed', text: manualRequestWarning },
  'investigation-before-answer': {
    kind: 'summary',
    text: '回答より先に調査をしました。質問への回答を先にしてください',
  },
} as const;

export type StopCheckId = keyof typeof stopCheckRegistry;
type CheckMessage = [StopCheckId, string | null];

/** 未登録の文言は警告し、他の検査の判定を保つ。 */
export function registeredCheck(message: string): StopCheckId | null {
  const match = (
    Object.entries(stopCheckRegistry) as [
      StopCheckId,
      { kind: string; text: string },
    ][]
  ).find(([, value]) => value.kind === 'fixed' && value.text === message);
  if (!match) {
    console.error('unregistered stop check message:', message);
    return null;
  }
  return match[0];
}

function registeredMessages(messages: string[]): CheckMessage[] {
  return messages.flatMap((message) => {
    const id = registeredCheck(message);
    return id ? [[id, message] as CheckMessage] : [];
  });
}

/** 記録直前にも不正なIDを拒否する。 */
export function checkedFires(
  fires: { id: StopCheckId; result: 'block' | 'warn' }[],
) {
  for (const fire of fires)
    if (!fire.id || !Object.hasOwn(stopCheckRegistry, fire.id))
      throw new Error('invalid stop check ID');
  return fires;
}

/** Block decision plus non-blocking warnings; unknown shapes yield neither (fail-open). */
export function inspectTurn(input: unknown, transcript: string): Inspection {
  const none: Inspection = { block: null, warning: null, fires: [] };
  try {
    if (!input || typeof input !== 'object' || Array.isArray(input))
      return none;
    const lines = transcript.split(/\r?\n/).filter((line) => line.trim());
    const entries = lines.map((line) => {
      try {
        return JSON.parse(line);
      } catch {
        return null;
      }
    });
    const boundary = entries.findLastIndex(
      (entry) => turnStart(entry) !== null,
    );
    if (boundary < 0) return none;
    const start = turnStart(entries[boundary])!;
    const texts: string[] = [];
    const textEntryIndexes: number[] = [];
    const afterDenial: string[] = [];
    let denied = false;
    let requested = false;
    const preventionCalls = new Set<string>();
    const userText = start.origin === 'human' ? start.text : '';
    // 通知の回の範囲は、直前の利用者の発言と同じ基準で判定する。前の返答にある質問を選択肢ごとそのまま置き直した回だけは、
    // 間に別の発言があっても止めないよう、この会話の利用者の発言すべてを依頼として判定する。
    const priorHumans = entries
      .slice(0, boundary + 1)
      .map((entry) => turnStart(entry))
      .filter((entry): entry is TurnStart => entry?.origin === 'human')
      .map((entry) => entry.text);
    const priorReplies = entries
      .slice(0, boundary)
      .filter((entry: any) => entry?.type === 'assistant')
      .map((entry: any) => plainText(entry.message?.content))
      .join('\n');
    const requestFor = (reply: string) =>
      start.origin === 'human'
        ? start.text
        : repeatsVisibleQuestion(priorReplies, reply)
          ? priorHumans.join('\n')
          : (priorHumans.at(-1) ?? '');
    let unwatched = false;
    let answered = false;
    let beforeAnswer = 0;
    const filingCalls = new Set<string>();
    let searchedExisting = false;
    let filingUnwatched = false;
    let followed = false;
    const asked: string[] = [];
    const toolQuestions: { question: string; options: string[] }[] = [];
    for (const [offset, entry] of entries.slice(boundary).entries()) {
      // Claude Code adds entry kinds over time (attachment, mode, last-prompt…). Only these two carry the conversation.
      if (
        !entry ||
        typeof entry !== 'object' ||
        (entry.type !== 'user' && entry.type !== 'assistant')
      )
        continue;
      // Earlier turns are only scanned for user-turn boundaries. Their content may
      // use formats this hook does not understand, so validate it only when current.
      const rawContent = entry.message?.content;
      if (entry.type === 'user') {
        const parts = content(rawContent);
        if (entry !== entries[boundary]) {
          for (const part of parts) {
            if (part.type !== 'tool_result') continue;
            if (toolDenied(part.content)) denied = true;
            if (
              part.tool_use_id &&
              preventionCalls.has(part.tool_use_id) &&
              part.is_error !== true &&
              !toolDenied(part.content)
            )
              requested = true;
            if (
              typeof part.tool_use_id !== 'string' ||
              !filingCalls.has(part.tool_use_id)
            )
              continue;
            const successful = content(part.content).some(
              (item) =>
                item.type === 'text' &&
                /^(?:起票しました（[0-9]+件）:|下書き: \S+|(?:修正指示|追加指示|回答|返信)を送りました)/m.test(
                  item.text!,
                ),
            );
            if (part.is_error !== true && successful) filingUnwatched = true;
          }
        }
      } else {
        const parts = content(rawContent);
        for (const part of parts) {
          if (part.type === 'text') {
            texts.push(part.text!);
            textEntryIndexes.push(boundary + offset);
            if (denied) afterDenial.push(part.text!);
            if (part.text!.trim().length >= SUBSTANTIVE_TEXT_LENGTH)
              answered = true;
          }
          if (part.type !== 'tool_use') continue;
          if (
            part.name === 'AskUserQuestion' &&
            Array.isArray(part.input!.questions)
          )
            for (const item of part.input!.questions as {
              question?: unknown;
              options?: unknown;
            }[]) {
              const question =
                typeof item?.question === 'string' ? item.question : '';
              asked.push(question);
              const options = Array.isArray(item?.options)
                ? item.options.flatMap((option: unknown) => {
                    if (typeof option === 'string') return [option];
                    if (!option || typeof option !== 'object') return [];
                    const value = option as {
                      label?: unknown;
                      description?: unknown;
                    };
                    return [value.label, value.description].filter(
                      (v): v is string => typeof v === 'string',
                    );
                  })
                : [];
              toolQuestions.push({ question, options });
            }
          const background = part.input!.run_in_background === true;
          if (followLaunch(part)) {
            filingUnwatched = false;
            followed = true;
          }
          // Sub-agent launches and background launches are not investigation before the answer.
          if (
            !answered &&
            !background &&
            part.name !== 'Task' &&
            part.name !== 'Agent'
          )
            beforeAnswer++;
          if (part.name !== 'Bash') continue;
          if (part.id && preventionCommand(part.input!.command!))
            preventionCalls.add(part.id);
          if (duplicateSearchCommand(part.input!.command!))
            searchedExisting = true;

          if (
            typeof part.id === 'string' &&
            issueCommand(part.input!.command!, true)
          )
            filingCalls.add(part.id);
          if (unwatchedBackground(part.input!)) unwatched = true;
        }
      }
    }
    if ('stop_hook_active' in input && input.stop_hook_active === true) {
      const cache = new Map<
        number,
        { blocked: boolean; questionInvalid: boolean }
      >();
      const inspectPrefix = (index: number) => {
        if (!cache.has(index)) {
          const previous = inspectTurn(
            {},
            lines.slice(0, index + 1).join('\n'),
          );
          const questionChecks: StopCheckId[] = [
            'report-question-position',
            'unnecessary-question',
            'filing-question',
            'scope',
            'question-format',
            'authorized-question',
            'irreversible-question',
            'stop-question',
          ];
          cache.set(index, {
            blocked: previous.block !== null,
            questionInvalid: previous.fires.some((fire) =>
              questionChecks.includes(fire.id),
            ),
          });
        }
        return cache.get(index)!;
      };
      return repeatedQuestionOnFirstContinuation(
        texts,
        textEntryIndexes,
        inspectPrefix,
      )
        ? {
            block: {
              decision: 'block',
              reason: [questionLastReason, noRepeatAfterBlock].join('。'),
            },
            warning: null,
            fires: checkedFires([
              { id: 'report-question-position', result: 'block' },
            ]),
          }
        : none;
    }
    const text = texts.join('\n');
    const humanTurns = entries
      .slice(0, boundary + 1)
      .map(turnStart)
      .filter((item): item is TurnStart => item?.origin === 'human')
      .map((item) => item.text);
    const policy = questionPolicy(text, asked, humanTurns);
    const permission = permissionWarning(text);
    const manual = manualRequest(text) ? manualRequestWarning : null;
    // While the user has asked for a direct PR in this session, fixes and recurrence prevention
    // go into that PR work, so an apology is not a reason to ask for a filed fix request.
    const directPR = directPRActive(humanTurns);
    if (
      hasApology(text, userText) &&
      !reference.test(stopProse(text)) &&
      !requested &&
      !directPR
    )
      return {
        block: { decision: 'block', reason },
        warning: permission,
        fires: checkedFires([
          { id: 'apology-prevention', result: 'block' },
          ...(permission
            ? [{ id: 'permission-settings' as const, result: 'warn' as const }]
            : []),
        ]),
      };
    const report = unreadReport(entries.slice(boundary));
    const turn = turnChecks(
      entries.slice(boundary),
      userText,
      (command) => parseCommands(command),
      content,
      start.origin === 'notification' ? start.text : '',
    );

    const lastIndex = textEntryIndexes.at(-1);
    let repeatedQuestion = false;
    for (let i = texts.length - 2; i >= 0 && lastIndex !== undefined; i--) {
      const previousIndex = textEntryIndexes[i];
      if (
        previousIndex >= lastIndex ||
        !repeatsVisibleQuestion(texts[i], texts.at(-1)!)
      )
        continue;
      const previous = inspectTurn(
        input,
        lines.slice(0, previousIndex + 1).join('\n'),
      );
      const questionChecks: StopCheckId[] = [
        'report-question-position',
        'unnecessary-question',
        'filing-question',
        'scope',
        'question-format',
        'authorized-question',
        'irreversible-question',
        'stop-question',
      ];
      repeatedQuestion = !previous.fires.some((fire) =>
        questionChecks.includes(fire.id),
      );
      break;
    }

    // Pair each emitted message with its ID at the decision site; ordering preserves the old output.
    const blocks: CheckMessage[] = [
      ...registeredMessages(turn.blocks),
      ['scope', scopeMention(text, requestFor(text))],
      ['filing-question', filingQuestion(text, requestFor(text), asked)],
      [
        'duplicate-search',
        filingProposal.test(text) && !searchedExisting ? duplicateReason : null,
      ],
      ['unwatched-background', unwatched ? watchReason : null],
      [
        'authorized-question',
        policy.authorized ? authorizedQuestionReason : null,
      ],
      [
        'stop-question',
        stopQuestion(texts.at(-1) ?? '', toolQuestions, userText)
          ? stopCheckRegistry['stop-question'].text
          : null,
      ],
      [
        'irreversible-question',
        policy.missingReason ? irreversibleQuestionReason : null,
      ],
      [
        'unnecessary-question',
        needlessQuestion(text, asked) ? decideReason : null,
      ],
      ...registeredMessages(
        [reportProblem(texts.at(-1) ?? '')].filter(
          (message): message is string => message !== null,
        ),
      ),
      [
        'report-question-position',
        repeatedQuestion ? questionLastReason : null,
      ],
      [
        'decision-log',
        turn.worked && !/判断の記録/.test(texts.at(-1) ?? '')
          ? decisionLogReason
          : null,
      ],
      ['filing-watch', filingUnwatched ? filingReason : null],
      [
        'watch-rearm',
        start.origin === 'notification' &&
        /【張り直し】/.test(start.text) &&
        !followed
          ? rearmReason
          : null,
      ],
      [
        'waiting-reference',
        start.origin === 'notification' &&
        !asked.length &&
        waitingOnly(texts.at(-1) ?? '')
          ? waitingReferenceReason
          : null,
      ],
      ['unread-operation', report],
      [
        'denied-manual',
        manualRequest(afterDenial.join('\n')) ? manualReason : null,
      ],
      [
        'instruction-wait',
        waitingForInstruction(text)
          ? stopCheckRegistry['instruction-wait'].text
          : null,
      ],
    ];
    const warnings: CheckMessage[] = [
      ['question-format', questionWarning(text)],
      ['permission-settings', permission],
      ['manual-request', manual],
      ...registeredMessages(turn.warnings),
      [
        'investigation-before-answer',
        beforeAnswer >= HEAVY_INVESTIGATION_CALLS
          ? `回答より先に調査（${beforeAnswer}回）をしました。質問への回答を先にしてください`
          : null,
      ],
    ];
    const stopReasons = blocks.filter(([, message]) => message);
    const warningReasons = warnings.filter(([, message]) => message);
    return {
      block: stopReasons.length
        ? {
            decision: 'block',
            reason: [
              ...stopReasons.map(([, message]) => message),
              ...(stopReasons.some(([id]) => id === 'waiting-reference')
                ? []
                : [noRepeatAfterBlock]),
            ].join('。'),
          }
        : null,
      warning: warningReasons.map(([, message]) => message).join('。') || null,
      fires: checkedFires([
        ...stopReasons.map(([id]) => ({ id, result: 'block' as const })),
        ...warningReasons.map(([id]) => ({ id, result: 'warn' as const })),
      ]),
    };
  } catch {
    return none;
  }
}

function allowResponse(now: () => Date, warning: string | null = null): string {
  try {
    const parts = new Intl.DateTimeFormat('en-US', {
      timeZone: 'Asia/Tokyo',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      hourCycle: 'h23',
    }).formatToParts(now());
    const value = (type: string) =>
      parts.find((part) => part.type === type)!.value;
    const stamp = `返答 ${value('month')}/${value('day')} ${value('hour')}:${value('minute')}（日本時間）`;
    return `${JSON.stringify({ systemMessage: warning ? `${stamp}。${warning}。` : stamp })}\n`;
  } catch {
    return warning
      ? `${JSON.stringify({ systemMessage: `${warning}。` })}\n`
      : '';
  }
}

/** I/O adapter kept injectable so malformed input/read errors need no subprocess tests. */
export function stopHookResponse(
  raw: string,
  readTranscript: (file: string) => string = (file) =>
    fs.readFileSync(file, 'utf8'),
  now: () => Date = () => new Date(),
  record: (fires: Inspection['fires'], cwd: unknown) => void = () => {},
): string {
  try {
    const input = JSON.parse(raw);
    if (typeof input?.transcript_path !== 'string') return allowResponse(now);
    const { block, warning, fires } = inspectTurn(
      input,
      readTranscript(input.transcript_path),
    );
    try {
      record(fires, input.cwd);
    } catch {}
    return block ? `${JSON.stringify(block)}\n` : allowResponse(now, warning);
  } catch {
    return allowResponse(now);
  }
}

function runStopHook(
  readInput: () => string = () => fs.readFileSync(0, 'utf8'),
  write: (output: string) => unknown = (output) => process.stdout.write(output),
  now: () => Date = () => new Date(),
): void {
  try {
    write(
      stopHookResponse(readInput(), undefined, now, (fires, cwd) =>
        recordFires(fires, cwd),
      ),
    );
  } catch {
    // Fail open, including unreadable standard input. Never log conversation contents.
  }
}

function isDirectRun(argv1: string | undefined, moduleUrl: string): boolean {
  if (argv1 === undefined) return false;
  const modulePath = fileURLToPath(moduleUrl);
  const argvPath = argv1.startsWith('file:') ? fileURLToPath(argv1) : argv1;
  try {
    return (
      fs.realpathSync.native(argvPath) === fs.realpathSync.native(modulePath)
    );
  } catch {
    return path.resolve(argvPath) === path.resolve(modulePath);
  }
}

if (isDirectRun(process.argv[1], import.meta.url)) runStopHook();
