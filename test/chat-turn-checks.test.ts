import assert from 'node:assert/strict';
import test from 'node:test';
import { spawnSync } from 'node:child_process';
import {
  heavyForeground,
  parseCommands,
  inspectTurn,
  sendsToIntake,
  onlyFilesIssue,
  changesWork,
  selfDetaching,
  scratchPath,
} from '../scripts/chat-stop-hook.ts';
import {
  isQuestionOnly,
  normalizeDeskArgs,
  normalizedDeskCommand,
  deskCommand,
  deskIssueRoute,
  isIssueScript,
  shellScript,
  timeoutCommand,
} from '../scripts/chat-turn-checks.ts';

type Step =
  | { text: string }
  | { bash: string; background?: boolean; timeout?: number };
const line = (value: unknown) => JSON.stringify(value);
// Build a transcript: one user message, then assistant steps in order.
function turn(user: string, steps: Step[]): string {
  const rows = [line({ type: 'user', message: { content: user } })];
  steps.forEach((step, index) => {
    if ('text' in step)
      rows.push(
        line({
          type: 'assistant',
          message: { content: [{ type: 'text', text: step.text }] },
        }),
      );
    else {
      const input: Record<string, unknown> = { command: step.bash };
      if (step.background) input.run_in_background = true;
      if (step.timeout) input.timeout = step.timeout;
      rows.push(
        line({
          type: 'assistant',
          message: {
            content: [
              { type: 'tool_use', id: `t${index}`, name: 'Bash', input },
            ],
          },
        }),
      );
      rows.push(
        line({
          type: 'user',
          message: {
            content: [
              { type: 'tool_result', tool_use_id: `t${index}`, content: 'ok' },
            ],
          },
        }),
      );
    }
  });
  return rows.join('\n');
}
const long =
  '回答です。状況は次のとおりで、確認できた範囲だけを書いています。詳細は以下のとおりです。判断の記録: なし。';

test('timeoutCommand は公開入口として timeout を一段展開する', () => {
  assert.deepEqual(
    timeoutCommand([
      'timeout',
      '30',
      'node',
      'scripts/desk-get.ts',
      'GET',
      '/api/issue',
    ]),
    ['node', 'scripts/desk-get.ts', 'GET', '/api/issue'],
  );
  assert.equal(timeoutCommand(['timeout', '--bad', '30', 'node']), null);
});

test('正規化済み窓口判定と公開入口は同じ結果を返す', () => {
  for (const [words, expected] of [
    [
      ['env', 'X=1', 'bash', '/home/user/desk', 'get', '--path', '/api/issues'],
      'get',
    ],
    [['node', 'scripts/desk-get.ts', 'GET', '/api/issues'], 'get'],
    [['env', 'desk-pc', 'operation'], 'operation'],
    [['desk', 'other'], null],
  ] as [string[], 'get' | 'operation' | null][]) {
    assert.equal(normalizedDeskCommand(normalizeDeskArgs(words)), expected);
    assert.equal(deskCommand(words), expected);
  }
});

test('質問・指摘の判定: 依頼語があれば質問だけとは扱わない', () => {
  for (const q of [
    '優先順位がおかしくない？',
    'なぜ止まっているの',
    '今の状況を教えて',
    '放置してない？',
  ])
    assert.equal(isQuestionOnly(q), true, q);
  for (const r of [
    'これ起票して',
    '任せた',
    '直して？',
    'A',
    'はい',
    '再試行してください',
  ])
    assert.equal(isQuestionOnly(r), false, r);
});

test('2 勝手に進めた: 質問への返答中の返信・再試行・クローズは止める', () => {
  for (const cmd of [
    '~/.issue-desk/desk create-issue.ts --project a/b --file x.md && desk-pc operation --method POST --path /api/close-requests --body-file /tmp/close.json',
    'node scripts/reply-issue.ts --draft abc --text hi',
    '~/.issue-desk/desk reply-issue.ts --number 12',
    'desk-pc operation --method POST --path /api/close-requests --body-file /tmp/close.json',
  ]) {
    const result = inspectTurn(
      {},
      turn('優先順位がおかしくない？', [{ bash: cmd }, { text: long }]),
    );
    assert.match(result.block?.reason ?? '', /質問・状況確認・指摘/, cmd);
  }
});

test('2 勝手に進めた: 読取・--check-only・依頼語つきの発言は止めない', () => {
  const ok = [
    [
      '優先順位がおかしくない？',
      '~/.issue-desk/desk desk-get.ts GET /api/issue?desk=3',
    ],
    [
      '優先順位がおかしくない？',
      '~/.issue-desk/desk create-issue.ts --project a/b --check-only --text x',
    ],
    [
      '優先順位がおかしくない？',
      '~/.issue-desk/desk create-issue.ts --check abc',
    ],
    [
      'これを起票して',
      '~/.issue-desk/desk create-issue.ts --project a/b --file x.md',
    ],
    ['A', '~/.issue-desk/desk create-issue.ts --project a/b --file x.md'],
    // 不具合は質問の回でも聞かずに起票する（新しい起票だけ）。
    [
      '優先順位がおかしくない？',
      '~/.issue-desk/desk create-issue.ts --project a/b --file x.md',
    ],
    [
      '優先順位がおかしくない？',
      'node scripts/create-issue.ts --draft abc --text 回答',
    ],
  ];
  for (const [user, cmd] of ok) {
    const reason =
      inspectTurn({}, turn(user, [{ bash: cmd }, { text: long }])).block
        ?.reason ?? '';
    assert.doesNotMatch(reason, /質問・状況確認・指摘/, `${user} ${cmd}`);
  }
});

test('1 放置: 止まりを報告しただけで読取も対処も無い返答は止める', () => {
  const report = '共通番号 12 は上限で停止しています。私が面倒を見ています。';
  const blocked = inspectTurn(
    {},
    turn('12を任せたよ', [{ text: report }]),
  ).block;
  assert.match(blocked?.reason ?? '', /具体的な対処/);
  const readOnly = inspectTurn(
    {},
    turn('12を任せたよ', [
      { bash: '~/.issue-desk/desk desk-get.ts GET /api/issue?desk=12' },
      { text: report },
    ]),
  ).block;
  assert.match(readOnly?.reason ?? '', /具体的な対処/);
});

