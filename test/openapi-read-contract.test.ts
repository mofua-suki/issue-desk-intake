import { test } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { runInNewContext } from 'node:vm';

const spec = JSON.parse(
  readFileSync(new URL('../api/openapi.json', import.meta.url), 'utf8'),
);
const response = (path) =>
  spec.paths[path].get.responses['200'].content['application/json'].schema;

function combinedSpec(parts?) {
  const dir = new URL('../api/openapi/', import.meta.url);
  const combined: Record<string, any> = {};
  const files =
    parts ??
    readdirSync(dir)
      .filter((name) => name.endsWith('.yaml'))
      .sort()
      .map((file) => {
        // この仕様はフロー形式。YAML の単一引用符内ではバックスラッシュはそのまま。
        const source = readFileSync(new URL(file, dir), 'utf8').replace(
          "'^[^\\u0000\\r\\n]+$'",
          '"^[^\\\\u0000\\\\r\\\\n]+$"',
        );
        return [file, runInNewContext(`(${source})`)];
      });
  for (const [file, part] of files) {
    for (const [key, value] of Object.entries<any>(part)) {
      if (key === 'paths') combined.paths = { ...combined.paths, ...value };
      else if (key === 'components') {
        combined.components ??= {};
        for (const [category, definitions] of Object.entries(value)) {
          const target = (combined.components[category] ??= {});
          for (const [name, definition] of Object.entries(definitions)) {
            if (Object.hasOwn(target, name))
              throw new Error(
                `Duplicate component ${category}.${name} in ${file}`,
              );
            target[name] = definition;
          }
        }
      } else combined[key] = value;
    }
  }
  return JSON.parse(JSON.stringify(combined));
}

test('componentsはカテゴリごとに順序を保って結合し、同じカテゴリの重複を拒否する', () => {
  const first = {
    components: {
      schemas: { A: { type: 'string' } },
      responses: { A: { description: 'ok' } },
    },
  };
  const second = {
    components: {
      schemas: { B: { type: 'number' } },
      responses: { B: { description: 'next' } },
    },
  };
  const merged = combinedSpec([
    ['first.yaml', first],
    ['second.yaml', second],
  ]);
  assert.deepEqual(Object.keys(merged.components), ['schemas', 'responses']);
  assert.deepEqual(Object.keys(merged.components.schemas), ['A', 'B']);
  assert.deepEqual(Object.keys(merged.components.responses), ['A', 'B']);
  assert.deepEqual(merged.components.responses.A, { description: 'ok' });
  for (const definition of [{ type: 'string' }, { type: 'number' }]) {
    assert.throws(
      () =>
        combinedSpec([
          ['first.yaml', first],
          ['second.yaml', { components: { schemas: { A: definition } } }],
        ]),
      /Duplicate component schemas\.A in second\.yaml/,
    );
  }
});

test('配布する結合仕様は正規の整形とバイト単位で一致する', () => {
  assert.equal(
    readFileSync(new URL('../api/openapi.json', import.meta.url), 'utf8'),
    JSON.stringify(combinedSpec(), null, 2) + '\n',
  );
});

// 説明が指すコード・資料の場所（ディレクトリ付きのパスと、コードのファイル名だけの言及）を、作業ツリーの実物と照らす。
// ほかのリポジトリの資料は issue-desk-worker/docs/… のようにリポジトリ名を前に付けて書く（照合の対象外）。
function missingCodeReferences(document) {
  const root = new URL('../', import.meta.url);
  const names = new Set<string>();
  const walk = (dir: URL) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      if (['node_modules', '.git', '.tmp'].includes(entry.name)) continue;
      if (entry.isDirectory()) walk(new URL(`${entry.name}/`, dir));
      else names.add(entry.name);
    }
  };
  walk(root);
  const missing = new Set<string>();
  const visit = (value) => {
    if (typeof value === 'string') {
      for (const [path] of value.matchAll(
        /(?<![\w./@{}:-])(?:server|client|shared|scripts|deploy|api|skills|test|docs)\/[\w./-]*[\w-]/g,
      ))
        if (!existsSync(new URL(path, root))) missing.add(path);
      for (const [name] of value.matchAll(
        /(?<![\w./-])[\w-]+\.(?:mjs|mts|cjs|ts|go|sh)(?![\w-])/g,
      ))
        if (!names.has(name)) missing.add(name);
    } else if (value && typeof value === 'object')
      Object.values(value).forEach(visit);
  };
  visit(document);
  return [...missing];
}

