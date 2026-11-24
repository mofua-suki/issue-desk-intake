import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs';
import {
  directPRActive,
  filingQuestion,
  inspectTurn,
  reportProblem,
  scopeMention,
  stopHookResponse,
} from '../scripts/chat-stop-hook.ts';

const turn = (user: string, reply: string) =>
  [
    JSON.stringify({ type: 'user', message: { content: user } }),
    JSON.stringify({
      type: 'assistant',
      message: { content: [{ type: 'text', text: reply }] },
    }),
  ].join('\n');

test('担当外に触れた返答は止める', () => {
  for (const reply of [
    '別のセッションが同じ修正を入れていました。',
    'ついでに要対応も見ました。',
    '依頼の範囲外ですが、受付の入力上限に問題があります。',
    '【質問分類：新しい権限】この修正を直接PRで直してよいですか。',
  ])
    assert.ok(inspectTurn({}, turn('APIを調査して', reply)).block, reply);
});

test('依頼の範囲の返答は止めない', () => {
  for (const reply of [
    '調査の結果を報告します。',
    '`別件` という語を検索しました。',
    '> 別のセッションの引用です\n完了しました。',
  ])
    assert.equal(
      inspectTurn({}, turn('APIを調査して', reply)).block,
      null,
      reply,
    );
});

test('利用者がその回に使った語は対象にしない', () => {
  assert.equal(
    scopeMention(
      '別のセッションは2件あります。',
      'ほかに別のセッションは動いている？',
    ),
    null,
  );
  assert.equal(scopeMention('直接PRで直してよいですか。', 'PRで直して'), null);
});

test('報告だけの直接PRの言及は質問と扱わない', () => {
  assert.equal(scopeMention('直接PRをマージしました。', 'APIを調査して'), null);
});

const withCall = (reply: string, command: string, error = false) =>
  [
    { type: 'user', message: { content: '調査して' } },
    {
      type: 'assistant',
      message: {
        content: [
          { type: 'text', text: reply },
          { type: 'tool_use', id: 'call', name: 'Bash', input: { command } },
        ],
      },
    },
    {
      type: 'user',
      message: {
        content: [
          {
            type: 'tool_result',
            tool_use_id: 'call',
            is_error: error,
            content: '完了',
          },
        ],
      },
    },
  ]
    .map((entry) => JSON.stringify(entry))
    .join('\n');

const workThenReply = (reply: string) =>
  [
    { type: 'user', message: { content: '直して' } },
    {
      type: 'assistant',
      message: {
        content: [
          {
            type: 'tool_use',
            id: 'edit',
            name: 'Edit',
            input: { file_path: '/home/u/a' },
          },
        ],
      },
    },
    {
      type: 'user',
      message: {
        content: [
          { type: 'tool_result', tool_use_id: 'edit', content: '完了' },
        ],
      },
    },
    {
      type: 'assistant',
      message: { content: [{ type: 'text', text: reply }] },
    },
  ]
    .map((entry) => JSON.stringify(entry))
    .join('\n');

test('停止後は有効な質問を再掲せず不足する判断の記録だけを足せる', () => {
  const question =
    '【質問分類：新しい権限】実行しますか。\nA: 実行する\nB: 実行しない';
  const original = workThenReply(question);
  const reason = inspectTurn({}, original).block?.reason ?? '';
  assert.match(reason, /判断の記録/);
  assert.match(reason, /質問が有効なままなら再掲しない/);
  assert.match(
    reason,
    /質問の内容を変えたとき、または指摘が質問自体に向いたときだけ/,
  );
  const addition = '判断の記録: なし';
  assert.equal(reportProblem(addition), null);
  const continued = `${original}\n${JSON.stringify({ type: 'assistant', message: { content: [{ type: 'text', text: addition }] } })}`;
  assert.equal(inspectTurn({}, continued).block, null);
});

const assistantReply = (text: string) =>
  JSON.stringify({
    type: 'assistant',
    message: { content: [{ type: 'text', text }] },
  });
const responseFor = (transcript: string, input: object = {}) =>
  JSON.parse(
    stopHookResponse(
      JSON.stringify({ transcript_path: '/transcript', ...input }),
      () => transcript,
    ),
  );
const shownQuestion =
  '【質問分類：新しい権限】実行しますか。\nA: 実行する\nB: 実行しない';

test('会話記録の入口で表示済み質問への追記と不要な再掲を区別する', () => {
  const original = workThenReply(shownQuestion);
  const active = { stop_hook_active: true };
  assert.match(responseFor(original).reason, /判断の記録/);
  assert.equal(
    responseFor(`${original}\n${assistantReply('判断の記録: なし')}`, active)
      .decision,
    undefined,
  );
  const repeated = `${original}\n${assistantReply(`判断の記録: なし\n${shownQuestion}`)}`;
  assert.match(responseFor(repeated, active).reason, /同じ質問を繰り返さない/);
  assert.equal(
    responseFor(`${repeated}\n${assistantReply(shownQuestion)}`, active)
      .decision,
    undefined,
  );
  assert.match(
    responseFor(
      `${original}\n${assistantReply('確認しました。')}\n${assistantReply(`判断の記録: なし\n${shownQuestion}`)}`,
    ).reason,
    /同じ質問を繰り返さない/,
  );
  const changed = shownQuestion.replace('実行しますか', '明日実行しますか');
  assert.equal(
    responseFor(
      `${original}\n${assistantReply(`判断の記録: なし\n${changed}`)}`,
      active,
    ).decision,
    undefined,
  );
  const changedChoice = shownQuestion.replace(
    'B: 実行しない',
    'B: 明日実行する',
  );
  assert.equal(
    responseFor(
      `${original}\n${assistantReply(`判断の記録: なし\n${changedChoice}`)}`,
      active,
    ).decision,
    undefined,
  );
  const badPosition = workThenReply(`${shownQuestion}\n報告です。`);
  assert.match(responseFor(badPosition).reason, /質問が返答の途中/);
  assert.equal(
    responseFor(
      `${badPosition}\n${assistantReply(`判断の記録: なし\n${shownQuestion}`)}`,
      active,
    ).decision,
    undefined,
  );
});

