import type { VariantFields } from './types.ts';
// 後続Issueの案コメントの共通形式（版1〜版3）。Go側の読取契約と照合する書出しと検証を保持する。
// 人が読む説明と、専用マーカー内のJSONを1件のコメントに並べる。受付が採用するのはJSONだけで、説明文から
// 内容を推測して補わない。旧形式の「後続Issueの案」コメントと、後続Issue本文の issue-desk-followup マーカーとは
// マーカー名が違うので取り違えない。
// 版2（Issue #1146）は版1の9項目に変更範囲の宣言 changeScope（{paths} だけ）を加えた10項目。ここでは形だけを確かめ、
// パスの中身は受付が起票時に確かめる（server/src/intake-proposals.mjs の proposalScope）。監視が出す実装由来の分割の案は、
// 変更範囲のパスを決められれば版2、決められなければ版1（Issue #1290。shared/src/proposal-scope-paths.mjs）。レビュー由来の
// フォローアップと細かい改善のまとめは版1。
// 版3（Issue #1536）は kind=followup だけの形で、目的・受入条件の代わりにマージ済みPR（pr）と、指摘・期待する動作と
// 直し方・テスト方法の組（items）を持つ。受付はこの4項目だけの本文で起票する（shared/src/followup-issue-body.mjs）。
//
// 見本（版1）:
//   後続Issueの案（実装由来の分割）
//   ...説明（< > & は文字参照にして、説明の中にマーカーを作らない）...
//   <!-- issue-desk-proposal {"version":1,"kind":"split","id":"…","source":"owner/repo#1",…} -->
//
// JSONの中の < > & はJSONのUnicodeエスケープ（u003c・u003e・u0026）で書く。値にどんな文字列があっても、マーカーの終わり（-->）や別の
// HTMLコメントは現れない。読み取りは1件のコメントにマーカーがちょうど1つのときだけ受け付ける。
import { createHash } from 'node:crypto';

// 変更範囲なしの版（proposalVersion）と変更範囲つきの版（proposalScopeVersion）。受付はどちらも読める（proposalVersions）。
const proposalVersion = 1;
const proposalScopeVersion = 2;
// 4項目のフォローアップの版（proposalFollowupVersion。kind=followup だけ）。
const proposalFollowupVersion = 3;
const proposalVersions = [
  proposalVersion,
  proposalScopeVersion,
  proposalFollowupVersion,
];
// improvements: 保留した細かい改善のまとめ（Issue #484）。受付は followup と同じ条件で起票する。
const proposalKinds = {
  split: '実装由来の分割',
  followup: 'レビュー由来のフォローアップ',
  improvements: '細かい改善のまとめ',
};
const proposalProviders = { codex: 'Codex', claude: 'Claude' };
const proposalLimits = {
  id: 128,
  title: 200,
  purpose: 8000,
  acceptance: 30,
  acceptanceItem: 2000,
  dependsOn: 30,
  scopePaths: 40,
  scopePath: 200,
  // 版3: マージ済みPRのURL、指摘の組の件数、組の各項目（problem・expected・test）の文字数。
  pr: 500,
  items: 5,
  itemText: 2000,
};
const proposalMarker = '<!-- issue-desk-proposal';
const baseKeys = [
  'version',
  'kind',
  'id',
  'source',
  'title',
  'purpose',
  'acceptance',
  'dependsOn',
  'provider',
];
const followupKeys = [
  'version',
  'kind',
  'id',
  'source',
  'title',
  'pr',
  'items',
  'dependsOn',
  'provider',
  'changeScope',
];
const itemKeys = ['problem', 'expected', 'test'];
const keysOf = (version) =>
  version === proposalFollowupVersion
    ? followupKeys
    : version === proposalScopeVersion
      ? [...baseKeys, 'changeScope']
      : baseKeys;
