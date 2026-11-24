import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { parseArgs, runCommand, UsageError } from '../scripts/direct-pr.ts';
import { patchHash } from '../shared/src/request-rules.ts';

const head = 'a'.repeat(40);
const other = 'b'.repeat(40);
// 利用者がチャットで指示した日時（今の1分前）。
const at = new Date(Date.now() - 60_000).toISOString();

function workspace() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'direct-pr-'));
  fs.writeFileSync(path.join(dir, 'body.md'), '問題・変更・検証結果');
  return {
    dir,
    save: path.join(dir, 'req', 'request.json'),
    body: path.join(dir, 'body.md'),
    done: () => fs.rmSync(dir, { recursive: true, force: true }),
  };
}

// 受付の代わり。POSTの本文を記録し、GETには states を順に返す（最後の状態を繰り返す）。
function desk(
  states: any[],
  { onPost = null as any, getError = null as any } = {},
) {
  const calls: any[] = [];
  let sent: any = null;
  let reads = 0;
  let closed = 0;
  const call = async (method, route, body) => {
    calls.push([method, route]);
    if (method === 'POST') {
      onPost?.(body);
      sent = body;
      return {
        request: { ...body, status: 'queued', claimedBy: null, result: null },
      };
    }
    if (getError && reads === 0) {
      reads++;
      throw getError;
    }
    const state = states[Math.min(reads++, states.length - 1)];
    if (state instanceof Error) throw state;
    return {
      request: {
        ...sent,
        ...state,
        payload: { ...sent.payload, ...(state.payload ?? {}) },
      },
    };
  };
  return {
    calls,
    get sent() {
      return sent;
    },
    closed: () => closed,
    open: async () => ({
      call,
      close: () => {
        closed++;
      },
    }),
  };
}

// テストの作業場所は os.tmpdir() の下なので、ホームの代わりにそこを渡す。
const home = os.tmpdir();
const quiet = {
  env: {},
  configured: () => null,
  log: () => {},
  err: () => {},
  sleep: async () => {},
  home,
};

test('作成は依頼JSONを送る前に保存し、同じIDで created まで待つ', async () => {
  const w = workspace();
  try {
    const out: string[] = [];
    const d = desk(
      [
        { status: 'queued' },
        { status: 'claimed', claimedBy: 'gt12' },
        {
          status: 'done',
          result: {
            status: 'created',
            number: 7,
            url: 'https://g/pulls/7',
            head,
          },
        },
      ],
      {
        onPost: (body) =>
          assert.deepEqual(JSON.parse(fs.readFileSync(w.save, 'utf8')), body),
      },
    );
    const code = await runCommand(
      [
        'create',
        '--repo',
        'o/r',
        '--save',
        w.save,
        '--title',
        '題',
        '--body-file',
        w.body,
        '--branch',
        'chat/topic',
        '--expect-head',
        head,
      ],
      { ...quiet, open: d.open, log: (line) => out.push(line) },
    );
    assert.equal(code, 0);
    assert.deepEqual(d.sent.payload, {
      branch: 'chat/topic',
      title: '題',
      body: '問題・変更・検証結果',
    });
    assert.match(d.sent.id, /^\d+-[a-f0-9]{8}$/);
    assert.equal(d.calls.filter(([m]) => m === 'POST').length, 1);
    assert.equal(JSON.parse(out.at(-1)).result.number, 7);
    assert.equal(d.closed(), 1);
  } finally {
    w.done();
  }
});

test('作られたPRのheadが期待と違う・パッチのSHA256が結果に無いときは成功にしない', async () => {
  const w = workspace();
  try {
    const d = desk([
      {
        status: 'done',
        result: {
          status: 'created',
          number: 7,
          url: 'u',
          head: 'b'.repeat(40),
        },
      },
    ]);
    assert.equal(
      await runCommand(
        [
          'create',
          '--repo',
          'o/r',
          '--save',
          w.save,
          '--title',
          '題',
          '--body-file',
          w.body,
          '--branch',
          'chat/topic',
          '--expect-head',
          head,
        ],
        { ...quiet, open: d.open },
      ),
      1,
    );
    const patch = path.join(w.dir, 'change.patch');
    fs.writeFileSync(patch, 'diff --git a/x b/x\n');
    const p = desk([
      {
        status: 'done',
        result: { status: 'created', number: 8, url: 'u', head },
      },
    ]);
    const save = path.join(w.dir, 'patch.json');
    assert.equal(
      await runCommand(
        [
          'create',
          '--repo',
          'o/r',
          '--save',
          save,
          '--title',
          '題',
          '--body-file',
          w.body,
          '--patch-file',
          patch,
          '--base',
          head,
        ],
        { ...quiet, open: p.open },
      ),
      1,
    );
    assert.equal(p.sent.payload.patchSha256, patchHash('diff --git a/x b/x\n'));
    assert.equal(p.sent.payload.branch, undefined);
  } finally {
    w.done();
  }
});