test('説明が指すコード・資料の場所は実在する', () => {
  assert.deepEqual(missingCodeReferences(spec), []);
  assert.deepEqual(
    missingCodeReferences({
      description:
        '無い場所は server/src/none.mjs と none-rules.mjs と docs/none.md#section、ある場所は api/openapi.json と create-issue.ts と docs/completion-plan.md#plan、ほかのリポジトリの資料は issue-desk-worker/docs/none.md、経路は /api/issues。',
    }),
    ['server/src/none.mjs', 'docs/none.md', 'none-rules.mjs'],
  );
});

test('明示終了の照合・終了・記録の経路が結合仕様にある', () => {
  for (const [action, method] of [
    ['inspect', 'post'],
    ['close', 'post'],
    ['archive', 'get'],
  ]) {
    assert.ok(
      spec.paths[`/api/desk-cleanup/${action}`]?.[method],
      `${method} ${action}`,
    );
  }
});

test('主要読取の配列・詳細と空配列に実装に対応する型がある', () => {
  assert.equal(
    response('/api/issues').items.$ref,
    '#/components/schemas/IssueRow',
  );
  assert.equal(response('/api/issue').$ref, '#/components/schemas/Issue');
  const search = response('/api/issues/search').oneOf;
  assert.equal(
    search[0].properties.issues.items.$ref,
    '#/components/schemas/IssueRow',
  );
  assert.deepEqual(search[1].not.required, ['issues']);
  for (const [path, component] of [
    ['/api/drafts', 'Draft'],
    ['/api/todos', 'Todo'],
  ]) {
    const item = response(path).properties.items.items;
    assert.equal(
      path === '/api/drafts' ? item.anyOf[0].$ref : item.$ref,
      `#/components/schemas/${component}`,
    );
    assert.equal(
      response(`${path}/{id}`).$ref,
      `#/components/schemas/${component}`,
    );
    assert.equal(
      spec.components.schemas[component].properties.id.type,
      'string',
    );
    assert.equal(spec.components.schemas[component].additionalProperties, true);
  }
  assert.equal(
    response(
      '/api/draft-withdrawals',
    ).properties.records.items.properties.used.type.includes('null'),
    true,
  );
  assert.equal(
    response('/api/v1/jobs').properties.jobs.items.$ref,
    '#/components/schemas/ReadJob',
  );
  assert.equal(
    response('/api/v1/jobs/{id}').properties.events.items.$ref,
    '#/components/schemas/ReadJobEvent',
  );
  assert.equal(response('/api/v1/jobs/{id}').properties.events.maxItems, 100);
  assert.equal(
    response('/api/v1/jobs/{id}').properties.dependsOn.maxItems,
    undefined,
  );
  for (const path of [
    '/api/stalls/alerts',
    '/api/attention/alerts',
    '/api/usage-review/alerts',
  ]) {
    assert.equal(response(path).items.type, 'object');
    assert.ok(Object.keys(response(path).items.properties).length);
    assert.equal(response(path).items.additionalProperties, true);
  }
  assert.equal(response('/api/full-test/alerts').maxItems, 0);
  assert.ok(
    spec.components.schemas.Draft.properties.project.type.includes('null'),
  );
  assert.match(
    spec.components.schemas.Draft.properties.project.description,
    /未定.*null/,
  );
  assert.doesNotMatch(
    spec.paths['/api/usage-review/alerts'].get.description,
    /常に空配列/,
  );
  assert.match(
    spec.paths['/api/usage-review/alerts'].get.description,
    /not_recouped.*未確認/,
  );
  assert.equal(response('/api/usage-review/alerts').maxItems, undefined);
});

test('型を固定しない GET はその理由を仕様に残す', () => {
  for (const [path, operations] of Object.entries<any>(spec.paths)) {
    const get = operations.get;
    if (!get?.responses?.['200']) continue;
    const content = Object.values<any>(get.responses['200'].content ?? {});
    const missing = !content.length || content.every((item) => !item.schema);
    const open = content.some(
      (item) =>
        item.schema?.type === 'object' &&
        item.schema.additionalProperties === true &&
        !item.schema.properties,
    );
    if (missing || open)
      assert.match(get.description ?? '', /型.*定義しない/, path);
  }
});

