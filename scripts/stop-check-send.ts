import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { connection, connectionArgs, openDesk } from './desk-client.ts';
import { replaceLines, withRecordLock } from './stop-check-record.ts';

export async function sendRecords(file, call) {
  let lines, snapshot;
  try {
    snapshot = fs.readFileSync(file, 'utf8');
    lines = snapshot.match(/[^\n]*\n/g) || [];
  } catch {
    return;
  }
  const sent = new Set();
  const malformed = new Set();
  const records = [];
  for (const line of lines) {
    try {
      const record = JSON.parse(line);
      records.push({ record, line });
    } catch {
      malformed.add(line);
    }
  }
  const rejected = new Set();
  async function send(batch) {
    try {
      await call(
        'POST',
        '/api/v1/stop-check-fires',
        batch.map((item) => item.record),
      );
      for (const item of batch) sent.add(item.line);
    } catch (error) {
      if (!(error?.status >= 400 && error.status < 500)) return; // Retry transport and server failures.
      if (batch.length > 1) {
        const middle = Math.floor(batch.length / 2);
        await send(batch.slice(0, middle));
        await send(batch.slice(middle));
      } else {
        try {
          fs.appendFileSync(`${file}.rejected`, batch[0].line, { mode: 0o600 });
          rejected.add(batch[0].line);
        } catch {} // Never discard a row that could not be quarantined.
      }
    }
  }
  for (let i = 0; i < records.length; i += 1000)
    await send(records.slice(i, i + 1000));
  if (!sent.size && !rejected.size && !malformed.size) return;
  try {
    withRecordLock(file, () => {
      const current = fs.readFileSync(file, 'utf8');
      if (!current.startsWith(snapshot)) return; // Another writer replaced the snapshot; retry later.
      let quarantined = false;
      if (malformed.size) {
        try {
          fs.appendFileSync(
            `${file}.rejected`,
            lines.filter((line) => malformed.has(line)).join(''),
            { mode: 0o600 },
          );
          quarantined = true;
        } catch {} // Keep malformed rows when quarantine fails.
      }
      const counts = new Map();
      for (const line of lines)
        if (
          sent.has(line) ||
          rejected.has(line) ||
          (quarantined && malformed.has(line))
        )
          counts.set(line, (counts.get(line) || 0) + 1);
      const remaining = lines.filter((line) => {
        const count = counts.get(line) || 0;
        if (!count) return true;
        counts.set(line, count - 1);
        return false;
      });
      remaining.push(
        snapshot.slice(lines.join('').length),
        current.slice(snapshot.length),
      );
      replaceLines(file, remaining);
    });
  } catch {}
}
async function main() {
  const parsed = connectionArgs(process.argv.slice(2));
  const desk = await openDesk(connection(parsed));
  try {
    await sendRecords(
      path.join(os.homedir(), '.issue-desk', 'stop-check-fires.jsonl'),
      desk.call,
    );
  } finally {
    desk.close();
  }
}
if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
)
  main().catch(() => {
    process.exitCode = 1;
  });
