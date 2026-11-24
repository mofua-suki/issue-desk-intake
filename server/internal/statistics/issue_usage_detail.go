package statistics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
)

// IssueUsageOptions controls the optional detail without changing the legacy report.
type IssueUsageOptions struct{ Detail bool }

func readIssueUsageDetail(ctx context.Context, tx *sql.Tx, project string, p Period, merged []AccountRecord, weights AccountRecord) (AccountRecord, error) {
	selected := append([]AccountRecord{}, merged...)
	unmerged, err := readJSONRows(ctx, tx, `SELECT row_to_json(j) FROM jobs j WHERE j.status <> 'merged' AND ($3='' OR j.repo=$3) AND EXISTS (SELECT 1 FROM usage_rows u WHERE u.repo=j.repo AND u.issue=j.issue AND u.at >= $1 AND u.at < $2) ORDER BY j.repo,j.issue`, p.From, p.To, project)
	if err != nil {
		return nil, err
	}
	selected = append(selected, unmerged...)
	if len(selected) == 0 {
		return AccountRecord{"description": detailDescription, "issues": []any{}}, nil
	}
	repos, issues, ids := make([]string, 0, len(selected)), make([]int64, 0, len(selected)), make([]int64, 0, len(selected))
	for _, job := range selected {
		repos = append(repos, fleet.Text(job["repo"]))
		issues = append(issues, int64(fleet.Number(job["issue"])))
		ids = append(ids, int64(fleet.Number(job["id"])))
	}
	type issueKey struct {
		repo  string
		issue int64
	}
	records := map[issueKey][]AccountRecord{}
	skipped := map[issueKey]int{}
	rows, err := tx.QueryContext(ctx, `SELECT u.repo,u.issue,u.json FROM usage_rows u JOIN (SELECT DISTINCT * FROM unnest($1::text[], $2::bigint[]) AS t(repo,issue)) targets ON u.repo=targets.repo AND u.issue=targets.issue ORDER BY u.at,u.key`, repos, issues)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key issueKey
		var raw string
		if err = rows.Scan(&key.repo, &key.issue, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var row AccountRecord
		if json.Unmarshal([]byte(raw), &row) != nil || row == nil {
			skipped[key]++
			continue
		}
		records[key] = append(records[key], row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	events, err := readJSONRows(ctx, tx, `SELECT row_to_json(e) FROM (SELECT DISTINCT ON (job_id) * FROM job_events WHERE job_id=ANY($1::bigint[]) AND kind='implementation_usage_limit' ORDER BY job_id,at DESC,seq DESC) e`, ids)
	if err != nil {
		return nil, err
	}
	eventsByJob := map[int64][]AccountRecord{}
	for _, event := range events {
		id := int64(fleet.Number(event["job_id"]))
		eventsByJob[id] = append(eventsByJob[id], event)
	}
	details := make([]any, 0, len(selected))
	for _, job := range selected {
		key := issueKey{fleet.Text(job["repo"]), int64(fleet.Number(job["issue"]))}
		details = append(details, buildIssueUsageDetail(job, records[key], skipped[key], eventsByJob[int64(fleet.Number(job["id"]))], weights))
	}
	return AccountRecord{"description": detailDescription, "issues": details}, nil
}

const detailDescription = "limitTally と内訳は保存済み usage_rows の同じ repo・issue の全行を対象とし、下書き経由の相談行を含みません。送信前の手元の行も含める上限判定とは異なる場合があります。内訳の generation=null は不明を意味します。"

func buildIssueUsageDetail(job AccountRecord, rows []AccountRecord, skipped int, events []AccountRecord, weights AccountRecord) AccountRecord {
	repo, issue := fleet.Text(job["repo"]), int64(fleet.Number(job["issue"]))
	tally := jobs.TallyUsage(rows, skipped, &jobs.Job{Repo: repo, Issue: issue}, weights)
	groups := map[string][]AccountRecord{}
	generations := map[string]any{}
	steps := map[string]string{}
	for _, row := range rows {
		var generation any
		if accountWhole(row["generation"]) {
			generation = row["generation"]
		}
		rowTally := jobs.TallyUsage([]AccountRecord{row}, 0, &jobs.Job{Repo: repo, Issue: issue}, weights)
		if len(rowTally.Steps) == 0 {
			continue
		}
		step := rowTally.Steps[0].Step
		key := fmt.Sprintf("%v\x00%s", generation, step)
		groups[key] = append(groups[key], row)
		generations[key] = generation
		steps[key] = step
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	breakdown := make([]any, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		total := accountTotals(group, weights)
		missing := jobs.TallyUsage(group, 0, &jobs.Job{Repo: repo, Issue: issue}, weights).MissingRows
		breakdown = append(breakdown, AccountRecord{"generation": generations[key], "step": steps[key], "usage": total, "missingRows": missing})
	}
	var limit any
	if len(events) > 0 {
		limit = AccountRecord{"at": events[0]["at"], "reason": fleet.Object(events[0]["data"])["reason"]}
	}
	finite := func(v float64) any {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return nil
		}
		return v
	}
	return AccountRecord{"repo": repo, "issue": issue, "merged": job["status"] == "merged", "status": job["status"], "scope_source": job["scope_source"], "usageLimit": limit, "breakdown": breakdown, "limitTally": AccountRecord{"measured": finite(tally.Measured), "estimated": finite(tally.Estimated), "missingRows": tally.MissingRows, "resolved": len(tally.UnresolvedSteps) == 0 && !tally.Unreadable && !math.IsInf(tally.Total, 0), "unreadable": tally.Unreadable, "unresolvedSteps": tally.UnresolvedSteps}}
}