test('使用量の工程は新名を受け付け旧名で出力すると明記する', () => {
  const step = spec.components.schemas.UsageDraftGroup.properties.step;
  assert.match(step.description, /出力は旧名にそろえる/);
  assert.match(step.description, /新名は受け付ける表記として列挙/);
  for (const name of [
    '受付の相談',
    '代理の回答',
    '起票直前の重複と依存の確認',
    '相談の下書き',
    'filingCheck',
    '利用者への質問の代理確認',
  ])
    assert.ok(step.enum.includes(name));
});

test('使用量の課金経路の許可値と欠測時の解釈を明記する', () => {
  const description = spec.paths['/api/v1/usage'].post.description;
  assert.match(description, /usage行のbillingは任意/);
  assert.match(description, /subscriptionは契約の枠/);
  assert.match(description, /api_creditはAPIクレジット/);
  assert.match(description, /欄なしとnullは課金経路の解釈上subscription扱い/);
  assert.match(description, /不正なbilling値は行ごとにrejected/);
  assert.match(description, /既存行の保存値は補完・書換えしない/);
});

test('lease期限切れの失敗結果を読取APIの契約に記載する', () => {
  const result = spec.components.schemas.ReadJob.properties.result;
  assert.match(result.description, /同じ世代.*3回以上/);
  assert.match(result.description, /lastStage.*今回のview.phase.*空でない/);
  assert.match(result.description, /lastHost.*不明/);
  assert.match(result.description, /lastStage.*以前の値を削除/);
  assert.match(
    result.description,
    /lastExecutionError.*オブジェクト.*無ければ以前の値を保持/,
  );
  for (const [name, type] of Object.entries({
    status: 'string',
    summary: 'string',
    leaseExpiredCount: 'integer',
    lastHost: 'string',
    lastStage: 'string',
    lastExecutionError: 'object',
  })) {
    assert.equal(result.properties[name].type, type);
  }
  assert.equal(result.additionalProperties, true);
  assert.equal(
    response('/api/v1/jobs').properties.jobs.items.$ref,
    '#/components/schemas/ReadJob',
  );
  assert.equal(
    response('/api/v1/jobs/{id}').properties.job.$ref,
    '#/components/schemas/ReadJob',
  );
});

test('診断4経路の認証・応答と入力境界を配布仕様に固定する', () => {
  const paths = [
    '/api/diagnostics/db',
    '/api/diagnostics/cpu',
    '/api/diagnostics/goroutines',
    '/api/diagnostics/activity',
  ];
  const combined = combinedSpec();
  const ids = [];
  for (const operations of Object.values(spec.paths))
    for (const op of Object.values(operations)) {
      if (op?.operationId) ids.push(op.operationId);
    }
  assert.equal(new Set(ids).size, ids.length);
  for (const path of paths) {
    for (const document of [spec, combined]) {
      const op = document.paths[path]?.get;
      assert.ok(op, path);
      assert.equal(
        op.operationId,
        `get_api_diagnostics_${path.split('/').at(-1)}`,
      );
      assert.deepEqual(op.security, [{ pcToken: [] }, { cliKey: [] }]);
      assert.equal(op['x-issue-desk-delegate'], true);
      const cpu = path.endsWith('/cpu');
      const profiled = cpu || path.endsWith('/goroutines');
      assert.deepEqual(
        Object.keys(op.responses).sort(),
        (cpu
          ? ['200', '400', '401', '403', '405', '409', '503']
          : path.endsWith('/activity')
            ? ['200', '401', '403', '405']
            : path.endsWith('/goroutines')
              ? ['200', '400', '401', '403', '405', '503']
              : ['200', '401', '403', '405', '503']
        ).sort(),
      );
      for (const [code, result] of Object.entries<any>(op.responses))
        if (code !== '200') {
          assert.equal(
            result.content['application/json'].schema.$ref,
            '#/components/schemas/Error',
          );
          assert.equal(result.headers, undefined);
        }
      if (cpu) {
        assert.equal(op.parameters.length, 2);
        const p = op.parameters[0];
        assert.equal(p.name, 'seconds');
        assert.equal(p.in, 'query');
        assert.equal(p.required, false);
        assert.deepEqual(p.schema, {
          type: 'integer',
          minimum: 1,
          maximum: 30,
          default: 10,
        });
        assert.match(p.description, /重複.*拒否/);
        assert.match(op.responses['409'].description, /取得中.*開始/);
        assert.match(op.responses['503'].description, /解析/);
      } else if (profiled) assert.equal(op.parameters.length, 1);
      else assert.equal(op.parameters, undefined);
      if (profiled) {
        const p = op.parameters.find((p) => p.name === 'limit');
        assert.equal(p.in, 'query');
        assert.equal(p.required, false);
        assert.deepEqual(p.schema, {
          type: 'integer',
          minimum: 1,
          maximum: 100,
          default: 30,
        });
        assert.match(p.description, /重複.*拒否/);
      }
      assert.match(op.responses['401'].description, /未認証/);
      assert.match(op.responses['403'].description, /画面.*運用代行/);
      assert.match(op.responses['405'].description, /認可後.*GET以外/);
    }
  }
});

