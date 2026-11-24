import fs from 'node:fs/promises';
import path from 'node:path';
import { randomUUID } from 'node:crypto';

// Both registration and NAS pruning use checkout/.paced-lock. Never reclaim a
// lock or a use record based on PID/age: an unknown child may still need files.
export async function protectDeskCheckout(checkout: string) {
  const root = path.dirname(checkout);
  if (
    path.basename(root) !== 'checkout' ||
    !/^[0-9a-f]{40}$/.test(path.basename(checkout))
  )
    return { release: async () => {} };
  const lock = path.join(root, '.paced-lock');
  const records = path.join(root, '.paced-use', path.basename(checkout));
  const record = path.join(records, randomUUID());
  await fs.mkdir(lock, { mode: 0o700 });
  try {
    if (!(await fs.stat(path.join(checkout, 'scripts'))).isDirectory())
      throw Error('desk-paced: 使用するcheckoutがありません。');
    await fs.mkdir(records, { recursive: true, mode: 0o700 });
    await fs.writeFile(
      record,
      JSON.stringify({ version: 1, checkout, pid: process.pid }) + '\n',
      {
        flag: 'wx',
        mode: 0o600,
      },
    );
  } finally {
    await fs.rmdir(lock);
  }
  return {
    async release() {
      await fs.mkdir(lock, { mode: 0o700 });
      try {
        await fs.unlink(record);
        try {
          await fs.rmdir(records);
        } catch (error) {
          if (error.code !== 'ENOTEMPTY' && error.code !== 'EEXIST')
            throw error;
        }
      } finally {
        await fs.rmdir(lock);
      }
    },
  };
}