test('1 放置: 読取のあとに対処・見張り・分類付きの質問があれば止めない', () => {
  const report = '共通番号 12 は上限で停止しています。原因は実装回数です。';
  const read = {
    bash: '~/.issue-desk/desk desk-get.ts GET /api/issue?desk=12',
  };
  const withReply = [
    read,
    { bash: '~/.issue-desk/desk reply-issue.ts --number 12 --text x' },
    { text: report },
  ];
  assert.doesNotMatch(
    inspectTurn({}, turn('12を任せたよ', withReply)).block?.reason ?? '',
    /具体的な対処/,
  );
  const withWatch = [
    read,
    {
      bash: 'sleep 600; ~/.issue-desk/desk desk-get.ts GET /api/issue?desk=12',
      background: true,
    },
    { text: report },
  ];
  assert.doesNotMatch(
    inspectTurn({}, turn('12を任せたよ', withWatch)).block?.reason ?? '',
    /具体的な対処/,
  );
  const asks = [
    read,
    { text: `${report}【質問分類：新しい権限】再試行を承認しますか？` },
  ];
  assert.doesNotMatch(
    inspectTurn({}, turn('12を任せたよ', asks)).block?.reason ?? '',
    /具体的な対処/,
  );
  // 止まりに触れない返答や、任せていない質問への報告は対象外。
  assert.equal(
    inspectTurn({}, turn('12を任せたよ', [{ text: '完了しました。' }])).block,
    null,
  );
  assert.equal(
    inspectTurn(
      {},
      turn('止まってる？', [{ text: '共通番号 12 が止まっています。' }]),
    ).block,
    null,
  );
});

test('4 まず答える: 質問への回答より先の重い前景呼び出しは止める', () => {
  for (const cmd of [
    'sleep 120',
    'node --test test/a.test.ts',
    'until false; do sleep 5; done',
    '~/.issue-desk/desk create-issue.ts --project a/b --check abc',
  ]) {
    const result = inspectTurn(
      {},
      turn('今どうなってる？', [{ bash: cmd }, { text: long }]),
    );
    assert.match(result.block?.reason ?? '', /回答より先に重い/, cmd);
  }
});

test('4 まず答える: 先に答えた後・バックグラウンド・軽い呼び出しは止めない', () => {
  const user = '今どうなってる？';
  assert.equal(
    inspectTurn({}, turn(user, [{ text: long }, { bash: 'sleep 120' }])).block,
    null,
  );
  assert.equal(
    inspectTurn(
      {},
      turn(user, [
        { bash: 'node scripts/stage-watch.ts -- sleep 120', background: true },
        { text: long },
      ]),
    ).block,
    null,
  );
  assert.equal(
    inspectTurn({}, turn(user, [{ bash: 'git status' }, { text: long }])).block,
    null,
  );
  assert.equal(
    inspectTurn(
      {},
      turn('テストを流して', [
        { bash: 'node --test test/a.test.ts' },
        { text: long },
      ]),
    ).block,
    null,
  );
});

test('4 長い処理は前景で連続させない: 2回以上で警告', () => {
  const two = turn('テストを流して', [
    { bash: 'node --test a.ts' },
    { bash: 'sleep 60' },
    { text: long },
  ]);
  assert.match(
    inspectTurn({}, two).warning ?? '',
    /run_in_background かサブエージェント/,
  );
  const one = turn('テストを流して', [
    { bash: 'node --test a.ts' },
    { text: long },
  ]);
  assert.doesNotMatch(
    inspectTurn({}, one).warning ?? '',
    /run_in_background かサブエージェント/,
  );
});

const tool = (id: string, name: string, input: Record<string, unknown>) => [
  line({
    type: 'assistant',
    message: { content: [{ type: 'tool_use', id, name, input }] },
  }),
  line({
    type: 'user',
    message: {
      content: [{ type: 'tool_result', tool_use_id: id, content: 'ok' }],
    },
  }),
];
const say = (text: string) =>
  line({ type: 'assistant', message: { content: [{ type: 'text', text }] } });
const human = (text: string) =>
  line({ type: 'user', origin: { kind: 'human' }, message: { content: text } });
const notified = (text: string) =>
  line({
    type: 'user',
    origin: { kind: 'task-notification' },
    message: { content: `<task-notification>${text}</task-notification>` },
  });
const follow = {
  command:
    '~/.issue-desk/desk follow-issues.ts --desk 7 --state /home/u/s.json',
  timeout_ms: 1800000,
};
const filed = [
  line({
    type: 'assistant',
    message: {
      content: [
        {
          type: 'tool_use',
          id: 'f',
          name: 'Bash',
          input: {
            command: 'node scripts/create-issue.ts --project a/b --text x',
          },
        },
      ],
    },
  }),
  line({
    type: 'user',
    message: {
      content: [
        {
          type: 'tool_result',
          tool_use_id: 'f',
          content: '起票しました（1件）: 共通番号7',
        },
      ],
    },
  }),
];

test('検査の素通り: 会話以外の行やメタ行があっても検査を続ける', () => {
  const rows = [
    human('なぜ止まっているの'),
    line({ type: 'attachment', attachment: { type: 'hook_success' } }),
    line({ type: 'mode', mode: 'auto' }),
    line({
      type: 'user',
      isMeta: true,
      message: { content: 'Base directory for this skill: x' },
    }),
    ...tool('a', 'Bash', { command: 'node scripts/reply-issue.ts --desk 7' }),
    line({
      type: 'assistant',
      message: {
        content: [{ type: 'server_tool_use' }, { type: 'text', text: long }],
      },
    }),
  ];
  assert.ok(inspectTurn({}, rows.join('\n')).block);
});

test('聞いただけ: 質問の回の変更・コミット・実装の起動は止め、読むだけと一時領域は止めない', () => {
  const stopped: [string, Record<string, unknown>][] = [
    ['Edit', { file_path: '/home/u/a.go' }],
    ['Write', { file_path: '/home/u/a.go' }],
    ['Write', { file_path: '/home/u/a.tmp/x.go' }],
    ['Bash', { command: 'git commit -m x' }],
    ['Bash', { command: 'git -C /r push origin b' }],
    ['Bash', { command: 'sed -i s/a/b/ a.go' }],
    ['Agent', { subagent_type: 'general-purpose', prompt: 'x' }],
  ];
  for (const [name, input] of stopped)
    assert.ok(
      inspectTurn(
        {},
        [
          human('これ動いてる？'),
          say(long),
          ...tool('a', name, input),
          say(long),
        ].join('\n'),
      ).block,
      name,
    );
  const allowed: [string, Record<string, unknown>][] = [
    ['Read', { file_path: '/home/u/a.go' }],
    ['Write', { file_path: '/tmp/scratch/a.md' }],
    ['Write', { file_path: '/home/u/repo/.tmp/notes.md' }],
    ['Bash', { command: 'git log -3' }],
    ['Agent', { subagent_type: 'Explore', prompt: 'x' }],
  ];
  for (const [name, input] of allowed)
    assert.equal(
      inspectTurn(
        {},
        [
          human('これ動いてる？'),
          say(long),
          ...tool('a', name, input),
          say(long),
        ].join('\n'),
      ).block,
      null,
      name,
    );
  assert.equal(
    inspectTurn(
      {},
      [
        human('これ直して'),
        ...tool('a', 'Edit', { file_path: '/home/u/a.go' }),
        say(long),
      ].join('\n'),
    ).block,
    null,
  );
});

