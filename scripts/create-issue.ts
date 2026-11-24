import { showDraftEstimate } from './lib/draft-estimate.ts';
import { retryDraft } from './lib/retry-draft.ts';
import { readEvidenceFile } from './lib/read-evidence.ts';
import { confirmedReceipt } from './lib/consultation-replay.ts';
import {
  showPreflight,
  consultationPreflight,
} from './lib/intake-preflight.ts';
import { duplicateStop, recordSent } from './lib/duplicate-guard.ts';
import {
  requestSizeWarning,
  missingChangeFilesWarning,
} from '../shared/src/intake-request-size.ts';
import { consultationSpecText } from '../shared/src/consultation-spec.generated.ts';
// Create an Issue through the running intake service. This is the only way agents (Claude, Codex)
// create an Issue: they never call Gitea's Issue API themselves (AGENTS.md). The text becomes the
// opening message of an intake consultation (POST /api/issues); the intake AI asks only for what is
// still missing and files the Issue itself, which then starts automatically. The provider defaults
// to codex; the agent passes claude only when the user asked for Claude.
import {
  openDesk,
  requestArgs,
  requestText,
  consult,
  checkDraft,
  filedText,
  awaitDraft,
  connectionArgs,
  connection,
  connectionUsage,
  takeHistoryRead,
  closedReferenceCheck,
  draftProject,
  type RequestArgs,
} from './desk-client.ts';

