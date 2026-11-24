import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  checkedFires,
  inspectTurn,
  registeredCheck,
  stopCheckRegistry,
} from '../scripts/chat-stop-hook.ts';
import type { StopCheckId } from '../scripts/chat-stop-hook.ts';

// コンパイラーが返却値の ID を registry のキーと一致する型として検査する。
type FireId = ReturnType<typeof inspectTurn>['fires'][number]['id'];
type SameId = [FireId] extends [StopCheckId]
  ? [StopCheckId] extends [FireId]
    ? true
    : false
  : false;
const fireIdIsStopCheckId: SameId = true;
void fireIdIsStopCheckId;

const ledger = JSON.parse(
  readFileSync(
    new URL('../server/internal/statistics/stop-checks.json', import.meta.url),
    'utf8',
  ),
);
const validDate = (s: unknown) =>
  typeof s === 'string' &&
  /^\d{4}-\d{2}-\d{2}$/.test(s) &&
  !Number.isNaN(Date.parse(s)) &&
  new Date(s).toISOString().slice(0, 10) === s;
function validate(rows: any[], registry = stopCheckRegistry) {
  assert.ok(Array.isArray(rows));
  const ids = new Set<string>();
  for (const row of rows) {
    assert.ok(
      typeof row.id === 'string' && row.id.length > 0 && !ids.has(row.id),
      'duplicate or empty ID',
    );
    ids.add(row.id);
    assert.ok(['hook', 'guard'].includes(row.kind));
    assert.ok(typeof row.purpose === 'string' && row.purpose.trim());
    assert.ok(
      typeof row.introductionNote === 'string' && row.introductionNote.trim(),
    );
    assert.ok(
      validDate(row.introducedAt) && validDate(row.nextReviewAt),
      'invalid date',
    );
    assert.equal(typeof row.counted, 'boolean');
    assert.ok(Array.isArray(row.reviews));
    for (const review of row.reviews) {
      assert.ok(validDate(review.date), 'invalid review date');
      assert.ok(
        ['残す', '直す', '外す'].includes(review.result),
        'invalid review result',
      );
      assert.equal(typeof review.note, 'string');
    }
    if (row.counted)
      assert.ok(
        row.kind === 'hook' && Object.hasOwn(registry, row.id),
        'unused counted ID',
      );
  }
  for (const id of Object.keys(registry))
    assert.ok(
      rows.some((row) => row.id === id && row.kind === 'hook' && row.counted),
      `missing registered ID: ${id}`,
    );
}

test('実際の台帳と登録が一致し不正な台帳を拒否する', () => {
  validate(ledger);
  assert.throws(() => validate(ledger.slice(1)), /missing registered ID/);
  assert.throws(
    () => validate([...ledger, { ...ledger[0], id: 'unused' }]),
    /unused counted ID/,
  );
  assert.throws(() => validate([...ledger, ledger[0]]), /duplicate/);
  for (const date of ['2026-2-01', '2026-02-30'])
    assert.throws(
      () =>
        validate([{ ...ledger[0], nextReviewAt: date }, ...ledger.slice(1)]),
      /invalid date/,
    );
  assert.throws(
    () =>
      validate([
        {
          ...ledger[0],
          reviews: [{ date: '2026-11-05', result: '保留', note: '' }],
        },
        ...ledger.slice(1),
      ]),
    /invalid review result/,
  );
  assert.throws(
    () =>
      validate([
        { ...ledger[0], reviews: [{ date: 'bad', result: '残す', note: '' }] },
        ...ledger.slice(1),
      ]),
    /invalid review date/,
  );
});

test('実際の発動一覧は登録済みIDと結果を返す', () => {
  const transcript = (user: string, parts: unknown[]) =>
    [
      { type: 'user', message: { content: user } },
      { type: 'assistant', message: { content: parts } },
    ]
      .map((x) => JSON.stringify(x))
      .join('\n');
  const cases = [
    inspectTurn(
      {},
      transcript('今どうなってる？', [
        {
          type: 'tool_use',
          name: 'Bash',
          input: { command: 'node --test a.ts' },
        },
        { type: 'text', text: '確認してください。' },
      ]),
    ),
    inspectTurn(
      {},
      transcript('進めて', [{ type: 'text', text: 'すみません。' }]),
    ),
    inspectTurn(
      {},
      transcript('進めて', [{ type: 'text', text: '担当外を調べます。' }]),
    ),
    inspectTurn({}, ''),
  ];
  assert.ok(cases.some((x) => x.fires.some((f) => f.result === 'block')));
  assert.ok(cases.some((x) => x.fires.some((f) => f.result === 'warn')));
  for (const result of cases)
    for (const fire of result.fires) {
      assert.ok(
        Object.hasOwn(stopCheckRegistry, fire.id),
        `unregistered fire: ${fire.id}`,
      );
      assert.ok(['block', 'warn'].includes(fire.result));
    }
  assert.deepEqual(cases.at(-1)!.fires, []);
});