test('聞いただけ: 止まりを聞かれた回に対処を求めない', () => {
  const rows = [
    human('放置してない？'),
    say('共通番号7は保留で止まっています。理由は削除の承認待ちです。'),
  ];
  assert.equal(inspectTurn({}, rows.join('\n')).block, null);
});

test('見張り: 起票のあとは follow-issues.ts だけを見張りと認める', () => {
  const ask = human('これ起票して');
  assert.ok(inspectTurn({}, [ask, ...filed, say(long)].join('\n')).block);
  for (const other of [
    ['Bash', { command: 'sleep 600', run_in_background: true }],
    ['ScheduleWakeup', { delaySeconds: 600 }],
    ['Bash', { command: 'desk follow-issues.ts --desk 7 --state /h/s' }],
  ] as const)
    assert.ok(
      inspectTurn(
        {},
        [
          ask,
          ...filed,
          ...tool('b', other[0], { ...other[1] }),
          say(long),
        ].join('\n'),
      ).block,
      JSON.stringify(other),
    );
  assert.equal(
    inspectTurn(
      {},
      [ask, ...filed, ...tool('b', 'Monitor', follow), say(long)].join('\n'),
    ).block,
    null,
  );
});

test('見張り: 通知の回は質問の回として扱わず、読まない放置と張り直し忘れを止める', () => {
  const asked = [human('状況は？'), say(long)];
  const attention = notified('【要対処】共通番号7: 状態=failed');
  assert.ok(inspectTurn({}, [...asked, attention, say(long)].join('\n')).block);
  const acted = [
    ...asked,
    attention,
    ...tool('r', 'Bash', { command: 'desk desk-get.ts GET /api/issue?desk=7' }),
    ...tool('s', 'Bash', { command: 'desk reply-issue.ts --desk 7' }),
    ...tool('e', 'Edit', { file_path: '/home/u/a' }),
    say(long),
  ];
  assert.equal(inspectTurn({}, acted.join('\n')).block, null);
  const rearm = notified('【張り直し】共通番号7');
  assert.ok(inspectTurn({}, [...asked, rearm, say(long)].join('\n')).block);
  assert.equal(
    inspectTurn(
      {},
      [...asked, rearm, ...tool('m', 'Monitor', follow), say(long)].join('\n'),
    ).block,
    null,
  );
  assert.equal(
    inspectTurn(
      {},
      [...asked, rearm, ...tool('m', 'Monitor', follow)].join('\n'),
    ).block,
    null,
    '起動だけで終えてよい',
  );
});

test('見張り: timeout の中の読取だけを原因の読取に数える', () => {
  const prefix = [
    human('状況は？'),
    say(long),
    notified('【要対処】共通番号7: 状態=failed'),
  ];
  for (const command of [
    'timeout 30 node scripts/desk-get.ts GET /api/issue?desk=12',
    'env X=1 timeout 30 node scripts/desk-get.ts GET /api/issues/search',
    'timeout -k 5 30 node scripts/desk-alerts.ts',
    'timeout --kill-after=5 --signal TERM 30s desk get --path /api/issue',
  ]) {
    const reason =
      inspectTurn(
        {},
        [...prefix, ...tool('r', 'Bash', { command }), say(long)].join('\n'),
      ).block?.reason ?? '';
    assert.doesNotMatch(reason, /原因の読取/, command);
  }
  for (const command of [
    'timeout 30 node scripts/reply-issue.ts',
    'timeout --bad 30 node scripts/desk-get.ts GET /api/issue?desk=1',
    'timeout desk get',
  ]) {
    const reason =
      inspectTurn(
        {},
        [...prefix, ...tool('r', 'Bash', { command }), say(long)].join('\n'),
      ).block?.reason ?? '';
    assert.match(reason, /原因の読取/, command);
  }
});

test('timeout の読取は軽く、テストと不明なコマンドは重い', () => {
  for (const command of [
    ...['list-issues', 'desk-alerts', 'usage-per-issue'].map(
      (name) => `timeout -k 5 30 node scripts/${name}.ts`,
    ),
    'env X=1 timeout 30 desk get --path /api/issue',
  ])
    assert.doesNotMatch(
      inspectTurn(
        {},
        turn('今どうなってる？', [{ bash: command }, { text: long }]),
      ).block?.reason ?? '',
      /回答より先に重い/,
      command,
    );
  for (const command of [
    'timeout 60 node --test test/a.test.ts',
    'timeout 60 go test ./...',
    'env timeout 60 command',
    'timeout --bad 30 node scripts/desk-get.ts GET /api/issue?desk=1',
    'timeout desk get',
  ])
    assert.match(
      inspectTurn(
        {},
        turn('今どうなってる？', [{ bash: command }, { text: long }]),
      ).block?.reason ?? '',
      /回答より先に重い/,
      command,
    );
  assert.match(
    inspectTurn(
      {},
      turn('今どうなってる？', [
        {
          bash: 'timeout 30 node scripts/desk-get.ts GET /api/issue?desk=1',
          timeout: 300000,
        },
        { text: long },
      ]),
    ).block?.reason ?? '',
    /回答より先に重い/,
  );
});