test('再入防止は二度目の続きと読取失敗を許可する', () => {
  const original = workThenReply(shownQuestion);
  const retry = `${original}\n${assistantReply(shownQuestion)}`;
  assert.match(
    responseFor(retry, { stop_hook_active: true }).reason,
    /同じ質問を繰り返さない/,
  );
  assert.equal(
    responseFor(`${retry}\n${assistantReply(shownQuestion)}`, {
      stop_hook_active: true,
    }).decision,
    undefined,
  );
  const result = JSON.parse(
    stopHookResponse(
      JSON.stringify({
        transcript_path: '/transcript',
        stop_hook_active: true,
      }),
      () => {
        throw new Error();
      },
    ),
  );
  assert.equal(result.decision, undefined);
});

test('新しい利用者回と引用やコード内の質問は表示済み質問と混同しない', () => {
  const original = workThenReply(shownQuestion);
  const next = `${original}\n${turn('もう一度確認して', shownQuestion)}`;
  assert.equal(responseFor(next).decision, undefined);
  for (const example of [
    `> ${shownQuestion.replace(/\n/g, '\n> ')}`,
    `\`\`\`\n${shownQuestion}\n\`\`\``,
    `\`${shownQuestion.replace(/\n/g, ' ')}\``,
  ]) {
    const transcript = `${workThenReply(example)}\n${assistantReply(`判断の記録: なし\n${shownQuestion}`)}`;
    assert.equal(responseFor(transcript).decision, undefined, example);
  }
  assert.equal(
    responseFor(
      `${original}\n${JSON.stringify({ type: 'assistant', message: { content: 42 } })}`,
    ).decision,
    undefined,
  );
  assert.equal(
    responseFor(
      `${original}\n${JSON.stringify({ type: 'assistant', message: { content: 42 } })}`,
      { stop_hook_active: true },
    ).decision,
    undefined,
  );
  assert.equal(
    responseFor(next, { stop_hook_active: true }).decision,
    undefined,
  );
  for (const example of [
    `> ${shownQuestion.replace(/\n/g, '\n> ')}`,
    `\`\`\`\n${shownQuestion}\n\`\`\``,
  ]) {
    assert.equal(
      responseFor(
        `${workThenReply(example)}\n${assistantReply(shownQuestion)}`,
        { stop_hook_active: true },
      ).decision,
      undefined,
    );
  }
});

test('配布される正本と補足資料は再掲の例外と判断の記録の追記を揃える', () => {
  for (const file of [
    'chat-instructions.md',
    'docs/chat-instructions.md',
    'skills/user-interaction/SKILL.md',
  ]) {
    const text = fs.readFileSync(
      new URL(`../${file}`, import.meta.url),
      'utf8',
    );
    assert.match(text, /表示済みの質問が有効なままなら再掲せず/, file);
    assert.match(
      text,
      /質問の内容を変えたときか指摘が質問自体に向いたときだけ/,
      file,
    );
    assert.match(text, /判断の記録が要る回は、その行だけを足/, file);
  }
});

test('通常の回は質問の後の報告と判断の記録の欠落を止める', () => {
  const question =
    '【質問分類：新しい権限】実行しますか。\nA: 実行する\nB: 実行しない';
  assert.match(
    inspectTurn({}, turn('確認して', `${question}\n報告です。`)).block
      ?.reason ?? '',
    /質問が返答の途中/,
  );
  assert.match(
    inspectTurn({}, workThenReply(question)).block?.reason ?? '',
    /判断の記録/,
  );
});

test('謝罪と再発防止の根拠を検査する', () => {
  for (const reply of [
    '私の判断ミスです。',
    '私も確かめずに報告しました。共通番号123は対応中です。',
    'ミスでした。',
    '誤りでした。',
    '報告の誤りです。',
    '見落としました。',
  ])
    assert.ok(inspectTurn({}, turn('調査して', reply)).block, reply);
  assert.equal(
    inspectTurn(
      {},
      turn(
        '調査して',
        '私の判断ミスです。再発防止は共通番号123に依頼しました。',
      ),
    ).block,
    null,
  );
  for (const reply of [
    '> 私の判断ミスです。\n完了しました。',
    '```\n私の判断ミスです。\n```',
    '`私の判断ミスです。` を引用しました。',
  ])
    assert.equal(inspectTurn({}, turn('調査して', reply)).block, null, reply);
  assert.equal(
    inspectTurn({}, turn('判断ミスではない？', '判断ミスではありません。'))
      .block,
    null,
  );
});

test('再発防止を送った成功済みの呼び出しだけを照合する', () => {
  const reply = '私の判断ミスです。判断の記録: なし';
  assert.equal(
    inspectTurn(
      {},
      withCall(
        reply,
        "node scripts/reply-issue.ts --text '再発防止の検査を追加' ",
      ),
    ).block,
    null,
  );
  assert.ok(
    inspectTurn(
      {},
      withCall(reply, "node scripts/reply-issue.ts --text '状況を確認' "),
    ).block,
  );
  assert.ok(
    inspectTurn(
      {},
      withCall(reply, "node scripts/reply-issue.ts --text '再発防止'", true),
    ).block,
  );
  assert.ok(
    inspectTurn(
      {},
      withCall(reply, "node scripts/reply-issue.ts --check --text '再発防止' "),
    ).block,
  );
  assert.equal(
    inspectTurn(
      {},
      withCall(reply, 'node scripts/reply-issue.ts --file request.txt'),
    ).block,
    null,
  );
  const before = [
    JSON.stringify({ type: 'user', message: { content: '調査して' } }),
    JSON.stringify({
      type: 'assistant',
      message: {
        content: [
          {
            type: 'tool_use',
            id: 'call',
            name: 'Bash',
            input: { command: "node scripts/reply-issue.ts --text '再発防止'" },
          },
        ],
      },
    }),
    JSON.stringify({
      type: 'user',
      message: {
        content: [
          { type: 'tool_result', tool_use_id: 'call', content: '完了' },
        ],
      },
    }),
    JSON.stringify({
      type: 'assistant',
      message: { content: [{ type: 'text', text: reply }] },
    }),
  ].join('\n');
  assert.equal(inspectTurn({}, before).block, null);
  assert.ok(
    inspectTurn(
      {},
      withCall(
        reply,
        'node scripts/create-issue.ts --project X --hold --file f',
      ),
    ).block,
  );
  assert.ok(
    inspectTurn(
      {},
      turn('調査して', '私の判断ミスです。修正依頼は共通番号123です。'),
    ).block,
  );
});