test('下書きの起票前判定の要約に結果・理由・上限の契約がある', () => {
  const summary = spec.components.schemas.Draft.properties.filingCheckSummary;
  assert.deepEqual(summary.required, ['revision', 'count', 'checks']);
  assert.equal(summary.properties.checks.maxItems, 20);
  const check = summary.properties.checks.items.properties;
  assert.deepEqual(check.result.enum, [
    'refused',
    'evidence_requested',
    'ai_failed',
    'invalid_answer',
    'allowed',
    null,
  ]);
  assert.ok(check.reason.type.includes('null'));
  assert.ok(summary.properties.count.type.includes('null'));
});

test('診断応答の必須項目・省略・null・配列上限を固定する', () => {
  const db = response('/api/diagnostics/db');
  const cpu = response('/api/diagnostics/cpu');
  const goroutines = response('/api/diagnostics/goroutines');
  for (const [schema, names] of [
    [
      db,
      [
        'color',
        'instanceId',
        'startedAt',
        'pid',
        'queries',
        'connections',
        'maxConnections',
      ],
    ],
    [
      cpu,
      [
        'color',
        'instanceId',
        'startedAt',
        'seconds',
        'totalSamples',
        'self',
        'cumulative',
      ],
    ],
    [goroutines, ['color', 'instanceId', 'startedAt', 'total', 'functions']],
  ]) {
    assert.deepEqual(schema.required, names);
    assert.equal(schema.type, 'object');
    assert.deepEqual(schema.properties.color.enum, [
      'blue',
      'green',
      'unknown',
    ]);
    assert.equal(schema.properties.instanceId.type, 'string');
    assert.equal(schema.properties.startedAt.format, 'date-time');
  }
  const q = db.properties.queries;
  assert.equal(q.type, 'array');
  assert.equal(q.maxItems, 100);
  assert.deepEqual(q.items.required, ['state', 'seconds', 'waitType']);
  assert.deepEqual(q.items.properties.waitType.type, ['string', 'null']);
  assert.equal(q.items.properties.query.type, 'string');
  assert.match(q.items.properties.query.description, /256バイト/);
  assert.equal(q.items.properties.seconds.type, 'number');
  assert.equal(db.properties.connections.type, 'array');
  assert.deepEqual(db.properties.connections.items.required, [
    'state',
    'count',
  ]);
  for (const [schema, names] of [
    [cpu, ['self', 'cumulative']],
    [goroutines, ['functions']],
  ])
    for (const name of names) {
      const array = schema.properties[name];
      assert.equal(array.type, 'array');
      assert.equal(array.maxItems, 100);
      assert.match(array.description, /空配列/);
      assert.deepEqual(array.items.required, ['function']);
      for (const [field, type] of Object.entries({
        function: 'string',
        file: 'string',
        selfNanoseconds: 'integer',
        cumulativeNanoseconds: 'integer',
        count: 'integer',
      }))
        assert.equal(array.items.properties[field].type, type);
      assert.match(
        array.items.properties.selfNanoseconds.description,
        /ナノ秒/,
      );
      assert.match(
        array.items.properties.cumulativeNanoseconds.description,
        /ナノ秒/,
      );
    }
  for (const [schema, fields] of [
    [db, ['pid', 'maxConnections']],
    [cpu, ['seconds', 'totalSamples']],
    [goroutines, ['total']],
  ])
    for (const field of fields)
      assert.equal(schema.properties[field].type, 'integer');
});