test('通知と質問は timeout 内の同じ読取コマンドを認識する', () => {
  const prefix = [
    human('状況は？'),
    say(long),
    notified('【要対処】共通番号7: 状態=failed'),
  ];
  const commands = [
    ...['list-issues', 'desk-alerts', 'usage-per-issue'].map(
      (name) => `timeout 30 env X=1 node scripts/${name}.ts`,
    ),
    'timeout 30 env X=1 node scripts/desk-get.ts GET /api/issue?desk=1',
    'timeout 30 sh ~/.issue-desk/desk-pc get --path /api/issue',
    'timeout 30 sh desk get --path /api/issue',
    'timeout 30 bash desk-pc get --path /api/issue',
    'timeout 30 node scripts/desk-get.ts GET /api/issue',
    'timeout 30 node scripts/create-issue.ts --check-only',
    'timeout 30 node scripts/reply-issue.ts --check 1',
  ];
  for (const command of commands) {
    assert.doesNotMatch(
      inspectTurn(
        {},
        [...prefix, ...tool('r', 'Bash', { command }), say(long)].join('\n'),
      ).block?.reason ?? '',
      /原因の読取/,
      command,
    );
    assert.doesNotMatch(
      inspectTurn(
        {},
        turn('今どうなってる？', [{ bash: command }, { text: long }]),
      ).block?.reason ?? '',
      /回答より先に重い/,
      command,
    );
  }
});

test('管理CLIの使用量レポートだけを通知と質問の読取に数える', () => {
  const prefix = [
    human('状況は？'),
    say(long),
    notified('【要対処】共通番号7: 状態=failed'),
  ];
  const report =
    '/usr/local/bin/intake admin --config /state/config.local.json --application-dir /app usage-per-issue report --from 2026-11-01 --to 2026-11-30 --repo owner/repo --format json --population all';
  for (const command of [
    report,
    `timeout 30 ${report}`,
    'timeout 30 intake admin usage-per-issue report --from=2026-11-01 --to=2026-11-30',
  ]) {
    assert.doesNotMatch(
      inspectTurn(
        {},
        [...prefix, ...tool('r', 'Bash', { command }), say(long)].join('\n'),
      ).block?.reason ?? '',
      /原因の読取/,
      command,
    );
    assert.doesNotMatch(
      inspectTurn(
        {},
        turn('今どうなってる？', [{ bash: command }, { text: long }]),
      ).block?.reason ?? '',
      /回答より先に重い/,
      command,
    );
  }
  for (const command of [
    'intake admin cli-key issue',
    'intake admin usage-per-issue delete --from 2026-11-01 --to 2026-11-30',
    'other admin usage-per-issue report --from 2026-11-01 --to 2026-11-30',
    `${report} --sqlite /tmp/db`,
    `${report} --format table`,
    'intake admin usage-per-issue report --from 2026-11-01',
  ]) {
    const wrapped = `timeout 30 ${command}`;
    assert.match(
      inspectTurn(
        {},
        [...prefix, ...tool('r', 'Bash', { command: wrapped }), say(long)].join(
          '\n',
        ),
      ).block?.reason ?? '',
      /原因の読取/,
      command,
    );
    assert.match(
      inspectTurn(
        {},
        turn('今どうなってる？', [{ bash: wrapped }, { text: long }]),
      ).block?.reason ?? '',
      /回答より先に重い/,
      command,
    );
  }
});

test('使用量レポートの不正な値は通知でも質問でも読取にならない', () => {
  const prefix = [
    human('状況は？'),
    say(long),
    notified('【要対処】共通番号7: 状態=failed'),
  ];
  const base = 'intake admin usage-per-issue report';
  for (const args of [
    '--from 2026-11-01 --to 2026-11-30 --format invalid',
    '--from 2026-11-01 --to 2026-11-30 --population invalid',
    '--from 2026-02-29 --to 2026-03-01',
    '--from 2026-11-31 --to 2026-12-01',
    '--from 2026-12-01 --to 2026-11-30',
    '--from 2026-01-01 --to 2026-04-03',
    '--from 2026-11-01 --to 2026-11-30 --repo invalid',
    '--from 2026-11-01 --to 2026-11-30 --format=',
  ]) {
    const command = `timeout 30 ${base} ${args}`;
    assert.match(
      inspectTurn(
        {},
        [...prefix, ...tool('r', 'Bash', { command }), say(long)].join('\n'),
      ).block?.reason ?? '',
      /原因の読取/,
      command,
    );
    assert.match(
      inspectTurn(
        {},
        turn('今どうなってる？', [{ bash: command }, { text: long }]),
      ).block?.reason ?? '',
      /回答より先に重い/,
      command,
    );
  }
  for (const args of [
    '--from 2024-02-29 --to 2024-02-29',
    '--from 2026-01-01 --to 2026-04-02 --format table --population merged',
  ]) {
    const command = `timeout 30 ${base} ${args}`;
    assert.doesNotMatch(
      inspectTurn(
        {},
        [...prefix, ...tool('r', 'Bash', { command }), say(long)].join('\n'),
      ).block?.reason ?? '',
      /原因の読取/,
      command,
    );
    assert.doesNotMatch(
      inspectTurn(
        {},
        turn('今どうなってる？', [{ bash: command }, { text: long }]),
      ).block?.reason ?? '',
      /回答より先に重い/,
      command,
    );
  }
});

test('入れ子の timeout は通知では読取でなく質問では重い', () => {
  const command =
    'timeout 30 timeout 10 node scripts/desk-get.ts GET /api/issue?desk=1';
  const prefix = [
    human('状況は？'),
    say(long),
    notified('【要対処】共通番号7: 状態=failed'),
  ];
  assert.match(
    inspectTurn(
      {},
      [...prefix, ...tool('r', 'Bash', { command }), say(long)].join('\n'),
    ).block?.reason ?? '',
    /原因の読取/,
  );
  assert.match(
    inspectTurn(
      {},
      turn('今どうなってる？', [{ bash: command }, { text: long }]),
    ).block?.reason ?? '',
    /回答より先に重い/,
  );
});

test('質問の後のおすすめ付き選択肢を認識し説明行は止める', () => {
  const question = '【質問分類：破壊的な操作】どちらにしますか？';
  for (const symbol of ['A', '1-A', '１-Ａ', '**A', '**1-A']) {
    for (const delimiter of [':', '：', '.', '．', ')', '）']) {
      const option = `- ${symbol}（おすすめ）${symbol.startsWith('**') ? '**' : ''}${delimiter} 残す`;
      assert.doesNotMatch(
        inspectTurn({}, turn('確認して', [{ text: `${question}\n${option}` }]))
          .block?.reason ?? '',
        /質問が返答の途中/,
        option,
      );
    }
  }
  for (const tail of [
    '説明が続きます。',
    'A（推奨）: 残す',
    'A（おすすめ） 残す',
  ])
    assert.match(
      inspectTurn({}, turn('確認して', [{ text: `${question}\n${tail}` }]))
        .block?.reason ?? '',
      /質問が返答の途中/,
      tail,
    );
});