test('分類のない指示待ちの末尾を止める', () => {
  for (const reply of [
    '『進めて』と言っていただければ修正指示を送ります。',
    '指示があれば進めます。',
    '「進めて」と言っていただければ、この2組から着手します。',
    '起票するなら言ってください。',
    'ご指示いただければ対応します。',
    '「進めて」と伝えてください。',
  ])
    assert.ok(inspectTurn({}, turn('調査して', reply)).block, reply);
  for (const reply of [
    '【質問分類：範囲外の変更】指示があれば進めます。',
    '> 指示があれば進めます。',
    '```\n指示があれば進めます。\n```',
    '`指示があれば進めます。`',
    '指示があれば動く仕組みです。',
    '指示があれば進めると前回伝えた件は完了しました。この先はありません。',
  ])
    assert.equal(inspectTurn({}, turn('調査して', reply)).block, null, reply);
});

test('謝罪だけの返答を止める理由は、不具合は聞かずに起票し、担当外は番号か事実だけにするよう促す', () => {
  const block = inspectTurn(
    {},
    turn('状況を教えて', '私のミスです。すみません。'),
  ).block;
  assert.ok(block);
  assert.match(block.reason, /聞かずに/);
  assert.match(block.reason, /担当外の問題は既存の依頼の共通番号/);
});

test('この会話で直接PRを明示した利用者には、謝罪の返答で起票を求めない', () => {
  const session = [
    JSON.stringify({
      type: 'user',
      message: {
        content: 'このリポジトリをリファクタリングして、直接PRでマージして。',
      },
    }),
    JSON.stringify({
      type: 'assistant',
      message: { content: [{ type: 'text', text: '始めます。' }] },
    }),
    turn(
      'なぜ止めなかったの',
      '私の判断ミスです。すみません。直接PRの作業に足して直します。',
    ),
  ].join('\n');
  assert.equal(inspectTurn({}, session).block, null);
});

test('直接PRの明示がない会話では、謝罪の返答で従来どおり起票を求める', () => {
  const session = [
    JSON.stringify({
      type: 'user',
      message: { content: 'このリポジトリをリファクタリングして。' },
    }),
    JSON.stringify({
      type: 'assistant',
      message: { content: [{ type: 'text', text: '始めます。' }] },
    }),
    turn('なぜ止めなかったの', '私の判断ミスです。すみません。'),
  ].join('\n');
  const block = inspectTurn({}, session).block;
  assert.ok(block);
  assert.match(block.reason, /聞かずに修正依頼を起票/);
});

test('直接PRの明示は、断り・引用・後からの起票の指示では有効にしない', () => {
  assert.equal(directPRActive(['直接PRでマージして']), true);
  assert.equal(directPRActive(['直接PRは作らないで']), false);
  assert.equal(directPRActive(['「直接PRで」とは言っていない']), false);
  assert.equal(directPRActive(['直接PRで直して', 'やっぱり起票して']), false);
  assert.equal(directPRActive(['起票して', '直接PRでやれ']), true);
  assert.equal(directPRActive(['直接PRは頼んでいない']), false);
  assert.equal(directPRActive(['起票しないで直接PRで直して']), true);
  assert.equal(directPRActive(['> 直接PRで直して']), false);
  assert.equal(
    directPRActive(['直接PRで直して', '直接PRの件は起票して']),
    false,
  );
  assert.equal(directPRActive(['直接PRの状況を教えて']), false);
  assert.equal(directPRActive(['直接PRで直す必要はない']), false);
  assert.equal(directPRActive(['```\n直接PRで直して\n```']), false);
  assert.equal(directPRActive(['"直接PRで直して" と書いてあった']), false);
  assert.equal(directPRActive(['直接PRでやれ', '直接PRの状況を教えて']), true);
  assert.equal(directPRActive(['直接PRを作って']), true);
  assert.equal(directPRActive(['直接PRで修正して']), true);
  assert.equal(directPRActive(['直接PRで直して', '起票してはいけない']), true);
  assert.equal(directPRActive(['直接PRで直して', '起票してほしくない']), true);
});

const turnWithCommand = (user: string, command: string, reply: string) =>
  [
    JSON.stringify({ type: 'user', message: { content: user } }),
    JSON.stringify({
      type: 'assistant',
      message: {
        content: [
          { type: 'tool_use', id: 't1', name: 'Bash', input: { command } },
        ],
      },
    }),
    JSON.stringify({
      type: 'user',
      message: {
        content: [
          { type: 'tool_result', tool_use_id: 't1', content: '{"issues":[]}' },
        ],
      },
    }),
    JSON.stringify({
      type: 'assistant',
      message: { content: [{ type: 'text', text: reply }] },
    }),
  ].join('\n');