test('承認付きマージは発言の原文と指示の日時の両方を要り、欠けたら接続しない', async () => {
  const w = workspace();
  try {
    const base = [
      'merge',
      '--repo',
      'o/r',
      '--save',
      w.save,
      '--number',
      '42',
      '--head',
      head,
    ];
    for (const extra of [
      ['--approval', '#42 の権限ファイルの変更を承認します'],
      ['--instruction-at', at],
      ['--approval', '承認します', '--instruction-at', at],
      [
        '--approval',
        '#42 の権限ファイルの変更を承認します',
        '--instruction-at',
        '2026-11-11T11:58:00',
      ],
      [
        '--approval',
        '#42 の権限ファイルの変更を承認します',
        '--instruction-at',
        new Date(Date.now() + 3_600_000).toISOString(),
      ],
      ['--approval-question', '#42 の権限ファイルの変更をマージしますか'],
    ]) {
      let opened = false;
      await assert.rejects(
        runCommand([...base, ...extra], {
          ...quiet,
          open: async () => {
            opened = true;
            throw Error('接続してはいけない');
          },
        }),
      );
      assert.equal(opened, false, extra.join(' '));
    }
    assert.equal(fs.existsSync(w.save), false);
  } finally {
    w.done();
  }
});

test('記号の回答は見せた質問文と組にして送り、受付が足す approvalDelegated だけを許す', async () => {
  const w = workspace();
  try {
    const d = desk([
      {
        status: 'done',
        payload: { approvalDelegated: true },
        result: { status: 'merged' },
      },
    ]);
    const code = await runCommand(
      [
        'merge',
        '--repo',
        'o/r',
        '--save',
        w.save,
        '--number',
        '42',
        '--head',
        head,
        '--approval-question',
        'PR #42 の権限ファイルの変更をマージしますか。A: マージする',
        '--approval-answer',
        'A',
        '--instruction-at',
        at,
      ],
      { ...quiet, open: d.open },
    );
    assert.equal(code, 0);
    assert.equal(
      d.sent.payload.approval,
      '質問「PR #42 の権限ファイルの変更をマージしますか。A: マージする」への利用者の回答: A',
    );
    assert.equal(d.sent.payload.instructionAt, at);
    const plain = desk([
      {
        status: 'done',
        payload: { approvalDelegated: true },
        result: { status: 'merged' },
      },
    ]);
    assert.equal(
      await runCommand(
        [
          'merge',
          '--repo',
          'o/r',
          '--save',
          path.join(w.dir, 'plain.json'),
          '--number',
          '42',
          '--head',
          head,
        ],
        { ...quiet, open: plain.open },
      ),
      1,
    );
  } finally {
    w.done();
  }
});