test('db-read は許可の検索・登録・応答の種類として公開される', () => {
  for (const contract of [combinedSpec(), spec]) {
    const parameter = contract.paths[
      '/api/permission-grants'
    ].get.parameters.find((p) => p.name === 'kind');
    assert.ok(parameter.schema.enum.includes('db-read'));
    for (const name of ['PermissionGrantRequest', 'PermissionGrant']) {
      assert.ok(
        contract.components.schemas[name].properties.kind.enum.includes(
          'db-read',
        ),
        name,
      );
    }
  }
});

test('db-query の認証・入力・上限・応答を配布契約に固定する', () => {
  for (const contract of [combinedSpec(), spec]) {
    const op = contract.paths['/api/diagnostics/db-query'].post;
    assert.deepEqual(op.security, [{ pcToken: [] }, { cliKey: [] }]);
    assert.equal(op['x-issue-desk-delegate'], true);
    assert.deepEqual(Object.keys(op.responses).sort(), [
      '200',
      '400',
      '401',
      '403',
      '409',
      '503',
      '504',
    ]);
    const input = op.requestBody.content['application/json'].schema;
    assert.deepEqual(input.required, ['target', 'sql']);
    assert.equal(input.additionalProperties, false);
    assert.deepEqual(input.properties.target.enum, ['shared', 'app']);
    assert.deepEqual(input.properties.maxRows, {
      type: 'integer',
      minimum: 1,
      maximum: 1000,
      default: 100,
    });
    const output = op.responses['200'].content['application/json'].schema;
    assert.deepEqual(output.required, ['columns', 'rows', 'truncated']);
    assert.equal(output.properties.rows.maxItems, 1000);
    assert.equal(output.properties.rows.items.type, 'array');
    assert.deepEqual(output.properties.columns.items.required, [
      'name',
      'type',
    ]);
    assert.deepEqual(output.properties.truncated.required, ['rows', 'bytes']);
    for (const [code, response] of Object.entries<any>(op.responses))
      if (code !== '200') {
        assert.equal(
          response.content['application/json'].schema.$ref,
          '#/components/schemas/Error',
        );
      }
  }
});

test('相談終了は検討中の受理と起票開始後の競合を公開する', () => {
  for (const document of [spec, combinedSpec()]) {
    const op = document.paths['/api/drafts/{id}/finish'].post;
    assert.deepEqual(op.security, [
      { screenSession: [], csrfToken: [] },
      { cliKey: [] },
      { pcToken: [] },
    ]);
    assert.equal(op['x-issue-desk-delegate'], true);
    for (const pattern of [
      /実行中を含むthinking・auto_ready/,
      /終了の保存.*先.*自動起票・復旧再開を行わない/,
      /submitting.*先.*409/,
      /GET \/api\/drafts\/\{id\}/,
      /起票済みIssue.*submit_unknown.*409/,
      /instructionAtが必須/,
      /未来は400/,
      /作成より前.*409/,
      /200は終了の保存完了/,
      /原文・添付・使用量・了承記録/,
    ])
      assert.match(op.description, pattern);
    const body = op.requestBody.content['application/json'].schema;
    assert.equal(body.properties.instructionAt.format, 'date-time');
    assert.match(
      body.properties.instructionAt.description,
      /PC別トークンでは必須/,
    );
    assert.equal(
      body.required,
      undefined,
      '画面・一時キーにPC専用条件を課さない',
    );
    const origin = op.parameters.find((p) => p.name === 'Origin');
    assert.equal(origin.required, false);
    assert.match(
      origin.description,
      /画面・一時キー.*同一Origin.*必須.*PC別トークンでは不要/,
    );
    assert.deepEqual(Object.keys(op.responses).sort(), [
      '200',
      '400',
      '401',
      '403',
      '404',
      '409',
      '502',
    ]);
    assert.equal(
      op.responses['200'].content['application/json'].schema.$ref,
      '#/components/schemas/Draft',
    );
  }
});