test('起票の提案は、既存の依頼を検索してからでないと止める', () => {
  const proposal =
    '【質問分類：範囲外の変更】同じ失敗を防ぐ修正依頼を送ってよいですか。';
  for (const reply of [
    proposal,
    '【質問分類：範囲外の変更】別の依頼として起票してよいですか。',
    '【質問分類：範囲外の変更】起票しますか。',
  ])
    assert.match(
      inspectTurn({}, turn('状況を教えて', reply)).block?.reason ?? '',
      /既にないか/,
      reply,
    );
  const searched = turnWithCommand(
    '状況を教えて',
    'cd ~ && ~/.issue-desk/desk list-issues.ts --duplicates --project owner/repo --query 517',
    proposal,
  );
  // 検索済みでも、不具合の起票を聞く質問は止める（改善案と書いた提案だけ通す）。
  assert.match(inspectTurn({}, searched).block?.reason ?? '', /聞かず/);
  assert.doesNotMatch(
    inspectTurn({}, searched).block?.reason ?? '',
    /既にないか/,
  );
  const improvement =
    '【質問分類：範囲外の変更】改善案として、表示を足す修正依頼を送ってよいですか。';
  const read = turnWithCommand(
    '状況を教えて',
    'node scripts/desk-get.ts GET /api/issue?desk=4521',
    improvement,
  );
  assert.match(inspectTurn({}, read).block?.reason ?? '', /既にないか/);
  const issueOnly = turnWithCommand(
    '状況を教えて',
    'node scripts/desk-get.ts GET /api/issues/search?q=517',
    improvement,
  );
  assert.match(inspectTurn({}, issueOnly).block?.reason ?? '', /既にないか/);
});

test('不具合の起票の可否は、分類が付いていても止める', () => {
  for (const reply of [
    '起票しますか？',
    '起票してよいですか。',
    '起票してもいいですか',
    '起票しておきましょうか。',
    '修正依頼を送ってよいですか。',
    '修正依頼を送りますか。',
    '起票しましたが、追加で起票しますか？',
    '承認なしで起票しますか？',
  ]) {
    const classified = `【質問分類：範囲外の変更】${reply}`;
    const transcript = turnWithCommand(
      '原因を調べて',
      'node scripts/desk-get.ts GET /api/issue?desk=4521',
      classified,
    );
    assert.match(
      inspectTurn({}, transcript).block?.reason ?? '',
      /聞かず/,
      reply,
    );
  }
});

test('AskUserQuestion で聞いた起票の可否も止める', () => {
  const transcript = [
    JSON.stringify({ type: 'user', message: { content: '原因を調べて' } }),
    JSON.stringify({
      type: 'assistant',
      message: {
        content: [
          {
            type: 'tool_use',
            id: 'q',
            name: 'AskUserQuestion',
            input: {
              questions: [
                {
                  question:
                    '【質問分類：範囲外の変更】この是正を起票しますか？',
                },
              ],
            },
          },
        ],
      },
    }),
  ].join('\n');
  assert.match(inspectTurn({}, transcript).block?.reason ?? '', /聞かず/);
  assert.equal(
    filingQuestion('', '原因を調べて', [
      '【質問分類：範囲外の変更】改善案として起票しますか？',
    ]),
    null,
  );
});

test('起票の報告・引用・コード・改善案・承認の質問と、利用者の起票相談は止めない', () => {
  for (const reply of [
    '起票しました。',
    '起票しています。',
    '```text\n起票しますか？\n```',
    '> 起票しますか？',
    '`起票しますか？` を引用しました。',
    '起票済みの依頼を確認しますか？',
  ])
    assert.equal(filingQuestion(reply, '原因を調べて'), null, reply);
  for (const reply of [
    '改善案として起票しますか？',
    '起票と直接PRをしてよいですか？',
    '上限超えの起票をしてよいですか？',
    '起票した依頼のPRをマージしてよいですか？',
    '起票済みの依頼を削除してよいですか？',
  ])
    assert.equal(filingQuestion(reply, '原因を調べて'), null, reply);
  assert.equal(filingQuestion('起票しますか？', '起票について教えて'), null);
});

test('起票の提案でない返答は、検索が無くても止めない', () => {
  for (const reply of [
    '既存の依頼（共通番号4521）があります。',
    'テストは通りました。',
    '起票はしていません。',
  ])
    assert.equal(
      inspectTurn({}, turn('状況を教えて', reply)).block,
      null,
      reply,
    );
});

const replyTurn = (command: string, output: string, tail: object[] = []) =>
  [
    JSON.stringify({ type: 'user', message: { content: '直して' } }),
    JSON.stringify({
      type: 'assistant',
      message: {
        content: [
          { type: 'tool_use', id: 'r1', name: 'Bash', input: { command } },
        ],
      },
    }),
    JSON.stringify({
      type: 'user',
      message: {
        content: [{ type: 'tool_result', tool_use_id: 'r1', content: output }],
      },
    }),
    ...tail.map((part) =>
      JSON.stringify({ type: 'assistant', message: { content: [part] } }),
    ),
    JSON.stringify({
      type: 'assistant',
      message: { content: [{ type: 'text', text: '修正指示を送りました。' }] },
    }),
  ].join('\n');

test('実装AIへ返信したら、見張りを付けずに返答を終えられない', () => {
  const command =
    '~/.issue-desk/desk reply-issue.ts --project a/b --number 40 --file ~/.issue-desk/requests/x.md';
  const output = '修正指示を送りました: a/b#40（受付の共通番号 #4542） 題名';
  assert.match(
    inspectTurn({}, replyTurn(command, output)).block?.reason ?? '',
    /見張らずに/,
  );
  const watched = replyTurn(command, output, [
    {
      type: 'tool_use',
      id: 'w1',
      name: 'Bash',
      input: {
        command: 'desk follow-issues.ts --desk 4542 --state /home/u/s.json',
        run_in_background: true,
      },
    },
  ]);
  assert.doesNotMatch(
    inspectTurn({}, watched).block?.reason ?? '',
    /見張らずに/,
  );
  // 改名前の .mjs が残る作業ツリーからの返信も見張りを求め、.mjs の名前の見張りは見張りと数えない。
  const oldReply = command.replace('reply-issue.ts', 'reply-issue.mjs');
  assert.match(
    inspectTurn({}, replyTurn(oldReply, output)).block?.reason ?? '',
    /見張らずに/,
  );
  const oldWatch = replyTurn(command, output, [
    {
      type: 'tool_use',
      id: 'w1',
      name: 'Bash',
      input: {
        command: 'desk follow-issues.mjs --desk 4542 --state /home/u/s.json',
        run_in_background: true,
      },
    },
  ]);
  assert.match(inspectTurn({}, oldWatch).block?.reason ?? '', /見張らずに/);
});

