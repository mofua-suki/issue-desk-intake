import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { syncDeskContract } from '../scripts/generate-desk-contract.ts';
import {
  deskContract,
  deskContractVersion,
  deskContractHeader,
  contractError,
  contractInstructions,
} from '../shared/src/desk-contract.ts';

test('契約生成は配布APIの挙動と指示を再現し、古い出力を拒否する', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'desk-contract-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const file = path.join(root, 'chat-instructions.md');
  fs.writeFileSync(
    file,
    'before\n<!-- issue-desk:desk-contract:begin -->\nold\n<!-- issue-desk:desk-contract:end -->\nafter\n',
  );
  assert.equal(await syncDeskContract(root), false);
  assert.equal(await syncDeskContract(root, true), true);
  assert.equal(await syncDeskContract(root), true);
  const generated = await import(
    pathToFileURL(path.join(root, 'scripts/lib/desk-contract.generated.ts'))
      .href
  );
  assert.deepEqual(generated.deskContract, deskContract);
  assert.equal(generated.deskContractVersion, deskContractVersion);
  assert.equal(generated.deskContractHeader, deskContractHeader);
  for (const code of [...Object.keys(deskContract.errors), 'UNKNOWN'])
    assert.deepEqual(generated.contractError(code), contractError(code));
  const instructions = fs.readFileSync(file, 'utf8');
  assert.ok(instructions.startsWith('before\n'));
  assert.ok(instructions.endsWith('\nafter\n'));
  assert.ok(
    instructions.includes(
      contractInstructions()
        .split('\n')
        .map((line) => `- ${line}`)
        .join('\n'),
    ),
  );
  fs.appendFileSync(
    path.join(root, 'scripts/lib/desk-contract.generated.ts'),
    '\n// stale\n',
  );
  assert.equal(await syncDeskContract(root), false);
  fs.writeFileSync(file, 'missing markers');
  await assert.rejects(syncDeskContract(root, true), /生成範囲/);
  assert.equal(fs.readFileSync(file, 'utf8'), 'missing markers');
});
