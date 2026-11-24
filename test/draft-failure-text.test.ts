import test from 'node:test';
import assert from 'node:assert/strict';
import {
  checkDraft,
  draftFailureText,
  filingCheckSummaryText,
} from '../scripts/desk-client.ts';

test('止まった相談は段階・原因・最後の診断を表示する', () => {
  const text = draftFailureText({
    error: '相談の回答を確認できませんでした。',
    lastFailure: {
      at: '2026-11-03T00:00:00Z',
      stage: '資料の読取',
      cause: 'permission denied',
      kind: 'execution',
    },
    aiDiagnostics: [
      { id: 'abc', kind: 'exit_nonzero', stage: 'exit', exitCode: 1 },
    ],
  });
  assert.match(text, /段階 資料の読取・種類 execution/);
  assert.match(text, /permission denied/);
  assert.match(text, /exit_nonzero.*終了コード 1.*診断ID abc/);
});

test('解決した相談の古い失敗と、原因の無い古い記録は表示しない', () => {
  assert.equal(draftFailureText({ lastFailure: { cause: '古い原因' } }), '');
  assert.equal(
    draftFailureText({ error: 'x', lastFailure: { at: 'a', message: 'm' } }),
    '',
  );
  assert.equal(draftFailureText({ error: 'x' }), '');
});

test('診断行の不正な値と欠落した終了コードを区別する', () => {
  const draft = { error: '停止', lastFailure: { cause: '原因' } };
  const render = (diagnostic) =>
    draftFailureText({ ...draft, aiDiagnostics: [diagnostic] });
  const broken = render({ id: 'valid', kind: {}, stage: {}, exitCode: {} });
  assert.match(
    broken,
    /診断: 不明（段階 不明・終了コード 不明・診断ID valid）/,
  );
  assert.ok(!broken.includes('[object Object]'));
  for (const exitCode of [undefined, null]) {
    assert.match(
      render({ id: 0, kind: '', stage: false, exitCode }),
      /診断: 不明（段階 不明・終了コード なし・診断ID 0）/,
    );
  }
  assert.match(
    render({ id: 'valid', kind: 0, stage: 'exit', exitCode: 0 }),
    /診断: 0（段階 exit・終了コード 0・診断ID valid）/,
  );
});

test('無出力時間切れの最後の診断と入力記録の全項目を表示する', () => {
  const text = draftFailureText({
    error: '停止',
    lastFailure: { cause: '時間切れ' },
    tier: { level: 'standard' },
    inputBudget: {
      records: [
        { sent: { bytes: 1 } },
        { sent: { bytes: 123 }, full: { total: { bytes: 456 } } },
      ],
    },
    aiDiagnostics: [
      { id: 'old', kind: 'exit_nonzero' },
      {
        id: 'new',
        kind: 'timeout_idle',
        model: 'model-x',
        effort: 'high',
        idleProcess: {
          processes: 1,
          children: 2,
          cpuMs: 3,
          windowMs: 4,
          cpuDeltaMs: 5,
          readBytesDelta: 6,
          writeBytesDelta: 7,
          established: 8,
          otherConnections: 9,
          txQueueBytes: 10,
          rxQueueBytes: 11,
        },
      },
    ],
  });
  for (const part of [
    '診断ID new',
    'model model-x',
    'effort high',
    'tier.level standard',
    '送信入力 123 bytes',
    '全体入力 456 bytes',
    '入力に改行とスキーマ',
    '同じ呼び出しに対応するとは限りません',
    'プロセス数 1',
    '子プロセス数 2',
    'CPU時間 3 ms',
    '観測期間 4 ms',
    'その間のCPU時間 5 ms',
    '読取 6 bytes',
    '書込 7 bytes',
    'TCP確立接続数 8',
    'その他のTCP接続数 9',
    '送信キュー 10 bytes',
    '受信キュー 11 bytes',
  ])
    assert.ok(text.includes(part), part);
  assert.ok(!text.includes('診断ID old'));
  assert.equal((text.match(/診断ID new/g) || []).length, 1);
});

test('壊れた詳細値は不明とし、数値と文字列は表示する', () => {
  const text = draftFailureText({
    error: '停止',
    lastFailure: { cause: '時間切れ' },
    tier: { level: false },
    inputBudget: {
      records: [{ sent: { bytes: {} }, full: { total: { bytes: [] } } }],
    },
    aiDiagnostics: [
      {
        id: {},
        kind: 'timeout_idle',
        model: ['x'],
        effort: true,
        idleProcess: {
          processes: {},
          children: [],
          cpuMs: false,
          windowMs: '',
          cpuDeltaMs: '5',
          readBytesDelta: 0,
          writeBytesDelta: {},
          established: [],
          otherConnections: true,
          txQueueBytes: {},
          rxQueueBytes: [],
        },
      },
    ],
  });
  for (const part of [
    '診断ID 不明',
    'model 不明',
    'effort 不明',
    'tier.level 不明',
    '送信入力 不明 bytes',
    '全体入力 不明 bytes',
    'プロセス数 不明',
    '子プロセス数 不明',
    'CPU時間 不明 ms',
    '観測期間 不明 ms',
    'その間のCPU時間 5 ms',
    '読取 0 bytes',
    '書込 不明 bytes',
    'TCP確立接続数 不明',
    'その他のTCP接続数 不明',
    '送信キュー 不明 bytes',
    '受信キュー 不明 bytes',
  ])
    assert.ok(text.includes(part), part);
  assert.ok(!text.includes('[object Object]'));
  assert.ok(!text.includes('最後の呼び出しの診断:'));
});

