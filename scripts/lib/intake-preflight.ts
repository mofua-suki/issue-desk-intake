// Node 18 のリモートCLIでも読み込む。受付AIを呼ばない送信前の手掛かり。
import {
  markdownLines,
  namedRequestFiles,
} from '../../shared/src/intake-request-size.ts';
import {
  draftPageLimit,
  readAllDraftPages,
} from '../../shared/src/desk-alert-reader.ts';
import { execFileSync } from 'node:child_process';

// 起票済み・終了・引き継ぎ済みの相談は「開いている相談」の候補にならない。受付で除外して全文を読まない
// （終了した相談が大半で、全文は1プロジェクトで十数MBになる）。古い受付が除外を無視しても下で落とす。
const closedDraftStatuses = 'submitted,closed,project_handoff';

const outside =
  /実機|本番環境|実サービス|実データ|NAS|docker\s+compose\s+run|ログ(?:を|の)?(?:確認|調査|取得)|ログ確認/i;
const negative =
  /無い|ない|検査されていない|未実装|未検査|未対応|存在しな|存在せず|実装されていな/i;

export function preflightLines(text) {
  let section = '';
  let depth = 0;
  return markdownLines(text).map(({ line, heading, fenced }, i) => {
    if (heading) {
      const nextDepth = heading.depth;
      if (/assumptions|利用者の判断と根拠/i.test(heading.title)) {
        section = heading.title.trim();
        depth = nextDepth;
      } else if (nextDepth <= depth) {
        section = '';
        depth = 0;
      }
    }
    return {
      line,
      number: i + 1,
      outside: outside.test(line),
      claim:
        !fenced &&
        /assumptions|利用者の判断と根拠/i.test(section) &&
        negative.test(line),
    };
  });
}

function claimTerms(line): string[] {
  // 識別子・パスと日本語の連続語を手掛かりにする。否定語そのものは検索しない。
  const tokens: string[] =
    line.match(
      /[A-Za-z][A-Za-z0-9_./-]{2,}|[\p{Script=Han}\p{Script=Katakana}ー]{2,}/gu,
    ) || [];
  return [
    ...new Set(
      tokens.filter(
        (word) =>
          !/^(assumptions|evidence|verified|false|true|前提|根拠|判断|利用者|事実|確認済|検査|未実装|未検査|未対応|存在|報告)$/.test(
            word,
          ),
      ),
    ),
  ];
}

function scopeOf(repo) {
  const merge = repo.autoMerge === true ? '自動マージ' : 'マージは人が行う';
  const deploy =
    repo.autoDeploy === true
      ? 'マージ後に設定された配置先へ反映・起動確認'
      : '配置先・本番環境への反映は行わない';
  return `実装・検証は隔離された作業ツリー内。本番環境・実機・実サービス・実データ・ログイン済みブラウザには触れない。${merge}。${deploy}。`;
}