test('返信の確認だけ(--check)や送信に失敗した返信は、見張りを求めない', () => {
  const output = '修正指示を送りました: a/b#40（受付の共通番号 #4542） 題名';
  assert.doesNotMatch(
    inspectTurn({}, replyTurn('desk reply-issue.ts --check abc', output)).block
      ?.reason ?? '',
    /見張らずに/,
  );
  assert.doesNotMatch(
    inspectTurn(
      {},
      replyTurn(
        'desk reply-issue.ts --project a/b --number 40 --text x',
        '送信に失敗しました',
      ),
    ).block?.reason ?? '',
    /見張らずに/,
  );
});

test('背景の見張りの中で、さらに & で切り離したものは見張りと数えない', () => {
  const command =
    '~/.issue-desk/desk reply-issue.ts --project a/b --number 40 --file ~/.issue-desk/requests/x.md';
  const output = '修正指示を送りました: a/b#40（受付の共通番号 #4542） 題名';
  const detached = replyTurn(command, output, [
    {
      type: 'tool_use',
      id: 'w2',
      name: 'Bash',
      input: {
        command:
          'desk follow-issues.ts --desk 4542 --state /home/u/s.json > /tmp/w.out 2>&1 &\nsleep 20; cat /tmp/w.out',
        run_in_background: true,
      },
    },
  ]);
  assert.match(inspectTurn({}, detached).block?.reason ?? '', /見張らずに/);
  const foreground = replyTurn(command, output, [
    {
      type: 'tool_use',
      id: 'w3',
      name: 'Bash',
      input: {
        command: 'desk follow-issues.ts --desk 4542 --state /home/u/s.json',
        run_in_background: true,
      },
    },
  ]);
  assert.doesNotMatch(
    inspectTurn({}, foreground).block?.reason ?? '',
    /見張らずに/,
  );
});

test('スクリプト名がない呼び出しと別のスクリプトは送信扱いにしない', () => {
  const reply = '私の判断ミスです。判断の記録: なし';
  for (const command of [
    'node',
    'env X=1 node',
    'node scripts/other.mjs --text 再発防止',
  ])
    assert.ok(inspectTurn({}, withCall(reply, command)).block, command);
  assert.equal(
    inspectTurn(
      {},
      withCall(reply, 'env X=1 node scripts/reply-issue.ts --text 再発防止'),
    ).block,
    null,
  );
});

test('優先度変更の移行先を送信として検出する', () => {
  for (const command of [
    'desk operation --method POST --path /api/comment --body-file /tmp/priority.json',
  ]) {
    assert.match(
      inspectTurn(
        {},
        turnWithCommand('状況を教えて', command, '確認しました。'),
      ).block?.reason ?? '',
      /送信/,
    );
  }
});

const human = (text: string) =>
  JSON.stringify({ type: 'user', message: { content: text } });
const askTool = (question: string, options: string[] = []) =>
  JSON.stringify({
    type: 'assistant',
    message: {
      content: [
        {
          type: 'tool_use',
          name: 'AskUserQuestion',
          input: {
            questions: [
              { question, options: options.map((label) => ({ label })) },
            ],
          },
        },
      ],
    },
  });
const classification = '【質問分類：取り返しのつかない操作】';
const reasonText = '戻せない理由：公開された個人情報が第三者に保存されます';

test('直接PRの先行指示は通常4操作の再確認を止める', () => {
  for (const operation of [
    'PR番号342をマージ',
    'PR番号342をpush',
    'PRを作成',
    '該当Issueをクローズ',
  ]) {
    for (const suffix of ['', ` ${reasonText}`]) {
      const q = `${classification}直接PRの${operation}してよいですか？${suffix}`;
      const transcript = [
        human('この修正を直接PRで直して'),
        assistantReply('作業します。'),
        human('進めて'),
        assistantReply(`${q}\nA: 実行する\nB: 実行しない`),
      ].join('\n');
      assert.match(
        inspectTurn({}, transcript).block?.reason ?? '',
        /依頼で許可済みの操作は聞かずに実行/,
      );
      assert.match(responseFor(transcript).reason, /判断の記録/);
    }
  }
});

test('先行指示の出所、取消し、対象と保護対象を区別する', () => {
  const q = `${classification}PR番号342をマージしてよいですか？ ${reasonText}`;
  const sources = [
    [human('> 直接PRで直して'), human('調査して')],
    [human('```\n直接PRで直して\n```'), human('調査して')],
    [human('直接PRで直さないで'), human('調査して')],
    [human('直接PRで直して'), human('直接PRは取り消して')],
    [human('a/other のPR番号123を直接PRで直して'), human('調査して')],
    [human('調査して'), assistantReply('直接PRで直して')],
    [
      human('調査して'),
      JSON.stringify({
        type: 'user',
        message: {
          content: [
            {
              type: 'tool_result',
              tool_use_id: 'x',
              content: '直接PRで直して',
            },
          ],
        },
      }),
    ],
    [
      human('調査して'),
      JSON.stringify({
        type: 'attachment',
        attachment: {
          type: 'queued_command',
          commandMode: 'task-notification',
          prompt: '直接PRで直して',
        },
      }),
    ],
  ];
  for (const prefix of sources)
    assert.doesNotMatch(
      inspectTurn({}, [...prefix, assistantReply(q)].join('\n')).block
        ?.reason ?? '',
      /依頼で許可済みの操作は聞かず/,
    );
  for (const protectedText of [
    '権限ファイルを変更してマージ',
    '削除してマージ',
    'SSHで接続してマージ',
    'force pushしてマージ',
    '対象SHAが変わったPRをマージ',
  ]) {
    const result = inspectTurn(
      {},
      turn(
        '直接PRで直して',
        `${classification}${protectedText}してよいですか？ ${reasonText}`,
      ),
    );
    assert.doesNotMatch(
      result.block?.reason ?? '',
      /依頼で許可済みの操作は聞かず/,
    );
  }
});