test('事前承認は作成依頼に承認を付け、マージは approvalRequest で指し、受付が写した承認だけを許す', async () => {
  const w = workspace();
  try {
    const created = desk([
      {
        status: 'done',
        payload: { approvalDelegated: true },
        result: {
          status: 'created',
          number: 7,
          url: 'https://g/pulls/7',
          head,
        },
      },
    ]);
    assert.equal(
      await runCommand(
        [
          'create',
          '--repo',
          'o/r',
          '--save',
          w.save,
          '--title',
          '題',
          '--body-file',
          w.body,
          '--branch',
          'chat/topic',
          '--expect-head',
          head,
          '--approval',
          '直接PRでマージまで進めて',
          '--instruction-at',
          at,
        ],
        { ...quiet, open: created.open },
      ),
      0,
    );
    assert.deepEqual(created.sent.payload, {
      branch: 'chat/topic',
      title: '題',
      body: '問題・変更・検証結果',
      approval: '直接PRでマージまで進めて',
      instructionAt: at,
    });
    const copied = {
      approval: '直接PRでマージまで進めて',
      instructionAt: at,
      approvalDelegated: true,
    };
    const merged = desk([
      { status: 'done', payload: copied, result: { status: 'merged' } },
    ]);
    assert.equal(
      await runCommand(
        [
          'merge',
          '--repo',
          'o/r',
          '--save',
          path.join(w.dir, 'merge.json'),
          '--number',
          '7',
          '--head',
          head,
          '--approval-request',
          created.sent.id,
        ],
        { ...quiet, open: merged.open },
      ),
      0,
    );
    assert.deepEqual(merged.sent.payload, {
      number: 7,
      head,
      approvalRequest: created.sent.id,
    });
    // approvalRequest の無いマージに受付が承認を足した記録は合わない。
    const forged = desk([
      { status: 'done', payload: copied, result: { status: 'merged' } },
    ]);
    assert.equal(
      await runCommand(
        [
          'merge',
          '--repo',
          'o/r',
          '--save',
          path.join(w.dir, 'forged.json'),
          '--number',
          '7',
          '--head',
          head,
        ],
        { ...quiet, open: forged.open },
      ),
      1,
    );
    assert.throws(
      () =>
        parseArgs(
          [
            'merge',
            '--repo',
            'o/r',
            '--save',
            path.join(w.dir, 'both.json'),
            '--number',
            '7',
            '--head',
            head,
            '--approval-request',
            created.sent.id,
            '--approval',
            '直接PRでマージまで進めて',
            '--instruction-at',
            at,
          ],
          Date.now(),
          home,
        ),
      /同時に指定できません/,
    );
    assert.throws(
      () =>
        parseArgs(
          [
            'merge',
            '--repo',
            'o/r',
            '--save',
            path.join(w.dir, 'bad.json'),
            '--number',
            '7',
            '--head',
            head,
            '--approval-request',
            'not-an-id',
          ],
          Date.now(),
          home,
        ),
      /approvalRequest/,
    );
  } finally {
    w.done();
  }
});

test('拒否・失敗・未知の結果・認証の拒否は成功にせず、読取の一時的な失敗は読み直す', async () => {
  const w = workspace();
  try {
    const args = (name: string) => [
      'merge',
      '--repo',
      'o/r',
      '--save',
      path.join(w.dir, `${name}.json`),
      '--number',
      '42',
      '--head',
      head,
    ];
    for (const [name, result] of [
      ['refused', { status: 'refused', message: '範囲外' }],
      ['failed', { status: 'failed' }],
      ['unknown', { status: 'odd' }],
      ['missing', null],
    ] as const)
      assert.equal(
        await runCommand(args(name), {
          ...quiet,
          open: desk([{ status: 'done', result }]).open,
        }),
        1,
        name,
      );
    const forbidden = desk([{ status: 'queued' }], {
      getError: Object.assign(Error('拒否'), { status: 403 }),
    });
    assert.equal(
      await runCommand(args('forbidden'), { ...quiet, open: forbidden.open }),
      1,
    );
    assert.equal(forbidden.calls.filter(([m]) => m === 'GET').length, 1);
    const flaky = desk(
      [{ status: 'done', result: { status: 'merged', number: 42, head } }],
      { getError: Object.assign(Error('切断'), { unreachable: true }) },
    );
    assert.equal(
      await runCommand(args('flaky'), { ...quiet, open: flaky.open }),
      0,
    );
    const wrong = desk([
      { status: 'done', result: { status: 'merged', head: other } },
    ]);
    assert.equal(
      await runCommand(args('wrong'), { ...quiet, open: wrong.open }),
      1,
    );
  } finally {
    w.done();
  }
});

test('受付の記録が送った依頼と違えば待たずに止める', async () => {
  const w = workspace();
  try {
    const d = desk([{ status: 'queued', payload: { head: other } }]);
    assert.equal(
      await runCommand(
        [
          'merge',
          '--repo',
          'o/r',
          '--save',
          w.save,
          '--number',
          '42',
          '--head',
          head,
        ],
        { ...quiet, open: d.open },
      ),
      1,
    );
    assert.equal(d.calls.filter(([m]) => m === 'GET').length, 1);
  } finally {
    w.done();
  }
});

