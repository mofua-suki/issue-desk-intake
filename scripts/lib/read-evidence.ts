import { readFileSync } from 'node:fs';

export function readEvidenceFile(path, project) {
  const evidence = JSON.parse(readFileSync(path, 'utf8'));
  const object = (v) =>
    v !== null && typeof v === 'object' && !Array.isArray(v);
  const nonempty = (v) => typeof v === 'string' && v.trim().length > 0;
  if (
    !object(evidence) ||
    evidence.version !== 1 ||
    !Array.isArray(evidence.documents) ||
    evidence.documents.length > 16 ||
    !Array.isArray(evidence.facts) ||
    evidence.facts.length > 64 ||
    Buffer.byteLength(JSON.stringify(evidence)) > 65536
  )
    throw Error('readEvidence の形式またはサイズが不正です。');
  const ids = new Set();
  for (const d of evidence.documents) {
    if (
      !object(d) ||
      !nonempty(d.id) ||
      ids.has(d.id) ||
      d.project !== project ||
      !nonempty(d.path) ||
      typeof d.revision !== 'string' ||
      !/^[0-9a-f]{40}$/.test(d.revision) ||
      !Number.isSafeInteger(d.start) ||
      !Number.isSafeInteger(d.end) ||
      d.start < 1 ||
      d.end < d.start ||
      typeof d.text !== 'string' ||
      !d.text.length
    )
      throw Error('readEvidence の資料が不正です。');
    ids.add(d.id);
    d.text = d.text.replace(/\r\n?/g, '\n');
  }
  for (const f of evidence.facts)
    if (
      !object(f) ||
      !nonempty(f.statement) ||
      !Array.isArray(f.documents) ||
      !f.documents.length ||
      f.documents.some((id) => !ids.has(id))
    )
      throw Error('readEvidence の事実または参照が不正です。');
  return evidence;
}