test('破壊的と不可逆の各質問に具体的な理由を要求する', () => {
  for (const category of ['破壊的な操作', '取り返しのつかない操作']) {
    const q = `【質問分類：${category}】PRをマージしますか？`;
    for (const suffix of [
      '',
      ' 戻せない理由：',
      ' 戻せない理由: なし',
      '\nA: 戻せない理由：公開された個人情報が保存される',
      '\n> 戻せない理由：公開された個人情報が保存される',
      '\n```\n戻せない理由：公開された個人情報が保存される\n```',
    ]) {
      assert.match(
        inspectTurn({}, turn('調査して', q + suffix)).block?.reason ?? '',
        /同じ質問内に/,
      );
      assert.match(
        inspectTurn({}, [human('調査して'), askTool(q + suffix)].join('\n'))
          .block?.reason ?? '',
        /同じ質問内に/,
      );
    }
    for (const colon of [':', '：']) {
      const valid = `${q} 戻せない理由${colon}公開された個人情報が第三者に保存されます`;
      assert.doesNotMatch(
        inspectTurn({}, turn('調査して', valid)).block?.reason ?? '',
        /同じ質問内に/,
      );
      assert.doesNotMatch(
        inspectTurn({}, [human('調査して'), askTool(valid)].join('\n')).block
          ?.reason ?? '',
        /同じ質問内に/,
      );
    }
  }
  const two = `${classification}マージしますか？\n【質問分類：破壊的な操作】削除しますか？ ${reasonText}`;
  assert.match(
    inspectTurn({}, turn('調査して', two)).block?.reason ?? '',
    /同じ質問内に/,
  );
  assert.match(
    inspectTurn(
      {},
      [
        human('調査して'),
        askTool(`${classification}マージしますか？`, [reasonText]),
      ].join('\n'),
    ).block?.reason ?? '',
    /同じ質問内に/,
  );
});

test('直接PRがない具体的理由付きマージと他の分類は追加検査を通る', () => {
  assert.equal(
    inspectTurn(
      {},
      turn('調査して', `${classification}PRをマージしますか？ ${reasonText}`),
    ).block,
    null,
  );
  for (const category of ['新しい権限', '上限超えの再試行']) {
    const result = inspectTurn(
      {},
      turn('調査して', `【質問分類：${category}】実行しますか？`),
    );
    assert.doesNotMatch(
      result.block?.reason ?? '',
      /同じ質問内に|依頼で許可済みの操作は聞かず/,
    );
  }
});

test('別対象と検証失敗の質問を直接PRだけで許可済みにしない', () => {
  for (const [instruction, question] of [
    [
      'a/first の修正を直接PRで直して',
      'b/second のPR番号342をマージしますか？',
    ],
    ['PR番号123を直接PRで直して', 'PR番号342をマージしますか？'],
    ['直接PRで直して', '別のPR番号342をマージしますか？'],
    ['直接PRで直して', 'テストが失敗したPR番号342をマージしますか？'],
    ['直接PRで直さないで', 'PR番号342をマージしますか？'],
  ]) {
    const transcript = turn(
      instruction,
      `${classification}${question} ${reasonText}`,
    );
    assert.doesNotMatch(
      inspectTurn({}, transcript).block?.reason ?? '',
      /依頼で許可済みの操作は聞かず/,
      instruction + question,
    );
  }
});

test('理由欄の単語だけや否定文を具体的な副作用と扱わない', () => {
  for (const reason of [
    '保存してください',
    'データを保存してください',
    '第三者に情報は漏れません',
    'データが失われません',
    '副作用が発生します',
    '情報を確認してください',
  ]) {
    for (const category of ['破壊的な操作', '取り返しのつかない操作']) {
      const question = `【質問分類：${category}】マージしますか？ 戻せない理由：${reason}`;
      for (const transcript of [
        turn('調査して', question),
        [human('調査して'), askTool(question)].join('\n'),
      ])
        assert.match(
          inspectTurn({}, transcript).block?.reason ?? '',
          /同じ質問内に/,
          reason,
        );
    }
  }
});

test('同じ行の別の説明から戻せない理由を流用しない', () => {
  const q = `${classification}PRをマージしますか？ 別の話です。${reasonText}`;
  assert.match(
    inspectTurn({}, turn('調査して', q)).block?.reason ?? '',
    /同じ質問内に/,
  );
  assert.match(
    inspectTurn({}, [human('調査して'), askTool(q)].join('\n')).block?.reason ??
      '',
    /同じ質問内に/,
  );
});

test('依頼番号と作成後のPR番号を別種の対象として照合する', () => {
  const transcript = turn(
    'Issue番号123を直接PRで直して',
    `${classification}PR番号342をマージしますか？ ${reasonText}`,
  );
  assert.match(
    inspectTurn({}, transcript).block?.reason ?? '',
    /依頼で許可済みの操作は聞かず/,
  );
});

test('後の利用者指示で対象が変われば先行指示を流用しない', () => {
  const q = `${classification}b/second のPR番号342をマージしますか？ ${reasonText}`;
  const transcript = [
    human('a/first の修正を直接PRで直して'),
    human('b/second を調査して'),
    assistantReply(q),
  ].join('\n');
  assert.doesNotMatch(
    inspectTurn({}, transcript).block?.reason ?? '',
    /依頼で許可済みの操作は聞かず/,
  );
});