test('見張り: 作業中に打たれた質問も回の区切りになる', () => {
  const queued = line({
    type: 'attachment',
    attachment: {
      type: 'queued_command',
      commandMode: 'prompt',
      prompt: 'これ動いてる？',
    },
  });
  const rows = [
    human('直して'),
    queued,
    ...tool('e', 'Edit', { file_path: '/home/u/a' }),
    say(long),
  ];
  assert.ok(inspectTurn({}, rows.join('\n')).block);
});

test('聞かずに決める: 許可しない分類や分類のない質問は止め、破壊的な操作の質問は通す', () => {
  const ask = human('これ直して');
  for (const reply of [
    'どちらの方式がよいですか？',
    '【質問分類：好み】どちらの色がよいですか？',
    '【質問分類：目的】この順序で進めてよいでしょうか。',
  ])
    assert.ok(inspectTurn({}, [ask, say(reply)].join('\n')).block, reply);
  for (const reply of [
    '【質問分類：破壊的な操作】共通番号7の古いデータを削除してよいですか？ 戻せない理由：保存した記録が失われます',
    '【質問分類：取り返しのつかない操作】公開してよいですか？ 戻せない理由：第三者に情報が保存されます',
    '手順は次のとおりです。',
    '> 引用のどれがよいですか？',
  ])
    assert.equal(
      inspectTurn({}, [ask, say(reply)].join('\n')).block,
      null,
      reply,
    );
});

test('聞かずに決める: AskUserQuestion でも同じ基準で止める', () => {
  const ask = human('これ直して');
  const question = (text: string) =>
    tool('q', 'AskUserQuestion', { questions: [{ question: text }] });
  assert.ok(
    inspectTurn(
      {},
      [ask, ...question('どちらにしますか？'), say(long)].join('\n'),
    ).block,
  );
  assert.equal(
    inspectTurn(
      {},
      [
        ask,
        ...question(
          '【質問分類：破壊的な操作】7を削除しますか？ 戻せない理由：保存した記録が失われます',
        ),
        say(long),
      ].join('\n'),
    ).block,
    null,
  );
});

test('聞かずに決める: 作業した回の最後の返答に判断の記録を求める', () => {
  const work = [
    human('これ直して'),
    ...tool('e', 'Edit', { file_path: '/home/u/a' }),
  ];
  assert.match(
    inspectTurn(
      {},
      [...work, say('直しました。テストも通しました。')].join('\n'),
    ).block!.reason,
    /判断の記録/,
  );
  assert.equal(
    inspectTurn(
      {},
      [
        ...work,
        say(
          `${long}\n\n判断の記録: 方式はAにしました。理由は戻しやすいからです。`,
        ),
      ].join('\n'),
    ).block,
    null,
  );
  assert.equal(
    inspectTurn(
      {},
      [
        human('状況を読んで'),
        ...tool('r', 'Read', { file_path: '/home/u/a' }),
        say(long),
      ].join('\n'),
    ).block,
    null,
  );
});

test('読める報告: 利用者が読めなかった実際の報告を止める', () => {
  const ask = human('状況は？');
  const question =
    '【質問分類：破壊的な操作】この2つの下書きを取り下げますか？\n\n- 1-A: 残す（おすすめ）\n- 1-B: 取り下げる\n\n「1-A」か「1-B」だけで答えられます。';
  const after = `${question}\n\n見出し\n案Aを起票しました。共通番号4568で、状態はqueuedです。`;
  assert.match(
    inspectTurn({}, [ask, say(after)].join('\n')).block!.reason,
    /質問が返答の途中/,
  );
  const repeated = `${question.split('\n')[0]}\n\n先ほどの質問のとおりです。\n\n- 1-A: 残す\n- 1-B: 取り下げる\n\nお願いしていること\n「1-A」か「1-B」で答えてください。`;
  assert.ok(inspectTurn({}, [ask, say(repeated)].join('\n')).block);
  assert.match(
    inspectTurn(
      {},
      [
        ask,
        say('見張りを付けない理由\n裏で動かしているのは送信1本です。'),
      ].join('\n'),
    ).block!.reason,
    /見張りを付けない理由/,
  );
  assert.match(
    inspectTurn(
      {},
      [ask, say('いま裏で動かしている処理は、ありません。')].join('\n'),
    ).block!.reason,
    /見張りを付けない理由/,
  );
  assert.match(
    inspectTurn({}, [ask, say('あ'.repeat(801))].join('\n')).block!.reason,
    /長すぎます/,
  );
  // 止めるのは表示の後なので、書き直しで同じ報告を2回読ませない。
  assert.match(
    inspectTurn({}, [ask, say('あ'.repeat(801))].join('\n')).block!.reason,
    /全文を書き直さず/,
  );
  assert.equal(
    inspectTurn({}, [ask, say('あ'.repeat(800))].join('\n')).block,
    null,
  );
});

test('読める報告: 結論と最後の質問1回の形は通す', () => {
  const ask = human('状況は？');
  const good =
    '案Aを起票しました（共通番号4568）。\n\n【質問分類：破壊的な操作】下書き2件を取り下げますか？ 戻せない理由：保存した記録が失われます\n\n- A: 残す（おすすめ）\n- B: 取り下げる\n\n「A」か「B」だけで答えられます。';
  assert.equal(inspectTurn({}, [ask, say(good)].join('\n')).block, null);
  assert.equal(
    inspectTurn(
      {},
      [ask, say(`結論です。\n\`\`\`\n${'x'.repeat(2000)}\n\`\`\``)].join('\n'),
    ).block,
    null,
  );
  assert.equal(
    inspectTurn({}, [ask, say('あ'.repeat(800))].join('\n')).block,
    null,
  );
});

test('5 呼び戻されない見張り: setsid・nohup・& で切り離した見張りだけの返答は止める', () => {
  for (const cmd of [
    '(setsid sh watch3.sh > out-watch3.txt 2>&1 &)',
    'nohup sh watch.sh > out.txt 2>&1 &',
    'while true; do ~/.issue-desk/desk desk-get.ts GET /api/issue?desk=1; sleep 60; done > out.txt 2>&1 &',
  ]) {
    const result = inspectTurn(
      {},
      turn('見張っておいて', [{ bash: cmd }, { text: long }]),
    );
    assert.match(result.block?.reason ?? '', /呼び戻す仕組みがありません/, cmd);
  }
});