const prPattern = /^https?:\/\/\S+$/;
const idPattern = /^[A-Za-z0-9_-]+$/;
const issuePattern = /^([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)#([1-9][0-9]{0,15})$/;

// "owner/repo#N" を { repo, number } に。読めなければ null。
function issueRef(text) {
  const match = typeof text === 'string' ? text.match(issuePattern) : null;
  if (!match) return null;
  const number = Number(match[2]);
  return Number.isSafeInteger(number) ? { repo: match[1], number } : null;
}
const text = (value, limit) =>
  typeof value === 'string' && !!value.trim() && value.length <= limit;

// 検証して、決まった項目順の写しを返す。通らなければ理由を載せて投げる。
function validateProposal(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value))
    throw Error('案がオブジェクトではありません');
  if (!Object.hasOwn(value, 'version')) throw Error('version がありません');
  if (!Number.isInteger(value.version))
    throw Error('version は整数で指定してください');
  if (!proposalVersions.includes(value.version))
    throw Error(`未対応の版です（version: ${value.version}）`);
  const keys =
    value.version === proposalFollowupVersion &&
    Object.hasOwn(value, 'priority')
      ? [...keysOf(value.version), 'priority']
      : keysOf(value.version);
  if (value.version === proposalVersion && Object.hasOwn(value, 'changeScope'))
    throw Error(
      `版1の案には changeScope を書けません（変更範囲の宣言は版${proposalScopeVersion}で書きます）`,
    );
  const extra = Object.keys(value).filter((k) => !keys.includes(k));
  // 優先度・モデル・権限などを案から指定させない。知らない項目は無視せず拒否する。
  if (extra.length)
    throw Error(`不明な項目があります: ${extra.join(', ').slice(0, 200)}`);
  const missing = keys.filter((k) => !Object.hasOwn(value, k));
  if (missing.length)
    throw Error(`必須項目がありません: ${missing.join(', ')}`);
  // Object.hasOwn はキーを文字列にするので、["split"] のような配列が通らないよう型を先に確かめる。
  if (
    typeof value.kind !== 'string' ||
    !Object.hasOwn(proposalKinds, value.kind)
  )
    throw Error('kind は followup・improvements・split のどれかです');
  const followup = value.version === proposalFollowupVersion;
  if (followup && value.kind !== 'followup')
    throw Error(`版${proposalFollowupVersion}の案の kind は followup だけです`);
  if (
    typeof value.id !== 'string' ||
    !idPattern.test(value.id) ||
    value.id.length > proposalLimits.id
  )
    throw Error(
      `id は${proposalLimits.id}文字以内の英数字・ハイフン・アンダースコアです`,
    );
  if (!issueRef(value.source))
    throw Error('source は owner/repo#番号 で指定してください');
  if (!text(value.title, proposalLimits.title) || /[\r\n]/.test(value.title))
    throw Error(
      `title は改行を含まない${proposalLimits.title}文字以内の文字列です`,
    );
  if (followup) return validFollowup(value);
  if (!text(value.purpose, proposalLimits.purpose))
    throw Error(`purpose は${proposalLimits.purpose}文字以内の文字列です`);
  if (
    !Array.isArray(value.acceptance) ||
    !value.acceptance.length ||
    value.acceptance.length > proposalLimits.acceptance ||
    !value.acceptance.every((x) => text(x, proposalLimits.acceptanceItem))
  )
    throw Error(
      `acceptance は1〜${proposalLimits.acceptance}件・各${proposalLimits.acceptanceItem}文字以内の文字列の配列です`,
    );
  validDependsAndProvider(value);
  const scope =
    value.version === proposalScopeVersion
      ? validScope(value.changeScope)
      : null;
  // 版1の写しは項目も順も今までどおり（保存済みの案の proposalFingerprint が変わらない）。
  return {
    version: value.version,
    kind: value.kind,
    id: value.id,
    source: value.source,
    title: value.title,
    purpose: value.purpose,
    acceptance: [...value.acceptance],
    dependsOn: [...value.dependsOn],
    provider: value.provider,
    ...(scope ? { changeScope: scope } : {}),
  };
}

function validDependsAndProvider(value) {
  if (
    !Array.isArray(value.dependsOn) ||
    value.dependsOn.length > proposalLimits.dependsOn ||
    !value.dependsOn.every(issueRef) ||
    new Set(value.dependsOn).size !== value.dependsOn.length
  )
    throw Error(
      `dependsOn は重複のない owner/repo#番号 の配列（${proposalLimits.dependsOn}件以内）です`,
    );
  if (
    typeof value.provider !== 'string' ||
    !Object.hasOwn(proposalProviders, value.provider)
  )
    throw Error('provider は codex か claude です');
}