test('直後の理由行は質問に属し、別の質問や引用からは流用しない', () => {
  const q = `${classification}送信しますか？`;
  for (const value of [
    `${q}\n戻せない理由：顧客にメールが届きます`,
    `${q}\n戻せない理由：注文を取り消せません`,
  ]) {
    for (const transcript of [
      turn('調査して', value),
      [human('調査して'), askTool(value)].join('\n'),
    ])
      assert.doesNotMatch(
        inspectTurn({}, transcript).block?.reason ?? '',
        /同じ質問内に/,
        value,
      );
  }
  for (const value of [
    `${q}\n\n${reasonText}`,
    `${q}\n> ${reasonText}`,
    `${q}\nA: ${reasonText}`,
    `${q} 【質問分類：破壊的な操作】削除しますか？\n${reasonText}`,
  ]) {
    assert.match(
      inspectTurn({}, turn('調査して', value)).block?.reason ?? '',
      /同じ質問内に/,
      value,
    );
  }
});

test('具体的な通知や課金の副作用を理由として受け付ける', () => {
  for (const reason of [
    '顧客にメールが届きます',
    '契約に費用が請求されます',
    '注文を取り消せません',
    '機密が第三者に知られます',
  ]) {
    const q = `${classification}実行しますか？ 戻せない理由：${reason}`;
    for (const transcript of [
      turn('調査して', q),
      [human('調査して'), askTool(q)].join('\n'),
    ])
      assert.doesNotMatch(
        inspectTurn({}, transcript).block?.reason ?? '',
        /同じ質問内に/,
        reason,
      );
  }
});

test('Issue停止質問の分類と選択肢を質問ごとに検査する', () => {
  const id = 'stop-question';
  for (const category of ['破壊的な操作', '取り返しのつかない操作']) {
    for (const verb of ['止めますか', '停止しますか']) {
      const transcript = turn(
        '調査して',
        `【質問分類：${category}】5428を今${verb}？`,
      );
      assert.ok(inspectTurn({}, transcript).fires.some((f) => f.id === id));
      assert.match(
        responseFor(transcript).reason,
        /停止は再開で戻せる操作で、利用枠・時間の無駄を止める停止は聞かずに行い、結果を報告/,
      );
    }
  }
  const q = `${classification}5428はどうしますか？`;
  const option = 'A: 停止する';
  for (const transcript of [
    turn('調査して', `${q}\n${option}`),
    [human('調査して'), askTool(q, [option])].join('\n'),
  ])
    assert.ok(inspectTurn({}, transcript).fires.some((f) => f.id === id));
  const separate = `${classification}5428をどうしますか？\nA: 続ける\n【質問分類：新しい権限】別件はどうしますか？\nB: 停止する`;
  assert.ok(
    !inspectTurn({}, turn('調査して', separate)).fires.some((f) => f.id === id),
  );
  const tools = JSON.stringify({
    type: 'assistant',
    message: {
      content: [
        {
          type: 'tool_use',
          name: 'AskUserQuestion',
          input: {
            questions: [
              { question: q, options: [{ label: '続ける' }] },
              {
                question: '【質問分類：新しい権限】別件はどうしますか？',
                options: [{ label: option }],
              },
            ],
          },
        },
      ],
    },
  });
  assert.ok(
    !inspectTurn({}, [human('調査して'), tools].join('\n')).fires.some(
      (f) => f.id === id,
    ),
  );
});

test('Issue停止質問の指示と対象外を区別する', () => {
  const q = `${classification}5428を停止しますか？`;
  const fired = (user: string, reply = q) =>
    inspectTurn({}, turn(user, reply)).fires.some(
      (f) => f.id === 'stop-question',
    );
  assert.equal(fired('5428を停止してください'), false);
  for (const user of [
    '5428を停止しますか？',
    '5428を停止して？',
    '5428を停止しないで',
    '> 5428を停止して',
    '「5428を停止して」と言った',
    '5679を停止して',
  ])
    assert.equal(fired(user), true, user);
  for (const verb of ['再開', '削除', 'マージ']) {
    const reply = `${classification}5428を${verb}しますか？ ${reasonText}`;
    const result = inspectTurn({}, turn('調査して', reply));
    assert.equal(result.block, null, verb);
    assert.ok(!result.fires.some((f) => f.id === 'stop-question'));
  }
  for (const reply of [
    `> ${q}`,
    `\`\`\`\n${q}\n\`\`\``,
    '5428を停止しました。',
  ]) {
    const result = inspectTurn({}, turn('調査して', reply));
    assert.equal(result.block, null, reply);
    assert.ok(!result.fires.some((f) => f.id === 'stop-question'));
  }
});

test('Issue停止質問を修正する継続は再掲として止めない', () => {
  const q = `${classification}5428を停止しますか？`;
  const transcript = [
    human('調査して'),
    assistantReply(q),
    assistantReply(
      `${q} 戻せない理由：公開された個人情報が第三者に保存されます`,
    ),
  ].join('\n');
  assert.equal(
    responseFor(transcript, { stop_hook_active: true }).decision,
    undefined,
  );
});

test('範囲外の変更・判断の根拠の変化の質問は止め、改善案の提案と上限超えの再試行は通す', () => {
  for (const reply of [
    '【質問分類：範囲外の変更】対象を広げて進めますか？',
    '【質問分類：範囲外の変更】改善案ですが、対象を広げて進めますか？',
    '【質問分類：判断の根拠の変化】前提が変わったので計画を見直しますか？',
  ])
    assert.match(
      inspectTurn({}, turn('進めて', reply)).block?.reason ?? '',
      /のほかは利用者に聞きません/,
      reply,
    );
  for (const reply of [
    '【質問分類：範囲外の変更】改善案として、表示を足す修正依頼を送ってよいですか。',
    '【質問分類：上限超えの再試行】実装回数の上限を超えて再試行しますか？',
  ])
    assert.doesNotMatch(
      inspectTurn({}, turn('進めて', reply)).block?.reason ?? '',
      /のほかは利用者に聞きません/,
      reply,
    );
});

