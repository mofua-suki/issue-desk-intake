// 表示だけを短くする。通信・保存・URL・キーには呼出元の完全名を使う。
// 登録一覧に別ownerの同名リポジトリがある場合は、識別できるよう完全名を残す。
export function shownProject(
  project: string | null | undefined,
  projects: readonly unknown[] = [],
): string | null | undefined {
  if (typeof project !== 'string' || !/^[^/]+\/[^/]+$/.test(project))
    return project;
  const name = project.slice(project.indexOf('/') + 1);
  return projects.some(
    (other) =>
      typeof other === 'string' &&
      other !== project &&
      other.endsWith(`/${name}`),
  )
    ? project
    : name;
}

// APIが生成する完全名付きのIssue/コミット参照も、識別子を保持したまま表示する。
export function shownRepositoryReference(
  reference: string | null | undefined,
  projects: readonly unknown[] = [],
): string | null | undefined {
  if (typeof reference !== 'string') return reference;
  const match = reference.match(/^([^/\s]+\/[^/#@\s]+)(#\d+|@[a-f0-9]+)$/i);
  return match
    ? `${shownProject(match[1], projects)}${match[2]}`
    : shownProject(reference, projects);
}
