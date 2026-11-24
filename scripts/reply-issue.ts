// Issueへの返信（実装AIの質問への回答・レビュー保留への修正指示・追加指示）を、動いている受付の POST /api/comment
// （action 'reply'）で送る（Issue #1121）。受付画面のIssue画面の「回答して再開」「修正指示を送る」「追加指示を送る」と
// 同じ経路・同じ判定（server/src/intake-actions.mjs の actions）。AIはGiteaへ直接コメントを書かずにこれを使う。
// - A9 MAX上で接続の指定が無ければ一時キー。--url/--token-file（または環境変数）と、受付が別のPCにある設定
//   （config.local.json の intake.remoteUrl）では、PC別トークン（scripts/desk-client.ts の connection・openDesk）。
//   受付がトークンからの返信を受けるのは、利用者が受付画面でそのトークンに運用の代行を許したときだけ（Issue #1120）。
// - 送信IDは送る前に決めて表示する。応答を失ったら --id <送信ID> で同じ送信として照合する。
// - 本文は8000 UTF-16コード単位まで。先頭が /codex の本文は監視がコマンドとして読むので、送る前に断る。画像は付けられない。
import fs from 'node:fs';
import { randomUUID, createHash } from 'node:crypto';
import { redactAll } from '../shared/src/io-redact.ts';
import {
  applyChoiceApproval,
  approvalInstructionAt,
  approvalStatementMinLength,
  choiceApprovalGuidance,
  instructionAtGuidance,
} from '../shared/src/approval-statement.ts';
import {
  openDesk,
  connectionArgs,
  connection,
  connectionUsage,
  configuredRemote,
} from './desk-client.ts';
import { invokedAs } from './lib/invoked.ts';

const usage = `使い方: node scripts/reply-issue.ts --project <owner/repo> --number <Issue番号> (--text <本文> | --file <ファイル>) [--id <送信ID>] [--allow-worker-commands <理由>] [--over-limit-statement <利用者の承認発言> --instruction-at <利用者が指示した日時> | --implementation-limit-retry]

準備: --project <owner/repo> --number <Issue番号> --desk <共通番号> --prepare --prepared <準備ファイル>
準備後の送信: 通常の送信引数に --prepared <準備ファイル> を追加します。

実装AIの質問への回答（実装の回答待ち）・修正指示（レビュー保留）・追加指示を、受付画面のIssue画面と同じ経路
（受付の POST /api/comment、action: reply）で送ります。--number はGiteaのIssue番号です（受付の共通番号ではありません）。
本文は --text か --file のどちらか1つで、8000 UTF-16コード単位まで。先頭が /codex のコマンドの本文は送りません（再試行は
issue-desk-issue-control Skillの再試行・停止・再開手順）。画像は付けられません（画像付きの返信は受付画面から送ります）。
操作指示の検査が誤検出した場合のみ --allow-worker-commands に理由を指定して解除できます。権限や承認条件は変わりません。
送る前に送信IDを表示します。応答が無かったときは、同じ引数に --id <送信ID> を付けて送り直してください
（受付が同じ送信として照合し、重ねて投稿しません）。
A9 MAX上では一時キーで送ります。他のPCからと、受付が別のPCにある設定（config.local.json の intake.remoteUrl）では、
受付画面で運用の代行を許したPC別トークンで送ります（許していないトークンは受付が断ります）。
上限超過のレビュー保留、実装回数の上限で止まった依頼、使用量の上限を超えた質問待ち（waiting）の依頼には --over-limit-statement に対象の共通番号かGitea番号を数字で含む利用者の発言そのままを指定できます。${approvalStatementMinLength}文字以上で、「承認します」などの定型語だけは不可です。
使用量の上限を超えた質問待ちの依頼への承認のない返信は受付が理由を示して断ります。
承認を聞くときは、質問文に対象番号と操作を書きます。記号（「A」など）の回答で足ります。
${choiceApprovalGuidance}
--over-limit-statement（--approval-question と --approval-answer を含む）には --instruction-at が必須です。${instructionAtGuidance}
期限付きプロジェクト許可は --implementation-limit-retry を指定し、本文に原因・直し方を記入します。
両指定は併用できず、どちらも運用の代行を許したトークン接続が必須です。
${connectionUsage}`;

// トークン接続で受付が403で断ったとき（運用の代行を許していないトークン）の案内。トークンの値は含めない。
const forbiddenHint =
  '受付がこのトークンでの返信を断りました（403）。受付画面でこのトークンに運用の代行を許すか、受付画面のIssue画面から送ってください。';