test('5 呼び戻されない見張り: Monitor・run_in_background・monitor-watch があれば止めない', () => {
  const detached = '(setsid sh watch3.sh > out-watch3.txt 2>&1 &)';
  const ok: Step[][] = [
    [
      { bash: detached },
      {
        bash: 'until grep -q done out.txt; do sleep 1; done',
        background: true,
      },
      { text: long },
    ],
    [
      { bash: detached },
      { bash: 'node scripts/monitor-watch.ts --json x' },
      { text: long },
    ],
    [{ bash: 'echo 1 && ls' }, { text: long }],
    [{ bash: 'sleep 5 && echo done' }, { text: long }],
  ];
  for (const steps of ok) {
    const result = inspectTurn({}, turn('見張っておいて', steps));
    assert.doesNotMatch(
      result.block?.reason ?? '',
      /呼び戻す仕組みがありません/,
    );
  }
});

test('env 前置きの個別スクリプトは送信と重複検索に反映する', () => {
  const parse = (command: string) => [
    { words: command.split(' '), background: false },
  ];
  assert.equal(
    sendsToIntake('env X=1 node scripts/create-issue.ts --text x', parse),
    true,
  );
  assert.equal(
    sendsToIntake('env X=1 node scripts/create-issue.ts --check 12', parse),
    false,
  );
  const proposal =
    '【質問分類：範囲外の変更】改善案として修正依頼を送ってよいですか。';
  assert.equal(
    inspectTurn(
      {},
      turn('状況を教えて', [
        {
          bash: 'env node scripts/list-issues.ts --duplicates --project owner/repo --query test',
        },
        { text: proposal },
      ]),
    ).block,
    null,
  );
  assert.match(
    inspectTurn(
      {},
      turn('状況を教えて', [
        { bash: 'env X=1 node scripts/reply-issue.ts --text x' },
        { text: '確認しました。' },
      ]),
    ).block?.reason ?? '',
    /受付への送信/,
  );
});

test('シェルのスクリプト引数は共通の境界で判定する', () => {
  for (const [words, script, normalized] of [
    [
      ['env', 'X=1', 'bash', '-x', '--', '/x/desk-pc', 'get'],
      '/x/desk-pc',
      ['desk-pc', 'get'],
    ],
    [
      ['sh', '-o', 'errexit', '/x/other', 'get'],
      '/x/other',
      ['sh', '-o', 'errexit', '/x/other', 'get'],
    ],
    [['bash', '-c', 'desk get'], null, ['desk', 'get']],
    [['bash', '-o'], null, ['bash', '-o']],
  ] as [string[], string | null, string[]][]) {
    assert.equal(shellScript(words), script);
    assert.deepEqual(normalizeDeskArgs(words), normalized);
  }
});

test('desk-pc のスクリプト呼出しを送信と読取に数える', () => {
  const parse = (command: string) => [
    { words: command.split(' '), background: false },
  ];
  for (const command of [
    'sh ~/.issue-desk/desk-pc reply-issue.ts 12',
    '~/.issue-desk/desk-pc create-issue.ts --text x',
  ])
    assert.equal(sendsToIntake(command, parse), true, command);
  assert.equal(
    sendsToIntake('desk-pc create-issue.ts --check 12', parse),
    false,
  );
  const proposal =
    '【質問分類：範囲外の変更】改善案として修正依頼を送ってよいですか。';
  assert.equal(
    inspectTurn(
      {},
      turn('状況を教えて', [
        {
          bash: 'sh ~/.issue-desk/desk-pc list-issues.ts --duplicates --project owner/repo --query test',
        },
        { text: proposal },
      ]),
    ).block,
    null,
  );
});

test('env 前置きのテストと変更操作も質問の回に検出する', () => {
  for (const cmd of [
    'env node --test test/a.test.ts',
    'env -i X=1 /usr/bin/node --test a.ts',
  ])
    assert.match(
      inspectTurn({}, turn('今どうなってる？', [{ bash: cmd }, { text: long }]))
        .block?.reason ?? '',
      /回答より先に重い/,
      cmd,
    );
  for (const cmd of [
    'env git commit -m x',
    'env -u X /usr/bin/git -C /r push origin b',
    'env X=1 sed -i s/a/b/ a.go',
  ])
    assert.match(
      inspectTurn(
        {},
        turn('今どうなってる？', [
          { text: long },
          { bash: cmd },
          { text: long },
        ]),
      ).block?.reason ?? '',
      /ファイルの変更/,
      cmd,
    );
  for (const cmd of ['env node app.js', 'env git status', 'env sed -n 1p a.go'])
    assert.equal(
      inspectTurn({}, turn('今どうなってる？', [{ bash: cmd }, { text: long }]))
        .block,
      null,
      cmd,
    );
  assert.equal(
    inspectTurn(
      {},
      turn('今どうなってる？', [
        { bash: 'env node --test a.ts', background: true },
        { text: long },
      ]),
    ).block,
    null,
  );
});

test('重い前景処理は各コマンドを一度だけ正規化する', () => {
  for (const [words, expected] of [
    [['env', 'X=1', 'node', 'scripts/create-issue.ts'], true],
    [
      [
        'env',
        'timeout',
        '-k',
        '5',
        '30',
        'node',
        'scripts/desk-get.ts',
        'GET',
        '/api/issue',
      ],
      false,
    ],
    [
      [
        'timeout',
        '30',
        'env',
        'X=1',
        'node',
        'scripts/desk-get.ts',
        'GET',
        '/api/issue',
      ],
      false,
    ],
    [['timeout', '60', 'node', '--test', 'a.ts'], true],
  ] as const) {
    let calls = 0;
    assert.equal(
      heavyForeground(
        { type: 'tool_use', name: 'Bash', input: { command: 'ignored' } },
        () => [{ words: [...words], background: false }],
        (...args) => {
          calls++;
          return normalizeDeskArgs(...args);
        },
      ),
      expected,
    );
    assert.equal(calls, 1);
  }
});

