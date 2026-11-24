// 5409 contract: one operation per shell fence under NAS/Ubuntu headings.
// Guidance consists of the immediately preceding paragraph/list, the text after
// that fence up to the next fence or a same/higher heading, and section/ancestor
// introductions. Sibling sections cannot supply missing or override unsafe advice.
// Every operation needs stop/report, no resend/bypass/auth or permission changes
// on 4xx/5xx/transport errors. List: missing/invalid disables is unknown, not empty.
// Set: unknown POST => reread list, compare provider/until/by/reason; unmatched =>
// unknown and stop. Explicit until matches sent value; omitted until uses setAt
// within request times and until=setAt+24h. Cancel: distinguish true/false from
// missing/nonboolean cancelled; reread after unknown outcome. Examples must not
// present malformed/missing responses as success, nor automatically resend POST.
// 5672 covers migration/references/distribution; 5673 covers windows/requests/time;
// this file's response/guidance cases complete the remaining 5347 contract.
// Shared classification is already tested in test/desk-get.test.ts:
// 「共通接続のエラーと対処を状態に関係なく安全に表示する」 (401/403/404/500).
// chat-http-bundle covers successful reads/404, not abnormal response classes.
// Here raw HTTP faults run THROUGH each Skill shell example and the shipped
// checkContractResponse/deskGet boundary; scoped prose checks complement, rather
// than prove, the executable behavior or arbitrary natural-language semantics.
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  existsSync,
  readdirSync,
  rmSync,
  cpSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import {
  operationExamples,
  exampleWindow,
  checkGuidance,
  verifyUnknownReadback,
} from './lib/provider-disable-examples.ts';
import {
  guidedExamples,
  besideExample,
  unsafeGuidance,
} from './lib/provider-disable-fixtures.ts';

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const skill = 'skills/provider-disable/SKILL.md';
const admin = 'skills/issue-desk-admin/SKILL.md';
const legacy = 'scripts/provider-disable.mjs';
const self = 'test/provider-disable-skill.test.ts';
const read = (root, name) => readFileSync(path.join(root, name), 'utf8');
const has = (root, name) => existsSync(path.join(root, name));
const put = (root, name, text) => {
  const dest = path.join(root, name);
  mkdirSync(path.dirname(dest), { recursive: true });
  writeFileSync(dest, text);
};

function registered(root) {
  try {
    const targets = JSON.parse(read(root, 'chat-deploy.json')).targets;
    if (!Array.isArray(targets)) throw new Error('targets が配列ではない');
    return {
      listed: targets.some(
        (x) => x && x.kind === 'skill' && x.name === 'provider-disable',
      ),
      error: null,
    };
  } catch (error) {
    return {
      listed: false,
      error: `chat-deploy.json を読めない: ${error.message}`,
    };
  }
}

