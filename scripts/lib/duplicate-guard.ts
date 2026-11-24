// 二重起票を送る前に止める。既存の開いた依頼・相談・TODO、または自分が直近に送った依頼と
// 題名・対象ファイルが大半重なるとき、--not-duplicate の明示が無ければ送らない（create-issue.ts が終了コード1にする）。
// 送信済みの台帳は ~/.issue-desk/sent-requests.json（ISSUE_DESK_SENT_LEDGER で差し替え可）。Node 18 でも読み込む。
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { referenceFiles } from './intake-preflight.ts';

const ledgerWindowMs = 24 * 60 * 60 * 1000;
const ledgerMax = 200;
const fileOverlap = 0.6;
const titleOverlap = 0.6;

function ledgerPath(env = process.env) {
  return (
    env.ISSUE_DESK_SENT_LEDGER ||
    path.join(os.homedir(), '.issue-desk', 'sent-requests.json')
  );
}

const clean = (value) =>
  String(value ?? '')
    .normalize('NFKC')
    .toLowerCase()
    .replace(/[\s\p{P}\p{S}]/gu, '');

function titleOf(text) {
  const line = String(text ?? '')
    .split(/\r?\n/)
    .map((row) => row.replace(/^#{1,6}\s*/, '').trim())
    .find(Boolean);
  return (line ?? '').slice(0, 200);
}

function grams(value) {
  const text = clean(value);
  const set = new Set();
  for (let i = 0; i <= text.length - 3; i++) set.add(text.slice(i, i + 3));
  return set;
}

export function similarTitle(a, b) {
  const left = clean(a),
    right = clean(b);
  if (left.length < 6 || right.length < 6) return false;
  if (left === right || left.includes(right) || right.includes(left))
    return true;
  const x = grams(a),
    y = grams(b);
  let common = 0;
  for (const gram of x) if (y.has(gram)) common++;
  const union = x.size + y.size - common;
  return union > 0 && common / union >= titleOverlap;
}

function sharedFiles(a, b) {
  const set = new Set(b);
  const common = [...new Set(a)].filter((file) => set.has(file));
  const smaller = Math.min(new Set(a).size, set.size);
  return common.length >= 2 &&
    smaller > 0 &&
    common.length / smaller >= fileOverlap
    ? common
    : [];
}

function rowTitle(row) {
  if (row.title) return row.title;
  return titleOf(
    row.body ||
      row.text ||
      row.messages?.find((m) => m.role === 'user')?.text ||
      '',
  );
}

/** 既存の開いた依頼・相談・TODOのうち、題名または対象ファイルが大半重なるもの。 */
export function strongDuplicates(text, rows) {
  const title = titleOf(text);
  const files = referenceFiles(text);
  return rows.filter((row) => {
    const body =
      row.body ||
      row.text ||
      row.messages?.find((m) => m.role === 'user')?.text ||
      '';
    return (
      similarTitle(title, rowTitle(row)) ||
      sharedFiles(files, referenceFiles(body)).length > 0
    );
  });
}

function readLedger(file = ledgerPath()) {
  try {
    const list = JSON.parse(fs.readFileSync(file, 'utf8'));
    return Array.isArray(list) ? list : [];
  } catch {
    return [];
  }
}

/** 自分が直近（24時間）に同じプロジェクトへ送った依頼のうち似たもの。 */
function recentSentDuplicates(
  text,
  project,
  { file = ledgerPath(), now = Date.now() } = {},
) {
  const title = titleOf(text);
  const files = referenceFiles(text);
  return readLedger(file).filter(
    (entry) =>
      entry.project === project &&
      now - Date.parse(entry.at) < ledgerWindowMs &&
      (similarTitle(title, entry.title) ||
        sharedFiles(files, entry.files ?? []).length > 0),
  );
}

export function recordSent(
  text,
  project,
  { file = ledgerPath(), now = Date.now(), draft = undefined } = {},
) {
  try {
    const list = readLedger(file).filter(
      (entry) => now - Date.parse(entry.at) < ledgerWindowMs,
    );
    list.push({
      project,
      at: new Date(now).toISOString(),
      title: titleOf(text),
      files: referenceFiles(text),
      draft,
    });
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, JSON.stringify(list.slice(-ledgerMax)));
  } catch {
    /* 台帳に書けなくても送信は妨げない。 */
  }
}

/** 止めるときは表示する文を返し、通すときは null。notDuplicate が明示されていれば通す。 */
export function duplicateStop({
  text,
  project,
  rows = [],
  notDuplicate,
  ledger = {},
}: {
  text: string;
  project: string;
  rows?: any[];
  notDuplicate?: unknown;
  ledger?: Record<string, any>;
}) {
  if (notDuplicate) return null;
  const existing = strongDuplicates(text, rows);
  const sent = recentSentDuplicates(text, project, ledger);
  if (!existing.length && !sent.length) return null;
  const lines = [
    '二重起票の疑いがあるため、何も送らずに止めました（終了コード1）。',
    ...existing.map(
      (row) =>
        `  既存の${row.kind ?? '依頼'}: ${rowTitle(row) || row.id || row.number}${row.number ? `（共通番号 ${row.desk ?? row.number}）` : ''}`,
    ),
    ...sent.map(
      (entry) =>
        `  自分が${entry.at}に送った依頼: ${entry.title}${entry.draft ? `（下書き ${entry.draft}）` : ''}`,
    ),
    '同じ内容なら送り直さず、汎用GET・--check で既存の進み具合を読んで追記・返信で扱ってください。',
    '読んだうえで別の依頼だと言えるときだけ、違いを書いて --not-duplicate <理由> を付けて送り直します。',
  ];
  return lines.join('\n');
}