test('欠落とnullは不明、実際の0は0として表示する', () => {
  const text = draftFailureText({
    error: '停止',
    lastFailure: { cause: '時間切れ' },
    tier: { level: null },
    inputBudget: {
      records: [{ sent: { bytes: 0 }, full: { total: { bytes: null } } }],
    },
    aiDiagnostics: [
      {
        kind: 'timeout_idle',
        model: null,
        idleProcess: {
          processes: 0,
          children: null,
          cpuMs: 0,
          rxQueueBytes: 0,
        },
      },
    ],
  });
  for (const part of [
    '診断ID 不明',
    'model 不明',
    'effort 不明',
    'tier.level 不明',
    '送信入力 0 bytes',
    '全体入力 不明 bytes',
    'プロセス数 0',
    '子プロセス数 不明',
    'CPU時間 0 ms',
    '観測期間 不明 ms',
    '受信キュー 0 bytes',
  ])
    assert.ok(text.includes(part), part);
});

test('旧形式の無出力時間切れも各項目を不明として表示する', () => {
  const text = draftFailureText({
    error: '停止',
    lastFailure: { cause: '時間切れ' },
    aiDiagnostics: [{ id: 'legacy', kind: 'timeout_idle' }],
  });
  for (const part of [
    '診断ID legacy',
    '送信入力 不明 bytes',
    '全体入力 不明 bytes',
    'プロセス数 不明',
    '子プロセス数 不明',
    'CPU時間 不明 ms',
    '観測期間 不明 ms',
    'その間のCPU時間 不明 ms',
    '読取 不明 bytes',
    '書込 不明 bytes',
    'TCP確立接続数 不明',
    'その他のTCP接続数 不明',
    '送信キュー 不明 bytes',
    '受信キュー 不明 bytes',
  ])
    assert.ok(text.includes(part), part);
});

test('他の失敗と表示条件は変わらない', () => {
  const diagnostic = {
    id: 'other',
    kind: 'exit_nonzero',
    stage: 'exit',
    exitCode: 2,
  };
  assert.equal(
    draftFailureText({
      error: '停止',
      lastFailure: { cause: '原因' },
      aiDiagnostics: [diagnostic],
    }),
    '直近の失敗（段階 不明・時刻不明）: 原因\n最後の呼び出しの診断: exit_nonzero（段階 exit・終了コード 2・診断ID other）',
  );
  assert.equal(
    draftFailureText({
      lastFailure: { cause: '原因' },
      aiDiagnostics: [{ kind: 'timeout_idle' }],
    }),
    '',
  );
  assert.ok(
    draftFailureText({
      retryAt: 'later',
      lastFailure: { cause: '原因' },
      aiDiagnostics: [{ kind: 'timeout_idle' }],
    }).includes('無出力時間切れの詳細'),
  );
});

async function checkOutput(draft) {
  const methods = [];
  const lines = [];
  const original = console.log;
  console.log = (...args) => lines.push(args.join(' '));
  try {
    await checkDraft(
      async (method) => {
        methods.push(method);
        return draft;
      },
      { check: draft.id, timeout: '1' },
      'scripts/create-issue.ts',
      '',
      1,
    );
  } finally {
    console.log = original;
  }
  return { text: lines.join('\n'), methods };
}

test('--check は起票前判定の全回の結果と理由を表示し、書き込まない', async () => {
  const checks = [
    'refused',
    'evidence_requested',
    'ai_failed',
    'invalid_answer',
    'allowed',
  ].map((result, index) => ({
    revision: 3,
    phase: index,
    result,
    reason: `理由${index + 1}`,
  }));
  const { text, methods } = await checkOutput({
    id: 'child',
    status: 'submit_error',
    revision: 3,
    messages: [],
    filingCheckSummary: { revision: 3, count: 5, checks },
  });
  [
    '拒否: 理由1',
    '資料追加の要求: 理由2',
    'AI失敗: 理由3',
    '回答不正: 理由4',
    '許可: 理由5',
    '5 回',
  ].forEach((part) => assert.ok(text.includes(part), part));
  assert.deepEqual([...new Set(methods)], ['GET']);
});

test('要約のない旧応答は例外なく何も足さず、欠けた回は未記録と表示する', async () => {
  const { text } = await checkOutput({
    id: 'old',
    status: 'draft',
    revision: 1,
    messages: [],
  });
  assert.ok(!text.includes('起票前の判定'));
  assert.equal(filingCheckSummaryText({}), '');
  assert.equal(
    filingCheckSummaryText({ filingCheckSummary: { checks: 'broken' } }),
    '',
  );
  assert.match(
    filingCheckSummaryText({
      filingCheckSummary: {
        revision: null,
        count: null,
        checks: [{ revision: null, result: null, reason: null }],
      },
    }),
    /版 未記録・未記録: 未記録/,
  );
});
