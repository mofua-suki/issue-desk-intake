import assert from 'node:assert/strict';
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  readFileSync,
  rmSync,
  readdirSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import {
  deskContractHeader,
  deskContractVersion,
} from '../../scripts/lib/desk-contract.generated.ts';

// Parse headings only outside fences. A section's introduction ends at its first
// child heading or fence, so a sibling's correct instructions cannot leak in.
export function operationExamples(markdown) {
  const lines = markdown.split('\n'),
    sections = [],
    examples = [],
    stack = [];
  const root = {
    depth: 0,
    start: -1,
    end: lines.length,
    introEnd: lines.length,
    parent: null,
  };
  stack.push(root);
  let fence;
  for (let i = 0; i < lines.length; i++) {
    const marker = /^ {0,3}(`{3,}|~{3,})(.*)$/.exec(lines[i]);
    if (fence) {
      if (
        marker &&
        marker[1][0] === fence.marker[0] &&
        marker[1].length >= fence.marker.length &&
        !marker[2].trim()
      ) {
        examples.push({
          ...fence,
          end: i,
          code: lines.slice(fence.start + 1, i).join('\n'),
        });
        fence = null;
      }
      continue;
    }
    if (marker) {
      const section = stack.at(-1);
      section.introEnd = Math.min(section.introEnd, i);
      fence = {
        marker: marker[1],
        language: marker[2].trim(),
        start: i,
        section,
      };
      continue;
    }
    const heading = /^ {0,3}(#{1,6})\s+(.+?)(?:\s+#+)?\s*$/.exec(lines[i]);
    if (!heading) continue;
    while (stack.at(-1).depth >= heading[1].length) stack.pop().end = i;
    const parent = stack.at(-1);
    parent.introEnd = Math.min(parent.introEnd, i);
    const section = {
      depth: heading[1].length,
      title: heading[2],
      start: i,
      end: lines.length,
      introEnd: lines.length,
      parent,
    };
    sections.push(section);
    stack.push(section);
  }
  assert.ok(!fence, '閉じていないコードフェンス');
  return examples
    .filter(
      (e) =>
        /^(?:sh|shell|bash)$/.test(e.language) &&
        /provider-disables|\.issue-desk\/desk/.test(e.code),
    )
    .map((e) => {
      const ancestors = [];
      for (let s = e.section; s; s = s.parent) ancestors.unshift(s);
      const previous = examples.filter((x) => x.end < e.start).at(-1);
      const before =
        lines
          .slice(
            Math.max(e.section.start + 1, previous ? previous.end + 1 : 0),
            e.start,
          )
          .join('\n')
          .trim()
          .split(/\n\s*\n/)
          .at(-1) || '';
      const next = examples.find((x) => x.start > e.end);
      const after = lines
        .slice(e.end + 1, Math.min(e.section.end, next?.start ?? lines.length))
        .join('\n');
      const common = ancestors
        .map((s) => lines.slice(s.start + 1, s.introEnd).join('\n'))
        .join('\n');
      const environment = ancestors
        .map((s) => s.title)
        .filter(Boolean)
        .reverse()
        .find((x) => /\b(?:NAS|Ubuntu)\b/i.test(x));
      assert.ok(environment, `行${e.start + 1}: NAS/Ubuntu の所属が必要`);
      return {
        ...e,
        line: e.start + 1,
        environment: /\bNAS\b/i.test(environment) ? 'NAS' : 'Ubuntu',
        guidance: [common, before, after].join('\n'),
      };
    });
}

// This is a deliberately bounded Japanese regression vocabulary, not a claim to
// understand arbitrary prose. Check unsafe clauses before required positive ones.
export function checkGuidance(example, call) {
  const text = example.guidance.replace(/`/g, '');
  const label = `${example.environment}:${call.operation}:行${example.line}`;
  const sentences = text
    .split(/[。\n]/)
    .map((s) => s.trim())
    .filter(Boolean);
  const forbidden = [
    /(?:欠測|欠落|不正応答).*(?:無効設定なし|設定なし|空一覧).*(?:扱う|みなす|判断する|変換する)(?:[、,]|$)/,
    /(?:結果不明|結果が不明|応答不明).*(?:再送する|送り直す|再実行する)(?:[、,]|$)/,
    /(?:認証|トークン).*(?:切り替える|変更する|差し替える)(?:[、,]|$)/,
    /権限.*(?:追加する|増やす|付与する)(?:[、,]|$)/,
    /(?:期限.*省略|until.*省略).*(?:自動.*再送する|自動.*送り直す)(?:[、,]|$)/,
  ];
  for (const sentence of sentences)
    for (const bad of forbidden)
      assert.doesNotMatch(sentence, bad, `${label}: 禁止案内`);
  const require = (pattern, description) =>
    assert.match(text, pattern, `${label}: ${description}`);
  require(/4xx|400.*401.*403.*409/, '4xx');
  require(/5xx|502.*503/, '5xx');
  require(/通信失敗|接続失敗|通信エラー/, '通信失敗');
  require(/停止|止め|中止/, '停止');
  require(/報告|伝える|知らせる/, '報告');
  require(/再送しない|送り直さない|再実行しない/, '再送禁止');
  require(/回り込まない|迂回しない|回避しない/, '迂回禁止');
  require(/403/, '403');
  require(/権限.{0,12}(?:追加しない|増やさない|付与しない)/, '権限追加禁止');
  require(/(?:認証|トークン).{0,12}(?:切り替えない|変更しない|差し替えない)/, '認証変更禁止');
  require(/未確認|確認できていない|確定できない/, '未確認');
  if (call.operation === 'list') {
    require(/disables/, '一覧フィールド');
    require(/欠測|欠落/, '欠測');
    require(/JSON/, 'JSON');
    require(/配列/, '配列');
    require(/(?:設定なし|無効設定なし|空一覧).{0,12}(?:扱わない|みなさない|判断しない)/, '空一覧と区別');
  } else {
    require(/一覧.{0,16}(?:読み直|再読|再取得)|再度.{0,8}一覧.{0,8}(?:読む|取得)/, '結果不明時の読取');
    require(/結果.{0,5}不明|応答.{0,5}不明/, '結果不明');
    if (call.operation === 'set') {
      for (const field of ['provider', 'until', 'by', 'reason'])
        require(new RegExp(field), `照合項目 ${field}`);
      require(/照合|突き合わせ|一致/, '照合');
      require(/(?:照合|確認|一致).{0,18}(?:できな|しな|取れな).{0,24}(?:未確認|確定できない|確認できていない)/, '照合不能時の未確認');
      if (call.body.until !== undefined)
        require(/until.{0,20}(?:送った値|送信値|指定値)/, '明示期限');
      else {
        require(/setAt.{0,24}(?:送信|要求).{0,12}(?:前後|開始.*終了)/, '送信時刻の範囲');
        require(/until.{0,12}setAt\s*(?:\+|＋)\s*24\s*(?:時間|h)/, '既定24時間');
      }
    } else {
      require(/cancelled\s*=\s*true.{0,16}(?:取り消した|取消済み)/, '取消あり');
      require(/cancelled\s*=\s*false.{0,16}(?:設定がなかった|設定なし)/, '取消対象なし');
      require(/cancelled.{0,18}(?:欠測|欠落)/, 'cancelled 欠測');
      require(/真偽値|boolean/, 'cancelled の型');
      require(/(?:どちらとも扱わない|いずれとも判断しない)/, '欠測を false と区別');
    }
  }
}

const fakeToken = 'idk_' + 'fixtureOnly'.repeat(5);
const repo = path.resolve(import.meta.dirname, '../..');
const contractURL = pathToFileURL(
  path.join(repo, 'scripts/lib/desk-contract-client.ts'),
).href;
const versionURL = pathToFileURL(
  path.join(repo, 'scripts/lib/desk-contract.generated.ts'),
).href;
const getURL = pathToFileURL(path.join(repo, 'scripts/desk-get.ts')).href;

// Only adapt window arguments to the shipped HTTP client. No domain response
// classification here: malformed/missing values must be caught by the Skill's
// executable example, rather than a test-only response judge.
const windowProgram = `
import fs from 'node:fs';
import { checkContractResponse } from ${JSON.stringify(contractURL)};
import { deskContractHeader, deskContractVersion } from ${JSON.stringify(versionURL)};
import { deskGet } from ${JSON.stringify(getURL)};
const a = process.argv.slice(2), option = k => a[a.indexOf(k) + 1];
const method = a[0] === 'get' ? 'GET' : option('--method'), route = option('--path');
const file = a.includes('--body-file') ? option('--body-file') : null;
const body = file ? JSON.parse(fs.readFileSync(file === '-' ? 0 : file, 'utf8')) : undefined;
const start = Date.now();
const record = { method, path: route, body, window: process.env.WINDOW_NAME, start };
const call = async () => {
  const token = fs.readFileSync(process.env.FAKE_TOKEN_FILE, 'utf8');
  const response = await fetch(process.env.FAKE_BASE + route, { method, redirect: 'manual', headers: { Authorization: 'Bearer ' + token, [deskContractHeader]: deskContractVersion, 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });
  const value = await response.json();
  checkContractResponse(response, value);
  return value;
};
// WINDOW_SHAPE=real mirrors the worker's desk CLI (internal/desk/command.go):
// every HTTP answer is an envelope on stdout, a refusal exits 1 with nothing on
// stderr, and a transport failure exits 2 with only a stderr message.
if (process.env.WINDOW_SHAPE === 'real') {
  try {
    const token = fs.readFileSync(process.env.FAKE_TOKEN_FILE, 'utf8');
    const response = await fetch(process.env.FAKE_BASE + route, { method, redirect: 'manual', headers: { Authorization: 'Bearer ' + token, [deskContractHeader]: deskContractVersion, 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });
    const text = await response.text(), clean = text.split(token).join('[REDACTED]');
    let value = clean, bodyType = clean ? 'text' : 'empty';
    try { if (clean) { value = JSON.parse(clean); bodyType = 'json'; } } catch {}
    console.log(JSON.stringify({ status: response.status, body: clean ? value : null, bodyType, redacted: clean !== text }));
    process.exitCode = response.ok ? 0 : 1;
  } catch { console.error('受付へ接続できません'); process.exitCode = 2; }
// deskGet is the shipped credential-safe output/error boundary, also for POST.
} else process.exitCode = await deskGet(call, { route }, console.log, { remote: true });
record.end = Date.now();
fs.appendFileSync(process.env.CAPTURE, JSON.stringify(record) + '\\n');
`;

function operation(call) {
  if (call.method === 'GET' && call.path === '/api/provider-disables')
    return 'list';
  const m = /^\/api\/provider-disables\/(codex|claude)(\/cancel)?$/.exec(
    call.path,
  );
  assert.ok(call.method === 'POST' && m, '捕捉したメソッドと経路');
  return m[2] ? 'cancel' : 'set';
}

export async function exampleWindow(markdown) {
  const examples = operationExamples(markdown);
  assert.ok(examples.length, '操作例が必要');
  const reference = Date.parse(
    /<!--\s*provider-disable-reference-time:\s*(\S+)\s*-->/.exec(markdown)?.[1],
  );
  assert.ok(Number.isFinite(reference), '期限明示例の固定受付基準時刻');
  const home = mkdtempSync(path.join(tmpdir(), 'provider-response-'));
  const calls = [],
    requests = [];
  let scenario: string | number = 'ok',
    saved = [],
    active;
  const server = createServer((req, res) => {
    let bytes = '';
    req.setEncoding('utf8').on('data', (b) => {
      bytes += b;
    });
    req.on('end', () => {
      const body = bytes ? JSON.parse(bytes) : undefined;
      requests.push({ method: req.method, path: req.url, body });
      assert.equal(
        req.headers.authorization === 'Bearer ' + fakeToken,
        true,
        '偽トークンを使う',
      );
      const op = operation({ method: req.method, path: req.url });
      const provider = req.url.split('/')[3];
      const setAt = new Date(
        body?.until ? reference : Date.now(),
      ).toISOString();
      const entry = {
        provider,
        setAt,
        until:
          body?.until ?? new Date(Date.parse(setAt) + 86400000).toISOString(),
        by: 'fixture-operator',
        ...(body?.reason !== undefined ? { reason: body.reason } : {}),
      };
      if (op === 'set') saved = [entry];
      if (op === 'cancel') saved = [];
      if (scenario === 'disconnect' || scenario === 'lost') {
        req.socket.destroy();
        return;
      }
      res.setHeader('content-type', 'application/json');
      res.setHeader(deskContractHeader, deskContractVersion);
      if (typeof scenario === 'number') {
        res.statusCode = scenario;
        res.end(
          JSON.stringify({ error: '拒否 Authorization: Bearer ' + fakeToken }),
        );
        return;
      }
      if (scenario === 'not-json') {
        res.end('not JSON');
        return;
      }
      if (scenario === 'wrong-type') {
        res.end('[]');
        return;
      }
      if (scenario === 'missing') {
        res.end(JSON.stringify(op === 'list' ? {} : { disables: [] }));
        return;
      }
      if (scenario === 'missing-disables') {
        res.end(
          JSON.stringify(
            op === 'cancel' ? { cancelled: false } : { set: entry },
          ),
        );
        return;
      }
      if (scenario === 'bad-field') {
        res.end(
          JSON.stringify(
            op === 'cancel'
              ? { disables: [], cancelled: 'false' }
              : { disables: {} },
          ),
        );
        return;
      }
      if (scenario === 'null-field') {
        res.end(
          JSON.stringify(
            op === 'cancel'
              ? { disables: [], cancelled: null }
              : { disables: null },
          ),
        );
        return;
      }
      if (scenario.startsWith?.('mismatch:'))
        saved = saved.map((e) => {
          const field = String(scenario).split(':')[1];
          if (field === 'provider')
            return {
              ...e,
              provider: e.provider === 'codex' ? 'claude' : 'codex',
            };
          if (field === 'by') return { ...e, by: 'someone-else' };
          if (field === 'reason') return { ...e, reason: 'different reason' };
          if (field === 'setAt')
            return {
              ...e,
              setAt: new Date(Date.parse(e.setAt) - 3600000).toISOString(),
              until: new Date(Date.parse(e.until) - 3600000).toISOString(),
            };
          return {
            ...e,
            until: new Date(Date.parse(e.until) + 60000).toISOString(),
          };
        });
      res.end(
        JSON.stringify({
          disables: saved,
          ...(op === 'set' ? { set: entry } : {}),
          ...(op === 'cancel' ? { cancelled: scenario !== 'absent' } : {}),
        }),
      );
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const base = `http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`;
  // Keep the port reserved and fail every connection immediately.
  const refused = createServer();
  refused.on('connection', (socket) => socket.destroy());
  await new Promise<void>((resolve) => refused.listen(0, '127.0.0.1', resolve));
  const refusedBase = `http://127.0.0.1:${(refused.address() as import('node:net').AddressInfo).port}`;
  const bin = path.join(home, '.issue-desk');
  mkdirSync(bin);
  writeFileSync(path.join(home, 'window.mjs'), windowProgram);
  const tokenFile = path.join(home, 'token');
  writeFileSync(tokenFile, fakeToken, { mode: 0o600 });
  const capture = path.join(home, 'calls');
  const env = {
    HOME: home,
    PATH: `${path.dirname(process.execPath)}:/usr/bin:/bin`,
    CAPTURE: capture,
    FAKE_TOKEN_FILE: tokenFile,
  };
  return {
    examples,
    calls,
    requests,
    async run(
      example,
      fault: string | number = 'ok',
      { keepState = false, shape = '' } = {},
    ) {
      scenario = fault;
      if (!keepState) saved = [];
      writeFileSync(capture, '');
      requests.length = 0;
      for (const name of ['desk', 'desk-pc']) {
        const script =
          name === 'desk' && example.environment === 'NAS'
            ? '#!/bin/sh\nexit 2\n'
            : `#!/bin/sh\nWINDOW_NAME=${name}; export WINDOW_NAME\nexec node "$HOME/window.mjs" "$@"\n`;
        writeFileSync(path.join(bin, name), script, { mode: 0o755 });
      }
      const outputFiles = () =>
        readdirSync(home, { withFileTypes: true, recursive: true })
          .filter(
            (entry) =>
              entry.isFile() &&
              ![
                'window.mjs',
                'token',
                'calls',
                '.issue-desk/desk',
                '.issue-desk/desk-pc',
              ].includes(
                path.relative(home, path.join(entry.parentPath, entry.name)),
              ),
          )
          .map((entry) => path.join(entry.parentPath, entry.name));
      for (const file of outputFiles()) rmSync(file);
      let stdout = '',
        stderr = '';
      const child = spawn('sh', ['-eu', '-c', example.code], {
        cwd: home,
        env: {
          ...env,
          ...(shape ? { WINDOW_SHAPE: shape } : {}),
          FAKE_BASE: fault === 'refused' ? refusedBase : base,
        },
        stdio: ['ignore', 'pipe', 'pipe'],
      });
      active = child;
      child.stdout.on('data', (b) => {
        stdout += b;
      });
      child.stderr.on('data', (b) => {
        stderr += b;
      });
      const code = await new Promise((resolve, reject) => {
        child.once('error', reject);
        child.once('close', resolve);
      });
      active = null;
      const files = outputFiles().map((file) => ({
        name: path.relative(home, file),
        content: readFileSync(file, 'utf8'),
      }));
      assert.equal(
        (stdout + stderr + files.map((file) => file.content).join('')).includes(
          fakeToken,
        ),
        false,
        '出力と生成ファイルに偽トークンを出さない',
      );
      const captured = readFileSync(capture, 'utf8')
        .split('\n')
        .filter(Boolean)
        .map((line) => JSON.parse(line))
        .map((c) => ({ ...c, operation: operation(c) }));
      assert.ok(captured.length, `行${example.line}: 窓口呼出しが必要`);
      for (const c of captured)
        assert.equal(
          c.window,
          example.environment === 'NAS' ? 'desk-pc' : 'desk',
        );
      calls.push(...captured);
      return { code, stdout, stderr, files, captured, requests: [...requests] };
    },
    async close() {
      active?.kill();
      server.closeAllConnections();
      refused.closeAllConnections();
      await new Promise((resolve) => refused.close(resolve));
      await new Promise((resolve) => server.close(resolve));
      rmSync(home, { recursive: true, force: true });
    },
  };
}

// Readback is a separate, explicit operation, as instructed in the prose. These
// assertions compare the observations with that instruction; they do not turn
// them into a domain success using a test-only classifier or resend the POST.
export async function verifyUnknownReadback(window) {
  const baseline = [];
  for (const example of window.examples) {
    const result = await window.run(example);
    assert.equal(result.code, 0);
    baseline.push({ example, call: result.captured[0] });
  }
  for (const { example, call } of baseline) {
    if (call.operation === 'list') continue;
    const list = baseline.find(
      (e) =>
        e.example.environment === example.environment &&
        e.call.operation === 'list',
    )?.example;
    assert.ok(list, '捕捉したGETメソッド・一覧経路で同じ環境の一覧例を選ぶ');
    const mismatches =
      call.operation === 'set'
        ? [
            'provider',
            'until',
            'by',
            'reason',
            ...(call.body.until === undefined ? ['setAt'] : []),
          ].map((f) => `mismatch:${f}`)
        : [];
    for (const followup of ['ok', 'missing', ...mismatches]) {
      const lost = await window.run(example, 'lost');
      assert.notEqual(lost.code, 0);
      assert.equal(lost.requests.length, 1);
      assert.equal(lost.captured.length, 1);
      checkGuidance(example, lost.captured[0]);
      const reread = await window.run(list, followup, { keepState: true });
      assert.equal(reread.captured[0].method, 'GET');
      assert.equal(reread.captured[0].path, '/api/provider-disables');
      assert.equal(reread.requests.length, 1);
      checkGuidance(list, reread.captured[0]);
      if (followup === 'missing') {
        assert.notEqual(reread.code, 0);
        assert.equal(reread.stdout.trim(), '');
        continue;
      }
      assert.equal(reread.code, 0);
      const entries = JSON.parse(reread.stdout).disables;
      if (call.operation === 'cancel') {
        assert.deepEqual(entries, []);
        continue;
      }
      assert.equal(entries.length, 1);
    }
  }
}
