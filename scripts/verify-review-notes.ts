import { spawn, spawnSync } from 'node:child_process';
import { createInterface } from 'node:readline';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  isTestPostgresURL,
  runCommand,
  withTestPostgres,
} from './with-test-postgres.ts';

// Keep required tests explicit: go test exits successfully even if -run matches
// nothing, or all PostgreSQL tests skip. A package-level "ok" is not evidence.
export const requiredTests = [
  ['consultations', 'TestSplitParentReconcileAfterLateFiling'],
  ['consultations', 'TestSplitParentReconcileNestedAndFinishedChildren'],
  ['consultations', 'TestSplitParentReconcileIncompleteAndInvalidFamily'],
  ['consultations', 'TestSplitParentReconcileBusyThenRetry'],
  ['consultations', 'TestSplitParentReconcileKeepsGroupLinkFailure'],
  ['consultations', 'TestSplitParentReconcileAtomicAndIdempotent'],
  ['anomalies', 'TestPostgresDetectMatchesLegacy'],
  ['anomalies', 'TestPostgresHistoryBatchResultsAndIndex'],
  ['reviewnotes', 'TestPostgresKnownOnlySkipsAIAndPublishes'],
  ['reviewnotes', 'TestPostgresOnlyUnknownNotesReachAI'],
  [
    'reviewnotes',
    'TestPostgresAIFailureKeepsCursorResultsAndCacheUntilNextDay',
  ],
  [
    'reviewnotes',
    'TestPostgresCacheRequiresVersionExpiryRepositoryAndOriginalRow',
  ],
  ['reviewnotes', 'TestPostgresCacheDoesNotMatchClippedPrefix'],
  ['reviewnotes', 'TestPostgresCacheAndAdoptionSaveAtomically'],
  ['reviewnotes', 'TestCacheStorageLimits'],
  ['reviewnotes', 'TestPostgresLongSourceIsNotCachedAndIsReclassified'],
  ['reviewnotes', 'TestAIClassifierBoundsAndTreatsInstructionsAsJSONData'],
  ['reviewnotes', 'TestAIClassifierRejectsUnsupportedAndAmbiguousAssignments'],
  ['startup', 'TestReviewNotesExistingCycleStartsAndCloseCancelsPostgres'],
  ['startup', 'TestReviewNotesCapacityAndTemporarySwitchAfterWaitPostgres'],
  ['startup', 'TestReviewNotesRunnerFailuresRecordUsagePostgres'],
  ['startup', 'TestReviewNotesDoesNotBlockRecoveryCyclePostgres'],
  ['startup', 'TestReviewNotesCycleAvoidsRepeatedDatabaseClaimPostgres'],
  ['startup', 'TestReviewNotesAttemptsSkipAndRetry'],
  ['startup', 'TestReviewNotesAttemptsMidnightWait'],
  ['ai', 'TestDataOnlyRejectsToolsBeforeLaunching'],
].map(([pkg, test]) => [`issue-desk-intake/server/internal/${pkg}`, test]);

/** go test を流す関数。既定は spawnSync で、テストは結果だけを返す関数に差し替える。 */
type Execute = (
  command: string,
  args: string[],
  options: {
    cwd: string;
    env: NodeJS.ProcessEnv;
    encoding: 'utf8';
    maxBuffer?: number;
  },
) => {
  status?: number | null;
  stdout?: string;
  stderr?: string;
  error?: Error;
};
function checkTestPostgres(env: NodeJS.ProcessEnv, output = console.log) {
  if (isTestPostgresURL(env.ISSUE_DESK_TEST_POSTGRES_DSN)) return true;
  output(
    '注意点の固定テストには、issue_desk_testで始まる専用DBのISSUE_DESK_TEST_POSTGRES_DSN（PostgreSQL URL）が必要です。未実行を成功として扱いません。',
  );
  return false;
}

export function verifyReviewNotes({
  env = process.env,
  execute = spawnSync as Execute,
  output = console.log,
  consultationsOnly = false,
}: {
  env?: NodeJS.ProcessEnv;
  execute?: Execute;
  output?: (line: string) => void;
  consultationsOnly?: boolean;
} = {}) {
  // Never print the connection string. Each test also checks the dedicated DB
  // name and migrates its own isolated schema using the shipped migrations.
  if (!checkTestPostgres(env, output)) return 1;
  const root = fileURLToPath(new URL('../', import.meta.url));
  const selectedTests = consultationsOnly
    ? requiredTests.filter(([pkg]) => pkg.endsWith('/consultations'))
    : requiredTests;
  const pattern = `^(${selectedTests.map(([, test]) => test).join('|')})$`;
  const result = execute(
    env.GO || 'go',
    [
      'test',
      '-json',
      '-count=1',
      '-timeout=5m',
      '-run',
      pattern,
      './internal/consultations',
      ...(consultationsOnly
        ? []
        : [
            './internal/anomalies',
            './internal/reviewnotes',
            './internal/startup',
            './internal/ai',
          ]),
    ],
    {
      cwd: path.join(root, 'server'),
      env,
      encoding: 'utf8',
      maxBuffer: 16 * 1024 * 1024,
    },
  );
  const actions = new Map();
  const diagnostics = [];
  let invalid = Boolean(result.error) || result.status !== 0;
  for (const line of (result.stdout || '').split('\n').filter(Boolean)) {
    try {
      const event = JSON.parse(line);
      if (typeof event.Output === 'string') diagnostics.push(event.Output);
      if (event.Action === 'fail' || event.Action === 'skip') invalid = true;
      if (event.Test && ['pass', 'fail', 'skip'].includes(event.Action)) {
        actions.set(`${event.Package}#${event.Test}`, event.Action);
      }
    } catch {
      invalid = true;
    }
  }
  for (const [pkg, test] of selectedTests) {
    const action = actions.get(`${pkg}#${test}`);
    if (action !== 'pass') invalid = true;
    output(
      `--- ${action === 'pass' ? 'PASS' : action === 'skip' ? 'SKIP' : action === 'fail' ? 'FAIL' : 'NOT RUN'}: ${test} (${pkg})`,
    );
  }
  if (invalid) {
    const url = new URL(env.ISSUE_DESK_TEST_POSTGRES_DSN);
    const secrets = [
      env.ISSUE_DESK_TEST_POSTGRES_DSN,
      url.password,
      decodeURIComponent(url.password),
    ].filter(Boolean);
    let diagnostic = `${diagnostics.join('')}${result.stderr || ''}`;
    for (const secret of secrets)
      diagnostic = diagnostic.replaceAll(secret, '[検証DB接続情報]');
    if (diagnostic) output(diagnostic.slice(-20_000));
    output(
      '注意点の固定テストが未実行・SKIP・失敗です。全件PASSの記録が必要です。',
    );
    return 1;
  }
  return 0;
}

