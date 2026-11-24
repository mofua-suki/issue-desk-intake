import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import {
  recordFires,
  projectName,
  MAX_BYTES,
  withRecordLock,
} from '../scripts/stop-check-record.ts';
import { sendRecords } from '../scripts/stop-check-send.ts';
import { stopHookResponse } from '../scripts/chat-stop-hook.ts';

const setup = () => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'stop-record-'));
  fs.mkdirSync(path.join(home, '.issue-desk'));
  return home;
};
const fileOf = (home: string) =>
  path.join(home, '.issue-desk', 'stop-check-fires.jsonl');
const fire = { id: 'scope-mention', result: 'block' };

test('発動の控えは限定項目とプロジェクトだけを保持し古い行から縮小する', () => {
  const home = setup();
  const file = fileOf(home);
  try {
    fs.writeFileSync(file, 'old\n'.repeat(MAX_BYTES / 4));
    recordFires(
      [fire],
      path.join(home, 'rc', 'project', 'secret-path'),
      home,
      new Date('2026-11-05T00:00:00Z'),
    );
    const text = fs.readFileSync(file, 'utf8');
    const row = JSON.parse(text.trimEnd().split('\n').at(-1)!);
    assert.deepEqual(Object.keys(row), [
      'id',
      'checkId',
      'result',
      'at',
      'project',
    ]);
    assert.equal(row.project, 'project');
    assert.equal(row.checkId, fire.id);
    assert.equal(row.result, 'block');
    assert.ok(fs.statSync(file).size <= MAX_BYTES / 2);
    const before = fs.readFileSync(file, 'utf8');
    recordFires([fire], home, home);
    assert.ok(fs.readFileSync(file, 'utf8').startsWith(before));
    for (const secret of [
      'secret-path',
      home,
      '返答本文',
      '理由文言',
      'ユーザー発言',
      'credential',
    ])
      assert.ok(!text.includes(secret));
    for (const cwd of [
      undefined,
      '/elsewhere',
      path.join(home, 'rc'),
      path.join(home, 'rc', 'bad name'),
    ])
      assert.equal(projectName(cwd, home), 'other');
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});

test('記録と起動の失敗はフックの判定と出力を変えない', () => {
  const home = setup();
  const transcript = [
    JSON.stringify({
      type: 'user',
      message: { content: '調査して credential' },
    }),
    JSON.stringify({
      type: 'assistant',
      message: {
        content: [{ type: 'text', text: '別のセッションの情報です。返答本文' }],
      },
    }),
  ].join('\n');
  const raw = JSON.stringify({
    transcript_path: '/secret-path',
    cwd: path.join(home, 'rc', 'project'),
  });
  const now = () => new Date('2026-11-05T00:00:00Z');
  try {
    const expected = stopHookResponse(raw, () => transcript, now);
    const run = () =>
      stopHookResponse(
        raw,
        () => transcript,
        now,
        (fires, cwd) => recordFires(fires, cwd, home, now()),
      );
    assert.equal(run(), expected);
    const text = fs.readFileSync(fileOf(home), 'utf8');
    for (const secret of [
      'credential',
      '返答本文',
      '別のセッション',
      '/secret-path',
      home,
    ])
      assert.ok(!text.includes(secret));
    fs.rmSync(fileOf(home));
    fs.mkdirSync(fileOf(home));
    assert.throws(() => fs.appendFileSync(fileOf(home), 'probe'));
    assert.equal(run(), expected);
    fs.rmSync(fileOf(home), { recursive: true });
    fs.rmSync(path.join(home, '.issue-desk'), { recursive: true });
    assert.equal(run(), expected);
    fs.mkdirSync(path.join(home, '.issue-desk'));
    fs.mkdirSync(fileOf(home));
    assert.equal(run(), expected);
    assert.equal(
      stopHookResponse(
        raw,
        () => transcript,
        now,
        () => {
          throw Error('failure');
        },
      ),
      expected,
    );
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});

test('終了した所有者のロックから自動回復して記録と送信を行う', async () => {
  const home = setup();
  const file = fileOf(home);
  try {
    const moduleURL = new URL(
      '../scripts/stop-check-record.ts',
      import.meta.url,
    ).href;
    const child = spawnSync(process.execPath, [
      '--input-type=module',
      '-e',
      'const { withRecordLock } = await import(process.argv[1]); withRecordLock(process.argv[2], () => process.exit(0));',
      moduleURL,
      file,
    ]);
    assert.equal(child.status, 0, child.stderr.toString());
    recordFires([fire], home, home);
    const recorded = JSON.parse(fs.readFileSync(file, 'utf8'));
    assert.equal(recorded.checkId, fire.id);
    const sent: string[] = [];
    await sendRecords(file, async (_m: string, _r: string, batch: any[]) =>
      sent.push(...batch.map((x) => x.id)),
    );
    assert.deepEqual(sent, [recorded.id]);
    assert.equal(fs.readFileSync(file, 'utf8'), '');
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});

test('稼働中の所有者のロックは回収せず解放後に記録する', () => {
  const home = setup();
  const file = fileOf(home);
  try {
    withRecordLock(file, () => {
      recordFires([fire], home, home);
      assert.equal(fs.existsSync(file), false);
      assert.throws(() => withRecordLock(file, () => {}));
    });
    recordFires([fire], home, home);
    assert.equal(JSON.parse(fs.readFileSync(file, 'utf8')).checkId, fire.id);
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});

test('1000件ずつ送信し失敗した配列を残し不正な行を除外する', async () => {
  const home = setup();
  const file = fileOf(home);
  const rows = Array.from({ length: 1001 }, (_, i) => ({
    id: String(i),
    checkId: 'scope',
    result: 'block',
    at: '2026-11-05T00:00:00Z',
    project: 'other',
  }));
  fs.writeFileSync(
    file,
    rows.map((x) => JSON.stringify(x) + '\n').join('') + 'bad\n',
  );
  try {
    const sizes: number[] = [];
    await sendRecords(
      file,
      async (_method: string, route: string, batch: any[]) => {
        assert.equal(route, '/api/v1/stop-check-fires');
        sizes.push(batch.length);
        if (batch.length === 1) throw Error('offline');
      },
    );
    assert.deepEqual(sizes, [1000, 1]);
    assert.equal(fs.readFileSync(file + '.rejected', 'utf8'), 'bad\n');
    assert.equal(
      fs.readFileSync(file, 'utf8'),
      JSON.stringify(rows[1000]) + '\n',
    );
    await sendRecords(file, async (_m: string, _r: string, batch: any[]) => {
      assert.deepEqual(batch, [rows[1000]]);
    });
    assert.equal(fs.readFileSync(file, 'utf8'), '');
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});

test('400の行を隔離し正常行を保存して通信障害は再送する', async () => {
  const home = setup();
  const file = fileOf(home);
  const good = {
    id: '123e4567-e89b-12d3-a456-426614174002',
    checkId: 'unasked-send',
    result: 'block',
    at: '2026-11-05T12:34:56Z',
    project: 'project-1',
  };
  const bad = {
    ...good,
    id: '123e4567-e89b-12d3-a456-426614174003',
    checkId: 'unknown',
  };
  const later = { ...good, id: '123e4567-e89b-12d3-a456-426614174004' };
  fs.writeFileSync(
    file,
    [good, bad, later].map((x) => JSON.stringify(x) + '\n').join(''),
  );
  try {
    const saved: string[] = [];
    let offline = true;
    const call = async (_m: string, _r: string, batch: any[]) => {
      if (batch.some((x) => x.checkId === 'unknown'))
        throw Object.assign(Error('bad request'), { status: 400 });
      if (offline && batch.some((x) => x.id === later.id))
        throw Error('offline');
      saved.push(...batch.map((x) => x.id));
    };
    await sendRecords(file, call);
    assert.deepEqual(saved, [good.id]);
    assert.equal(
      fs.readFileSync(file + '.rejected', 'utf8'),
      JSON.stringify(bad) + '\n',
    );
    assert.equal(fs.readFileSync(file, 'utf8'), JSON.stringify(later) + '\n');
    offline = false;
    await sendRecords(file, call);
    assert.deepEqual(saved, [good.id, later.id]);
    assert.equal(fs.readFileSync(file, 'utf8'), '');
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});

test('4xxと不正JSONを除き一時障害と送信中の追記を保存後に再送する', async () => {
  const home = setup();
  const file = fileOf(home);
  const row = (id: string) => ({
    id,
    checkId: 'scope',
    result: 'block',
    at: '2026-11-05T00:00:00Z',
    project: 'other',
  });
  const rows = ['ok', 'bad400', 'bad403', 'offline', 'server'].map(row);
  const line = (x: object) => JSON.stringify(x) + '\n';
  fs.writeFileSync(file, rows.map(line).join('') + 'invalid json\n');
  const received: string[] = [];
  let appended = false;
  try {
    await sendRecords(file, async (_m: string, _r: string, batch: any[]) => {
      if (!appended) {
        fs.appendFileSync(file, line(rows[0]) + line(row('new')));
        appended = true;
      }
      if (batch.length > 1) throw Object.assign(Error(), { status: 400 });
      const id = batch[0].id;
      if (id === 'bad400' || id === 'bad403')
        throw Object.assign(Error(), { status: id === 'bad400' ? 400 : 403 });
      if (id === 'offline') throw Error('offline');
      if (id === 'server') throw Object.assign(Error(), { status: 503 });
      received.push(id);
    });
    assert.deepEqual(received, ['ok']);
    assert.ok(
      fs.readFileSync(file + '.rejected', 'utf8').includes('invalid json\n'),
    );
    assert.deepEqual(
      fs
        .readFileSync(file + '.rejected', 'utf8')
        .trim()
        .split('\n')
        .slice(0, 2)
        .map((line) => JSON.parse(line))
        .map((x) => x.id),
      ['bad400', 'bad403'],
    );
    assert.deepEqual(
      fs
        .readFileSync(file, 'utf8')
        .trim()
        .split('\n')
        .map((line) => JSON.parse(line))
        .map((x) => x.id),
      ['offline', 'server', 'ok', 'new'],
    );
    await sendRecords(file, async (_m: string, _r: string, batch: any[]) => {
      received.push(...batch.map((x) => x.id));
    });
    assert.deepEqual(received, ['ok', 'offline', 'server', 'ok', 'new']);
    assert.equal(fs.readFileSync(file, 'utf8'), '');
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});

test('不正JSONを重複も含め隔離し、失敗時は控えに残して再試行する', async () => {
  const home = setup();
  const file = fileOf(home);
  const bad = 'broken JSON\n';
  const good = JSON.stringify({ id: 'good' }) + '\n';
  try {
    fs.writeFileSync(file, bad + good + bad + 'unfinished');
    fs.mkdirSync(file + '.rejected');
    await sendRecords(file, async () => {});
    assert.equal(fs.readFileSync(file, 'utf8'), bad + bad + 'unfinished');
    fs.rmSync(file + '.rejected', { recursive: true });
    await sendRecords(file, async () => {
      throw Error('unexpected send');
    });
    assert.equal(fs.readFileSync(file + '.rejected', 'utf8'), bad + bad);
    assert.equal(fs.readFileSync(file, 'utf8'), 'unfinished');
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});

test('隔離または原子更新の失敗では対象を控えに残す', async () => {
  const home = setup();
  const file = fileOf(home);
  const line = JSON.stringify({ id: 'rejected' }) + '\n';
  try {
    fs.writeFileSync(file, line);
    fs.mkdirSync(file + '.rejected');
    await sendRecords(file, async () => {
      throw Object.assign(Error(), { status: 403 });
    });
    assert.equal(fs.readFileSync(file, 'utf8'), line);
    fs.rmSync(file + '.rejected', { recursive: true });
    const rename = fs.renameSync;
    try {
      await sendRecords(file, async () => {
        fs.renameSync = (() => {
          throw Error('rename failed');
        }) as typeof fs.renameSync;
      });
    } finally {
      fs.renameSync = rename;
    }
    assert.equal(fs.readFileSync(file, 'utf8'), line);
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});

test('隔離後の原子更新失敗と再送では隔離行が重複する', async () => {
  for (const malformed of [true, false]) {
    const home = setup();
    const file = fileOf(home);
    const line = malformed
      ? 'broken JSON\n'
      : JSON.stringify({ id: 'rejected' }) + '\n';
    const rename = fs.renameSync;
    try {
      fs.writeFileSync(file, line);
      fs.renameSync = (() => {
        throw Error('rename failed');
      }) as typeof fs.renameSync;
      try {
        await sendRecords(file, async () => {
          throw Object.assign(Error('bad request'), { status: 400 });
        });
      } finally {
        fs.renameSync = rename;
      }
      assert.equal(fs.readFileSync(file, 'utf8'), line);
      assert.equal(fs.readFileSync(file + '.rejected', 'utf8'), line);
      await sendRecords(file, async () => {
        throw Object.assign(Error('bad request'), { status: 400 });
      });
      assert.equal(fs.readFileSync(file + '.rejected', 'utf8'), line + line);
      assert.equal(fs.readFileSync(file, 'utf8'), '');
    } finally {
      fs.renameSync = rename;
      fs.rmSync(home, { recursive: true, force: true });
    }
  }
});

test('実際の記録処理による送信中の追記を保持する', async () => {
  const home = setup();
  const file = fileOf(home);
  const initial = {
    id: 'initial',
    checkId: 'scope',
    result: 'block',
    at: '2026-11-05T00:00:00Z',
    project: 'other',
  };
  try {
    fs.writeFileSync(file, JSON.stringify(initial) + '\n');
    await sendRecords(file, async () => recordFires([fire], home, home));
    const saved = fs
      .readFileSync(file, 'utf8')
      .trim()
      .split('\n')
      .map((line) => JSON.parse(line));
    assert.equal(saved.length, 1);
    assert.equal(saved[0].checkId, fire.id);
    assert.notEqual(saved[0].id, initial.id);
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});
