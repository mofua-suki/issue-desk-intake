// CLI handoff to tools verified by the current session. No network, shell execution or background scheduling.
import fs from 'node:fs';
import path from 'node:path';
import { randomUUID } from 'node:crypto';
import { pathToFileURL } from 'node:url';
import {
  startMonitor,
  nextMonitor,
  receiveMonitor,
  cancelMonitor,
} from './lib/monitor-runtime.ts';
import type {
  MonitorConfig,
  MonitorState,
  Receipt,
} from './lib/monitor-contract.ts';

type Input = { config?: MonitorConfig; receipt?: Receipt; reason?: string };
type IO = { now: () => number; id: () => string };

export function monitorCli(
  argv: string[],
  input: Input,
  io: IO = { now: Date.now, id: randomUUID },
): MonitorState {
  const [command, flag, file, ...extra] = argv;
  if (
    !['start', 'next', 'result', 'status', 'cancel'].includes(command) ||
    flag !== '--file' ||
    !file ||
    extra.length
  )
    throw Error(
      'Usage: monitor-watch.ts start|next|result|status|cancel --file <absolute session record>; JSON input on stdin',
    );
  if (!path.isAbsolute(file))
    throw Error('Session record path must be absolute');
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const lock = `${file}.lock`;
  fs.mkdirSync(lock); // One writer, including cancellation; fail closed on overlap or a crashed owner.
  const temporary = `${file}.${io.id()}.tmp`;
  try {
    const now = io.now();
    let changed = command !== 'status';
    let state: MonitorState;
    if (command === 'start') {
      if (fs.existsSync(file))
        throw Error(
          'Record already exists; use status or a distinct session record',
        );
      state = startMonitor(input.config!, io.id(), now);
    } else {
      state = JSON.parse(fs.readFileSync(file, 'utf8'));
      if (state.version !== 1) throw Error('Unsupported monitor record');
      if (command === 'next') state = nextMonitor(state, now);
      if (command === 'result')
        state = receiveMonitor(state, input.receipt!, now);
      if (command === 'cancel') state = cancelMonitor(state, input.reason!);
      if (
        command === 'status' &&
        state.status === 'active' &&
        state.config.end.at !== undefined &&
        now >= state.config.end.at
      ) {
        state = nextMonitor(state, now);
        changed = true;
      }
    }
    if (changed) {
      fs.writeFileSync(temporary, `${JSON.stringify(state, null, 2)}\n`, {
        mode: 0o600,
        flag: 'wx',
      });
      fs.renameSync(temporary, file);
    }
    return state;
  } finally {
    fs.rmSync(temporary, { force: true });
    fs.rmdirSync(lock);
  }
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href
) {
  try {
    const command = process.argv[2];
    const input = ['start', 'result', 'cancel'].includes(command)
      ? JSON.parse(fs.readFileSync(0, 'utf8'))
      : {};
    process.stdout.write(
      `${JSON.stringify(monitorCli(process.argv.slice(2), input))}\n`,
    );
  } catch {
    // Neither tool output nor configuration (which may contain private observations) is echoed on error.
    process.stderr.write(
      'monitor-watch: request rejected; check arguments, receipt, record and exclusive access\n',
    );
    process.exitCode = 2;
  }
}
