import assert from 'node:assert/strict';
import test from 'node:test';
import { inspectTurn } from '../scripts/chat-stop-hook.ts';

test('TODO開始の窓口操作は質問だけの回で送信を止め、取得は止めない', () => {
  for (const [command, blocked] of [
    ['sh ~/.issue-desk/desk-pc get --path /api/todos/123', false],
    [
      'sh ~/.issue-desk/desk-pc operation --method POST --path /api/todos/123/start --body-file -',
      true,
    ],
    [
      '~/.issue-desk/desk operation --method POST --path /api/todos/123/start --body-file -',
      true,
    ],
  ] as const) {
    const transcript = [
      { type: 'user', message: { content: '状況を教えて' } },
      {
        type: 'assistant',
        message: {
          content: [{ type: 'tool_use', name: 'Bash', input: { command } }],
        },
      },
      {
        type: 'assistant',
        message: { content: [{ type: 'text', text: '状況を確認しました。' }] },
      },
    ]
      .map((value) => JSON.stringify(value))
      .join('\n');
    assert.equal(
      /受付への送信/.test(inspectTurn({}, transcript).block?.reason ?? ''),
      blocked,
      command,
    );
  }
});