test('バックグラウンド送信の出力は前置き付き読取でも確認済みになる', () => {
  const launch = {
    bash: 'node scripts/create-issue.ts --text x > /tmp/result.txt',
    background: true,
  };
  const report = { text: '起票しました。判断の記録: 出力を確認。' };
  const reason = (steps: Step[]) =>
    inspectTurn({}, turn('起票して', steps)).block?.reason ?? '';
  assert.match(reason([launch, report]), /結果を読まず/);
  for (const command of [
    'cat /tmp/result.txt',
    'env cat /tmp/result.txt',
    'env -i X=1 /usr/bin/tail /tmp/result.txt',
    "bash -c 'cat /tmp/result.txt'",
  ])
    assert.doesNotMatch(
      reason([launch, { bash: command }, report]),
      /結果を読まず/,
      command,
    );
  for (const command of [
    'sh head /tmp/result.txt',
    'bash cat /tmp/result.txt',
    'env X=1 /bin/sh head /tmp/result.txt',
  ])
    assert.match(
      reason([launch, { bash: command }, report]),
      /結果を読まず/,
      command,
    );
  assert.match(
    reason([launch, { bash: 'env cat /tmp/other.txt' }, report]),
    /結果を読まず/,
  );
  assert.match(
    reason([launch, report, { bash: 'env cat /tmp/result.txt' }]),
    /結果を読まず/,
  );
});

test('名前の一部が一致するスクリプトは受付の読取に数えない', () => {
  const prefix = [
    human('状況は？'),
    say(long),
    notified('【要対処】共通番号7: 状態=failed'),
  ];
  for (const script of ['x-create-issue', 'my-reply-issue']) {
    const command = `node scripts/${script}.ts --check abc`;
    assert.match(
      inspectTurn(
        {},
        [...prefix, ...tool('r', 'Bash', { command }), say(long)].join('\n'),
      ).block?.reason ?? '',
      /原因の読取/,
      command,
    );
    const launch = {
      bash: 'node scripts/create-issue.ts --draft abc',
      background: true,
    };
    assert.match(
      inspectTurn(
        {},
        turn('起票して', [
          launch,
          { bash: command },
          { text: '起票しました。' },
        ]),
      ).block?.reason ?? '',
      /結果を読まず/,
      command,
    );
  }
});

test('バックグラウンドの受付操作は結果を読むまで完了報告を止める', () => {
  for (const script of [
    'create-issue',
    'close-issue',
    'desk-ops',
    'reply-issue',
  ]) {
    const launch = {
      bash: `node scripts/${script}.ts --number 12 > /tmp/result.txt`,
      background: true,
    };
    const report = { text: '起票しました。' };
    const reason = (steps: Step[]) =>
      inspectTurn({}, turn('起票して', steps)).block?.reason ?? '';
    assert.match(reason([launch, report]), /結果を読まず/, script);
    assert.doesNotMatch(
      reason([launch, { bash: 'cat /tmp/result.txt' }, report]),
      /結果を読まず/,
      script,
    );
  }
  const similar = {
    bash: 'node scripts/x-reply-issue.ts --number 12',
    background: true,
  };
  assert.doesNotMatch(
    inspectTurn({}, turn('起票して', [similar, { text: '起票しました。' }]))
      .block?.reason ?? '',
    /結果を読まず/,
  );
});

test('env 前置きの重い前景処理は質問への回答より先に止める', () => {
  for (const cmd of [
    'env npm test',
    'env X=1 go test ./...',
    'env sleep 60',
    'env pnpm test',
    'env yarn test',
    'env docker build .',
    'env docker-compose up',
    'env timeout 60 command',
    'env while true; do sleep 1; done',
  ])
    assert.match(
      inspectTurn({}, turn('今どうなってる？', [{ bash: cmd }, { text: long }]))
        .block?.reason ?? '',
      /回答より先に重い/,
      cmd,
    );
  for (const cmd of [
    'env npm install',
    'env go version',
    'env sleep 5',
    'env docker ps',
  ])
    assert.equal(
      inspectTurn({}, turn('今どうなってる？', [{ bash: cmd }, { text: long }]))
        .block,
      null,
      cmd,
    );
  assert.equal(
    inspectTurn(
      {},
      turn('今どうなってる？', [
        { bash: 'env npm test', background: true },
        { text: long },
      ]),
    ).block,
    null,
  );
});

test('シェルのスクリプト引数は窓口操作や送信と認識しない', () => {
  const parse = (command: string) => [
    { words: command.split(' '), background: false },
  ];
  for (const command of [
    'sh create-issue.ts --draft x',
    'bash scripts/create-issue.ts --draft x',
    'env X=1 sh other operation',
  ]) {
    assert.equal(sendsToIntake(command, parse), false, command);
    const result = inspectTurn(
      {},
      turn('今どうなってる？', [{ bash: command }, { text: long }]),
    );
    assert.doesNotMatch(
      result.block?.reason ?? '',
      /受付への送信|回答より先に重い/,
      command,
    );
  }
  const launch = { bash: 'sh create-issue.ts --draft x', background: true };
  assert.doesNotMatch(
    inspectTurn({}, turn('起票して', [launch, { text: '起票しました。' }]))
      .block?.reason ?? '',
    /結果を読まず/,
  );
});

test('前置き付きの変更・切り離し・重い処理を判定する', () => {
  const parse = (command: string) => [
    { words: command.split(' '), background: false },
  ];
  const bash = (command: string) => ({
    type: 'tool_use',
    name: 'Bash',
    input: { command },
  });
  assert.equal(changesWork(bash('env X=1 git commit -m x'), parse), true);
  assert.equal(selfDetaching('env X=1 nohup node w.mjs'), true);
  assert.match(
    inspectTurn(
      {},
      turn('作業して', [{ bash: 'env X=1 nohup node w.mjs' }, { text: long }]),
    ).block?.reason ?? '',
    /見張りがありません/,
  );
  assert.equal(heavyForeground(bash('sh scripts/verify.sh'), parse), true);
  assert.equal(heavyForeground(bash('env X=1 sleep 60'), parse), true);
});

test('シェルのオプションと -c 内の各コマンドを実際の会話記録から判定する', () => {
  for (const cmd of [
    "bash -x -c 'echo ok; env X=1 git commit -m x'",
    "env X=1 sh -c 'git commit -m x'",
    'bash -x scripts/verify.sh',
  ]) {
    const reason =
      inspectTurn({}, turn('今どうなってる？', [{ bash: cmd }, { text: long }]))
        .block?.reason ?? '';
    assert.match(reason, /ファイルの変更|回答より先に重い/, cmd);
  }
  assert.equal(
    selfDetaching("bash -c 'echo ok; env X=1 nohup node w.mjs'"),
    true,
  );
  assert.equal(selfDetaching("bash -c 'echo nohup'"), false);
  assert.match(
    inspectTurn(
      {},
      turn('今どうなってる？', [
        { bash: "bash -c 'env X=1 sleep 60'" },
        { text: long },
      ]),
    ).block?.reason ?? '',
    /回答より先に重い/,
  );
  assert.match(
    inspectTurn(
      {},
      turn('状況を教えて', [
        { bash: "bash -c 'node scripts/reply-issue.ts --text x'" },
        { text: long },
      ]),
    ).block?.reason ?? '',
    /受付への送信/,
  );
});

