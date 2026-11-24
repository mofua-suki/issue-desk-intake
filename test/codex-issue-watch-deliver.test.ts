import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { EventEmitter } from 'node:events';
import crypto from 'node:crypto';
import { main as watchMain } from '../scripts/codex-issue-watch.ts';
import { Rpc } from '../scripts/codex-issue-watch-deliver.ts';
import { deliver } from '../scripts/codex-issue-watch-deliver.ts';
import {
  saveWatchState,
  resumeWatchState,
} from '../scripts/codex-issue-watch-state.ts';

test('Rpc は各長さのサーバーフレームを分割受信して応答を返す', async () => {
  class Socket extends EventEmitter {
    setTimeout() {
      return this;
    }
    write(_data: Buffer, callback: (error?: Error) => void) {
      callback();
      return true;
    }
    destroy() {
      this.emit('close');
      return this;
    }
  }
  const socket = new Socket();
  const rpc = new Rpc(socket as any, Buffer.alloc(0));
  try {
    for (const [index, length] of [125, 126, 127, 65536].entries()) {
      const prefix = JSON.stringify({ id: index + 1, result: '' });
      const result = 'x'.repeat(length - Buffer.byteLength(prefix));
      const payload = Buffer.from(JSON.stringify({ id: index + 1, result }));
      assert.equal(payload.length, length);
      const extra = length < 126 ? 0 : length < 65536 ? 2 : 8;
      const header = Buffer.alloc(2 + extra);
      header[0] = 0x81;
      header[1] = extra === 0 ? length : extra === 2 ? 126 : 127;
      if (extra === 2) header.writeUInt16BE(length, 2);
      if (extra === 8) header.writeBigUInt64BE(BigInt(length), 2);
      const response = rpc.call('test', {});
      const frame = Buffer.concat([header, payload]);
      socket.emit('data', frame.subarray(0, 3));
      socket.emit('data', frame.subarray(3));
      assert.equal(await response, result);
    }
  } finally {
    rpc.close();
  }
});

test('RPC は通知を受信中でも処理の期限を超えると接続を閉じる', async () => {
  class Socket extends EventEmitter {
    write(_data: Buffer, callback: (error?: Error) => void) {
      callback();
      return true;
    }
    destroy() {
      this.emit('close');
      return this;
    }
  }
  const socket = new Socket();
  const controller = new AbortController();
  const rpc = new Rpc(socket as any, Buffer.alloc(0), controller.signal);
  const response = rpc.call('thread/read', {});
  socket.emit('data', Buffer.from([0x81, 2, 0x7b, 0x7d]));
  controller.abort();
  await assert.rejects(response, /connection lost/);
  await assert.rejects(rpc.call('thread/read', {}), /connection closed/);
});

async function fixture(t: any) {
  const { deliver } = await import('../scripts/codex-issue-watch-deliver.ts');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'desk-injected-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const journal = path.join(dir, 'journal');
  fs.mkdirSync(journal, { mode: 0o700 });
  const registration = path.join(dir, 'reg.json');
  const reg = {
    host: os.hostname(),
    conversationId: 'c1',
    socket: path.join(dir, 'sock'),
    journal,
    codexVersion: '0.171.0',
  };
  const save = () =>
    fs.writeFileSync(registration, JSON.stringify(reg), { mode: 0o600 });
  save();
  const messages: any[] = [];
  const items: any[] = [];
  const state = {
    owner: process.getuid!(),
    status: 'idle',
    wrong: false,
    defer: false,
    lost: false,
    race: false,
    version: '0.171.0',
    readFailure: false,
  };
  const calls: string[] = [];
  const dependencies: any = {
    socketStat: () => ({ isSocket: () => true, uid: state.owner }),
    connect: async () => ({
      notify: (method: string) => calls.push(method),
      close: () => {},
      call: async (method: string, params: any) => {
        calls.push(method);
        if (method === 'thread/read' && state.readFailure)
          throw Error('read failed');
        if (method === 'thread/read')
          return {
            thread: {
              id: state.wrong ? 'other' : 'c1',
              status: { type: state.status },
              turns: [{ items }],
            },
          };
        if (method === 'turn/start') {
          messages.push(params);
          if (state.race) state.status = 'active';
          if (!state.defer)
            items.push({
              type: 'functionCallOutput',
              name: params.toolOutput.name,
              output: params.toolOutput.output,
            });
          if (state.lost) throw Error('response lost');
        }
        return method === 'initialize'
          ? { userAgent: `codex_app_server/${state.version}` }
          : {};
      },
    }),
  };
  const request = {
    notificationId: 'n1',
    conversationId: 'c1',
    target: 'draft:d1',
    state: 'held',
  };
  const call = (overrides: Record<string, string> = {}) =>
    deliver({ ...request, ...overrides }, registration, dependencies);
  const verify = (overrides: Record<string, string> = {}) =>
    deliver({ ...request, ...overrides }, registration, dependencies, true);
  return {
    call,
    verify,
    state,
    messages,
    items,
    calls,
    reg,
    save,
    registration,
    dir,
  };
}