const usage = `使い方: node scripts/create-issue.ts --project <owner/repo> [--provider codex|claude] [--consultation-provider codex|claude]
                                  [--priority high|normal] [--priority-reason <理由>] --tier light|standard|heavy
                                  [--send-id <UUID>] [--implementation-tier light|standard|heavy] [--not-duplicate <理由>] [--over-spec <理由>]
       node scripts/create-issue.ts --spec
                                  [--replace-issues <Gitea番号[,番号...]>]
                                  [--despite-pending <下書きID> --reason <理由>]
                                  [--history-read <読んだ要点>] [--read-evidence-file <JSON>]
                                  [--text <本文> | --file <パス>] [--timeout <秒>]
       node scripts/create-issue.ts --project <owner/repo> --hold --tier light|standard|heavy [--provider codex|claude] [--consultation-provider codex|claude] [--text <本文> | --file <パス>]
       node scripts/create-issue.ts --project <owner/repo> --check-only --text <本文>
       node scripts/create-issue.ts --draft <下書きID> [--tier light|standard|heavy] --text <受付AIの質問への回答>
       node scripts/create-issue.ts --draft <下書きID> --retry
       node scripts/create-issue.ts --draft <下書きID> --submit
       node scripts/create-issue.ts --check <下書きID> [--timeout <秒>]

同じ送り手・同じプロジェクトの似た下書きが検討中・起票待ち・起票中・結果不明なら、相談を作らず終了コード1で止まります。
送り直さず --check <下書きID> で待ち、止まった印は desk-alerts.ts の ai_stopped などで確認します。
意図して別の依頼を作るときだけ --despite-pending <下書きID> --reason <理由> を両方指定します。
理由は前後の空白を除いて1〜500文字で、対象ID・日時とともに記録されます（新しい依頼 --project にだけ使用）。

--hold は相談だけのモードで始めます（新しい依頼 --project にだけ。--draft とは同時に指定できません）。受付AIが起票できると
判断しても自動では起票せず、下書きを確定待ちにします。方針が固まったら --draft <下書きID> --submit で確定して起票します。
--retry は --draft と使い、本文や --tier などを付けずに相談の再試行を送ります。結果不明なら --check で確認します。
--submit は下書きを読んで版（revision）を取り、確定を送ります。自分のトークンで --hold で始めた確定待ちの下書きだけ確定できます。
--submit は --draft とだけ使い、本文・--tier・--hold などとは同時に指定できません。起票できたら共通番号と題名を表示して
終了コード0、受付が断ったら理由を表示して1、結果が未確認なら --check の案内を表示して2です。

--consultation-provider は相談元のAIです。Codexからはcodex、Claudeからはclaudeを指定します。省略はcodexです。
--provider は実装の実行先です。省略するとcodexです。利用者がClaudeを指示したときだけ claude を指定します。
--priority は優先度（high: 割り込み、normal: 通常）で、利用者が明示したときに付けます。省略すると自動（監視のAIが判定）です。
high は docs/scheduling.md『Agent Interrupts』の条件に当てはまるとエージェントが判断したときも付けられます
（パイプラインを止めている問題の修正・利用枠や時間の大きな無駄を止める対策・利用者が急ぎと言ったもの・後続の依頼が進まない土台）。
high には --priority-reason で理由（1〜500文字）を必ず添えます。受付が記録して画面に出します（待っている割り込みの
件数では断りません）。high 以外や --draft には付けられません。
割り込みは、実行中の作業を中断せず、AIの判定や通常依頼／フォローアップの区分より先に着手します。
--tier は受付の相談の難易度（light: 低、standard: 中、heavy: 高）で、新しい依頼（--project）には必ず付けます。
依頼の難しさで依頼元のエージェントが判断して選びます（通常は light、難しければ上げる）。受付は難易度を判定せず、
管理設定の intake.tiers のその難易度で相談します。無いと何も送らず終了コード1で止まります。実装のモデルは変わりません。
--draft と一緒に付けると回答と一緒に送り、次のターンからその難易度で相談します（任意。上げる・下げるのどちらも。同じ難易度・
intake.tiers が無いときは変わりません）。--retry・--submit・--check には付けません。
--implementation-tier は利用者が実装の難易度（light: 低、standard: 中、heavy: 高）を明示したときだけ付けます。
実装のモデル・推論レベルを管理設定（implementation.tiers）のその難易度にします。省略すると受付が起票時に判定します。
--spec は相談が受け付ける範囲（docs/consultation-spec.md）を表示します。接続も送信もしません。
--over-spec は新しい依頼（--project）に付けられる互換用引数です。対象ファイル4件は目安で、超過しても警告して送信を続けます。
--not-duplicate は重複でない理由（1〜500文字）を記録します。新しい依頼（--project）にだけ付けられます。
既存の開いた依頼・相談・TODO、または自分が24時間以内に送った依頼（~/.issue-desk/sent-requests.json）と題名・対象ファイルが
大半重なるとき、この指定が無ければ何も送らず終了コード1で止まります。既存を issue-desk-issue-control Skillの汎用GETで読み、違いを書いて付けます。
候補は受付AIへ手がかりとしても渡され、この指定でも候補の確認は省略しません。
--replace-issues は同じ --project の置き換え元Gitea Issue番号をカンマ区切りで指定し、下書きと分割子へ保存します。
--read-evidence-file は新規相談の読取範囲と事実を本文と別に渡します。形式は docs/read-evidence.md を参照してください。
--history-read は、本文が参照した同じ受付の依頼（共通番号N・owner/repo#N・#N）にマージされずに閉じたものがあるとき、
送る前に止まるのを解く印です。止まると一覧（共通番号・題名・閉じた日・読むコマンド）を表示して何も送らず終了コード1にします。
一覧の依頼を issue-desk-issue-control Skillの汎用GETで読み、読んだ要点（1〜500文字）を付けて送り直します。参照を読めなかったときは警告を出して送ります。
--draft の回答にも使えます（--check・--submit には使えません）。
本文は --text、--file、または標準入力から渡します。受付AIが不足分を質問したら --draft で回答します。
このコマンドは受付AIに相談してIssueを起票します。「TODOとして」と言われたメモはここへ渡さず、
issue-desk-operations Skillの汎用TODO保存手順で受付のTODOに保存してください（受付画面なら「TODOとして保存」）。
受付に届くとすぐ「下書き: <ID>」を表示します。NASの一時キー窓口で結果待ち中に接続が切れた場合は、
送り直さず表示された下書きIDの --check で確認します。それ以外の接続では期限まで問い合わせを続けます。
終了コード: 0 は状態を受け取れた（処理中のまま期限になったときも含み、その結果は未確認）、1 は受付へ送れなかった・受付AIのエラー・起票失敗、
2 は下書きは受付済みで結果が未確認（送り直さず、表示された --check で確認します）。
--send-id は新規相談の送信IDです。応答を失った場合、同じ本文・引数とIDで再実行します。
--check-only は新規相談本文の警告だけを確認します。受付のGETだけを使い、送信・受付AI呼び出しは行いません。
以前読んだ質問に回答するときは、表示された版を --expect-revision <版> で指定してください。省略時の送信直前GETでは、それ以前の変更を検出できません。
--check は何も送らずに下書きの状態と受付AIの返事を読むだけです（--draft は回答を送ります）。
${connectionUsage}`;