// Resolve links from the admin document, not from the repository root.
function documentLinks(root, document) {
  if (!has(root, document)) return [];
  const text = read(root, document);
  return [...text.matchAll(/\[[^\]]*\]\(([^\s)]+)(?:\s+"[^"]*")?\)/g)]
    .map((m) => m[1].split('#')[0])
    .filter(Boolean)
    .filter((link) => !/^(?:[a-z]+:|\/\/|\/)/i.test(link))
    .map((link) =>
      path.resolve(root, path.dirname(document), decodeURIComponent(link)),
    );
}

function files(root, relative) {
  const full = path.join(root, relative);
  if (!existsSync(full)) return [];
  const stat = readdirSync(full, { withFileTypes: true });
  return stat.flatMap((entry) => {
    const name = path.join(relative, entry.name);
    if (name === 'client/node_modules' || name === 'client/dist') return [];
    if (entry.isDirectory()) return files(root, name);
    return entry.isFile() ? [name] : [];
  });
}

function legacyReferences(root, allowAdmin = false) {
  const roots = [
    'scripts',
    'skills',
    'deploy',
    'test',
    'shared',
    'client',
    'docs',
  ];
  const names = roots.flatMap((dir) => files(root, dir));
  names.push(
    ...readdirSync(root, { withFileTypes: true })
      .filter((e) => e.isFile() && /\.(?:md|txt|sh|mjs|js|ts)$/i.test(e.name))
      .map((e) => e.name),
  );
  const failures = [];
  for (const name of names) {
    if (name === self || name === legacy || (allowAdmin && name === admin))
      continue;
    const text = read(root, name);
    // printf arguments are data, except for shell command substitutions.
    const executable = text
      .split('\n')
      .map((line) =>
        line.replace(
          /\bprintf\s+(?:'[^']*'|"[^"]*"|[^;&|\n])*/g,
          (invocation) => {
            const substitutions =
              invocation.match(/\$\([^)]*\)|`[^`]*`|'[^']*'/g) || [];
            return substitutions
              .filter((part) => !part.startsWith("'"))
              .join(' ');
          },
        ),
      )
      .join('\n');
    // Imports can span lines; resolve relative specifiers against the importing file.
    const importPattern =
      /(?:\b(?:import|export)\s+(?:[^'";]*?\s+from\s*)?|\bimport\s*\(\s*|\brequire\s*\(\s*)['"]([^'"]*provider-disable\.mjs)['"]/g;
    for (const match of executable.matchAll(importPattern)) {
      const target = match[1];
      const resolved = path.resolve(root, path.dirname(name), target);
      if (resolved !== path.join(root, 'shared/src/provider-disable.ts'))
        failures.push(
          `${name}:${executable.slice(0, match.index).split('\n').length}: import ${target}`,
        );
    }
    const lines = executable.split('\n');
    for (const [i, line] of lines.entries()) {
      if (!line.includes('provider-disable.mjs')) continue;
      const unquoted = line.replace(
        /['"]([^'"]*provider-disable\.mjs)['"]/g,
        '$1',
      );
      if (
        /\b(?:node(?:js)?|desk-pc|(?:~\/\.issue-desk\/)?desk)\s+(?:[^\s`"']+\s+)*[^\s`"']*provider-disable\.mjs\b/.test(
          unquoted,
        )
      ) {
        failures.push(`${name}:${i + 1}: 実行参照`);
      }
    }
  }
  return failures;
}

function inspect(root) {
  const present = has(root, skill);
  const old = has(root, legacy);
  const { listed, error } = registered(root);
  const links = documentLinks(root, admin);
  const guided = links.includes(path.join(root, skill));
  if (!present && old && !listed && !error)
    return { state: 'before', errors: legacyReferences(root, true) };
  const errors = [];
  if (error) errors.push(error);
  if (!present) errors.push(`${skill} がない`);
  if (!listed) errors.push('chat-deploy.json に未登録');
  if (old) errors.push(`${legacy} が残存`);
  if (!guided) errors.push('admin から新 Skill へのリンクがない');
  for (const link of links)
    if (!existsSync(link)) errors.push(`admin のリンク切れ: ${link}`);
  for (const link of documentLinks(root, skill))
    if (!existsSync(link)) errors.push(`Skill のリンク切れ: ${link}`);
  errors.push(...legacyReferences(root));
  return {
    state: present && listed && !old && guided ? 'after' : 'partial',
    errors,
  };
}