test('通知の回に利用者への質問を指すだけの返答は止め、質問の全文を置いた返答・利用者への質問でない報告・利用者への返答は止めない', () => {
  const notified = (reply: string, request = '進めて') =>
    [
      human(request),
      assistantReply('進めます。'),
      JSON.stringify({
        type: 'attachment',
        attachment: {
          type: 'queued_command',
          commandMode: 'task-notification',
          prompt: '【完了】共通番号1: 状態=completed',
        },
      }),
      JSON.stringify({
        type: 'assistant',
        message: { content: [{ type: 'text', text: reply }] },
      }),
    ].join('\n');
  for (const reply of [
    '共通番号1が完了しました。主因の修正は、表示済みの質問1の回答待ちのままです。',
    '状態に変化はなく、質問1〜3の回答待ちは変わりません。',
    '質問2は引き続き承認待ちです。',
    '残りは利用者の回答待ちのままです。',
    '先ほどの質問の回答待ちのままです。',
    '上記の質問への回答をお待ちしています。',
    '質問 1 の回答待ちのままです。',
    '質問1（マージの可否）の回答待ちです。',
    '質問1の回答を待っています。',
    '質問1・2はご回答待ちです。',
    '表示済みの質問1（【質問分類：新しい権限】）の回答待ちのままです。',
    '質問1の回答待ちのままです。\n\n見張りを続けます。',
    '質問1の回答待ちのまま、監視を続けます。',
    '共通番号1が完了しました。Workerは動いていますが、質問1の回答待ちです。',
    'ユーザーの回答待ちです。',
    '残りは質問1の返答待ちです。',
    '残りは利用者の確認待ちです。',
    '表示済みの質問は有効なままです。',
  ]) {
    const reason = inspectTurn({}, notified(reply)).block?.reason ?? '';
    assert.match(reason, /質問と選択肢の全文がありません/, reply);
    assert.doesNotMatch(reason, /有効なままなら再掲しない/, reply);
  }
  const restated =
    '共通番号1が完了しました。\n\n【質問分類：新しい権限】残りを直接PRで直してよいですか。\n- A: 進める\n- B: 進めない';
  assert.equal(
    inspectTurn({}, notified(restated, '直接PRで直して')).block,
    null,
  );
  const recommended =
    '質問1の回答待ちの間に共通番号1が完了しました。\n\n【質問分類：新しい権限】残りを直接PRで直してよいですか。\n- **A（おすすめ）**: 進める\n- B: 進めない';
  assert.equal(
    inspectTurn({}, notified(recommended, '直接PRで直して')).block,
    null,
  );
  assert.equal(
    inspectTurn(
      {},
      notified(
        'ご判断をお待ちします。\n\n【質問分類：新しい権限】残りを直接PRで直してよいですか。\n\n- A: 進める\n- B: 進めない',
        '直接PRで直して',
      ),
    ).block,
    null,
  );
  const unreadable = [
    human('APIを調査して'),
    JSON.stringify({ type: 'assistant', message: {} }),
    notified('質問1の回答待ちのままです。').split('\n').slice(2).join('\n'),
  ].join('\n');
  assert.match(
    inspectTurn({}, unreadable).block?.reason ?? '',
    /質問と選択肢の全文がありません/,
  );
  const question =
    '【質問分類：新しい権限】残りを直接PRで直してよいですか。\n- A: 進める\n- B: 進めない';
  const later = (first: string, reply: string) =>
    [
      human('直接PRで直して'),
      assistantReply(first),
      human('ところで今のNASの時刻は？'),
      assistantReply('10時です。'),
      notified(reply).split('\n').slice(2).join('\n'),
    ].join('\n');
  assert.equal(inspectTurn({}, later(question, restated)).block, null);
  assert.match(
    inspectTurn({}, later('進めます。', restated)).block?.reason ?? '',
    /直接PR|担当外|範囲/,
  );
  for (const reply of [
    '共通番号1は実装AIの回答待ちのままです。',
    '受付AIの前の質問には回答済みです。',
    '共通番号1の回答待ちは解消し、実装が再開しました。',
    '以上の質問への回答を受付へ送りました。',
    '共通番号1は、実装AIへ送った質問1の回答待ちです。',
    '受付AIからの質問2は判断待ちのため、相談は進んでいません。',
    '受付AIが相談画面に表示済みの質問へ、資料から回答を送りました。',
    '表示済みの質問1には回答をいただいたので、実装を再開しました。',
    '共通番号1が完了したため、表示済みの質問は取り下げます。回答は不要です。',
    '利用者の回答待ちは解消し、実装が再開しました。',
    '共通番号1は実装中です。利用者の回答待ちの項目はありません。',
    '利用者の承認待ちだった削除は、承認を受けて実行しました。',
    '質問1は取り下げたので、回答待ちではなくなりました。',
    '共通番号1はCodexが出した質問の回答待ちです。',
    '共通番号1はレビュー待ちです。',
    '利用者の承認が要る操作は保留しています。',
    '共通番号3の実装が質問を出して回答待ちになったので、資料から回答を送りました。',
    '利用者の回答を待っていた共通番号3は、回答を受けて完了しました。',
    '相談は受付の回答待ちのままです。',
    '共通番号5481（利用者の回答待ちを伝えるだけの返答を止める）がマージされました。',
    '共通番号12はレビューの承認待ちのままです。',
    '共通番号1は、こちらから送った質問への返事待ちです。',
    '共通番号1が完了しました。利用者の回答待ちは残っていません。',
    '共通番号1が完了し、利用者の承認待ちは無くなりました。',
    '共通番号1は完了しました。質問1の回答待ちで止めていた共通番号2も再開しました。',
    '判断の記録: 利用者の判断待ちにはせず、おすすめ案で進めた。',
    '共通番号5481（利用者の回答待ち（通知の回）を止める）がマージされました。',
    '共通番号5481: 利用者の回答待ちを伝えるだけの返答を止める — 完了しました。',
  ])
    assert.equal(inspectTurn({}, notified(reply)).block, null, reply);
  assert.doesNotMatch(
    inspectTurn({}, turn('質問は？', '表示済みの質問は1件です。')).block
      ?.reason ?? '',
    /質問と選択肢の全文がありません/,
  );
});
