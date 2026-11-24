import assert from 'node:assert/strict';
import test from 'node:test';
import { inspectTurn, sendsToIntake } from '../scripts/chat-stop-hook.ts';
import {
  commandBasename,
  deskCommand,
  deskIssueRoute,
} from '../scripts/chat-turn-checks.ts';

function turn(user: string, command: string, reply: string): string {
  return [
    { type: 'user', message: { content: user } },
    {
      type: 'assistant',
      message: {
        content: [
          { type: 'tool_use', id: 't', name: 'Bash', input: { command } },
        ],
      },
    },
    {
      type: 'user',
      message: {
        content: [{ type: 'tool_result', tool_use_id: 't', content: '{}' }],
      },
    },
    {
      type: 'assistant',
      message: { content: [{ type: 'text', text: reply }] },
    },
  ]
    .map((value) => JSON.stringify(value))
    .join('\n');
}
const proposal =
  '【質問分類：範囲外の変更】改善案として修正依頼を送ってよいですか。';
const searches = [
  "~/.issue-desk/desk get --path '/api/issues/search?project=a%2Fb&state=open&q=x'",
  '~/.issue-desk/desk-pc get --path=/api/issues/search?page=2',
  "sh ~/.issue-desk/desk-pc get --path '/api/issues/search?limit=1'",
  'bash ~/.issue-desk/desk get --path /api/issues/search',
  'env MODE=test bash ~/.issue-desk/desk-pc get --path /api/issues/search',
  "node scripts/desk-get.ts GET '/api/issues/search?q=x'",
  "~/.issue-desk/desk desk-get.ts GET '/api/issues/search?q=x'",
  "scripts/desk-get.ts GET '/api/issues/search?q=x'",
];
test('desk-pc のスクリプト送信は実パーサーを通して質問だけの回を止める', () => {
  const result = inspectTurn(
    {},
    turn(
      '状況を教えて',
      'sh ~/.issue-desk/desk-pc reply-issue.ts 12',
      '状況を確認しました。',
    ),
  );
  assert.match(result.block?.reason ?? '', /受付への送信/);
  assert.ok(
    result.fires.some(
      (fire) => fire.id === 'unasked-send' && fire.result === 'block',
    ),
  );
});
test('窓口コマンドの返り値は実行形式と操作を識別する', () => {
  const cases: [string[], 'operation' | 'get' | null][] = [
    [['desk', 'operation', 'create'], 'operation'],
    [['desk-pc', 'operation', 'reply'], 'operation'],
    [['/home/user/.issue-desk/desk', 'get'], 'get'],
    [['sh', '/home/user/.issue-desk/desk-pc', 'get'], 'get'],
    [['bash', '/home/user/.issue-desk/desk', 'operation'], 'operation'],
    [['env', 'A=1', 'bash', '/home/user/.issue-desk/desk-pc', 'get'], 'get'],
    [['env', '-i', '-u', 'A', '--', 'desk', 'operation'], 'operation'],
    [['node', 'scripts/desk-get.ts', 'GET', '/api/drafts'], 'get'],
    [['desk', 'scripts/desk-get.ts', 'GET', '/api/drafts'], 'get'],
    [['scripts/desk-get.ts', 'GET'], 'get'],
    [['desk-get.ts', 'POST'], null],
    [['desk', 'doctor'], null],
    [['bash', '-x', 'desk', 'operation'], 'operation'],
    [['echo', 'desk', 'operation'], null],
    [['bash', '-c', 'desk operation'], 'operation'],
    [[], null],
  ];
  for (const [words, expected] of cases)
    assert.equal(deskCommand(words), expected, words.join(' '));
});