test('実際のシェル解析を通して重い前景処理を判定する', () => {
  for (const [command, expected] of [
    ["bash -c 'echo ok; sleep 60'", true],
    ["bash -c 'sleep 60 &'", false],
    ["bash -c 'echo ok; sleep 5'", false],
    ['sh scripts/verify.sh', true],
  ] as const) {
    assert.equal(
      heavyForeground(
        { type: 'tool_use', name: 'Bash', input: { command } },
        parseCommands,
      ),
      expected,
      command,
    );
  }
});

test('位置引数を変えるオプションと引数付き結合オプションを認識しない', () => {
  for (const option of [
    '-s',
    '-i',
    '-xs',
    '-sc',
    '-cs',
    '-oc',
    '-co',
    '-Oc',
    '-cO',
  ]) {
    assert.equal(
      deskCommand(['bash', option, 'desk', 'operation']),
      null,
      option,
    );
    assert.equal(deskCommand(['bash', option, 'desk operation']), null, option);
  }
  assert.equal(deskCommand(['bash', '-xc', 'desk operation']), 'operation');
  assert.equal(
    deskCommand(['bash', '-o', 'errexit', 'desk', 'operation']),
    'operation',
  );
});

test('対話オプションと -c を組み合わせた送信や前景処理を検出する', () => {
  for (const option of ['-ic', '-ci', '-xic', '-i -c', '-i -x -c']) {
    const send = `bash ${option} 'node scripts/reply-issue.ts --text x'`;
    assert.match(
      inspectTurn({}, turn('状況を教えて', [{ bash: send }, { text: long }]))
        .block?.reason ?? '',
      /受付への送信/,
      send,
    );
    const heavy = `bash ${option} 'sleep 60'`;
    assert.match(
      inspectTurn(
        {},
        turn('今どうなってる？', [{ bash: heavy }, { text: long }]),
      ).block?.reason ?? '',
      /回答より先に重い/,
      heavy,
    );
    assert.equal(
      deskCommand(['bash', ...option.split(' '), 'desk operation']),
      'operation',
      option,
    );
  }
  for (const option of ['-o', '-O', '+o', '+O']) {
    const command = `bash ${option} errexit -c 'node scripts/reply-issue.ts --text x'`;
    assert.match(
      inspectTurn({}, turn('状況を教えて', [{ bash: command }, { text: long }]))
        .block?.reason ?? '',
      /受付への送信/,
      command,
    );
  }
});

test('どちらの公開入口を先に読み込んでも検査を利用できる', () => {
  for (const first of ['chat-turn-checks', 'chat-stop-hook']) {
    const script = `const first = await import('./scripts/${first}.ts'); const hook = await import('./scripts/chat-stop-hook.ts'); if (!hook.stopCheckRegistry['irreversible-question'].text || !first) throw new Error('missing exports');`;
    const result = spawnSync(
      process.execPath,
      ['--input-type=module', '-e', script],
      { cwd: new URL('..', import.meta.url), encoding: 'utf8' },
    );
    assert.equal(result.status, 0, result.stderr);
  }
});

test('一時領域: /tmp と clone した作業ツリーの .tmp/ だけを一時領域と数える', () => {
  for (const file of [
    '/tmp/a',
    '/home/u/repo/.tmp/a.md',
    '/home/u/rc/x/.tmp/d/b.log',
  ])
    assert.equal(scratchPath(file, '/home/u'), true, file);
  for (const file of [
    '/home/u/a.tmp/x.go',
    '/home/u/.tmp/x',
    '/home/u/Workspace/issue-desk-intake/.tmp/x',
    '/r/.git/.tmp/x',
    '/home/u/repo/.tmp/../src/a.go',
    '/tmp/../home/u/a.go',
    'repo/.tmp/a',
    42,
  ])
    assert.equal(scratchPath(file, '/home/u'), false, String(file));
});

test('送信・重い前景は改名前の .mjs の名前でも数え、汎用GETの読取は .ts の名前だけを数える', () => {
  const parse = (command: string) => [
    { words: command.split(' '), background: false },
  ];
  for (const ext of ['ts', 'mjs']) {
    const counted = ext === 'ts';
    assert.equal(
      sendsToIntake(`node scripts/create-issue.${ext} --text x`, parse),
      true,
      ext,
    );
    assert.equal(
      sendsToIntake(`node scripts/create-issue.${ext} --check 12`, parse),
      false,
      ext,
    );
    assert.equal(
      sendsToIntake(`~/.issue-desk/desk-pc reply-issue.${ext} 12`, parse),
      true,
      ext,
    );
    assert.equal(
      sendsToIntake(`desk desk-ops.${ext} notice`, parse),
      true,
      ext,
    );
    assert.equal(sendsToIntake(`desk desk-ops.${ext} list`, parse), false, ext);
    assert.equal(
      onlyFilesIssue(`node scripts/create-issue.${ext} --text x`, parse),
      true,
      ext,
    );
    assert.equal(isIssueScript(`scripts/reply-issue.${ext}`), true, ext);
    assert.equal(
      heavyForeground(
        {
          name: 'Bash',
          input: { command: `node scripts/create-issue.${ext} --text x` },
        } as any,
        parse,
      ),
      true,
      ext,
    );
    assert.equal(
      normalizedDeskCommand(
        normalizeDeskArgs([
          'node',
          `scripts/desk-get.${ext}`,
          'GET',
          '/api/issues',
        ]),
      ),
      counted ? 'get' : null,
      ext,
    );
    assert.equal(
      deskIssueRoute([
        'node',
        `scripts/desk-get.${ext}`,
        'GET',
        '/api/issue?desk=1',
      ]),
      counted ? 'read' : null,
      ext,
    );
  }
  assert.equal(
    inspectTurn(
      {},
      turn('状況を教えて', [
        {
          bash: 'env node scripts/list-issues.ts --duplicates --project owner/repo --query test',
        },
        {
          text: '【質問分類：範囲外の変更】改善案として修正依頼を送ってよいですか。',
        },
      ]),
    ).block,
    null,
  );
  assert.equal(
    sendsToIntake('node scripts/create-issue.js --text x', parse),
    false,
  );
  assert.equal(isIssueScript('scripts/reply-issue.tsx'), false);
});