// --draft <ID> --submit: 確定待ちの相談（hold）を確定して起票する。下書きを読んで revision を取り、
// POST /api/drafts/<ID>/submit で送る。0 起票した、1 受付が断った、2 結果が未確認（--check で確認）。
async function submitDraft(
  call,
  { draft, timeout }: RequestArgs,
  script,
  hint = '',
  interval = 3000,
) {
  // 版だけを使うので要約で読む。
  const current = await call('GET', `/api/drafts/${draft}?view=summary`);
  let result;
  try {
    result = await call('POST', `/api/drafts/${draft}/submit`, {
      revision: current.revision,
    });
  } catch (e) {
    if (e.status >= 400 && e.status < 500) {
      console.error(
        `受付が断ったため、下書き ${draft} は確定されていません: ${e.message}`,
      );
      return 1;
    }
    return submitUnconfirmed(draft, e.message, script, hint);
  }
  if (result?.accepted === true && result.id === draft) {
    console.log(`受付済み\n下書き: ${draft}`);
    return awaitDraft(call, draft, timeout, true, script, hint, interval, {});
  }
  if (!result?.issue)
    return submitUnconfirmed(draft, `状態: ${result?.status}`, script, hint);
  console.log(`下書き: ${draft}\n\n${await filedText(call, result)}`);
  return 0;
}

function submitUnconfirmed(draft, reason, script, hint) {
  console.log(
    `\n下書き ${draft} の確定の結果は未確認です（${reason}）。\n送り直さず、読むだけの --check で確認してください: node ${script}${hint} --check ${draft}`,
  );
  return 2;
}