// Keep evidence from the full Go run without retaining all test output.
export async function verifyGoResults(
  packages: string[],
  {
    race = false,
    env = process.env,
    signal,
  }: { race?: boolean; env?: NodeJS.ProcessEnv; signal?: AbortSignal } = {},
) {
  const required = requiredTests.filter(
    ([pkg]) => pkg.endsWith('/consultations') === race,
  );
  const actions = new Map<string, string>();
  const tails = new Map<string, string>();
  let invalid = false;
  const child = spawn(
    env.GO || 'go',
    [
      'test',
      '-json',
      '-count=1',
      ...(race ? ['-race'] : ['-timeout', '8m']),
      ...packages,
    ],
    { env, signal, stdio: ['ignore', 'pipe', 'inherit'] },
  );
  const closed = new Promise<number | null>((resolve) => {
    child.on('error', () => {
      invalid = true;
    });
    child.on('close', resolve);
  });
  const printDiagnostic = (key: string) => {
    let diagnostic = tails.get(key) || '';
    if (!diagnostic) return;
    const dsn = env.ISSUE_DESK_TEST_POSTGRES_DSN || '';
    if (dsn) {
      const url = new URL(dsn);
      for (const secret of [
        dsn,
        url.password,
        decodeURIComponent(url.password),
      ].filter(Boolean))
        diagnostic = diagnostic.replaceAll(secret, '[検証DB接続情報]');
    }
    console.log(diagnostic);
  };
  const lines = createInterface({ input: child.stdout });
  for await (const line of lines) {
    let event;
    try {
      event = JSON.parse(line);
    } catch {
      invalid = true;
      console.error(line);
      continue;
    }
    const pkg = event.Package || '';
    const outputKey = event.ImportPath ? 'build:' + event.ImportPath : pkg;
    if (typeof event.Output === 'string')
      tails.set(
        outputKey,
        ((tails.get(outputKey) || '') + event.Output).slice(-20000),
      );
    if (event.Action === 'build-fail') {
      invalid = true;
      printDiagnostic(outputKey);
      tails.delete(outputKey);
      continue;
    }
    const relevant = required.some(
      ([p, t]) =>
        p === pkg && (event.Test === t || event.Test?.startsWith(t + '/')),
    );
    if (relevant && (event.Action === 'skip' || event.Action === 'fail'))
      invalid = true;
    if (event.Test && ['pass', 'skip', 'fail'].includes(event.Action))
      actions.set(`${pkg}#${event.Test}`, event.Action);
    if (event.Action === 'fail') {
      invalid = true;
      if (event.Test) console.log(`--- FAIL: ${event.Test} (${pkg})`);
      if (event.FailedBuild) {
        const buildKey = 'build:' + event.FailedBuild;
        printDiagnostic(buildKey);
        tails.delete(buildKey);
      }
      printDiagnostic(pkg);
    }
    if (!event.Test && ['pass', 'fail', 'skip'].includes(event.Action))
      console.log(
        `${event.Action === 'pass' ? 'ok' : event.Action.toUpperCase()}\t${pkg}`,
      );
  }
  const status = await closed;
  if (status !== 0) invalid = true;
  for (const [pkg, test] of required) {
    const action = actions.get(`${pkg}#${test}`);
    console.log(
      `--- ${action === 'pass' ? 'PASS' : action === 'skip' ? 'SKIP' : action === 'fail' ? 'FAIL' : 'NOT RUN'}: ${test} (${pkg})`,
    );
    if (action !== 'pass') invalid = true;
  }
  return invalid ? 1 : 0;
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  try {
    if (process.argv[2] === '--check-dsn') {
      process.exitCode = checkTestPostgres(process.env) ? 0 : 1;
    } else if (process.argv[2] === '--normal-go') {
      process.exitCode = await verifyGoResults(process.argv.slice(3));
    } else if (process.argv[2] === '--consultations-race') {
      process.exitCode = await verifyGoResults(['./internal/consultations'], {
        race: true,
      });
    } else {
      process.exitCode = process.env.ISSUE_DESK_TEST_POSTGRES_DSN
        ? verifyReviewNotes({
            consultationsOnly: process.argv[2] === '--consultations-only',
          })
        : await withTestPostgres((env, signal) =>
            runCommand(
              process.execPath,
              [fileURLToPath(import.meta.url), ...process.argv.slice(2)],
              env,
              signal,
            ),
          );
    }
  } catch (error) {
    console.error(
      error instanceof Error && error.name !== 'AbortError'
        ? error.message
        : '固定検証を中止しました。',
    );
    process.exitCode = 1;
  }
}
