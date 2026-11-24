import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import crypto from 'node:crypto';
import { pathToFileURL } from 'node:url';

const id = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$(?![\s\S])/;
const target =
  /^(draft:[A-Za-z0-9][A-Za-z0-9_-]{0,127}|desk:[1-9][0-9]*)$(?![\s\S])/;
const states = new Set(['held', 'decision', 'failed', 'completed']);
const toolName = 'issue_desk_watch';
const usage = `Usage: node scripts/codex-issue-watch-deliver.ts --registration FILE --notification-id ID --conversation-id ID --target draft:ID|desk:NUMBER --state held|decision|failed|completed
結果: JSON status = delivered | busy | offline | unsupported | unknown | invalid
登録ファイルには host, conversationId, socket, journal, codexVersion を指定する。ID は英数字、_、-。結果不明は同じIDで照合し、再送しない。`;

type Message = {
  notificationId: string;
  conversationId: string;
  target: string;
  state: string;
  guidance?: boolean;
};
function frame(value: object) {
  const data = Buffer.from(JSON.stringify(value));
  const extra = data.length < 126 ? 0 : data.length < 65536 ? 2 : 8;
  const result = Buffer.alloc(2 + extra + 4 + data.length);
  result[0] = 0x81;
  result[1] = 0x80 | (extra === 0 ? data.length : extra === 2 ? 126 : 127);
  if (extra === 2) result.writeUInt16BE(data.length, 2);
  if (extra === 8) result.writeBigUInt64BE(BigInt(data.length), 2);
  const mask = crypto.randomBytes(4);
  mask.copy(result, 2 + extra);
  for (let i = 0; i < data.length; i++)
    result[2 + extra + 4 + i] = data[i] ^ mask[i % 4];
  return result;
}