const maxText = 8000;
const sendId = /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/;
// 返信の種類（受付の actions の answer・fix・reply と同じ状態の分け方。表示だけで、送る action は reply）。
const kinds = { waiting: '回答', review_held: '修正指示' };
const replyKind = (status) => kinds[status] || '追加指示';
const shown = { queued: '受付済み（次に空いたPCが取ります）' };

export function parseArgs(argv: string[]) {
  // 値の名前は引数のまま（over-limit-statement など）。applyChoiceApproval が同じ形で読み書きする。
  const out: Record<string, any> = {};
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--prepare' && !out.prepare) {
      out.prepare = true;
      continue;
    }
    if (
      argv[i] === '--implementation-limit-retry' &&
      !out.implementationLimitRetry
    ) {
      out.implementationLimitRetry = true;
      continue;
    }
    const name = argv[i].match(
      /^--(project|number|text|file|id|over-limit-statement|approval-question|approval-answer|instruction-at|allow-worker-commands|prepared|desk)$/,
    )?.[1];
    if (!name || out[name] !== undefined)
      throw Error(`引数を確認してください: ${argv[i]}\n${usage}`);
    const value = argv[++i];
    if (value === undefined || value.startsWith('--'))
      throw Error(`--${name} の値がありません。\n${usage}`);
    out[name] = value;
  }
  applyChoiceApproval(out, 'over-limit-statement');
  // 承認発言には利用者がチャットで指示した日時を必須にし、発言の無い日時は断る（受付も同じ判定）。
  if (out['over-limit-statement'] !== undefined)
    out['instruction-at'] = approvalInstructionAt(out['instruction-at']);
  else if (out['instruction-at'] !== undefined)
    throw Error(
      '--instruction-at は --over-limit-statement（または --approval-question）と一緒に指定してください。',
    );
  if (
    out['allow-worker-commands'] !== undefined &&
    !out['allow-worker-commands'].trim()
  )
    throw Error('--allow-worker-commands には空でない理由が必要です。');
  if (!out.project || !/^[1-9]\d*$/.test(out.number || '')) throw Error(usage);
  if (out.text !== undefined && out.file !== undefined)
    throw Error(`--text と --file は同時に指定できません。\n${usage}`);
  if (!out.prepare && out.text === undefined && out.file === undefined)
    throw Error(`--text か --file で本文を指定してください。\n${usage}`);
  if (
    out.prepare &&
    (!out.prepared ||
      !/^[1-9]\d*$/.test(out.desk || '') ||
      out.text !== undefined ||
      out.file !== undefined ||
      out.id)
  )
    throw Error(
      '準備には --desk <共通番号> と --prepared <保存先> を指定し、本文・送信IDは指定しないでください。',
    );
  if (out.id !== undefined && !sendId.test(out.id))
    throw Error(`--id には送り直す返信の送信IDを指定してください。\n${usage}`);
  if (out.implementationLimitRetry && out['over-limit-statement'] !== undefined)
    throw Error('期限付き上限許可と個別承認は併用できません。');
  return out;
}

const digest = (value) =>
  createHash('sha256').update(JSON.stringify(value)).digest('hex');
