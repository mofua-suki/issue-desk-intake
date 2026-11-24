import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import path from 'node:path';
import assert from 'node:assert/strict';
import test from 'node:test';

const root = fileURLToPath(new URL('..', import.meta.url));
const require = createRequire(
  new URL('../client/package.json', import.meta.url),
);
let typescriptPath;
try {
  typescriptPath = require.resolve('typescript');
} catch (error) {
  if (error.code !== 'MODULE_NOT_FOUND') throw error;
  throw new Error(
    'TypeScript が見つかりません。client/ で pnpm install --frozen-lockfile を実行してください。',
    { cause: error },
  );
}
const ts = require(typescriptPath);
const allowed = JSON.parse(
  readFileSync(new URL('./test-lint-allow.json', import.meta.url), 'utf8'),
);
const shared = 'テスト名を一意に特定できない（共有の読取）';
const unknown = 'テスト名を一意に特定できません';

// Indicators identify review candidates, not proof that a test is low quality.
// 検出しない形: 動的に組み立てたパス、関数の引数、別ファイルからの値。
// フィクスチャ文字列はこのファイルのコードとして検出しないため、検査自体の許可項目は不要。
function scan(file, source) {
  const tree = ts.createSourceFile(
    file,
    source,
    ts.ScriptTarget.Latest,
    true,
    file.endsWith('.ts') ? ts.ScriptKind.TS : ts.ScriptKind.JS,
  );
  const declarations = new Map();
  const all = [];
  function collect(node) {
    all.push(node);
    if (
      ts.isVariableDeclaration(node) &&
      ts.isIdentifier(node.name) &&
      node.initializer
    ) {
      const list = declarations.get(node.name.text) ?? [];
      list.push(node);
      declarations.set(node.name.text, list);
    }
    ts.forEachChild(node, collect);
  }
  collect(tree);
  function resolve(node, visiting = new Set()) {
    if (!node) return null;
    if (
      node.getText(tree) === 'import.meta.dirname' ||
      (ts.isIdentifier(node) && node.text === '__dirname')
    )
      return '';
    if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node))
      return node.text;
    if (ts.isIdentifier(node)) {
      const values = declarations.get(node.text) ?? [];
      if (visiting.has(node.text)) return null;
      const scope = (n) => {
        const blockScoped = !!(n.parent.flags & ts.NodeFlags.BlockScoped);
        for (let p = n.parent; p; p = p.parent)
          if (
            (blockScoped && ts.isBlock(p)) ||
            ts.isFunctionLike(p) ||
            ts.isSourceFile(p)
          )
            return p;
      };
      const ancestors = [];
      for (let p = node.parent; p; p = p.parent)
        if (ts.isBlock(p) || ts.isFunctionLike(p) || ts.isSourceFile(p))
          ancestors.push(p);
      for (const parent of ancestors) {
        if (
          ts.isFunctionLike(parent) &&
          parent.parameters.some(
            (p) => ts.isIdentifier(p.name) && p.name.text === node.text,
          )
        )
          return null;
        const matches = values.filter((value) => scope(value) === parent);
        if (matches.length > 1) return null;
        if (matches.length === 1) {
          const next = new Set(visiting);
          next.add(node.text);
          return resolve(matches[0].initializer, next);
        }
      }
      return null;
    }
    if (ts.isParenthesizedExpression(node) || ts.isAsExpression(node))
      return resolve(node.expression, visiting);
    if (
      ts.isNewExpression(node) &&
      node.expression.getText(tree) === 'URL' &&
      node.arguments?.length === 2 &&
      node.arguments[1].getText(tree) === 'import.meta.url'
    )
      return resolve(node.arguments[0], visiting);
    if (
      ts.isCallExpression(node) &&
      /^(?:path\.)?(?:join|resolve)$/.test(node.expression.getText(tree))
    ) {
      const parts = node.arguments.map((arg) => resolve(arg, visiting));
      return parts.every((part) => part !== null) ? parts.join('/') : null;
    }
    return null;
  }
  const counts = new Map();
  function testCall(node) {
    if (
      !ts.isCallExpression(node) ||
      !/^(?:test|it)(?:\.only)?$/.test(node.expression.getText(tree))
    )
      return null;
    return node;
  }
  function mentionsIdentifier(node, name) {
    if (ts.isIdentifier(node)) {
      const isKey =
        ts.isPropertyAssignment(node.parent) && node.parent.name === node;
      return !isKey && node.text === name;
    }
    return (
      ts.forEachChild(
        node,
        (child) => mentionsIdentifier(child, name) || undefined,
      ) ?? false
    );
  }

  for (const node of all) {
    const call = testCall(node);
    if (call && call.arguments[0] && ts.isStringLiteralLike(call.arguments[0]))
      counts.set(
        call.arguments[0].text,
        (counts.get(call.arguments[0].text) ?? 0) + 1,
      );
  }
  function testName(node, isRead = false) {
    for (let p = node.parent; p; p = p.parent) {
      if (ts.isFunctionLike(p)) {
        const call = p.parent && testCall(p.parent);
        if (!call || !call.arguments.includes(p))
          return isRead ? shared : unknown;
        const name = call.arguments[0];
        return name &&
          ts.isStringLiteralLike(name) &&
          counts.get(name.text) === 1
          ? name.text
          : unknown;
      }
      // skip options belong to the test call rather than its callback.
      const call = testCall(p);
      if (
        call &&
        call.arguments[0] &&
        ts.isStringLiteralLike(call.arguments[0])
      )
        return counts.get(call.arguments[0].text) === 1
          ? call.arguments[0].text
          : unknown;
    }
    return isRead ? shared : unknown;
  }
  const findings = [];
  function add(marker, node, isRead = false) {
    const name = testName(node, isRead);
    if (!findings.some((f) => f.marker === marker && f.name === name))
      findings.push({ file, name, marker });
  }
  const hasComparison =
    /\bassert\.(?:match|doesNotMatch|equal|notEqual|deepEqual|deepStrictEqual|strictEqual|ok)\s*\(|\.(?:match|includes|test)\s*\(|===|!==/.test(
      source,
    );
  for (const node of all) {
    if (
      hasComparison &&
      ts.isCallExpression(node) &&
      /(?:^|\.)(?:readFileSync|readFile|read)$/.test(
        node.expression.getText(tree),
      )
    ) {
      const target = resolve(node.arguments[0]);
      if (
        target &&
        !/(?:^|\/)(?:testdata|fixtures?)(?:\/|$)/i.test(target) &&
        /(?:\.go|\.md|\.sh|(?:^|\/)README|(?:^|\/)SKILL\.md|(?:^|\/)openapi(?:\/|\.|$))/i.test(
          target,
        )
      )
        add('製品ファイルの文字列照合', node, true);
    }
    if (ts.isPropertyAssignment(node) && node.name.getText(tree) === 'skip') {
      const text = node.initializer.getText(tree);
      const env =
        /process\.env\b/.test(text) ||
        [...declarations].some(
          ([name, values]) =>
            values.length === 1 &&
            /process\.env\b/.test(values[0].initializer.getText(tree)) &&
            new RegExp(`\\b${name}\\b`).test(text),
        );
      if (env) add('環境変数なしでskipする計測', node);
    }
    if (
      ts.isPropertyAssignment(node) &&
      /^(?:skip|todo)$/.test(node.name.getText(tree)) &&
      (node.initializer.kind === ts.SyntaxKind.TrueKeyword ||
        (ts.isStringLiteralLike(node.initializer) && node.initializer.text))
    ) {
      const owner = node.parent?.parent;
      if (testCall(owner)) add('常に省略', node);
      else if (
        ts.isCallExpression(owner) &&
        /^describe(?:\.only)?$/.test(owner.expression.getText(tree)) &&
        owner.arguments[1] === node.parent &&
        owner.arguments[0] &&
        ts.isStringLiteralLike(owner.arguments[0])
      )
        findings.push({
          file,
          name: owner.arguments[0].text,
          marker: '常に省略',
        });
    }
    // 条件なしで本体の直下に置いた t.skip() / t.todo() も常に省略する。
    if (
      ts.isExpressionStatement(node) &&
      ts.isCallExpression(node.expression) &&
      /\.(?:skip|todo)$/.test(node.expression.expression.getText(tree)) &&
      ts.isBlock(node.parent) &&
      ts.isFunctionLike(node.parent.parent) &&
      testCall(node.parent.parent.parent)
    )
      add('常に省略', node);
    if (
      ts.isCallExpression(node) &&
      /^(?:test|it|describe)\.(?:skip|todo)$/.test(
        node.expression.getText(tree),
      ) &&
      node.arguments[0] &&
      ts.isStringLiteralLike(node.arguments[0])
    )
      findings.push({ file, name: node.arguments[0].text, marker: '常に省略' });
    if (
      ts.isCallExpression(node) &&
      /(?:^|\.)skip$/.test(node.expression.getText(tree))
    ) {
      const block = node.parent?.parent?.getText(tree) ?? '';
      if (
        /process\.env\b/.test(block) ||
        [...declarations].some(
          ([name, values]) =>
            values.length === 1 &&
            /process\.env\b/.test(values[0].initializer.getText(tree)) &&
            new RegExp(`\\b${name}\\b`).test(block),
        )
      )
        add('環境変数なしでskipする計測', node);
    }
    if (
      (ts.isFunctionDeclaration(node) || ts.isVariableDeclaration(node)) &&
      node.name &&
      ts.isIdentifier(node.name) &&
      /^legacy\w*/i.test(node.name.text)
    ) {
      const name = node.name.text;
      const comparison = all.find(
        (n) =>
          ts.isCallExpression(n) &&
          /^assert\.(?:equal|deepEqual|strictEqual|deepStrictEqual|match)$/.test(
            n.expression.getText(tree),
          ) &&
          n.arguments.some((argument) => mentionsIdentifier(argument, name)),
      );
      if (comparison) add('旧実装との比較', comparison);
    }
  }
  return findings;
}

function check(findings, entries) {
  assert.ok(Array.isArray(entries), '許可一覧は配列で指定してください');
  const key = (x) => JSON.stringify([x.file, x.name, x.marker]);
  const seen = new Set();
  const found = new Set(findings.map(key));
  for (const entry of entries) {
    assert.ok(
      typeof entry.file === 'string' &&
        entry.file &&
        typeof entry.name === 'string' &&
        entry.name &&
        typeof entry.marker === 'string' &&
        entry.marker &&
        typeof entry.reason === 'string' &&
        entry.reason.trim(),
      '許可項目には対象と理由が必要です',
    );
    const k = key(entry);
    assert.ok(!seen.has(k), `許可項目が重複しています: ${k}`);
    assert.ok(found.has(k), `検出されなくなった許可項目: ${k}`);
    seen.add(k);
  }
  return findings
    .filter((f) => !seen.has(key(f)))
    .map(
      (f) =>
        `${f.file}: ${f.name}: ${f.marker} — 挙動を検証するテストに直すか、理由を付けて許可一覧に足す`,
    );
}

// 固定検証が実行時に使う対象定義だけを登録済みとして扱う。
function verifiedGlobs(targets) {
  const suites = new Map();
  for (const line of targets.trim().split('\n')) {
    const [suite, ...patterns] = line.trim().split(/ +/);
    assert.ok(['root', 'client'].includes(suite), '不明なテスト区分: ' + suite);
    assert.ok(!suites.has(suite), '重複したテスト区分: ' + suite);
    assert.ok(
      patterns.length &&
        patterns.every((pattern) =>
          /^[\w./-]*\*[\w.*-]*\.test\.\w+$/.test(pattern),
        ),
      '不正なテスト対象: ' + line,
    );
    suites.set(suite, patterns);
  }
  assert.ok(
    suites.has('root') && suites.has('client'),
    'rootとclientのテスト対象が必要です',
  );
  return [
    ...suites.get('client').map((pattern) => 'client/' + pattern),
    ...suites.get('root'),
  ];
}

const globPattern = (glob) =>
  new RegExp(
    `^${glob
      .split('*')
      .map((part) => part.replace(/[.+?^${}()|[\]\\]/g, '\\$&'))
      .join('[^/]*')}$`,
  );
const testFileName =
  /(?:\.(?:test|spec)\.[cm]?[jt]sx?|_test\.go)$|(?:^|\/)test_[^/]*\.py$|_test\.py$/;

// テストの名前の形をしたファイルのうち、固定検証（node --test のglob と server/ の go test）が実行しないもの。
// server/ の _test.go は go test が拾うので対象外にし、ビルドタグで外れるものは server/internal/testlint が検出する。
function unrunTestFiles(files, globs) {
  const patterns = globs.map(globPattern);
  return files
    .filter(
      (file) =>
        testFileName.test(file) &&
        !/(?:^|\/)(?:node_modules|testdata|fixtures?|\.git|\.tmp)\//.test(file),
    )
    .filter(
      (file) =>
        !(file.startsWith('server/') && file.endsWith('_test.go')) &&
        !patterns.some((pattern) => pattern.test(file)),
    )
    .map((file) => ({
      file,
      name: 'ファイル単位',
      marker: '固定検証で実行されないテストファイル',
    }));
}

function repositoryFiles(dir = '') {
  return readdirSync(path.join(root, dir), { withFileTypes: true }).flatMap(
    (entry) => {
      const file = dir ? `${dir}/${entry.name}` : entry.name;
      if (entry.isDirectory())
        return ['node_modules', '.git', '.tmp'].includes(entry.name)
          ? []
          : repositoryFiles(file);
      return entry.isFile() ? [file] : [];
    },
  );
}

function repositoryFindings() {
  const globs = verifiedGlobs(
    readFileSync(
      path.join(root, 'scripts/verify-node-test-targets.list'),
      'utf8',
    ),
  );
  const patterns = globs.map(globPattern);
  const files = repositoryFiles();
  return [
    ...unrunTestFiles(files, globs),
    ...files
      .filter((file) => patterns.some((pattern) => pattern.test(file)))
      .sort()
      .flatMap((file) =>
        scan(file, readFileSync(path.join(root, file), 'utf8')),
      ),
  ];
}

test('出荷するJS・TSテストの目印は理由付きで管理する', () => {
  assert.deepEqual(check(repositoryFindings(), allowed), []);
});

test('目印、除外、許可、古い許可と診断を検査する', () => {
  const source = `test('source', () => { const s = readFileSync('server/a.go'); assert.match(s, /x/); });\ntest('fixture', () => { readFileSync('testdata/a.md'); });\ntest('bench', { skip: !process.env.RUN_BENCH }, () => {});\nfunction legacyCalc() {}\ntest('comparison', () => assert.equal(legacyCalc(), current()));`;
  const findings = scan('test/example.test.mjs', source);
  assert.deepEqual(
    findings.map((f) => f.marker),
    [
      '製品ファイルの文字列照合',
      '環境変数なしでskipする計測',
      '旧実装との比較',
    ],
  );
  assert.deepEqual(
    scan(
      'test/regex.test.ts',
      `const legacy = 'label'; test('regex', () => assert.match(response, /legacy-command/));`,
    ),
    [],
  );
  assert.equal(findings[0].name, 'source');
  assert.match(
    check(findings, [])[0],
    /test\/example\.test\.mjs: source: 製品ファイルの文字列照合.*挙動を検証するテストに直すか、理由を付けて許可一覧に足す/,
  );
  const entries = findings.map((f) => ({ ...f, reason: '理由' }));
  assert.deepEqual(check(findings, entries), []);
  assert.throws(
    () => check(findings, [{ ...entries[0], reason: ' ' }]),
    /理由が必要/,
  );
  assert.throws(
    () => check(findings.slice(1), entries),
    /検出されなくなった許可項目/,
  );
  assert.equal(
    scan(
      'test/a.test.ts',
      `const text = read('README.md'); assert.match(text, /x/);`,
    )[0].name,
    shared,
  );
  assert.deepEqual(
    scan(
      'test/a.test.ts',
      `test('fixture', () => { const x = readFileSync('fixtures/a.go'); assert.match(x, /x/); });`,
    ),
    [],
  );
  const skipped = scan(
    'test/bench.test.ts',
    `const enabled = process.env.BENCH; test('timing', t => { if (!enabled) t.skip(); });`,
  );
  assert.equal(skipped[0].name, 'timing');
  assert.equal(skipped[0].marker, '環境変数なしでskipする計測');
});

test('リテラル由来のパスと共有読取を追い、動的パスは対象外にする', () => {
  const source =
    "test('first', () => {});\nconst file = 'server/a.go'; const alias = file; const text = readFileSync(alias);\nfunction helper() { return readFileSync(path.join('docs', 'a.md')); }\ntest('actual', () => { const p = new URL('../api/openapi.json', import.meta.url); const x = readFileSync(p); assert.match(x, /x/); });\ntest('template', () => { const p = `docs/a.md`; assert.match(readFileSync(p), /x/); });\ntest('resolve', () => { const p = path.resolve('scripts', 'a.sh'); assert.match(readFileSync(p), /x/); });\ntest('dynamic', () => { const p = 'docs/' + name + '.md'; assert.match(readFileSync(p), /x/); });";
  const found = scan('test/example.test.mjs', source);
  assert.deepEqual(
    found.map((f) => f.name),
    [shared, 'actual', 'template', 'resolve'],
  );
  assert.match(
    check(found, [])[0],
    /テスト名を一意に特定できない（共有の読取）/,
  );
  assert.equal(
    scan(
      'test/path.test.mjs',
      "test('dirname', () => { const p = path.join(import.meta.dirname, '../server/a.go'); assert.match(readFileSync(p), /x/); });",
    )[0].name,
    'dirname',
  );
  const dynamic =
    "function sharedRead(p) { return readFileSync(p); } test('dynamic', () => { const p = `docs/${name}.md`; assert.match(readFileSync(p), /x/); });";
  assert.deepEqual(scan('test/dynamic.test.ts', dynamic), []);
  assert.deepEqual(
    scan(
      'test/shadow.test.ts',
      `const p = 'a.go'; function helper(p) { return readFileSync(p); } assert.match('x', /x/);`,
    ),
    [],
  );
});

test('常に省略するテストと、固定検証が実行しないテストファイルを検出する', () => {
  const source =
    "test('opt', { skip: true }, () => {});\ntest('todo', { todo: '後で' }, () => {});\ntest.skip('method', () => {});\nit.todo('pending');\ntest('env', { skip: !process.env.X }, () => {});\ntest('off', { skip: false }, () => {});\nconst config = { skip: true };\ntest('run', () => {});\ntest('body', t => { t.skip('off'); run(); });\ntest('guarded', t => { if (!ok) { t.skip('off'); return; } run(); });\ndescribe('group', { skip: true }, () => {});\ndescribe('group-run', () => {});";
  const found = scan('test/skip.test.mjs', source).filter(
    (f) => f.marker === '常に省略',
  );
  assert.deepEqual(found.map((f) => f.name).sort(), [
    'body',
    'group',
    'method',
    'opt',
    'pending',
    'todo',
  ]);
  const globs = verifiedGlobs(
    'client test/*.test.ts\nroot test/*.test.mjs test/*.test.ts\n',
  );
  assert.deepEqual(globs, [
    'client/test/*.test.ts',
    'test/*.test.mjs',
    'test/*.test.ts',
  ]);
  const files = [
    'test/a.test.mjs',
    'test/b.test.ts',
    'client/test/c.test.ts',
    'server/internal/x/x_test.go',
    'scripts/d.test.mjs',
    'client/src/e.spec.ts',
    'test/lib/f.test.mjs',
    'deploy/g_test.go',
    'tools/test_h.py',
    'test/fixtures/i.test.mjs',
    'client/node_modules/j.test.js',
    'scripts/k.mjs',
  ];
  assert.deepEqual(
    unrunTestFiles(files, globs).map((f) => f.file),
    [
      'scripts/d.test.mjs',
      'client/src/e.spec.ts',
      'test/lib/f.test.mjs',
      'deploy/g_test.go',
      'tools/test_h.py',
    ],
  );
  assert.throws(
    () => verifiedGlobs('echo node --test samples/*.test.ts'),
    /不明なテスト区分/,
  );
  assert.throws(
    () => verifiedGlobs('client test/*.test.ts\nroot --import test/*.test.ts'),
    /不正なテスト対象/,
  );
});
