import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { requestRows, rowGroup } from '../src/ui/request-classification.ts';
import type { RequestRow } from '../src/ui/list-types.ts';

const { cases } = JSON.parse(
  readFileSync(
    new URL('../../test/fixtures/list-scope-cases.json', import.meta.url),
    'utf8',
  ),
);
// The Go HTTP fixture verifies these wire fields against the real projected
// response, as well as the selected keys and complete retained row contents.
const activeRows = (
  issues: RequestRow[],
  drafts: RequestRow[],
  closes: RequestRow[] = [],
) =>
  requestRows(issues, drafts, closes)
    .filter((row) =>
      ['attention', 'issues', 'waiting', 'stopped'].includes(rowGroup(row)),
    )
    .map((row) => ({ ...row, group: rowGroup(row) }))
    .sort((a, b) =>
      `${a.kind}:${a.project}#${a.number}:${a.id}`.localeCompare(
        `${b.kind}:${b.project}#${b.number}:${b.id}`,
      ),
    );

test('shared list scope cases preserve attention and in-progress rows', () => {
  if (process.argv.includes('--http-input')) {
    const responses = JSON.parse(readFileSync(0, 'utf8'));
    assert.equal(
      responses.length,
      cases.length + 1,
      'all shared HTTP cases must be checked',
    );
    const items = (
      body: { rows: { type: string; item: RequestRow }[] },
      type: string,
    ) => body.rows.filter((row) => row.type === type).map((row) => row.item);
    for (const c of responses) {
      assert.deepEqual(
        activeRows(
          items(c.active, 'issue'),
          items(c.active, 'draft'),
          items(c.active, 'closeRequest'),
        ),
        activeRows(
          items(c.full, 'issue'),
          items(c.full, 'draft'),
          items(c.full, 'closeRequest'),
        ),
        c.name,
      );
      if (c.name === 'work decisions') {
        const issues = requestRows(items(c.active, 'issue'), [], []);
        const groups = c.work.groups as Record<string, RequestRow[]>;
        const expected: Record<number, string> = {
          901: 'stopped',
          902: 'attention',
          903: 'waiting',
          905: 'stopped',
        };
        for (const row of issues) {
          const group = rowGroup(row);
          assert.equal(group, expected[row.number!]);
          const actual = groups[group].find(
            (item) => item.number === row.number,
          );
          assert.ok(
            actual,
            `HTTP work group missing ${row.number}: ${JSON.stringify(c.work)}`,
          );
          assert.equal(actual.error, row.error);
          assert.equal(actual.requiresUser, row.requiresUser);
          assert.equal(actual.nextAction, row.nextAction);
          assert.equal(actual.dependencyReason, row.dependencyReason);
          if (row.number === 905) assert.match(actual.error!, /未確認/);
        }
        for (const group of ['attention', 'running', 'waiting', 'stopped']) {
          assert.equal(c.work.counts[group], groups[group].length);
          assert.equal(
            groups[group].length,
            issues.filter(
              (row) =>
                (rowGroup(row) === 'issues' ? 'running' : rowGroup(row)) ===
                group,
            ).length,
          );
        }
      }
    }
    return;
  }
  for (const c of cases) {
    const full = c.wire as {
      issues: RequestRow[];
      drafts: RequestRow[];
      closes?: RequestRow[];
    };
    const issues = full.issues.filter((row) =>
      c.activeIssues.includes(row.number),
    );
    const drafts = full.drafts.filter((row) => c.activeDrafts.includes(row.id));
    assert.deepEqual(
      activeRows(issues, drafts, full.closes),
      activeRows(full.issues, full.drafts, full.closes),
      c.name,
    );
  }
});