test('同一会話への配信と対象拒否', async (t) => {
  const f = await fixture(t);
  for (const [i, state] of [
    'held',
    'decision',
    'failed',
    'completed',
  ].entries()) {
    const target = i % 2 ? 'desk:8' : 'draft:d1';
    assert.equal(
      await f.call({ notificationId: `n${i}`, target, state }),
      'delivered',
    );
    assert.deepEqual(f.messages[i], {
      threadId: 'c1',
      input: [],
      toolOutput: {
        name: 'issue_desk_watch',
        namespace: null,
        retain: true,
        output: `Issue Desk 追跡通知\n通知ID: n${i}\n対象: ${target}\n状態: ${state}`,
      },
    });
  }
  assert.equal(await f.call({ conversationId: 'other' }), 'invalid');
  assert.equal(await f.call({ target: 'desk:1\n' }), 'invalid');
  f.state.owner++;
  assert.equal(await f.call(), 'invalid');
  f.state.owner--;
  f.reg.host = 'other';
  f.save();
  assert.equal(await f.call(), 'invalid');
  f.reg.host = os.hostname();
  f.save();
  const { deliver } = await import('../scripts/codex-issue-watch-deliver.ts');
  assert.equal(
    await deliver(
      {
        notificationId: 'x',
        conversationId: 'c1',
        target: 'draft:d1',
        state: 'held',
      },
      path.join(f.dir, 'missing'),
    ),
    'invalid',
  );
  f.state.wrong = true;
  assert.equal(await f.call({ notificationId: 'wrong' }), 'unknown');
  assert.equal(f.messages.length, 4);
});

test('配信確認と再照合', async (t) => {
  const f = await fixture(t);
  f.state.status = 'active';
  assert.equal(await f.call(), 'busy');
  f.state.status = 'systemError';
  assert.equal(await f.call(), 'offline');
  f.state.status = 'notLoaded';
  assert.equal(await f.call(), 'unsupported');
  f.state.status = 'idle';
  f.state.defer = true;
  f.state.race = true;
  assert.equal(await f.call(), 'unknown');
  f.items.push({
    type: 'functionCallOutput',
    name: 'issue_desk_watch',
    output: '別の通知',
  });
  assert.equal(await f.call(), 'unknown');
  assert.equal(f.messages.length, 1);
  assert.equal(f.state.status, 'active');
  f.items.push({
    type: 'functionCallOutput',
    name: 'issue_desk_watch',
    output: f.messages[0].toolOutput.output,
  });
  f.state.wrong = true;
  assert.equal(await f.call(), 'unknown');
  f.state.wrong = false;
  assert.equal(await f.call(), 'delivered');
  assert.equal(f.messages.length, 1);
  f.state.status = 'idle';
  f.state.race = false;
  f.state.lost = true;
  assert.equal(await f.call({ notificationId: 'lost-pending' }), 'unknown');
  assert.equal(await f.call({ notificationId: 'lost-pending' }), 'unknown');
  assert.equal(f.messages.length, 2);
  f.state.defer = false;
  assert.equal(await f.call({ notificationId: 'lost' }), 'delivered');
  assert.equal(await f.call({ notificationId: 'lost' }), 'delivered');
  assert.equal(f.messages.length, 3);
  assert.equal(f.calls.includes('turn/steer'), false);
  assert.equal(f.calls.includes('turn/interrupt'), false);
});