export class Rpc {
  socket: import('node:net').Socket;
  closed = false;
  buffer = Buffer.alloc(0);
  sequence = 0;
  pending = new Map<
    number,
    { resolve: (value: any) => void; reject: (error: Error) => void }
  >();
  constructor(
    socket: import('node:net').Socket,
    head: Buffer,
    signal = AbortSignal.timeout(5000),
  ) {
    this.socket = socket;
    const abort = () => socket.destroy(new Error('rpc timeout'));
    signal.addEventListener('abort', abort, { once: true });
    socket.once('close', () => signal.removeEventListener('abort', abort));
    socket.on('data', (chunk: Buffer) => this.read(chunk));
    socket.on('error', () => this.fail());
    socket.on('close', () => this.fail());
    if (signal.aborted) abort();
    else if (head.length) this.read(head);
  }
  fail() {
    this.closed = true;
    for (const entry of this.pending.values())
      entry.reject(new Error('connection lost'));
    this.pending.clear();
  }
  read(chunk: Buffer) {
    this.buffer = Buffer.concat([this.buffer, chunk]);
    while (this.buffer.length >= 2) {
      const b = this.buffer;
      if ((b[0] & 0x80) === 0 || (b[1] & 0x80) !== 0) {
        this.socket.destroy();
        return;
      }
      let size = b[1] & 127;
      let offset = 2;
      if (size === 126) {
        if (b.length < 4) return;
        size = b.readUInt16BE(2);
        offset = 4;
      } else if (size === 127) {
        if (b.length < 10) return;
        const n = b.readBigUInt64BE(2);
        if (n > 16n * 1024n * 1024n) {
          this.socket.destroy();
          return;
        }
        size = Number(n);
        offset = 10;
      }
      if (size > 16 * 1024 * 1024) {
        this.socket.destroy();
        return;
      }
      if (b.length < offset + size) return;
      const content = b.subarray(offset, offset + size);
      this.buffer = b.subarray(offset + size);
      if ((b[0] & 15) === 8) {
        this.socket.destroy();
        return;
      }
      if ((b[0] & 15) !== 1) continue;
      try {
        const msg = JSON.parse(content.toString());
        const entry = this.pending.get(msg.id);
        if (!entry) continue;
        this.pending.delete(msg.id);
        if (msg.error) entry.reject(new Error('rpc error'));
        else entry.resolve(msg.result);
      } catch {
        this.socket.destroy();
        return;
      }
    }
  }
  call(method: string, params: object) {
    if (this.closed) return Promise.reject(new Error('connection closed'));
    const key = ++this.sequence;
    return new Promise<any>((resolve, reject) => {
      this.pending.set(key, { resolve, reject });
      this.socket.write(frame({ id: key, method, params }), (error) => {
        if (error) {
          this.pending.delete(key);
          reject(error);
        }
      });
    });
  }
  notify(method: string) {
    this.socket.write(frame({ method }));
  }
  close() {
    this.socket.destroy();
  }
}
function connect(socketPath: string): Promise<Rpc> {
  return new Promise((resolve, reject) => {
    const signal = AbortSignal.timeout(5000);
    const key = crypto.randomBytes(16).toString('base64');
    const request = http.request({
      socketPath,
      signal,
      path: '/',
      headers: {
        Connection: 'Upgrade',
        Upgrade: 'websocket',
        'Sec-WebSocket-Version': '13',
        'Sec-WebSocket-Key': key,
      },
    });
    request.on('upgrade', (response, socket, head) => {
      const expected = crypto
        .createHash('sha1')
        .update(key + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11')
        .digest('base64');
      if (response.headers['sec-websocket-accept'] !== expected) {
        socket.destroy();
        reject(new Error('bad upgrade'));
        return;
      }
      resolve(new Rpc(socket, head, signal));
    });
    request.on('response', (response) => {
      response.resume();
      reject(new Error('upgrade rejected'));
    });
    request.on('error', reject);
    request.end();
  });
}
function privateFile(file: string) {
  const fd = fs.openSync(file, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
  try {
    const stat = fs.fstatSync(fd);
    if (
      !stat.isFile() ||
      stat.uid !== process.getuid?.() ||
      (stat.mode & 0o077) !== 0
    )
      throw Error('unsafe registration');
    return JSON.parse(fs.readFileSync(fd, 'utf8'));
  } finally {
    fs.closeSync(fd);
  }
}
function privateDirectory(dir: string) {
  const stat = fs.lstatSync(dir);
  if (
    !stat.isDirectory() ||
    stat.uid !== process.getuid?.() ||
    (stat.mode & 0o077) !== 0
  )
    throw Error('unsafe journal');
}
function history(thread: any, req: Message, text: string) {
  if (thread?.id !== req.conversationId || !Array.isArray(thread.turns))
    throw Error('incomplete history');
  for (const turn of thread.turns) {
    if (!Array.isArray(turn.items)) throw Error('incomplete history');
    for (const item of turn.items)
      if (
        item?.type === 'functionCallOutput' &&
        item.name === toolName &&
        item.output === text
      )
        return true;
  }
  return false;
}
type Connection = Pick<Rpc, 'call' | 'notify' | 'close'>;
export async function deliver(
  req: Message,
  file: string,
  dependencies: {
    connect: (socketPath: string) => Promise<Connection>;
    socketStat: typeof fs.lstatSync;
  } = { connect, socketStat: fs.lstatSync },
  verifyOnly = false,
) {
  if (
    !req ||
    typeof req.notificationId !== 'string' ||
    typeof req.conversationId !== 'string' ||
    typeof req.target !== 'string' ||
    !id.test(req.notificationId) ||
    !id.test(req.conversationId) ||
    !target.test(req.target) ||
    !states.has(req.state)
  )
    return 'invalid';
  let reg: any;
  try {
    reg = privateFile(file);
    if (
      reg.host !== os.hostname() ||
      reg.conversationId !== req.conversationId ||
      !path.isAbsolute(reg.socket) ||
      !path.isAbsolute(reg.journal)
    )
      return 'invalid';
    privateDirectory(reg.journal);
    const sock = dependencies.socketStat(reg.socket);
    if (!sock.isSocket() || sock.uid !== process.getuid?.()) return 'invalid';
  } catch {
    return 'invalid';
  }
  if (reg.codexVersion !== '0.171.0') return 'unsupported';
  const label = `Issue Desk 追跡通知\n通知ID: ${req.notificationId}\n対象: ${req.target}\n状態: ${req.state}`;
  const text = req.guidance
    ? label +
      '\nこの回で受付の対象を読み、原因を確認してください。許可済みの対処は既存の経路で続け、明示停止・承認・上限を守ってください。調査と対処の証拠はこの会話から scripts/codex-issue-watch.ts record で記録してください（手順は docs/chat-operations.md「起票後の見張り」）。'
    : label;
  const key = crypto
    .createHash('sha256')
    .update(JSON.stringify([req.conversationId, req.notificationId]))
    .digest('hex');
  const journal = path.join(reg.journal, key);
  let rpc: Connection;
  try {
    rpc = await dependencies.connect(reg.socket);
  } catch {
    return 'offline';
  }
  try {
    const initialized = await rpc.call('initialize', {
      clientInfo: { name: 'issue_desk_watch', version: '1' },
      capabilities: { experimentalApi: true },
    });
    if (
      typeof initialized?.userAgent !== 'string' ||
      !/(?:^|[ /])0\.171\.0(?:$|[\s;)])/.test(initialized.userAgent)
    )
      return 'unsupported';
    rpc.notify('initialized');
    const read = async () =>
      (
        await rpc.call('thread/read', {
          threadId: req.conversationId,
          includeTurns: true,
        })
      )?.thread;
    const thread = await read();
    if (history(thread, req, text)) return 'delivered';
    if (verifyOnly) return 'unknown';
    if (fs.existsSync(journal)) return 'unknown';
    if (thread.status?.type === 'active') return 'busy';
    if (thread.status?.type === 'systemError') return 'offline';
    if (thread.status?.type !== 'idle') return 'unsupported';
    try {
      const fd = fs.openSync(journal, 'wx', 0o600);
      try {
        fs.writeFileSync(fd, text);
        fs.fsyncSync(fd);
      } finally {
        fs.closeSync(fd);
      }
      const dirFd = fs.openSync(reg.journal, fs.constants.O_RDONLY);
      try {
        fs.fsyncSync(dirFd);
      } finally {
        fs.closeSync(dirFd);
      }
    } catch {
      return 'unknown';
    }
    try {
      await rpc.call('turn/start', {
        threadId: req.conversationId,
        input: [],
        toolOutput: {
          name: toolName,
          namespace: null,
          output: text,
          retain: true,
        },
      });
    } catch {
      /* The request may have been accepted. */
    }
    try {
      return history(await read(), req, text) ? 'delivered' : 'unknown';
    } catch {
      return 'unknown';
    }
  } catch {
    return 'unknown';
  } finally {
    rpc.close();
  }
}
async function main(args: string[]) {
  if (args.length === 1 && args[0] === '--help') {
    console.log(usage);
    return;
  }
  const fields: Record<string, string> = {};
  const names = new Set([
    '--registration',
    '--notification-id',
    '--conversation-id',
    '--target',
    '--state',
  ]);
  for (let i = 0; i < args.length; i += 2) {
    if (!names.has(args[i]) || fields[args[i]] !== undefined || !args[i + 1]) {
      console.log(JSON.stringify({ status: 'invalid' }));
      process.exitCode = 2;
      return;
    }
    fields[args[i]] = args[i + 1];
  }
  const req = {
    notificationId: fields['--notification-id'],
    conversationId: fields['--conversation-id'],
    target: fields['--target'],
    state: fields['--state'],
  };
  if (
    !fields['--registration'] ||
    !path.isAbsolute(fields['--registration']) ||
    !req.notificationId ||
    !req.conversationId ||
    !req.target ||
    !req.state ||
    !id.test(req.notificationId) ||
    !id.test(req.conversationId) ||
    !target.test(req.target) ||
    !states.has(req.state)
  ) {
    console.log(JSON.stringify({ status: 'invalid' }));
    process.exitCode = 2;
    return;
  }
  console.log(
    JSON.stringify({ status: await deliver(req, fields['--registration']) }),
  );
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href)
  void main(process.argv.slice(2));