test('待ち切れなければ75で終え、resume は同じIDを読み、記録が無いときだけ同じ本文で送る', async () => {
  const w = workspace();
  try {
    let clock = 0;
    const timing = {
      now: () => clock,
      sleep: async (ms) => {
        clock += ms;
      },
    };
    const first = desk([{ status: 'claimed', claimedBy: 'gt12' }]);
    assert.equal(
      await runCommand(
        [
          'merge',
          '--repo',
          'o/r',
          '--save',
          w.save,
          '--number',
          '42',
          '--head',
          head,
          '--wait-minutes',
          '1',
        ],
        { ...quiet, ...timing, open: first.open },
      ),
      75,
    );
    const saved = JSON.parse(fs.readFileSync(w.save, 'utf8'));
    // 既にある保存先は新しい依頼に使わない。
    await assert.rejects(
      runCommand(
        [
          'merge',
          '--repo',
          'o/r',
          '--save',
          w.save,
          '--number',
          '42',
          '--head',
          head,
        ],
        {
          ...quiet,
          open: async () => {
            throw Error('接続してはいけない');
          },
        },
      ),
      /resume/,
    );
    const calls: any[] = [];
    const known = async (method, route, body) => {
      calls.push([method, route, body]);
      return {
        request: { ...saved, status: 'done', result: { status: 'merged' } },
      };
    };
    assert.equal(
      await runCommand(['resume', '--save', w.save], {
        ...quiet,
        open: async () => ({ call: known, close: () => {} }),
      }),
      0,
    );
    assert.deepEqual(
      calls.map(([m, r]) => [m, r]),
      [
        ['GET', `/api/v1/requests/${saved.id}`],
        ['GET', `/api/v1/requests/${saved.id}`],
      ],
    );
    const posted: any[] = [];
    let reads = 0;
    const missing = async (method, _route, body) => {
      if (method === 'POST') {
        posted.push(body);
        return { request: { ...body, status: 'queued' } };
      }
      if (reads++ === 0) throw Object.assign(Error('なし'), { status: 404 });
      return {
        request: { ...saved, status: 'done', result: { status: 'merged' } },
      };
    };
    assert.equal(
      await runCommand(['resume', '--save', w.save], {
        ...quiet,
        open: async () => ({ call: missing, close: () => {} }),
      }),
      0,
    );
    assert.deepEqual(posted, [saved]);
  } finally {
    w.done();
  }
});

test('引数の誤りは接続する前に断る', () => {
  const w = workspace();
  try {
    for (const argv of [
      ['push'],
      [
        'create',
        '--repo',
        'o/r',
        '--save',
        'relative.json',
        '--title',
        '題',
        '--body-file',
        w.body,
        '--branch',
        'b',
      ],
      [
        'create',
        '--repo',
        'o/r',
        '--save',
        w.save,
        '--title',
        '題',
        '--body-file',
        w.body,
      ],
      [
        'create',
        '--repo',
        'o/r',
        '--save',
        w.save,
        '--title',
        '題',
        '--body-file',
        w.body,
        '--branch',
        'codex/issue-3',
        '--expect-head',
        head,
      ],
      [
        'create',
        '--repo',
        'o/r',
        '--save',
        w.save,
        '--title',
        '題',
        '--body-file',
        w.body,
        '--branch',
        'b',
      ],
      [
        'create',
        '--repo',
        'o/r',
        '--save',
        w.save,
        '--title',
        '題',
        '--body-file',
        w.body,
        '--branch',
        'b',
        '--approval',
        '承認の発言の原文です',
      ],
      [
        'merge',
        '--repo',
        'o/r',
        '--save',
        w.save,
        '--number',
        '0',
        '--head',
        head,
      ],
      [
        'merge',
        '--repo',
        'o/r',
        '--save',
        w.save,
        '--number',
        '4',
        '--head',
        'abc',
      ],
      [
        'merge',
        '--repo',
        'bad',
        '--save',
        w.save,
        '--number',
        '4',
        '--head',
        head,
      ],
      ['resume', '--save', w.save, '--wait-minutes', '0'],
    ])
      assert.throws(
        () => parseArgs(argv, Date.now(), home),
        undefined,
        argv.join(' '),
      );
  } finally {
    w.done();
  }
});

test('--save とその親がホームの下に無ければ断る', () => {
  const w = workspace();
  try {
    const args = (save: string) => [
      'merge',
      '--repo',
      'o/r',
      '--save',
      save,
      '--number',
      '4',
      '--head',
      head,
    ];
    const inside = path.join(w.dir, 'home');
    fs.mkdirSync(inside);
    assert.throws(
      () =>
        parseArgs(
          args(path.join(w.dir, 'outside', 'x.json')),
          Date.now(),
          inside,
        ),
      /ホーム/,
    );
    // ホームの中のリンクが外を指していても、実パスで断る。
    fs.symlinkSync(w.dir, path.join(inside, 'link'));
    assert.throws(
      () =>
        parseArgs(
          args(path.join(inside, 'link', 'x.json')),
          Date.now(),
          inside,
        ),
      /ホーム/,
    );
    assert.throws(() => parseArgs(args(inside), Date.now(), inside), /ホーム/);
    assert.equal(
      parseArgs(args(path.join(inside, 'a', 'b', 'x.json')), Date.now(), inside)
        .request.kind,
      'merge_manual',
    );
  } finally {
    w.done();
  }
});