const safe = (value) =>
  redactAll(value)
    .replace(
      /\b((?:token|password|secret|api[_-]?key)\s*[=:]\s*["']?)[^\s"',;]+/gi,
      '$1[REDACTED]',
    )
    .replace(/\b(?:Bearer|Basic)\s+[^\s"']+/gi, '[REDACTED]');

function snapshot(d, args) {
  if (
    !d ||
    d.project !== args.project ||
    Number(d.number) !== Number(args.number) ||
    !Number.isSafeInteger(Number(d.desk)) ||
    Number(d.desk) <= 0 ||
    (args.desk && Number(d.desk) !== Number(args.desk))
  )
    throw Error('対象または共通番号が一致しません。再準備してください。');
  if (typeof d.body !== 'string' || !Array.isArray(d.comments))
    throw Error('未確認: Issue本文またはコメントが欠落しています。');
  const comments = d.comments
    .filter((c) => c.kind !== 'log')
    .map((c) => {
      if (
        !c ||
        (typeof c.id !== 'number' && typeof c.id !== 'string') ||
        !c.id ||
        typeof c.body !== 'string' ||
        typeof c.createdAt !== 'string' ||
        !c.createdAt ||
        typeof c.updatedAt !== 'string' ||
        !c.updatedAt
      )
        throw Error('未確認: コメントのID・日時・本文が欠落しています。');
      return {
        id: c.id,
        createdAt: c.createdAt,
        updatedAt: c.updatedAt,
        author: c.author ?? '',
        kind: c.kind ?? '',
        body: c.body,
      };
    })
    .sort(
      (a, b) =>
        a.createdAt.localeCompare(b.createdAt) ||
        String(a.id).localeCompare(String(b.id)),
    );
  return {
    project: d.project,
    number: Number(d.number),
    desk: Number(d.desk),
    body: d.body,
    comments,
  };
}

function displayed(s) {
  const sections = [
    ['Issue本文', s.body],
    ...s.comments.map((c) => [
      `コメント ${c.id} (${c.createdAt}, 更新 ${c.updatedAt}, 出典 ${c.author || '不明'})`,
      c.body,
    ]),
  ];
  const hidden = sections
    .filter(([, body]) => safe(body) !== body)
    .map(([name]) => name);
  const output = sections
    .map(([name, body]) => `${safe(name)}\n${safe(body)}`)
    .join('\n\n');
  if (output.length > 1000000)
    throw Error('未確認: 資料が出力上限を超えています。');
  return { output, hidden };
}

function loadPreparation(file, args) {
  let p;
  try {
    p = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch {
    throw Error('準備データを読めません。再準備してください。');
  }
  if (
    !p ||
    p.version !== 1 ||
    p.project !== args.project ||
    p.number !== Number(args.number) ||
    !Number.isSafeInteger(p.desk) ||
    p.desk < 1 ||
    !/^[a-f0-9]{64}$/.test(p.fingerprint) ||
    (args.desk && p.desk !== Number(args.desk)) ||
    (p.attempt &&
      (!sendId.test(p.attempt.id) || !/^[a-f0-9]{64}$/.test(p.attempt.payload)))
  )
    throw Error('不正な準備データまたは対象違いです。再準備してください。');
  return p;
}

function savePreparation(file, p) {
  fs.writeFileSync(file, JSON.stringify(p, null, 2) + '\n', { mode: 0o600 });
}

async function prepare(call, args, log) {
  let d;
  try {
    d = await call(
      'GET',
      `/api/issue?${new URLSearchParams({ project: args.project, number: args.number })}`,
    );
  } catch {
    throw Error('準備の詳細取得に失敗しました。再準備してください。');
  }
  const s = snapshot(d, args);
  const { output, hidden } = displayed(s);
  log(
    `対象: ${safe(s.project)}#${s.number} 共通番号 #${s.desk}\n指紋: ${digest(s)}\n${output}`,
  );
  if (hidden.length)
    throw Error(`未確認: 秘匿された ${hidden.join('、')} を照合できません。`);
  savePreparation(args.prepared, {
    version: 1,
    project: s.project,
    number: s.number,
    desk: s.desk,
    fingerprint: digest(s),
  });
  return 0;
}

// 既知の操作と実行要求を照合する補助検査。自然言語の全表現を判定するものではない。
function workerCommand(text) {
  const command =
    /\b(?:git\s+(?:show|log|diff|cat-file|fetch)\b|curl\b|wget\b|gh\s+api\b)/gi;
  const lines = text.split(/\n/);
  for (const line of lines) {
    if (/^\s*```/.test(line)) continue;
    for (const part of line.split(
      /(?<=[。！？、,])|(?<=(?:しないで|使わないで|読まないで|禁止ですが|禁止して))(?=\s*\S)/,
    )) {
      command.lastIndex = 0;
      const match = command.exec(part);
      if (!match) continue;
      const prohibited =
        /(?:実行|使用|利用|取得|参照|確認|読)(?:しない|しません|するな|しなくて|できない|めない|まない)|禁止|使わない|使えない|読めない/.test(
          part,
        );
      const requestPattern =
        /(?:実行|取得|参照|確認|表示|調査|使用|利用|実施)(?:して(?:ください|下さい|ほしい|欲しい|もら|頂|いただ|(?=$|[。！？、,\s]))|する(?:よう|こと|必要)|を(?:お願い|行って)|せよ|(?=$|[。！？、,\s]))|(?:調べ|読んで|使って|試して|叩いて|走らせて)(?:ください|下さい|ほしい|欲しい|(?=$|[。！？、,\s]))|(?:してください|して下さい|せよ)|\b(?:run|execute)\b/i;
      const standalone =
        !/[\u3040-\u30ff\u3400-\u9fff]/.test(part) &&
        /^\s*(?:[$>]\s*)?`?\s*(?:git\s+(?:show|log|diff|cat-file|fetch)\b|curl\b|wget\b|gh\s+api\b)/i.test(
          part,
        );
      if (!prohibited && (requestPattern.test(part) || standalone))
        return /^git\b/i.test(match[0]) ? '履歴管理操作' : '外部取得';
    }
  }
  return null;
}

// 送る本文（前後の空白を除く）。空・長すぎる・先頭が /codex（監視がコマンドとして読む。client/src/core.mjs の command と同じ
// 読み方）のときは送らずに断る。readFile はテストで差し替えるためのもの。
function replyText(args, readFile = (file) => fs.readFileSync(file, 'utf8')) {
  let text = args.text;
  if (args.file !== undefined) {
    try {
      text = readFile(args.file);
    } catch (e) {
      throw Error(
        `本文のファイルを読めません（${args.file}）: ${e.code || '読み取りエラー'}`,
      );
    }
  }
  text = String(text ?? '')
    .replace(/^\uFEFF/, '')
    .trim();
  if (!text) throw Error('返信の本文が空です。');
  if (text.length > maxText)
    throw Error(
      `本文が長すぎます（${text.length} UTF-16コード単位）。${maxText} UTF-16コード単位以内にしてください。`,
    );
  if (/^\/codex\b/i.test(text))
    throw Error(
      '先頭が /codex のコマンドの本文は送れません（監視がコマンドとして読みます）。再試行は issue-desk-issue-control Skillの再試行・停止・再開手順から行ってください。',
    );
  const operation = workerCommand(text);
  if (
    operation &&
    !(
      typeof args['allow-worker-commands'] === 'string' &&
      args['allow-worker-commands'].trim()
    )
  )
    throw Error(
      `${operation}の実行指示を検出しました。実装環境で利用できない履歴管理操作や許可されていない外部取得を要求しないでください。必要な差分・資料本文は依頼者側またはコントローラーが確認して貼り、8000 UTF-16コード単位以内に整理してください。分けて送る場合も各本文に必要な根拠を含めてください。`,
    );
  return text;
}

// 返信を送り、結果を表示する。終了コード: 0 は送った（または照合で送信済みを確認した）。失敗は例外。
// remote: トークン接続（403を運用の代行の案内にする）。
export async function replyIssue(
  call,
  args,
  text,
  log = console.log,
  { remote = false } = {},
) {
  if (
    (args['over-limit-statement'] !== undefined ||
      args.implementationLimitRetry) &&
    !remote
  )
    throw Error('上限超過の承認はトークン接続が必要です。');
  if (args['over-limit-statement'] !== undefined)
    approvalInstructionAt(args['instruction-at']);
  const id = args.id || randomUUID();
  log(`送信ID: ${id}`);
  const recovery = `\n送信ID: ${id}\n応答を受け取れなかったときは、GET /api/comment?id=${id} で受理と結果を確認し、未受理の場合だけ同じ引数と --id ${id} で再送してください。`;
  const query = new URLSearchParams({
    project: args.project,
    number: args.number,
  });
  const target = (d) =>
    `${d.project || args.project}#${d.number || args.number}${d.desk ? `（受付の共通番号 #${d.desk}）` : ''} ${d.title ?? ''}`.trim();
  // 準備済みの照合（--prepared）は本文・コメントの指紋を取るので全文を読む。それ以外で使うのは
  // project・number・desk・title・status・url だけなので、要約（view=summary）を読む。
  const summaryQuery = new URLSearchParams({
    project: args.project,
    number: args.number,
    view: 'summary',
  });
  let before;
  try {
    before = await call(
      'GET',
      `/api/issue?${args.prepared ? query : summaryQuery}`,
    );
  } catch (e) {
    if (args.prepared)
      throw Error('送信前の詳細取得に失敗しました。再準備してください。');
    throw e;
  }
  if (args.prepared) {
    const p = loadPreparation(args.prepared, args);
    const payload = digest({
      project: args.project,
      number: Number(args.number),
      text,
      statement: args['over-limit-statement'] ?? null,
      ...(args['instruction-at']
        ? { instructionAt: args['instruction-at'] }
        : {}),
      implementationLimitRetry: !!args.implementationLimitRetry,
    });
    const s = snapshot(before, { ...args, desk: String(p.desk) });
    if (p.attempt) {
      if (!args.id || p.attempt.id !== id || p.attempt.payload !== payload)
        throw Error(
          '送信済み照合のID・本文・承認指定が一致しません。新規送信には再準備してください。',
        );
    } else {
      if (displayed(s).hidden.length)
        throw Error(
          '未確認: 秘匿された資料を照合できません。再準備してください。',
        );
      if (digest(s) !== p.fingerprint)
        throw Error(
          'Issue本文またはコメントが更新されました。再準備してください。',
        );
      p.attempt = { id, payload };
      savePreparation(args.prepared, p);
    }
  }
  try {
    const receipt = await call('POST', '/api/comment', {
      async: true,
      id,
      project: args.project,
      number: Number(args.number),
      action: 'reply',
      text,
      ...(args['over-limit-statement'] !== undefined
        ? {
            overLimitStatement: args['over-limit-statement'],
            instructionAt: args['instruction-at'],
          }
        : {}),
      ...(args.implementationLimitRetry
        ? { implementationLimitRetry: true }
        : {}),
    });
    if (receipt?.accepted === true) {
      if (
        receipt.id !== id ||
        receipt.project !== args.project ||
        receipt.number !== Number(args.number)
      )
        throw Error('返信の受理対象が一致しません。');
      log(`受付済み: 送信ID ${id}`);
      const deadline = Date.now() + 900000;
      let state = receipt;
      while (state.status === 'queued' && Date.now() < deadline) {
        await new Promise((resolve) => setTimeout(resolve, 3000));
        state = await call(
          'GET',
          `/api/comment?${new URLSearchParams({ id })}`,
        );
        if (
          state.id !== id ||
          state.project !== args.project ||
          state.number !== Number(args.number)
        )
          throw Error('返信の照会対象が一致しません。');
      }
      if (state.status !== 'done' || !state.result)
        throw Error(
          state.error ||
            `返信結果は未確認です。送信ID ${id} の GET /api/comment?id=${id} で確認してください。`,
        );
    }
  } catch (e) {
    // 許可の無いトークンは受付が何も受け取らずに断る（送り直しても同じ）ので、送り直しではなく許可か画面を案内する。
    if (remote && e.status === 403)
      throw Error(
        `${args.prepared ? '受付が送信を拒否しました。' : e.message}\n${forbiddenHint}`,
      );
    throw Error(
      `${args.prepared ? '受付への送信結果を確認できません。' : e.message}${recovery}`,
    );
  }
  log(
    `${replyKind(before.status)}を送りました: ${args.prepared ? safe(target(before)) : target(before)}`,
  );
  // 送信は済んでいるので、この後の読み直しに失敗しても送り直させない（0で終わり、確かめ方を示す）。
  let after;
  try {
    after = await call('GET', `/api/issue?${summaryQuery}`);
  } catch (e) {
    log(
      `送信後の状態を読めませんでした: ${args.prepared ? '読取失敗' : e.message}`,
    );
    log(
      `状態は issue-desk-issue-control Skillの汎用GETか受付画面で確かめてください。送り直しは不要です。`,
    );
    return 0;
  }
  log(`状態: ${shown[after.status] || after.status}`);
  if (after.url && !args.prepared) log(after.url);
  return 0;
}

// コマンドの本体。接続先は他のスクリプトと同じ connection（引数・環境変数・intake.remoteUrl の設定・一時キーの順）。
// env・configured・open・log・readFile はテストで差し替えるためのもの。
export async function runCommand(
  argv,
  {
    env = process.env,
    configured = configuredRemote,
    open = openDesk,
    log = console.log,
    readFile,
  }: {
    env?: NodeJS.ProcessEnv;
    configured?: () => any;
    open?: typeof openDesk;
    log?: (line: string) => void;
    readFile?: (file: string) => string;
  } = {},
) {
  const parsed = connectionArgs(argv, usage);
  const args = parseArgs(parsed.argv);
  // 本文の不備は受付に触れる前に断る。
  const text = args.prepare ? null : replyText(args, readFile);
  const conn = connection(parsed, env, configured);
  const { call, close } = await open(conn);
  try {
    return args.prepare
      ? await prepare(call, args, log)
      : await replyIssue(call, args, text, log, { remote: !!conn });
  } finally {
    close();
  }
}

// コマンドとして実行したときだけ動く（テストは runCommand などを読み込んで差し替えた受付に対して動かす）。
// 実体のパスで比べる。
if (invokedAs(import.meta.url))
  runCommand(process.argv.slice(2)).then(
    (code) => (process.exitCode = code),
    (e) => {
      console.error(e.message);
      process.exitCode = 1;
    },
  );