const user = (text: string) => ({ type: 'user', message: { content: text } });
const assistant = (...parts: object[]) => ({
  type: 'assistant',
  message: { content: parts },
});
const text = (value: string) => ({ type: 'text', text: value });
const bash = (command: string, extra: object = {}) => ({
  type: 'tool_use',
  id: 'call',
  name: 'Bash',
  input: { command, ...extra },
});
const result = (value: string) => ({
  type: 'user',
  message: {
    content: [{ type: 'tool_result', tool_use_id: 'call', content: value }],
  },
});
const transcriptOf = (...entries: object[]) =>
  entries.map((entry) => JSON.stringify(entry)).join('\n');

test('全検査を実際の会話で発動し登録済みIDを網羅する', () => {
  const cases: Record<string, object[]> = {
    'unasked-send': [
      user('状況は？'),
      assistant(bash('node scripts/reply-issue.ts --number 3 --text x')),
    ],
    'unasked-work': [
      user('状況は？'),
      assistant({ type: 'tool_use', name: 'Edit', input: { file_path: 'a' } }),
    ],
    'attention-unread': [
      {
        type: 'attachment',
        attachment: {
          type: 'queued_command',
          commandMode: 'task-notification',
          prompt: '【要対処】',
        },
      },
      assistant(text('確認します。')),
    ],
    'answer-first': [user('状況は？'), assistant(bash('node --test a.ts'))],
    'silent-watch': [user('進めて'), assistant(bash('nohup watch x &'))],
    'stall-unhandled': [user('任せて'), assistant(text('依頼は停止中です。'))],
    'foreground-repeat': [
      user('進めて'),
      assistant(bash('node --test a.ts'), bash('node --test b.ts')),
    ],
    'apology-prevention': [user('進めて'), assistant(text('すみません。'))],
    scope: [user('進めて'), assistant(text('担当外を調べます。'))],
    'filing-question': [
      user('進めて'),
      assistant(text('【質問分類：新しい権限】起票してよいですか。')),
    ],
    'duplicate-search': [
      user('進めて'),
      assistant(text('起票してよいですか。')),
    ],
    'unwatched-background': [
      user('進めて'),
      assistant(bash('nohup node long.js > /tmp/out &')),
    ],
    'authorized-question': [
      user('直接PRで直して'),
      assistant(
        text(
          '【質問分類：取り返しのつかない操作】PRをマージしますか？ 戻せない理由：公開情報が第三者に保存されます',
        ),
      ),
    ],
    'stop-question': [
      user('進めて'),
      assistant(text('【質問分類：破壊的な操作】5428を停止しますか？')),
    ],
    'irreversible-question': [
      user('進めて'),
      assistant(text('【質問分類：破壊的な操作】削除しますか？')),
    ],
    'unnecessary-question': [user('進めて'), assistant(text('どうしますか？'))],
    'report-length': [user('進めて'), assistant(text('あ'.repeat(801)))],
    'report-watch-note': [
      user('進めて'),
      assistant(text('裏で動かしている処理はありません。')),
    ],
    'report-question-position': [
      user('進めて'),
      assistant(text('【質問分類：新しい権限】実行しますか？\n完了しました。')),
    ],
    'decision-log': [
      user('直して'),
      assistant(
        { type: 'tool_use', name: 'Edit', input: { file_path: 'a' } },
        text('完了しました。'),
      ),
    ],
    'filing-watch': [
      user('直して'),
      assistant(bash('node scripts/reply-issue.ts --text x')),
      result('修正指示を送りました: a/b#40'),
      assistant(text('完了しました。')),
    ],
    'watch-rearm': [
      {
        type: 'attachment',
        attachment: {
          type: 'queued_command',
          commandMode: 'task-notification',
          prompt: '【張り直し】',
        },
      },
      assistant(text('確認します。')),
    ],
    'denied-manual': [
      user('進めて'),
      assistant(bash('do-it')),
      result('Permission for this action was denied'),
      assistant(text('実行してください。\n```\ncommand\n```')),
    ],
    'unread-operation': [
      user('進めて'),
      assistant(
        bash('node scripts/create-issue.ts', { run_in_background: true }),
        text('起票しました。'),
      ),
    ],
    'waiting-reference': [
      {
        type: 'attachment',
        attachment: {
          type: 'queued_command',
          commandMode: 'task-notification',
          prompt: '【完了】共通番号1: 状態=completed',
        },
      },
      assistant(
        text(
          '共通番号1が完了しました。主因の修正は、表示済みの質問1の回答待ちのままです。',
        ),
      ),
    ],
    'instruction-wait': [
      user('進めて'),
      assistant(text('指示があれば進めます。')),
    ],
    'question-format': [user('進めて'), assistant(text('どうしますか？'))],
    'permission-settings': [
      user('進めて'),
      assistant(text('許可設定を追加してください。')),
    ],
    'manual-request': [
      user('進めて'),
      assistant(text('実行してください。\n```\ncommand\n```')),
    ],
    'investigation-before-answer': [
      user('進めて'),
      assistant(...Array.from({ length: 8 }, () => bash('ls'))),
    ],
  };
  const fixed = Object.entries(stopCheckRegistry)
    .filter(([, entry]) => entry.kind === 'fixed')
    .map(([id]) => id);
  assert.ok(fixed.includes('unread-operation'));
  const all = new Set<string>();
  for (const [id, entries] of Object.entries(cases)) {
    const inspected = inspectTurn({}, transcriptOf(...entries));
    const fires = inspected.fires;
    if (fixed.includes(id))
      assert.ok(
        (inspected.block?.reason ?? '').includes(
          stopCheckRegistry[id as keyof typeof stopCheckRegistry].text,
        ) ||
          (inspected.warning ?? '').includes(
            stopCheckRegistry[id as keyof typeof stopCheckRegistry].text,
          ),
        id,
      );
    assert.ok(
      fires.some((fire) => fire.id === id),
      `${id}: ${JSON.stringify(fires)}`,
    );
    for (const fire of fires) {
      assert.ok(
        fire.id && Object.hasOwn(stopCheckRegistry, fire.id),
        `invalid ID: ${fire.id}`,
      );
      all.add(fire.id);
    }
  }
  assert.deepEqual([...all].sort(), Object.keys(stopCheckRegistry).sort());
});