function distribute(root) {
  const home = mkdtempSync(path.join(tmpdir(), 'provider-disable-home-'));
  try {
    const shell =
      'DIR=$REPO; DRY_RUN=0; NL="\n"; say() { printf "%s\\n" "$*"; }; plan() { :; }; . "$DIR/deploy/nas/update-skills.sh"; skills_listed';
    const run = spawnSync('sh', ['-c', shell], {
      encoding: 'utf8',
      env: {
        PATH: process.env.PATH,
        HOME: home,
        REPO: root,
        SKILLS_HOST_NODE: '1',
        SKILLS_NODE_MANAGED_EXTERNALLY: '1',
      },
    });
    assert.equal(run.status, 0, run.stderr);
    assert.doesNotMatch(run.stdout, /\[update:skills-failed\]/);
    for (const destination of ['.claude/skills', '.codex/skills']) {
      const base = path.join(home, destination);
      assert.equal(read(base, 'provider-disable/SKILL.md'), read(root, skill));
      for (const link of documentLinks(base, 'provider-disable/SKILL.md'))
        assert.ok(existsSync(link), `配布先の Skill のリンク切れ: ${link}`);
      const deployedAdmin = read(base, 'issue-desk-admin/SKILL.md');
      assert.equal(deployedAdmin, read(root, admin));
      const links = [
        ...deployedAdmin.matchAll(/\[[^\]]*\]\(([^\s)]+)(?:\s+"[^"]*")?\)/g),
      ].map((m) => m[1].split('#')[0]);
      assert.ok(
        links.some(
          (link) =>
            path.resolve(base, 'issue-desk-admin', link) ===
              path.join(base, 'provider-disable/SKILL.md') &&
            existsSync(path.resolve(base, 'issue-desk-admin', link)),
        ),
        '配布先で admin のリンクを解決できる',
      );
    }
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
}

function fixture() {
  const root = mkdtempSync(path.join(tmpdir(), 'provider-disable-fixture-'));
  // Exercise the actual distribution implementation and its hook dependencies.
  for (const dir of ['deploy/nas', 'scripts'])
    cpSync(path.join(repo, dir), path.join(root, dir), { recursive: true });
  rmSync(path.join(root, legacy), { force: true });
  // Keep the shipped scripts so distribution has all current hook sources.
  put(root, skill, '# provider-disable\n');
  put(root, admin, '[provider-disable](../provider-disable/SKILL.md)\n');
  put(
    root,
    'chat-deploy.json',
    JSON.stringify({
      targets: [
        { kind: 'skill', name: 'provider-disable' },
        { kind: 'skill', name: 'issue-desk-admin' },
      ],
    }),
  );
  return root;
}

test('実リポジトリの移行状態と配布構成', () => {
  const result = inspect(repo);
  assert.notEqual(
    result.state,
    'partial',
    `移行途中: ${result.errors.join('; ')}`,
  );
  assert.deepEqual(result.errors, []);
  if (result.state === 'before') {
    assert.ok(has(repo, legacy));
    assert.ok(has(repo, admin));
  } else distribute(repo);
});

test('完成構成を通常の配布関数で配布する', () => {
  const root = fixture();
  try {
    assert.deepEqual(inspect(root), { state: 'after', errors: [] });
    distribute(root);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('移行前の不正な配布設定と admin 以外の旧参照を拒否する', () => {
  const root = fixture();
  try {
    rmSync(path.join(root, skill));
    put(root, legacy, '// legacy\n');
    put(root, admin, 'node scripts/provider-disable.mjs\n');
    put(root, 'chat-deploy.json', '{"targets":[]}');
    assert.deepEqual(inspect(root), { state: 'before', errors: [] });
    for (const invalid of ['{', '{"targets":{}}']) {
      put(root, 'chat-deploy.json', invalid);
      const result = inspect(root);
      assert.equal(result.state, 'partial');
      assert.match(result.errors.join('\n'), /chat-deploy\.json を読めない/);
    }
    put(root, 'chat-deploy.json', '{"targets":[]}');
    put(root, 'docs/reference.md', 'node scripts/provider-disable.mjs\n');
    const result = inspect(root);
    assert.equal(result.state, 'before');
    assert.match(result.errors.join('\n'), /docs\/reference\.md:1: 実行参照/);
    rmSync(path.join(root, 'docs/reference.md'));
    put(root, 'scripts/legacy-command', 'node scripts/provider-disable.mjs\n');
    assert.match(
      inspect(root).errors.join('\n'),
      /scripts\/legacy-command:1: 実行参照/,
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('登録漏れ、リンク切れ、旧 CLI 参照を拒否する', () => {
  const root = fixture();
  try {
    put(root, 'chat-deploy.json', '{"targets":[]}');
    assert.match(inspect(root).errors.join('\n'), /未登録/);
    put(
      root,
      'chat-deploy.json',
      JSON.stringify({
        targets: [
          { kind: 'skill', name: 'provider-disable' },
          { kind: 'skill', name: 'issue-desk-admin' },
        ],
      }),
    );
    put(
      root,
      admin,
      '[provider-disable](../provider-disable/SKILL.md)\n[missing](missing.md)\n',
    );
    assert.match(inspect(root).errors.join('\n'), /リンク切れ/);
    put(root, admin, '[provider-disable](../provider-disable/SKILL.md)\n');
    put(root, 'deploy/legacy-command', 'node scripts/provider-disable.mjs\n');
    assert.match(
      inspect(root).errors.join('\n'),
      /deploy\/legacy-command:1: 実行参照/,
    );
    rmSync(path.join(root, 'deploy/legacy-command'));
    for (const command of [
      'node scripts/provider-disable.mjs',
      '~/.issue-desk/desk provider-disable.mjs',
      'desk-pc provider-disable.mjs',
      'node /app/scripts/provider-disable.mjs',
      'import x from "../scripts/provider-disable.mjs"',
      'import("../scripts/provider-disable.mjs")',
      'require("../scripts/provider-disable.mjs")',
    ]) {
      put(root, 'docs/reference.md', command);
      assert.match(inspect(root).errors.join('\n'), /参照|import/, command);
    }
    put(
      root,
      'docs/reference.md',
      'printf "%s\\n" "node scripts/provider-disable.mjs"\n',
    );
    assert.deepEqual(inspect(root).errors, []);
    for (const literal of [
      `import("../scripts/provider-disable.mjs")`,
      `require("../scripts/provider-disable.mjs")`,
      `import x from "../scripts/provider-disable.mjs"`,
    ]) {
      put(root, 'docs/reference.md', `printf '%s\\n' '${literal}'\n`);
      assert.deepEqual(inspect(root).errors, [], literal);
    }
    for (const command of [
      'printf "%s" "$(node scripts/provider-disable.mjs)"',
      'printf "%s" `node scripts/provider-disable.mjs`',
      `printf '%s' "$(node 'scripts/provider-disable.mjs')"`,
    ]) {
      put(root, 'docs/reference.md', command);
      assert.match(inspect(root).errors.join('\n'), /実行参照/, command);
    }
    put(
      root,
      'docs/reference.md',
      "printf '%s' '$(node scripts/provider-disable.mjs)'\n",
    );
    assert.deepEqual(inspect(root).errors, []);
    for (const separator of ['&&', '||', ';', '|']) {
      put(
        root,
        'docs/reference.md',
        `printf '確認\\n' ${separator} node scripts/provider-disable.mjs\n`,
      );
      assert.match(inspect(root).errors.join('\n'), /実行参照/, separator);
    }
    put(
      root,
      'docs/reference.md',
      'printf "%s\\n" "node scripts/provider-disable.mjs"\n',
    );
    put(
      root,
      'shared/src/provider-settings.ts',
      'import { providerInstead } from "./provider-disable.ts";\n',
    );
    assert.deepEqual(inspect(root).errors, []);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

// The examples use a fixed reference and an explicit JST-to-offset pair.
const exampleSkill = `# provider-disable
<!-- provider-disable-reference-time: 2026-11-08T10:00:00+09:00 -->
<!-- provider-disable-jst: 2026-11-09 10:00 = 2026-11-09T10:00+09:00 -->
## NAS
\`\`\`sh
sh ~/.issue-desk/desk-pc get --path /api/provider-disables
\`\`\`
\`\`\`sh
node -e 'process.stdout.write(JSON.stringify({reason:"maintenance"}))' > "$HOME/body.json"
sh ~/.issue-desk/desk-pc operation --method POST \\
  --path /api/provider-disables/codex --body-file "$HOME/body.json"
\`\`\`
\`\`\`sh
printf '%s' '{}' | sh ~/.issue-desk/desk-pc operation --method POST --path /api/provider-disables/codex/cancel --body-file -
\`\`\`
## Ubuntu
\`\`\`sh
~/.issue-desk/desk get --path /api/provider-disables
\`\`\`
\`\`\`sh
printf '%s' '{"until":"2026-11-09T10:00+09:00"}' > "$HOME/body.json"
~/.issue-desk/desk operation --method POST --path /api/provider-disables/claude --body-file "$HOME/body.json"
\`\`\`
\`\`\`sh
~/.issue-desk/desk operation --method POST --path /api/provider-disables/claude/cancel --body-file "$HOME/body.json"
\`\`\`
`;
// Cancel must use an empty JSON object, even when another example wrote a file.
const validExamples = exampleSkill.replace(
  '~/.issue-desk/desk operation --method POST --path /api/provider-disables/claude/cancel --body-file "$HOME/body.json"',
  `printf '%s' '{}' | ~/.issue-desk/desk operation --method POST --path /api/provider-disables/claude/cancel --body-file -`,
);

function checkedDate(value) {
  const m =
    /^(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d)(?::(\d\d)(?:\.(\d{1,3}))?)?(Z|[+-]\d\d:\d\d)$/.exec(
      value,
    );
  if (!m) throw Error('日時形式');
  const [, y, mo, d, h, mi, s = '0', fraction = '', zone] = m;
  const date = new Date(0);
  date.setUTCFullYear(+y, +mo - 1, +d);
  date.setUTCHours(+h, +mi, +s, +(fraction.padEnd(3, '0') || 0));
  if (
    date.getUTCFullYear() !== +y ||
    date.getUTCMonth() !== +mo - 1 ||
    date.getUTCDate() !== +d ||
    +h > 23 ||
    +mi > 59 ||
    +s > 59
  )
    throw Error('存在しない日時');
  if (zone !== 'Z') {
    const zh = +zone.slice(1, 3),
      zm = +zone.slice(4, 6);
    if (zh > 23 || zm > 59) throw Error('不正なオフセット');
    date.setTime(
      date.getTime() - (zone[0] === '+' ? 1 : -1) * (zh * 60 + zm) * 60000,
    );
  }
  return date.getTime();
}
function checkBody(provider, body, now) {
  if (!['codex', 'claude'].includes(provider)) throw Error('provider');
  if (!body || typeof body !== 'object' || Array.isArray(body))
    throw Error('本文');
  if (Object.hasOwn(body, 'until')) {
    if (typeof body.until !== 'string') throw Error('until');
    const end = checkedDate(body.until);
    if (end <= now || end - now > 7 * 86400000) throw Error('期限');
  }
  if (
    Object.hasOwn(body, 'reason') &&
    (typeof body.reason !== 'string' ||
      body.reason.length > 200 ||
      // eslint-disable-next-line no-control-regex -- 入力や表示から制御文字を除外する安全性検証。
      /[\u0000-\u001f\u007f-\u009f]/u.test(body.reason))
  )
    throw Error('reason');
}
function inspectExamples(markdown) {
  const reference =
    /<!--\s*provider-disable-reference-time:\s*(\S+)\s*-->/.exec(markdown);
  const pair =
    /<!--\s*provider-disable-jst:\s*(\d{4}-\d\d-\d\d[ T]\d\d:\d\d(?::\d\d)?)\s*=\s*(\S+)\s*-->/.exec(
      markdown,
    );
  assert.ok(reference && pair, '基準時刻と日本時間の対応が必要');
  const now = checkedDate(reference[1]);
  assert.equal(pair[2], pair[1].replace(' ', 'T') + '+09:00');
  checkedDate(pair[2]);
  const home = mkdtempSync(path.join(tmpdir(), 'provider-examples-'));
  try {
    const bin = path.join(home, '.issue-desk');
    mkdirSync(bin);
    // This request-only recorder supplies a normal response so executable
    // response guards can run too; raw faults use exampleWindow below.
    const recorder = `#!/bin/sh\nnode -e 'const fs=require("fs"); const a=process.argv.slice(1); let b=""; process.stdin.setEncoding("utf8"); process.stdin.on("data",x=>b+=x); process.stdin.on("end",()=>{fs.appendFileSync(process.env.CAPTURE, JSON.stringify({args:a,stdin:b,window:process.env.WINDOW_NAME})+"\\n"); console.log(JSON.stringify({disables:[],set:{provider:a[a.indexOf("--path")+1]?.split("/")[3]},cancelled:true}));});' "$@"\n`;
    writeFileSync(
      path.join(bin, 'desk-pc'),
      `WINDOW_NAME=desk-pc\nexport WINDOW_NAME\n${recorder}`,
      { mode: 0o755 },
    );
    const captures = [];
    let section = '',
      pairedUntil = false;
    for (const example of operationExamples(markdown)) {
      const { code, environment } = example;
      section = environment;
      assert.ok(section, '環境の節が必要');
      // In NAS the similarly named local runner accepts only scripts.
      writeFileSync(
        path.join(bin, 'desk'),
        section === 'NAS'
          ? '#!/bin/sh\ncase "$1" in *.mjs|*.ts) exit 0;; *) exit 2;; esac\n'
          : `WINDOW_NAME=desk\nexport WINDOW_NAME\n${recorder}`,
        { mode: 0o755 },
      );
      const capture = path.join(home, 'capture');
      writeFileSync(capture, '');
      const run = spawnSync('sh', ['-eu', '-c', code], {
        cwd: home,
        encoding: 'utf8',
        env: {
          HOME: home,
          PATH: `${bin}:${path.dirname(process.execPath)}:/usr/bin:/bin`,
          CAPTURE: capture,
        },
      });
      assert.equal(run.status, 0, `例の実行失敗: ${run.stderr}`);
      const calls = readFileSync(capture, 'utf8')
        .trim()
        .split('\n')
        .filter(Boolean)
        .map((line) => JSON.parse(line));
      assert.ok(calls.length, '窓口の呼出しが必要');
      for (const call of calls) {
        assert.equal(call.window, section === 'NAS' ? 'desk-pc' : 'desk');
        const args = call.args;
        assert.ok(!args.includes('--config'), '--config は窓口が付与');
        const option = (name) => {
          const i = args.indexOf(name);
          return i < 0 ? undefined : args[i + 1];
        };
        const endpoint = option('--path');
        let operation;
        if (args[0] === 'get' && endpoint === '/api/provider-disables')
          operation = 'list';
        else {
          assert.equal(args[0], 'operation');
          assert.equal(option('--method'), 'POST');
          const match =
            /^\/api\/provider-disables\/(codex|claude)(\/cancel)?$/.exec(
              endpoint || '',
            );
          assert.ok(match, '経路');
          operation = match[2] ? 'cancel' : 'set';
          const file = option('--body-file');
          assert.ok(file, '本文');
          const bodyPath = file === '-' ? null : path.resolve(home, file);
          if (bodyPath)
            assert.ok(
              bodyPath.startsWith(home + path.sep),
              '本文ファイルは隔離 HOME 内',
            );
          const body = JSON.parse(
            bodyPath ? readFileSync(bodyPath, 'utf8') : call.stdin,
          );
          if (operation === 'cancel') assert.deepEqual(body, {});
          else {
            checkBody(match[1], body, now);
            if (body.until === pair[2]) pairedUntil = true;
          }
        }
        captures.push(`${section}:${operation}`);
      }
    }
    for (const env of ['NAS', 'Ubuntu'])
      for (const op of ['list', 'set', 'cancel'])
        assert.ok(captures.includes(`${env}:${op}`), `${env}:${op} が必要`);
    assert.ok(pairedUntil, '日本時間と対応する期限を設定例に含める');
    return captures;
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
}

test('操作例を隔離した窓口で実行し環境と要求を照合する', () => {
  inspectExamples(validExamples);
  const state = inspect(repo);
  if (state.state === 'after') inspectExamples(read(repo, skill));
  else assert.equal(state.state, 'before');
  inspectExamples(validExamples.replace('maintenance', 'planned work'));
});

test('誤った窓口、経路、メソッド、本文と設定指定を拒否する', () => {
  const changes = [
    ['sh ~/.issue-desk/desk-pc get', 'sh ~/.issue-desk/desk get'],
    ['~/.issue-desk/desk get', 'sh ~/.issue-desk/desk-pc get'],
    ['--method POST', '--method GET'],
    ['/codex/cancel', '/codex'],
    ['/provider-disables/codex --body', '/provider-disables/other --body'],
    ["printf '%s' '{}' | sh", `printf '%s' '{"reason":"x"}' | sh`],
    ['{"until":"2026-11-09T10:00+09:00"}', '{"until":""}'],
    ['{"until":"2026-11-09T10:00+09:00"}', '{"until":null}'],
    ['{"until":"2026-11-09T10:00+09:00"}', '{bad}'],
    ['get --path', 'get --config /tmp/config --path'],
  ];
  for (const [from, to] of changes) {
    assert.ok(validExamples.includes(from));
    assert.throws(
      () => inspectExamples(validExamples.replace(from, to)),
      undefined,
      `${from} → ${to}`,
    );
  }
});

test('固定受付時刻と UTF-16 の境界で設定例を判定する', () => {
  const withBody = (body) =>
    validExamples.replace(
      'JSON.stringify({reason:"maintenance"})',
      `JSON.stringify(${JSON.stringify(body)})`,
    );
  for (const until of [
    '2026-11-08T09:59+09:00',
    '2026-11-08T10:00+09:00',
    '2026-11-15T10:00:00.001+09:00',
    '',
    null,
    '2026-02-30T10:00+09:00',
  ])
    assert.throws(() => inspectExamples(withBody({ until })));
  for (const until of ['2026-11-08T10:01+09:00', '2026-11-15T10:00+09:00'])
    inspectExamples(withBody({ until }));
  inspectExamples(withBody({}));
  inspectExamples(withBody({ reason: '😀'.repeat(100) }));
  for (const reason of [
    '😀'.repeat(100) + 'a',
    '\u0000',
    '\u007f',
    '\u0085',
    '\u009f',
  ])
    assert.throws(() => inspectExamples(withBody({ reason })));
  assert.throws(() =>
    inspectExamples(
      validExamples
        .replace('2026-11-09 10:00 =', '2026-02-30 10:00 =')
        .replace('2026-11-09T10:00+09:00 -->', '2026-02-30T10:00+09:00 -->'),
    ),
  );
});

const guidanceSources = () =>
  [0, 1, 2]
    .map((n) => guidedExamples(validExamples, n))
    .concat(inspect(repo).state === 'after' ? [read(repo, skill)] : []);

test('停止・読取・未確認の言い換えと移行後の実Skillを操作結果に対応付ける', async () => {
  for (const source of guidanceSources()) {
    inspectExamples(source);
    const window = await exampleWindow(source);
    try {
      for (const example of window.examples) {
        const result = await window.run(example);
        assert.equal(result.code, 0, `${example.line}: ${result.stderr}`);
        assert.equal(result.captured.length, 1, '1フェンス1操作');
        const call = result.captured[0];
        checkGuidance(example, call);
        const value = JSON.parse(result.stdout);
        assert.ok(Array.isArray(value.disables));
        if (call.operation === 'set') {
          assert.equal(value.set.provider, call.path.split('/')[3]);
          assert.equal(value.set.reason, call.body.reason);
          assert.equal(value.set.by, 'fixture-operator');
          if (call.body.until !== undefined)
            assert.equal(value.set.until, call.body.until);
          else {
            assert.ok(
              Date.parse(value.set.setAt) >= call.start &&
                Date.parse(value.set.setAt) <= call.end,
            );
            assert.equal(
              Date.parse(value.set.until) - Date.parse(value.set.setAt),
              86400000,
            );
          }
        }
        if (call.operation === 'cancel') {
          assert.equal(value.cancelled, true);
          const absent = await window.run(example, 'absent');
          assert.equal(absent.code, 0);
          assert.equal(JSON.parse(absent.stdout).cancelled, false);
          checkGuidance(example, absent.captured[0]);
        }
      }
    } finally {
      await window.close();
    }
  }
});

test('各Skill例は生のHTTP異常・通信失敗・不正応答・欠測を成功や空一覧にしない', async () => {
  for (const source of [guidanceSources()[0], ...guidanceSources().slice(3)]) {
    const window = await exampleWindow(source);
    try {
      for (const example of window.examples)
        for (const fault of [
          400,
          401,
          403,
          404,
          409,
          502,
          503,
          'refused',
          'disconnect',
          'not-json',
          'wrong-type',
          'missing',
          'missing-disables',
          'bad-field',
          'null-field',
        ]) {
          const result = await window.run(example, fault);
          const label = `${example.environment}:行${example.line}:${fault}`;
          assert.notEqual(result.code, 0, label);
          assert.equal(result.captured.length, 1, `${label}: 窓口の再送なし`);
          assert.equal(
            result.requests.length,
            fault === 'refused' ? 0 : 1,
            `${label}: HTTP再送なし`,
          );
          assert.doesNotMatch(
            result.stdout,
            /"disables"\s*:\s*\[\s*\]|"cancelled"\s*:\s*(?:true|false)|"set"\s*:|成功|設定なし/,
            label,
          );
          for (const file of result.files) {
            assert.doesNotMatch(
              file.content,
              /成功|設定なし/,
              `${label}: ${file.name}`,
            );
            if (file.name !== 'result.json') continue;
            let value;
            try {
              value = JSON.parse(file.content);
            } catch {
              continue;
            }
            const op = result.captured[0].operation;
            const complete =
              value &&
              !Array.isArray(value) &&
              Array.isArray(value.disables) &&
              (op === 'list' ||
                (op === 'set'
                  ? typeof value.set?.provider === 'string'
                  : typeof value.cancelled === 'boolean'));
            assert.ok(
              !complete,
              `${label}: ${file.name} に完全な成功応答を残さない`,
            );
          }
          checkGuidance(example, result.captured[0]);
        }
    } finally {
      await window.close();
    }
  }
});

// The real window prints refusals on stdout and exits 1 without stderr. Each
// shipped example must still surface the refusal once, on stderr, without rerun.
test('実窓口の出力の形でも実Skillの各例が拒否理由を標準エラーへ出す', async () => {
  const sources = guidanceSources().slice(3);
  assert.equal(sources.length, inspect(repo).state === 'after' ? 1 : 0);
  for (const source of sources) {
    const window = await exampleWindow(source);
    try {
      for (const example of window.examples) {
        const label = `${example.environment}:行${example.line}`;
        const ok = await window.run(example, 'ok', { shape: 'real' });
        assert.equal(ok.code, 0, `${label}: ${ok.stderr}`);
        assert.ok(Array.isArray(JSON.parse(ok.stdout).disables), label);
        for (const status of [400, 403, 409, 503]) {
          const refused = await window.run(example, status, { shape: 'real' });
          assert.notEqual(refused.code, 0, `${label}:${status}`);
          assert.equal(
            refused.stdout.trim(),
            '',
            `${label}:${status}: 拒否を標準出力に出さない`,
          );
          assert.match(
            refused.stderr,
            new RegExp(`"status":${status}\\b`),
            `${label}:${status}: 状態コード`,
          );
          assert.match(
            refused.stderr,
            /"error":"拒否/,
            `${label}:${status}: 拒否理由`,
          );
          assert.match(
            refused.stderr,
            /"redacted":true/,
            `${label}:${status}: 伏せた旨`,
          );
          assert.equal(
            refused.requests.length,
            1,
            `${label}:${status}: 再送なし`,
          );
        }
        for (const fault of [
          'not-json',
          'wrong-type',
          'missing',
          'disconnect',
        ]) {
          const unknown = await window.run(example, fault, { shape: 'real' });
          assert.notEqual(unknown.code, 0, `${label}:${fault}`);
          assert.equal(unknown.stdout.trim(), '', `${label}:${fault}`);
          assert.match(
            unknown.stderr,
            /未確認/,
            `${label}:${fault}: 黙って終わらない`,
          );
          assert.equal(
            unknown.requests.length,
            1,
            `${label}:${fault}: 再送なし`,
          );
        }
      }
    } finally {
      await window.close();
    }
  }
});

test('一つの操作例だけの危険な案内と別節だけの正しい案内を拒否する', async () => {
  const source = guidanceSources()[0],
    originals = operationExamples(source);
  for (const original of originals) {
    for (const unsafe of unsafeGuidance) {
      const changed = besideExample(source, original, unsafe),
        window = await exampleWindow(changed);
      try {
        const example = window.examples.find((e) => e.line === original.line);
        const result = await window.run(example);
        assert.equal(result.code, 0);
        assert.throws(
          () => checkGuidance(example, result.captured[0]),
          (error: Error) =>
            error.message.includes(`行${original.line}: 禁止案内`),
        );
        for (const other of window.examples.filter((e) => e !== example)) {
          const otherResult = await window.run(other);
          assert.equal(otherResult.code, 0);
          checkGuidance(other, otherResult.captured[0]);
        }
      } finally {
        await window.close();
      }
    }
    const lines = source.split('\n'),
      local = lines.slice(original.end + 1, original.section.end).join('\n');
    lines.splice(original.end + 1, original.section.end - original.end - 1);
    const window = await exampleWindow(
      lines.join('\n') + '\n## 別の節\n' + local,
    );
    try {
      const example = window.examples.find((e) => e.line === original.line),
        result = await window.run(example);
      assert.equal(result.code, 0);
      assert.throws(
        () => checkGuidance(example, result.captured[0]),
        (error: Error) => error.message.includes(`行${original.line}:`),
      );
      for (const other of window.examples.filter((e) => e !== example)) {
        const otherResult = await window.run(other);
        assert.equal(otherResult.code, 0);
        checkGuidance(other, otherResult.captured[0]);
      }
    } finally {
      await window.close();
    }
  }
});

test('各設定例で照合不能時の未確認案内が欠けると拒否する', async () => {
  const sentences = [
    '照合できなければ未確認で停止する。',
    '一致しなければ確定できないので中止する。',
    '確認が取れなければ確認できていないとして止める。',
  ];
  for (let variant = 0; variant < sentences.length; variant++) {
    const source = guidedExamples(validExamples, variant);
    for (const original of operationExamples(source).filter(
      (e) => /--method POST/.test(e.code) && !/\/cancel/.test(e.code),
    )) {
      const lines = source.split('\n');
      const index = lines.findIndex(
        (line, i) =>
          i > original.section.start &&
          i < original.section.end &&
          line.includes(sentences[variant]),
      );
      assert.notEqual(index, -1);
      lines[index] = lines[index].replace(
        sentences[variant],
        '未確認という状態を区別する。',
      );
      const window = await exampleWindow(lines.join('\n'));
      try {
        for (const example of window.examples) {
          const result = await window.run(example);
          assert.equal(result.code, 0);
          if (example.line === original.line)
            assert.throws(
              () => checkGuidance(example, result.captured[0]),
              /照合不能時の未確認/,
            );
          else checkGuidance(example, result.captured[0]);
        }
      } finally {
        await window.close();
      }
    }
  }
});

test('POST結果不明は再送せず実行例の一覧で照合し一致不能なら未確認に止める', async () => {
  for (const source of [guidanceSources()[0], ...guidanceSources().slice(3)]) {
    const window = await exampleWindow(source);
    try {
      await verifyUnknownReadback(window);
    } finally {
      await window.close();
    }
  }
});
