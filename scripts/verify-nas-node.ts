import { spawnSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const required =
  'TestNASBoundedHealthVerifierPreservesPipeAndStrictMeaning/sh/node';
/** go test を流す関数。既定は spawnSync で、テストは結果だけを返す関数に差し替える。 */
type Execute = (
  command: string,
  args: string[],
  options: { cwd: string; env: NodeJS.ProcessEnv; encoding: 'utf8' },
) => {
  status?: number | null;
  stdout?: string;
  stderr?: string;
  error?: Error;
};
export function verifyNASNode({
  execute = spawnSync as Execute,
  env = process.env,
  output = console.log,
}: {
  execute?: Execute;
  env?: NodeJS.ProcessEnv;
  output?: (line: string) => void;
} = {}) {
  const root = fileURLToPath(new URL('../', import.meta.url));
  const result = execute(
    env.GO || 'go',
    [
      'test',
      '-json',
      '-count=1',
      '-run',
      '^TestNASBoundedHealthVerifierPreservesPipeAndStrictMeaning$',
      './internal/startup',
    ],
    {
      cwd: path.join(root, 'server'),
      env: { ...env, NAS_TEST_NODE_BINARY: env.NODE || process.execPath },
      encoding: 'utf8',
    },
  );
  let passed = false;
  let invalid = Boolean(result.error) || result.status !== 0;
  for (const line of (result.stdout || '').split('\n').filter(Boolean)) {
    try {
      const event = JSON.parse(line);
      if (event.Action === 'fail' || event.Action === 'skip') invalid = true;
      if (event.Test === required && event.Action === 'pass') passed = true;
    } catch {
      invalid = true;
    }
  }
  if (!passed || invalid) {
    output(
      `${required}: PASS が必要です\n${result.stdout || ''}${result.stderr || ''}${result.error || ''}`,
    );
    return 1;
  }
  output(`--- PASS: ${required}`);
  return 0;
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  process.exitCode = verifyNASNode();
}