test('承認・接続の引数の誤りは理由を示して引数の誤り（UsageError）として断る', async () => {
  const w = workspace();
  try {
    const base = [
      'merge',
      '--repo',
      'o/r',
      '--save',
      w.save,
      '--number',
      '42',
      '--head',
      head,
    ];
    for (const [extra, reason] of [
      [['--approval-question', 'PR #42 をマージしますか'], /--approval-answer/],
      [
        [
          '--approval-question',
          'マージしますか',
          '--approval-answer',
          'A',
          '--instruction-at',
          at,
        ],
        /対象の番号/,
      ],
      [
        ['--approval', '#42 の権限ファイルの変更を承認します'],
        /--instruction-at/,
      ],
      [['--instruction-at', at], /一緒に指定/],
      [['--approval', '承認します', '--instruction-at', at], /8文字以上/],
      [
        [
          '--approval',
          '#42 の権限ファイルの変更を承認します',
          '--instruction-at',
          '2026-11-11T11:58:00',
        ],
        /日時/,
      ],
      [
        [
          '--approval',
          '#42 の権限ファイルの変更を承認します',
          '--approval-question',
          'PR #42 をマージしますか',
          '--approval-answer',
          'A',
        ],
        /併用/,
      ],
      [['--url', 'https://desk.example/'], /--token-file|両方/],
      [['--url'], /--url/],
    ] as const) {
      await assert.rejects(
        runCommand([...base, ...extra], {
          ...quiet,
          open: async () => {
            throw Error('接続してはいけない');
          },
        }),
        (e: any) =>
          e instanceof UsageError &&
          reason.test(e.message) &&
          /使い方/.test(e.message),
        extra.join(' '),
      );
    }
  } finally {
    w.done();
  }
});

test('POSTの失敗は、5xx・接続不能なら受理不明の75、4xx・409なら1', async () => {
  const w = workspace();
  try {
    for (const [name, error, code] of [
      ['unavailable', Object.assign(Error('一時停止'), { status: 503 }), 75],
      ['unreachable', Object.assign(Error('切断'), { unreachable: true }), 75],
      ['bad', Object.assign(Error('不正'), { status: 400 }), 1],
      ['conflict', Object.assign(Error('別内容'), { status: 409 }), 1],
    ] as const) {
      const calls: string[] = [];
      const call = async (method: string) => {
        calls.push(method);
        if (method === 'POST') throw error;
        return {};
      };
      assert.equal(
        await runCommand(
          [
            'merge',
            '--repo',
            'o/r',
            '--save',
            path.join(w.dir, `${name}.json`),
            '--number',
            '42',
            '--head',
            head,
          ],
          { ...quiet, open: async () => ({ call, close: () => {} }) },
        ),
        code,
        name,
      );
      assert.deepEqual(calls, ['POST'], name);
    }
    const lines: string[] = [];
    const d = desk([{ status: 'queued' }], { onPost: () => {} });
    const changed = async (method, route, body) =>
      method === 'POST'
        ? { request: { ...body, repo: 'o/other', status: 'queued' } }
        : (await d.open()).call(method, route, body);
    assert.equal(
      await runCommand(
        [
          'merge',
          '--repo',
          'o/r',
          '--save',
          path.join(w.dir, 'changed.json'),
          '--number',
          '42',
          '--head',
          head,
        ],
        {
          ...quiet,
          err: (line) => lines.push(line),
          open: async () => ({ call: changed, close: () => {} }),
        },
      ),
      1,
    );
    assert.match(lines.join('\n'), /受付に残っている可能性/);
    assert.match(lines.join('\n'), /取り消/);
  } finally {
    w.done();
  }
});