// HEAD を一度固定し、その版の tracked files だけ読む。作業ツリーや別プロジェクトは検索しない。
function readPinned(
  repo,
  run = (command, args, options) =>
    execFileSync(command, args, { ...options, encoding: 'utf8' }),
) {
  if (!repo?.localPath) throw Error('登録された localPath を利用できません');
  const git = (args) =>
    run('git', ['-C', repo.localPath, ...args], {
      encoding: 'utf8',
      timeout: 10000,
      maxBuffer: 16 * 1024 * 1024,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
  const revision = git(['rev-parse', '--verify', 'HEAD^{commit}']).trim();
  if (!/^[a-f0-9]{40,64}$/.test(revision))
    throw Error('固定コミットを取得できません');
  const names = git([
    'ls-tree',
    '-r',
    '-z',
    '--name-only',
    revision,
    '--',
    'test/',
    'skills/',
    'docs/',
  ])
    .split('\0')
    .filter(Boolean);
  const files = [];
  let incomplete = false;
  for (const name of names) {
    try {
      files.push({ name, text: git(['show', `${revision}:${name}`]) });
    } catch {
      incomplete = true;
    }
  }
  return { revision, files, incomplete };
}

function preflightReport(text, repo, snapshot) {
  const lines = preflightLines(text);
  const scope = repo
    ? scopeOf(repo)
    : '登録内容を取得・表示できません（この接続では登録設定を利用できません）';
  const messages = [`送信前点検: 登録された automation の実装範囲: ${scope}`];
  for (const row of lines.filter((row) => row.outside))
    messages.push(
      `警告: 範囲外作業の可能性 L${row.number}: ${row.line}\n  登録範囲: ${scope}`,
    );
  const claims = lines.filter((row) => row.claim);
  if (claims.length) {
    if (!snapshot)
      messages.push(
        '警告: 対象プロジェクトの固定版を読めませんでした。否定の主張は未確認です。',
      );
    else {
      messages.push(`否定の主張の検索版: ${snapshot.revision}`);
      if (snapshot.incomplete)
        messages.push(
          '警告: 固定版の一部を読めませんでした。検索は不完全です。',
        );
      for (const row of claims) {
        const terms = claimTerms(row.line);
        const hits = snapshot.files.filter((file) =>
          terms.some((term) =>
            `${file.name}\n${file.text}`
              .toLowerCase()
              .includes(term.toLowerCase()),
          ),
        );
        if (hits.length)
          messages.push(
            `警告: 既存の検査・記述がある可能性 L${row.number}: ${row.line}\n  識別語: ${terms.join(', ')}\n  ファイル: ${hits.map((file) => file.name).join(', ')}`,
          );
        else
          messages.push(
            `L${row.number}: 検索ヒットなし（不存在を確認した意味ではありません）。`,
          );
      }
    }
  }
  messages.push(
    '点検は警告用の手掛かりです。警告を確認し、依頼文を修正してから送ってください。送信は止めません。受付AIの basisConflicts による根拠照合・起票抑止は引き続き行います。',
  );
  return messages.join('\n');
}

export function showPreflight({
  text,
  project,
  config = undefined,
  read = readPinned,
  warn = console.error,
}) {
  const repo = config?.repositories?.find((item) => item.name === project);
  let snapshot;
  if (repo && preflightLines(text).some((row) => row.claim)) {
    try {
      snapshot = read(repo);
    } catch {
      /* 読取失敗の詳細には資格情報が含まれうるため表示しない。 */
    }
  }
  warn(preflightReport(text, repo, snapshot));
}

export function referenceFiles(text) {
  // 番号付き依頼と Git の明示的な参照名は、名指しファイルから除く。
  return namedRequestFiles(
    String(text).replace(/[\w.-]+\/[\w.-]+#\d+\b/g, ' '),
  ).filter(
    (file) =>
      !/^(?:(?:origin|upstream)\/|refs\/(?:heads|remotes)\/)/.test(file),
  );
}

// 各ファイルの直後だけを調べる。他のファイルの範囲指定で不足を隠さない。
function missingReferenceScopes(text) {
  const missing = new Set();
  const scoped = new Set();
  for (const line of String(text).split(/\r?\n/)) {
    // 共有候補はディレクトリ・欄名も含む。範囲警告だけを既知のファイル表記に絞る。
    const files = referenceFiles(line).filter(
      (file) =>
        /\.(?:mjs|cjs|js|jsx|ts|tsx|mts|cts|md|json|jsonl|ya?ml|toml|lock|py|ps1|sh|cmd|bat|service|timer|webmanifest|svg|html|css|txt)$/i.test(
          file,
        ) ||
        /(?:^|\/)(?:\.[A-Za-z][\w.-]*|Dockerfile|Makefile|GNUmakefile|Containerfile|Procfile|LICENSE|LICENCE|COPYING|NOTICE|README)$/.test(
          file,
        ),
    );
    for (const file of files) {
      let start = line.indexOf(file);
      while (start !== -1) {
        const prefix = line.slice(0, start);
        // b.ts の範囲を a/b.ts の指定で満たしたことにしない（./ は表記の違い）。
        const boundary =
          !/[A-Za-z0-9_@.+/-]$/.test(prefix) ||
          /(?:^|[^A-Za-z0-9_@.+/-])\.\/$/.test(prefix);
        const tail = line.slice(start + file.length);
        const scope =
          /^[`）)]?\s*(?::\s*\d+(?:\s*[-–〜]\s*\d+)?|#L\d+(?:-L?\d+)?|\(?L\d+(?:\s*[-–〜]\s*L?\d+)?|(?:の)?\s*\d+\s*[-–〜]\s*\d+\s*行|(?:の)?\s*(?:『[^』]+』|「[^」]+」|節\s*\S+)|\s*lines?\s+\d+(?:-\d+)?)/i;
        if (boundary && scope.test(tail)) scoped.add(file);
        start = line.indexOf(file, start + file.length);
      }
      missing.add(file);
    }
  }
  return [...missing].filter((file) => !scoped.has(file));
}

function ambiguousNumbers(text) {
  const result = [];
  for (const match of String(text).matchAll(/#\d+\b/g)) {
    const before = text.slice(0, match.index);
    const after = text.slice(match.index + match[0].length);
    if (/[\w.-]+\/[\w.-]+$/.test(before)) continue;
    if (/(?:共通番号|Gitea(?:番号)?)[\s`*]*[:：]?[\s`*「（([]*$/i.test(before))
      continue;
    if (/^[\s`*」\])]*[（(](?:共通番号|Gitea(?:番号)?)[）)]/i.test(after))
      continue;
    result.push(match[0]);
  }
  return [...new Set(result)];
}

function opening(text) {
  const source = String(text || '');
  const purpose = source.match(
    /^#{1,6}[ \t]*目的[ \t]*\r?\n([\s\S]*?)(?=^#{1,6}\s|(?![\s\S]))/m,
  )?.[1];
  return (purpose ?? source.trim().replace(/^#{1,6}[^\n]*(?:\n|$)/, ''))
    .trim()
    .slice(0, 240);
}

function overlaps(a, b) {
  const clean = (value) =>
    String(value)
      .normalize('NFKC')
      .toLowerCase()
      .replace(/[\s\p{P}\p{S}]/gu, '');
  const left = clean(a),
    right = clean(b);
  if (left.length < 4 || right.length < 4) return false;
  for (let i = 0; i <= left.length - 4; i++)
    if (right.includes(left.slice(i, i + 4))) return true;
  return false;
}

function duplicateCandidates(text, rows) {
  const paths = referenceFiles(text);
  return rows.filter((row) => {
    const body =
      row.body ||
      row.text ||
      row.messages?.find((m) => m.role === 'user')?.text ||
      '';
    return (
      overlaps(opening(text), opening(body)) ||
      referenceFiles(body).some((file) => paths.includes(file))
    );
  });
}

// 認可は既存の読み取りAPIが判定する。403の種類は内容を取得せず照合不能として残す。
export async function readPreflightCandidates(
  call,
  project,
  {
    concurrency = 8,
    timeoutMs = 60000,
    now = () => performance.now(),
    setTimer = setTimeout,
    clearTimer = clearTimeout,
  } = {},
) {
  const rows = [],
    unavailable = [];
  const details = [];
  const expires = now() + timeoutMs;
  const timeout = Symbol('preflight timeout');
  let timedOut = false;
  let timer;
  const deadline = new Promise((resolve) => {
    timer = setTimer(() => resolve(timeout), timeoutMs);
    timer.unref?.();
  });
  const read = async (route) => {
    if (timedOut || now() >= expires) {
      timedOut = true;
      throw timeout;
    }
    // race が遅延した拒否も受け取る。期限後の完了は rows・警告へ触れない。
    const result = await Promise.race([call('GET', route), deadline]);
    if (result === timeout || now() >= expires) {
      timedOut = true;
      throw timeout;
    }
    return result;
  };
  try {
    for (const [kind, route] of [
      ['依頼', '/api/issues/search'],
      ['相談', '/api/drafts'],
      ['TODO', '/api/todos'],
    ]) {
      try {
        const list =
          kind === '相談' || kind === 'TODO'
            ? await readAllDraftPages(
                (page) =>
                  read(
                    `${route}?${new URLSearchParams({ project, page: String(page), limit: String(draftPageLimit), ...(kind === '相談' ? { excludeStatus: closedDraftStatuses } : {}) })}`,
                  ),
                '一覧の形式が不明',
              )
            : [];
        if (kind === '依頼') {
          for (let page = 1; ; page++) {
            const value = await read(
              `${route}?${new URLSearchParams({ project, state: 'open', limit: '100', page: String(page) })}`,
            );
            if (
              !Array.isArray(value?.issues) ||
              !Number.isSafeInteger(value.total) ||
              value.total < 0 ||
              typeof value.hasMore !== 'boolean' ||
              (value.hasMore && !value.issues.length)
            )
              throw Error('一覧の形式が不明');
            // 読めなかった候補のリポジトリがあり行がそろっていない応答は、重複なしと扱わない。
            if (value.incomplete === true) throw Error('一覧が途中まで');
            list.push(...value.issues);
            if (!value.hasMore) break;
          }
        }
        if (!Array.isArray(list)) throw Error('一覧の形式が不明');
        for (const item of list) {
          // 相談の正規の未定レコード。対象プロジェクトの候補ではないが一覧の走査は続ける。
          if (
            kind === '相談' &&
            item?.project === null &&
            item.unassigned === true
          )
            continue;
          if (!item || typeof item.project !== 'string')
            throw Error('項目の形式が不明');
          if (item.project !== project) continue;
          if (kind === '依頼' && item.state !== 'open') continue;
          if (
            kind === '相談' &&
            (item.issue ||
              item.issues?.length ||
              closedDraftStatuses.split(',').includes(item.status))
          )
            continue;
          if (kind === 'TODO' && (item.draftId || item.removedAt)) continue;
          const index = rows.length;
          rows.push({ ...item, kind });
          if (kind === '依頼') details.push({ item, index });
        }
      } catch (error) {
        if (error === timeout) break;
        unavailable.push(kind);
      }
    }
    let next = 0;
    await Promise.all(
      Array.from(
        { length: Math.min(concurrency, details.length) },
        async () => {
          while (next < details.length && !timedOut) {
            const { item, index } = details[next++];
            try {
              const detail = await read(
                `/api/issue?${new URLSearchParams({ project, number: String(item.number) })}`,
              );
              if (typeof detail?.body !== 'string')
                throw Error('詳細の形式が不明');
              rows[index] =
                detail.state === 'closed' ? null : { ...detail, kind: '依頼' };
            } catch (error) {
              if (error !== timeout) unavailable.push('依頼の詳細（一部）');
            }
          }
        },
      ),
    );
    const failureOrder = ['依頼', '依頼の詳細（一部）', '相談', 'TODO'];
    return {
      rows: rows.filter(Boolean),
      unavailable: [...new Set(unavailable)].sort(
        (a, b) => failureOrder.indexOf(a) - failureOrder.indexOf(b),
      ),
      ...(timedOut ? { timedOut: true } : {}),
    };
  } finally {
    clearTimer(timer);
  }
}

export async function consultationPreflight({
  text,
  project,
  call,
  draft = false,
  warn = console.error,
  candidateOptions = undefined,
  onCandidates = undefined,
}) {
  const warningTypes = [];
  const files = missingReferenceScopes(text);
  if (files.length) {
    warningTypes.push('reference_scope');
    warn(`警告: 行範囲・節の指定が見つからないファイル: ${files.join(', ')}`);
  }
  if (!draft) {
    const numbers = ambiguousNumbers(text);
    if (numbers.length) {
      warningTypes.push('ambiguous_number');
      warn(
        `警告: 番号表記が曖昧です（共通番号・Gitea番号を明示）: ${numbers.join(', ')}`,
      );
    }
    const { rows, unavailable, timedOut } = await readPreflightCandidates(
      call,
      project,
      candidateOptions,
    );
    const candidates = duplicateCandidates(text, rows);
    onCandidates?.({ rows, candidates, unavailable, timedOut });
    if (candidates.length) {
      warningTypes.push('duplicate_candidate');
      warn(
        `警告: 重複の可能性がある候補: ${candidates.map((r) => `${r.kind}: ${r.title || r.id || r.number}`).join(', ')}`,
      );
    }
    if (unavailable.length || timedOut) {
      warningTypes.push('duplicate_unavailable');
      if (unavailable.length)
        warn(
          `警告: 候補照合不能（権限・取得失敗）: ${unavailable.join(', ')}。候補なしとは判定していません。`,
        );
      if (timedOut)
        warn(
          '警告: 候補照合不能（時間切れ）。未取得の一覧・未読詳細が残っています。候補なしとは判定していません。',
        );
    } else if (!candidates.length)
      warn(
        '読み取れた開いている依頼・相談・TODOに重複候補は見つかりませんでした。',
      );
  }
  return warningTypes;
}