// 版3の pr・items・changeScope（version・kind・id・source・title は validateProposal で確かめ済み）。changeScope は
// null か版2と同じ {paths}。写しは changeScope を null でも持つ（往復しても必須項目がそろう）。
function validFollowup(value) {
  let priority;
  if (Object.hasOwn(value, 'priority')) {
    const p = value.priority;
    if (
      !p ||
      typeof p !== 'object' ||
      Array.isArray(p) ||
      Object.keys(p).length !== 2 ||
      p.level !== 'high' ||
      typeof p.reason !== 'string'
    )
      throw Error('priority は level=high と reason のみを指定してください');
    const reason = p.reason.replace(
      /^\p{White_Space}+|\p{White_Space}+$/gu,
      '',
    );
    if (!reason || [...reason].length > 500)
      throw Error('priority.reason は空白を除き1〜500文字で指定してください');
    priority = { level: 'high', reason };
  }
  if (
    typeof value.pr !== 'string' ||
    value.pr.length > proposalLimits.pr ||
    !prPattern.test(value.pr)
  )
    throw Error(
      `pr は http:// か https:// で始まる空白を含まない${proposalLimits.pr}文字以内の文字列です`,
    );
  const itemsError = `items は1〜${proposalLimits.items}件の {problem, expected, test}（各${proposalLimits.itemText}文字以内の空でない文字列）の配列です`;
  if (
    !Array.isArray(value.items) ||
    !value.items.length ||
    value.items.length > proposalLimits.items
  )
    throw Error(itemsError);
  for (const item of value.items) {
    if (!item || typeof item !== 'object' || Array.isArray(item))
      throw Error(itemsError);
    const extra = Object.keys(item).filter((k) => !itemKeys.includes(k));
    if (extra.length)
      throw Error(
        `items に不明な項目があります: ${extra.join(', ').slice(0, 200)}`,
      );
    if (!itemKeys.every((k) => text(item[k], proposalLimits.itemText)))
      throw Error(itemsError);
  }
  validDependsAndProvider(value);
  const scope =
    value.changeScope === null ? null : validScope(value.changeScope);
  return {
    version: value.version,
    kind: value.kind,
    id: value.id,
    source: value.source,
    title: value.title,
    pr: value.pr,
    items: value.items.map((x) => ({
      problem: x.problem,
      expected: x.expected,
      test: x.test,
    })),
    dependsOn: [...value.dependsOn],
    provider: value.provider,
    changeScope: scope,
    ...(priority ? { priority } : {}),
  };
}

// 版2の変更範囲の宣言の形。{paths} だけを持ち、paths は1〜scopePaths 件・各 scopePath 文字以内の文字列。
function validScope(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value))
    throw Error('changeScope は {paths} のオブジェクトです');
  const extra = Object.keys(value).filter((k) => k !== 'paths');
  if (extra.length)
    throw Error(
      `changeScope に不明な項目があります: ${extra.join(', ').slice(0, 200)}`,
    );
  const { paths } = value;
  if (
    !Array.isArray(paths) ||
    !paths.length ||
    paths.length > proposalLimits.scopePaths ||
    !paths.every((x) => text(x, proposalLimits.scopePath))
  )
    throw Error(
      `changeScope.paths は1〜${proposalLimits.scopePaths}件・各${proposalLimits.scopePath}文字以内の文字列の配列です`,
    );
  return { paths: [...paths] };
}

// 同じ識別子（元リポジトリ・元Issue番号・案ID）の案が同じ内容かどうかの比較に使う。
export const proposalFingerprint = (p) =>
  createHash('sha256')
    .update(JSON.stringify(validateProposal(p)))
    .digest('hex');

// 説明の中に < > & をそのまま書かない。説明がマーカーやHTMLコメントとして読まれないようにする。
const shown = (value) =>
  value
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;');
const payload = (p) =>
  JSON.stringify(p)
    .replaceAll('<', '\\u003c')
    .replaceAll('>', '\\u003e')
    .replaceAll('&', '\\u0026');

// 書き出し。JSONはコメントの最後に置く（長い説明が途中で切られてもJSONが残るように）。
export function formatProposal(value) {
  const p: VariantFields<ReturnType<typeof validateProposal>> =
    validateProposal(value);
  const deps = p.dependsOn.length
    ? p.dependsOn.join(', ')
    : 'なし（元Issueのみ）';
  // 版3は4項目の節（指摘・直し方・テスト方法は同じ番号で対応させる）。
  const numbered = (key) =>
    p.items.map(
      (x, i) =>
        `${i + 1}. ${shown(x[key]).trimStart().replaceAll('\n', '\n   ')}`,
    );
  const content =
    p.version === proposalFollowupVersion
      ? [
          '## 元Issue',
          `${p.source} / マージ済みPR: ${shown(p.pr)}`,
          '',
          '## 指摘',
          ...numbered('problem'),
          '',
          '## 期待する動作と直し方',
          ...numbered('expected'),
          '',
          '## 受入条件（テスト方法）',
          ...numbered('test'),
        ]
      : [
          '## 目的',
          shown(p.purpose),
          '',
          '## 受入条件',
          ...p.acceptance.map((x) => `- ${shown(x)}`),
        ];
  return [
    `後続Issueの案（${proposalKinds[p.kind]}）`,
    '',
    shown(p.title),
    '',
    ...content,
    '',
    // 版2の変更範囲の宣言（人が読む一覧。受付が使うのはJSONの changeScope だけ）。
    ...(p.changeScope
      ? [
          '## 変更範囲（案の宣言）',
          ...p.changeScope.paths.map((x) => `- ${shown(x)}`),
          '',
        ]
      : []),
    `元Issue:${p.source} / 追加の依存先: ${deps} / 実行先: ${proposalProviders[p.provider]}`,
    '',
    `${proposalMarker} ${payload(p)} -->`,
  ].join('\n');
}