test('CLI はヘルプと未対応結果を返す', async (t) => {
  const help = spawnSync(
    process.execPath,
    ['scripts/codex-issue-watch-deliver.ts', '--help'],
    { encoding: 'utf8' },
  );
  assert.equal(help.status, 0);
  assert.match(help.stdout, /--notification-id/);
  assert.match(help.stdout, /unsupported/);
  const f = await fixture(t);
  f.reg.codexVersion = '0.169.1';
  f.save();
  assert.equal(await f.call(), 'unsupported');
  assert.equal(f.messages.length, 0);
  const cli = spawnSync(
    process.execPath,
    [
      'scripts/codex-issue-watch-deliver.ts',
      '--registration',
      f.registration,
      '--notification-id',
      'n1',
      '--conversation-id',
      'c1',
      '--target',
      'draft:d1',
      '--state',
      'held',
    ],
    { encoding: 'utf8' },
  );
  assert.equal(cli.status, 0);
  assert.equal(JSON.parse(cli.stdout).status, 'invalid');
  assert.deepEqual(fs.readdirSync(path.join(f.dir, 'journal')), []);
});

test('合成アプリサーバーの配信後にrecord入口が履歴を照合する', async (t) => {
  const dir = fs.mkdtempSync('/tmp/cw-');
  const sockets = path.join(dir, 's');
  const base = path.join(dir, '.issue-desk');
  const watch = path.join(base, 'codex-watch');
  const recovery = path.join(base, 'recovery');
  const journal = path.join(watch, 'journal');
  for (const folder of [sockets, base, watch, recovery, journal])
    fs.mkdirSync(folder, { mode: 0o700 });
  const conversationId = '11111111-1111-4111-8111-111111111111';
  const sendId = '22222222-2222-4222-8222-222222222222';
  const draftId = '33333333-3333-4333-8333-333333333333';
  const notificationId = '44444444-4444-4444-8444-444444444444';
  const target = `draft:${draftId}`;
  const identity = { sendId, conversationId, targetId: target, notificationId };
  const digest = crypto
    .createHash('sha256')
    .update(JSON.stringify([sendId, conversationId, target, notificationId]))
    .digest('hex');
  const file = path.join(watch, `${digest}.json`);
  const pending = {
    ...identity,
    notification: 'pending' as const,
    response: { kind: 'pending' as const },
  };
  saveWatchState(file, pending);
  const privateWrite = (fileName: string, value: object) =>
    fs.writeFileSync(fileName, JSON.stringify(value), { mode: 0o600 });
  privateWrite(path.join(recovery, `${sendId}.json`), {
    sendId,
    threadId: conversationId,
    host: os.hostname(),
    receiptState: 'unreconciled',
  });
  privateWrite(path.join(watch, `event-${notificationId}.json`), {
    startedAt: 1,
  });
  const draft = {
    accepted: true,
    requestId: sendId,
    draftId,
    id: draftId,
    project: 'example/project',
    status: 'stopped',
  };
  fs.writeFileSync(
    path.join(base, 'desk'),
    `#!/bin/sh\nprintf '%s\\n' '${JSON.stringify(draft)}'\n`,
    { mode: 0o700 },
  );
  const socket = path.join(sockets, 'a'.repeat(64));
  const registration = path.join(dir, 'registration.json');
  privateWrite(registration, {
    host: os.hostname(),
    conversationId,
    socket,
    journal,
    codexVersion: '0.171.0',
  });
  const state: {
    items: any[];
    sent: any[];
    wrong: boolean;
    fail: boolean;
    saveFailure: boolean;
  } = { items: [], sent: [], wrong: false, fail: false, saveFailure: false };
  fs.writeFileSync(socket, '');
  const connection = {
    socketStat: () => ({ isSocket: () => true, uid: process.getuid!() }) as any,
    connect: async () => {
      class ServerSocket extends EventEmitter {
        write(frame: Buffer, callback?: (error?: Error) => void) {
          let size = frame[1] & 127;
          let offset = 2;
          if (size === 126) {
            size = frame.readUInt16BE(2);
            offset = 4;
          }
          if (size === 127) {
            size = Number(frame.readBigUInt64BE(2));
            offset = 10;
          }
          const mask = frame.subarray(offset, offset + 4);
          const content = Buffer.from(
            frame.subarray(offset + 4, offset + 4 + size),
          );
          for (let i = 0; i < content.length; i++) content[i] ^= mask[i % 4];
          const message = JSON.parse(content.toString());
          if (message.id) {
            let result: any = {};
            let error: any;
            if (message.method === 'initialize')
              result = { userAgent: 'codex_app_server/0.171.0' };
            if (message.method === 'thread/read') {
              if (state.fail)
                error = { code: -1, message: 'history unavailable' };
              else {
                if (state.saveFailure) fs.chmodSync(file, 0o644);
                result = {
                  thread: {
                    id: state.wrong ? 'other' : conversationId,
                    status: { type: 'idle' },
                    turns: [{ items: state.items }],
                  },
                };
              }
            }
            if (message.method === 'turn/start')
              state.sent.push(message.params);
            const payload = Buffer.from(
              JSON.stringify(
                error ? { id: message.id, error } : { id: message.id, result },
              ),
            );
            const header =
              payload.length < 126
                ? Buffer.from([0x81, payload.length])
                : Buffer.from([
                    0x81,
                    126,
                    payload.length >> 8,
                    payload.length & 255,
                  ]);
            queueMicrotask(() =>
              this.emit('data', Buffer.concat([header, payload])),
            );
          }
          callback?.();
          return true;
        }
        destroy() {
          this.emit('close');
          return this;
        }
      }
      return new Rpc(new ServerSocket() as any, Buffer.alloc(0));
    },
  };
  const oldHome = process.env.HOME;
  const oldThread = process.env.CODEX_THREAD_ID;
  process.env.HOME = dir;
  process.env.CODEX_THREAD_ID = conversationId;
  t.after(async () => {
    if (oldHome === undefined) delete process.env.HOME;
    else process.env.HOME = oldHome;
    if (oldThread === undefined) delete process.env.CODEX_THREAD_ID;
    else process.env.CODEX_THREAD_ID = oldThread;
    fs.rmSync(dir, { recursive: true, force: true });
  });
  const req = {
    conversationId,
    notificationId,
    target,
    state: 'held',
    guidance: true,
  };
  assert.equal(await deliver(req, registration, connection), 'unknown');
  assert.equal(state.sent.length, 1);
  const output = state.sent[0].toolOutput.output;
  const record = () =>
    watchMain(['record', notificationId, target, 'read'], {
      socketDirectory: sockets,
      connection,
    });
  const rejected = async () => {
    await assert.rejects(record());
    assert.deepEqual(resumeWatchState(file, identity), pending);
    assert.equal(state.sent.length, 1);
  };
  for (const item of [
    { type: 'userMessage', name: 'issue_desk_watch', output },
    {
      type: 'functionCallOutput',
      name: 'issue_desk_watch',
      output: output.replace(notificationId, sendId),
    },
    {
      type: 'functionCallOutput',
      name: 'issue_desk_watch',
      output: output.replace(target, 'desk:99'),
    },
  ]) {
    state.items = [item];
    await rejected();
  }
  state.items = [
    { type: 'functionCallOutput', name: 'issue_desk_watch', output },
  ];
  state.wrong = true;
  await rejected();
  state.wrong = false;
  state.fail = true;
  await rejected();
  state.fail = false;
  state.saveFailure = true;
  await assert.rejects(record());
  fs.chmodSync(file, 0o600);
  state.saveFailure = false;
  assert.deepEqual(resumeWatchState(file, identity), pending);
  assert.equal(state.sent.length, 1);
  await record();
  assert.deepEqual(resumeWatchState(file, identity), {
    ...pending,
    notification: 'delivered',
    response: { kind: 'investigated', evidenceId: notificationId },
  });
  assert.equal(state.sent.length, 1);
});
