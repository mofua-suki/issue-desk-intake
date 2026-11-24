// create-issue.ts が受付へ送る前の確認: 本文が参照した同じ受付の依頼のうち、マージされずに閉じたものを一覧にして
// 止める（--history-read で読んだ要点を添えたときだけ送る）。参照の読み取りは共通の resolveTarget。
import { resolveTarget } from '../desk-target.ts';

const historyReadMax = 500;
const maxReferences = 10;

// --draft の裸の #N を照合するため、下書きから受付プロジェクトを読む（project だけを使うので要約で読む）。
// 読み取りに失敗した場合は project 不明として既存の共通番号・owner/repo#N の確認を続ける。
export async function draftProject(call, draft) {
  try {
    const value = await call('GET', `/api/drafts/${draft}?view=summary`);
    return typeof value?.project === 'string' && value.project
      ? value.project
      : undefined;
  } catch {
    return undefined;
  }
}

// argv から --history-read <要点> を取り出す（1〜500文字）。残りの argv を返す。
export function takeHistoryRead(argv, usage = '') {
  const rest = [];
  let historyRead;
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] !== '--history-read') {
      rest.push(argv[i]);
      continue;
    }
    const value = argv[++i];
    if (historyRead !== undefined)
      throw Error(`引数を確認してください: --history-read\n${usage}`);
    if (value === undefined || value.startsWith('--'))
      throw Error(`--history-read の値がありません。\n${usage}`);
    historyRead = value.trim();
    const length = [...historyRead].length;
    if (length < 1 || length > historyReadMax)
      throw Error(
        `--history-read は前後の空白を除いて1〜${historyReadMax}文字の読んだ要点で指定してください。\n${usage}`,
      );
  }
  return { argv: rest, historyRead };
}

// 本文の参照を拾う: 共通番号N / owner/repo#N / #N（#N は project のときだけ）。重複は除き、上限までにする。
function extractReferences(text, project) {
  const found = new Map();
  const add = (key, ref) => found.has(key) || found.set(key, ref);
  const body = String(text ?? '');
  for (const m of body.matchAll(/共通番号\s*(\d+)/g))
    add(`desk:${Number(m[1])}`, { desk: String(Number(m[1])) });
  for (const m of body.matchAll(
    /(?<![\w./-])([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)#(\d+)/g,
  ))
    add(`${m[1].toLowerCase()}#${Number(m[2])}`, {
      project: m[1],
      number: String(Number(m[2])),
    });
  if (project)
    for (const m of body.matchAll(/(?<![\w/#&])#(\d+)/g))
      add(`${String(project).toLowerCase()}#${Number(m[1])}`, {
        project,
        number: String(Number(m[1])),
      });
  return [...found.values()]
    .filter((r) => r.desk !== '0' && r.number !== '0')
    .slice(0, maxReferences);
}

// マージされた依頼か。受付の詳細は、マージ済みを status=completed（PRの merged も見る）で示す。
const merged = (d) =>
  d.status === 'completed' ||
  d.pr?.merged === true ||
  d.pr?.result === 'merged';
const label = (r) =>
  r.desk ? `共通番号${r.desk}` : `${r.project}#${r.number}`;
const day = (value) => (value ? String(value).slice(0, 10) : '不明');

// 戻り値: { stop } 。stop=true のとき呼び出し側は何も送らず終了コード1にする。
export async function closedReferenceCheck({
  call,
  text,
  project = undefined,
  historyRead = undefined,
  log = console.log,
  warn = console.error,
}) {
  const refs = extractReferences(text, project);
  if (!refs.length) return { stop: false };
  const closed = [];
  const unchecked = [];
  for (const ref of refs) {
    let detail;
    try {
      detail = await resolveTarget(call, ref);
    } catch (e) {
      // 400・403・404 は受付の依頼ではない参照として読み飛ばす。status なし・502/503/504（受付の再起動中など）は
      // 確認を省いて送る。それ以外の status は参照ごとに「確かめられなかった」として載せる。
      if (typeof e?.status !== 'number' || [502, 503, 504].includes(e.status)) {
        warn(
          `警告: 参照した依頼の状態を読めなかったため、閉じた依頼の確認を省いて送ります（${e?.message ?? e}）。`,
        );
        return { stop: false };
      }
      if (![400, 403, 404].includes(e.status))
        unchecked.push({ ref, reason: e.message });
      continue;
    }
    if (detail?.state === 'closed' && !merged(detail))
      closed.push({ ref, detail });
  }
  if (!closed.length && !unchecked.length) return { stop: false };
  const lines = [
    '本文が参照した依頼に、マージされずに閉じたものがあります。送る前に閉じた理由・失敗の理由・レビューの指摘を読んでください:',
  ];
  for (const { ref, detail } of closed) {
    const desk = detail.desk ?? ref.desk;
    const read = desk
      ? `/api/issue?${new URLSearchParams({ desk: String(desk) })}`
      : `/api/issue?${new URLSearchParams({ project: detail.project ?? ref.project, number: String(detail.number ?? ref.number) })}`;
    lines.push(
      `- ${desk ? `共通番号${desk}` : label(ref)} ${detail.title ?? ''}（閉じた日: ${day(detail.closedAt)}） 読む: node scripts/desk-get.ts GET '${read}'`,
    );
  }
  for (const { ref, reason } of unchecked)
    lines.push(
      `- ${label(ref)} （状態を確かめられませんでした: ${reason}） 読む: node scripts/desk-get.ts GET '/api/issue?${new URLSearchParams(ref.desk ? { desk: String(ref.desk) } : { project: ref.project, number: String(ref.number) })}'`,
    );
  if (historyRead !== undefined) {
    lines.push(`読んだ要点: ${historyRead}`);
    log(lines.join('\n'));
    return { stop: false };
  }
  lines.push(
    '読んだ上で送るときは --history-read "<読んだ要点>"（1〜500文字）を付けて送り直してください。何も送らずに止めました。',
  );
  warn(lines.join('\n'));
  return { stop: true };
}