async function main() {
  if (process.argv[2] === '--spec') {
    console.log(consultationSpecText);
    return;
  }
  const parsed = connectionArgs(process.argv.slice(2), usage);
  const { argv, historyRead } = takeHistoryRead(parsed.argv, usage);
  const checkOnly = argv.includes('--check-only');
  const args = requestArgs(
    argv.filter((arg) => arg !== '--check-only'),
    usage,
  );
  if (
    checkOnly &&
    (!args.project ||
      args.draft ||
      args.check ||
      args.submit ||
      args.sendId ||
      args.replaceIssues ||
      historyRead !== undefined)
  )
    throw Error('--check-only は新規相談（--project）の本文にだけ指定します。');
  if (historyRead !== undefined && (args.check || args.submit || args.retry))
    throw Error(
      `--history-read は本文を送るとき（--project・--draft の回答）にだけ指定します。\n${usage}`,
    );
  // The intake no longer judges the consultation tier; the requester always chooses it for a new request.
  if (args.project && !checkOnly && !args.tier)
    throw Error(
      `新しい依頼（--project）には --tier light|standard|heavy を必ず付けてください。依頼の難しさで判断します（通常は light、難しければ上げる）。\n${usage}`,
    );
  if (args.project && !args.provider) args.provider = 'codex';
  if (args.project && !['codex', 'claude'].includes(args.provider))
    throw Error(
      `--provider codex|claude を指定してください（省略するとcodex）。\n${usage}`,
    );
  if (args.draft && args.provider)
    throw Error(
      `--provider は新しい依頼（--project）にだけ指定します。\n${usage}`,
    );
  if (
    args.consultationProvider !== undefined &&
    (args.draft || !['codex', 'claude'].includes(args.consultationProvider))
  )
    throw Error(
      '--consultation-provider は新しい相談で codex または claude を指定します。',
    );
  if (checkOnly && args.readEvidenceFile !== undefined)
    throw Error('--read-evidence-file は --check-only と併用できません。');
  if (args.readEvidenceFile !== undefined)
    args.readEvidence = readEvidenceFile(args.readEvidenceFile, args.project);
  const conn = connection(parsed);
  // --check は送らずに読むだけなので本文を取らない。
  const text = args.check || args.submit || args.retry ? '' : requestText(args);
  if (!args.check && !args.submit && !args.retry && !text)
    throw Error(
      `依頼内容が空です。--text、--file、または標準入力で渡してください。\n${usage}`,
    );
  const warning =
    !args.check && !args.submit && !args.retry
      ? requestSizeWarning(text)
      : null;
  if (warning) console.error(warning);
  const missing =
    args.project && !checkOnly ? missingChangeFilesWarning(text) : null;
  if (missing) console.error(missing);
  const { call, close, config } = await openDesk(conn);
  try {
    let warningTypes = [];
    let preflightRows = [];
    if (!args.check && !args.submit && !args.retry) {
      if (args.project && !checkOnly)
        showPreflight({ text, project: args.project, config });
      warningTypes = await consultationPreflight({
        text,
        project: args.project,
        call,
        draft: !!args.draft,
        onCandidates: ({ rows }) => (preflightRows = rows),
      });
    }
    // 二重起票: 既存の開いた依頼・相談・TODOや自分が直近に送った依頼と大半重なるときは、--not-duplicate が無ければ送らない。
    const receipt = args.sendId
      ? await confirmedReceipt(call, args.sendId)
      : null;
    if (receipt)
      console.error(
        {
          confirmed: '送信IDの受領確認: 受領確認済み。',
          missing: '送信IDの受領確認: 未受領（見つかりません）。',
          mismatch: '送信IDの受領確認: 受領の内容が一致しません。',
          failed: '送信IDの受領確認: 確認失敗（受領を確認できません）。',
        }[receipt],
      );
    const replay = receipt === 'confirmed';
    const duplicate =
      !replay && args.project && !args.draft && !args.check && !args.submit
        ? duplicateStop({
            text,
            project: args.project,
            rows: preflightRows,
            notDuplicate: args.notDuplicate,
          })
        : null;
    if (duplicate) {
      if (checkOnly) warningTypes.push('duplicate_blocked');
      else {
        console.error(duplicate);
        if (args.sendId && receipt !== 'missing')
          console.error(
            `受領を確認してください: node scripts/create-issue.ts${conn?.hint || ''} --check ${args.sendId}`,
          );
        process.exitCode = 1;
        return;
      }
    }
    if (checkOnly) {
      if (warning) warningTypes.push('over_spec');
      console.log(JSON.stringify({ event: 'intake-preflight', warningTypes }));
      return;
    }
    if (args.retry)
      process.exitCode = await retryDraft(
        call,
        args,
        'scripts/create-issue.ts',
        conn?.hint,
        undefined,
        { base: conn?.base, publicUrl: config?.intake?.publicUrl },
      );
    else if (args.submit)
      process.exitCode = await submitDraft(
        call,
        args,
        'scripts/create-issue.ts',
        conn?.hint,
      );
    else if (args.check)
      process.exitCode = await checkDraft(
        call,
        args,
        'scripts/create-issue.ts',
        conn?.hint,
        undefined,
        {
          base: conn?.base,
          publicUrl: config?.intake?.publicUrl,
        },
      );
    else if (
      !replay &&
      (
        await closedReferenceCheck({
          call,
          text,
          project:
            args.project ??
            (args.draft ? await draftProject(call, args.draft) : undefined),
          historyRead,
        })
      ).stop
    )
      process.exitCode = 1;
    else {
      console.log(JSON.stringify({ event: 'intake-preflight', warningTypes }));
      const estimateRevision = args.draft
        ? await showDraftEstimate(call, args.draft, text)
        : undefined;
      process.exitCode = await consult(
        call,
        {
          ...args,
          text,
          expectRevision: args.expectRevision ?? estimateRevision,
        },
        'scripts/create-issue.ts',
        conn?.hint,
        undefined,
        {
          base: conn?.base,
          publicUrl: config?.intake?.publicUrl,
        },
      );
      if (args.project && process.exitCode !== 1)
        recordSent(text, args.project);
    }
  } finally {
    close();
  }
}

main().catch((e) => {
  console.error(e.message);
  process.exitCode =
    e?.nasHandoff && process.env.ISSUE_DESK_NAS_HANDOFF === 'read' ? 75 : 1;
});