test('resume で受付の記録が保存した依頼と違えば送らず、作成の resume は head を照合する', async () => {
  const w = workspace();
  try {
    const first = desk([{ status: 'claimed' }]);
    let clock = 0;
    assert.equal(
      await runCommand(
        [
          'create',
          '--repo',
          'o/r',
          '--save',
          w.save,
          '--title',
          '題',
          '--body-file',
          w.body,
          '--branch',
          'chat/topic',
          '--expect-head',
          head,
          '--wait-minutes',
          '1',
        ],
        {
          ...quiet,
          now: () => clock,
          sleep: async (ms) => {
            clock += ms;
          },
          open: first.open,
        },
      ),
      75,
    );
    const saved = JSON.parse(fs.readFileSync(w.save, 'utf8'));
    // ブランチ方式の作成の resume は --expect-head が必須。
    assert.throws(
      () => parseArgs(['resume', '--save', w.save], Date.now(), home),
      /--expect-head/,
    );
    const methods: string[] = [];
    const mismatch = async (method: string) => {
      methods.push(method);
      return {
        request: {
          ...saved,
          payload: { ...saved.payload, branch: 'chat/other' },
          status: 'queued',
        },
      };
    };
    assert.equal(
      await runCommand(['resume', '--save', w.save, '--expect-head', head], {
        ...quiet,
        open: async () => ({ call: mismatch, close: () => {} }),
      }),
      1,
    );
    assert.deepEqual(methods, ['GET']);
    const wrongHead = async () => ({
      request: {
        ...saved,
        status: 'done',
        result: { status: 'created', number: 7, url: 'u', head: other },
      },
    });
    const errors: string[] = [];
    assert.equal(
      await runCommand(['resume', '--save', w.save, '--expect-head', head], {
        ...quiet,
        err: (line: string) => errors.push(line),
        open: async () => ({ call: wrongHead, close: () => {} }),
      }),
      1,
    );
    assert.match(errors.join('\n'), /期待した/);
    const right = async () => ({
      request: {
        ...saved,
        status: 'done',
        result: { status: 'created', number: 7, url: 'u', head },
      },
    });
    assert.equal(
      await runCommand(['resume', '--save', w.save, '--expect-head', head], {
        ...quiet,
        open: async () => ({ call: right, close: () => {} }),
      }),
      0,
    );
  } finally {
    w.done();
  }
});

test('取消済み・未知の依頼の状態は成功にしない', async () => {
  const w = workspace();
  try {
    const args = (name: string) => [
      'merge',
      '--repo',
      'o/r',
      '--save',
      path.join(w.dir, `${name}.json`),
      '--number',
      '42',
      '--head',
      head,
    ];
    const lines: string[] = [];
    assert.equal(
      await runCommand(args('cancelled'), {
        ...quiet,
        err: (line) => lines.push(line),
        open: desk([{ status: 'done', result: { status: 'cancelled' } }]).open,
      }),
      1,
    );
    assert.match(lines.join('\n'), /cancelled/);
    assert.equal(
      await runCommand(args('odd'), {
        ...quiet,
        open: desk([{ status: 'paused' }]).open,
      }),
      1,
    );
  } finally {
    w.done();
  }
});

test('パッチ方式は厳密なUTF-8で読み、結果の patchSha256 が一致すれば成功', async () => {
  const w = workspace();
  try {
    const patch = path.join(w.dir, 'change.patch');
    const text = 'diff --git a/x b/x\n+日本語\n';
    fs.writeFileSync(patch, '﻿' + text);
    const d = desk([
      {
        status: 'done',
        result: {
          status: 'created',
          number: 9,
          url: 'u',
          head,
          patchSha256: patchHash(text),
        },
      },
    ]);
    assert.equal(
      await runCommand(
        [
          'create',
          '--repo',
          'o/r',
          '--save',
          w.save,
          '--title',
          '題',
          '--body-file',
          w.body,
          '--patch-file',
          patch,
          '--base',
          head,
        ],
        { ...quiet, open: d.open },
      ),
      0,
    );
    assert.equal(d.sent.payload.patch, text);
    assert.deepEqual(Object.keys(d.sent.payload).sort(), [
      'base',
      'body',
      'patch',
      'patchSha256',
      'title',
    ]);
    const broken = path.join(w.dir, 'broken.patch');
    fs.writeFileSync(broken, Buffer.from([0x64, 0x69, 0x66, 0x66, 0xff, 0x0a]));
    assert.throws(
      () =>
        parseArgs(
          [
            'create',
            '--repo',
            'o/r',
            '--save',
            path.join(w.dir, 'broken.json'),
            '--title',
            '題',
            '--body-file',
            w.body,
            '--patch-file',
            broken,
            '--base',
            head,
          ],
          Date.now(),
          home,
        ),
      /UTF-8/,
    );
  } finally {
    w.done();
  }
});