test('未登録や空のIDを発動前に拒否する', () => {
  const warnings: string[] = [];
  const original = console.error;
  console.error = (...args: unknown[]) => {
    warnings.push(args.join(' '));
  };
  try {
    assert.equal(registeredCheck('未登録の検査文言'), null);
    assert.match(warnings[0], /unregistered stop check message/);
  } finally {
    console.error = original;
  }
  assert.throws(
    () => checkedFires([{ id: '' as never, result: 'block' }]),
    /invalid stop check ID/,
  );
  assert.throws(
    () => checkedFires([{ id: 'unknown' as never, result: 'warn' }]),
    /invalid stop check ID/,
  );
});

test('固定の許可設定警告は登録文言と実際の出力が一致する', () => {
  for (const reply of [
    '許可設定を追加してください。',
    'すみません。許可設定を追加してください。',
  ]) {
    const inspected = inspectTurn(
      {},
      transcriptOf(user('進めて'), assistant(text(reply))),
    );
    assert.ok(
      inspected.fires.some((fire) => fire.id === 'permission-settings'),
    );
    assert.ok(
      inspected.warning?.endsWith(
        stopCheckRegistry['permission-settings'].text,
      ),
    );
    if (reply.startsWith('すみません'))
      assert.equal(
        inspected.warning,
        stopCheckRegistry['permission-settings'].text,
      );
  }
});

test('未登録の turn 検査は警告し他のブロックを保つ', () => {
  const entry = stopCheckRegistry['unasked-send'] as {
    kind: string;
    text: string;
  };
  const originalText = entry.text;
  const originalError = console.error;
  const warnings: string[] = [];
  console.error = (...args: unknown[]) => {
    warnings.push(args.join(' '));
  };
  try {
    entry.text = '登録から外した文言';
    const inspected = inspectTurn(
      {},
      transcriptOf(
        user('状況は？'),
        assistant(
          bash('node scripts/reply-issue.ts --number 3 --text x'),
          text('担当外を調べます。'),
        ),
      ),
    );
    assert.ok(inspected.fires.some((fire) => fire.id === 'scope'));
    assert.ok(inspected.block?.reason.includes(stopCheckRegistry.scope.text));
    assert.ok(!inspected.fires.some((fire) => fire.id === 'unasked-send'));
    assert.ok(
      warnings.some((warning) =>
        warning.includes('unregistered stop check message'),
      ),
    );
  } finally {
    entry.text = originalText;
    console.error = originalError;
  }
});