test('env とシェル経由の窓口を識別する', () => {
  assert.match(
    inspectTurn(
      {},
      turn(
        '状況を教えて',
        'env MODE=test desk operation create',
        '状況を確認しました。',
      ),
    ).block?.reason ?? '',
    /受付への送信/,
  );
  for (const command of [
    'sh ~/.issue-desk/desk-pc operation create',
    'bash ~/.issue-desk/desk operation create',
    'env MODE=test bash ~/.issue-desk/desk-pc operation reply',
  ])
    assert.match(
      inspectTurn({}, turn('状況を教えて', command, '状況を確認しました。'))
        .block?.reason ?? '',
      /受付への送信/,
      command,
    );
  assert.match(
    inspectTurn(
      {},
      turn(
        '状況を教えて',
        'env node scripts/desk-get.ts GET /api/issues/search',
        proposal,
      ),
    ).block?.reason ?? '',
    /既にないか/,
  );
  assert.equal(
    inspectTurn(
      {},
      turn(
        '状況を教えて',
        'env node scripts/list-issues.ts --duplicates --project a/b --query x',
        proposal,
      ),
    ).block,
    null,
  );
  assert.equal(
    deskIssueRoute([
      'env',
      'A=1',
      'bash',
      '/x/desk',
      'get',
      '--path',
      '/api/issue?desk=12',
    ]),
    'read',
  );
  assert.equal(
    deskIssueRoute([
      'env',
      'node',
      'scripts/desk-get.ts',
      'POST',
      '/api/issues/search',
    ]),
    null,
  );
});
test('重複照合の入口は起票提案を通しIssueだけの読取は止める', () => {
  for (const command of [
    'node scripts/list-issues.ts --duplicates --project a/b --query x',
    'env MODE=test node scripts/list-issues.ts --project a/b --query x --duplicates',
    'sh ~/.issue-desk/desk-pc list-issues.ts --duplicates --project a/b --query x',
  ])
    assert.equal(
      inspectTurn({}, turn('状況を教えて', command, proposal)).block,
      null,
      command,
    );
  for (const command of [
    ...searches,
    'echo hello',
    'desk get --path /api/drafts',
    'desk get --path /api/issues/searching',
    'desk get --path /api/issue?desk=1',
    'node scripts/desk-get.ts POST /api/issues/search',
    'node scripts/desk-get.ts GET /api/issues/search',
    'node scripts/desk-get.ts GET /api/issue?desk=1',
    'node scripts/list-issues.ts --duplicates --query x',
    'node scripts/list-issues.ts --duplicates --project a/b',
  ])
    assert.match(
      inspectTurn({}, turn('状況を教えて', command, proposal)).block?.reason ??
        '',
      /既にないか/,
      command,
    );
});
test('読取は放置の検査を回避せず送信にも数えない', () => {
  for (const command of [
    ...searches,
    "sh ~/.issue-desk/desk-pc get --path '/api/issue?desk=12'",
    'node scripts/desk-get.ts GET /api/issue?desk=12',
  ]) {
    const report = '共通番号 12 は上限で停止しています。私が面倒を見ています。';
    assert.match(
      inspectTurn({}, turn('12を任せたよ', command, report)).block?.reason ??
        '',
      /具体的な対処/,
      command,
    );
    assert.equal(
      sendsToIntake(command, () => [
        { words: command.split(' '), background: false },
      ]),
      false,
      command,
    );
  }
});
test('経路判定は対象と完全一致し GET と窓口を確認する', () => {
  assert.equal(
    deskIssueRoute(['desk', 'get', '--path', '/api/issue?desk=1']),
    'read',
  );
  assert.equal(
    deskIssueRoute(['desk-pc', 'get', '--path=/api/issues/search']),
    'search',
  );
  for (const words of [
    ['desk', 'get', '--path', '/api/drafts'],
    ['echo', 'get', '--path', '/api/issues/search'],
    ['node', 'desk-get.ts', 'POST', '/api/issues/search'],
    ['desk', 'get', '--path', '/api/issue/1'],
  ])
    assert.equal(deskIssueRoute(words), null);
});

test('窓口の operation は送信として質問だけの回を止める', () => {
  for (const command of [
    'desk operation create',
    'desk-pc operation reply',
    '~/.issue-desk/desk operation retry',
    '~/.issue-desk/desk-pc operation close',
    'node scripts/reply-issue.ts 12',
  ]) {
    assert.match(
      inspectTurn({}, turn('状況を教えて', command, '状況を確認しました。'))
        .block?.reason ?? '',
      /受付への送信/,
      command,
    );
  }
  for (const command of [
    'desk doctor',
    'desk get --path /api/drafts',
    'desk-pc doctor',
    'desk-pc get --path /api/drafts',
    'sh ~/.issue-desk/desk-pc get --path /api/drafts',
    'echo desk operation',
    'node scripts/create-issue.ts --check 12',
  ]) {
    assert.doesNotMatch(
      inspectTurn({}, turn('状況を教えて', command, '状況を確認しました。'))
        .block?.reason ?? '',
      /受付への送信/,
      command,
    );
  }
});

test('任された停止の報告では全経路の get を読取に数える', () => {
  const reply =
    '共通番号 12 は停止中です。【質問分類：新しい権限】対処してよいですか？';
  for (const command of [
    ...searches,
    'desk get --path /api/drafts',
    'desk-pc get --path /api/other',
    '~/.issue-desk/desk get --path /anything',
    '~/.issue-desk/desk-pc get --path /anything',
    'sh ~/.issue-desk/desk-pc get --path /anything',
    'node scripts/desk-get.ts GET /api/drafts',
    'scripts/desk-get.ts GET /anything',
    'node scripts/desk-get.ts GET /api/issue?desk=12',
    'sh ~/.issue-desk/desk-pc desk-get.ts GET /api/issue?desk=12',
    'desk-pc desk-get.ts GET /api/drafts',
  ]) {
    assert.equal(
      inspectTurn({}, turn('12を任せたよ', command, reply)).block,
      null,
      command,
    );
  }
  for (const command of [
    'desk doctor',
    'desk-pc doctor',
    'desk-get.ts POST /api/drafts',
    'echo desk get --path /api/drafts',
  ]) {
    assert.match(
      inspectTurn({}, turn('12を任せたよ', command, reply)).block?.reason ?? '',
      /具体的な対処/,
      command,
    );
  }
});

test('実行ファイル名は両方の区切り文字で共通に取り出す', () => {
  assert.equal(commandBasename('/usr/bin/node'), 'node');
  assert.equal(commandBasename('C:\\tools\\desk'), 'desk');
  assert.equal(commandBasename(undefined), '');
  assert.equal(deskCommand(['C:\\tools\\desk', 'operation']), 'operation');
});
