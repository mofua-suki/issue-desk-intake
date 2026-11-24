package statistics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"sort"
)

// ReadIssueUsageReportWith uses a repeatable, read-only PostgreSQL snapshot and the
// caller's read-only application PostgreSQL transaction. Draft contents never enter the report.
// The options optionally include issue-level detail.
func ReadIssueUsageReportWith(ctx context.Context, db, local *sql.DB, project string, p Period, weights AccountRecord, options IssueUsageOptions) (AccountRecord, error) {
	if err := validateBreakdownWeights(weights); err != nil {
		return nil, err
	}
	ltx, err := local.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer ltx.Rollback()
	targets, err := readDraftTargets(ctx, ltx)
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	data, err := ReadReworkRows(ctx, tx, project, p)
	if err != nil {
		return nil, err
	}
	split, err := attributeDraftUsage(ctx, tx, &data, targets)
	if err != nil {
		return nil, err
	}
	var detail any
	if options.Detail {
		detail, err = readIssueUsageDetail(ctx, tx, project, p, data.Jobs, weights)
		if err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if err = ltx.Commit(); err != nil {
		return nil, err
	}
	sort.SliceStable(data.Usage, func(i, j int) bool { return accountAt(data.Usage[i]["at"]).Before(accountAt(data.Usage[j]["at"])) })
	report := BuildIssueUsageReport(data, weights, AccountRecord{"fromDate": p.From.In(JST).Format("2006-01-02"), "toDate": p.To.Add(-1).In(JST).Format("2006-01-02"), "from": ISO(p.From), "to": ISO(p.To)}, AccountRecord{"draftPolicy": "first-filed-issue", "splitRows": split})
	if options.Detail {
		report["detail"] = detail
	}
	return report, nil
}

// draftTarget is the first issue a consultation filed, and whether it filed several.
type draftTarget struct {
	repo  string
	issue int64
	split bool
}

// readDraftTargets reads the issue each consultation's usage is attributed to.
func readDraftTargets(ctx context.Context, ltx *sql.Tx) (map[string]draftTarget, error) {
	drafts, err := ltx.QueryContext(ctx, bindSQL(`SELECT id,json FROM drafts`))
	if err != nil {
		return nil, err
	}
	targets := map[string]draftTarget{}
	for drafts.Next() {
		var id, raw string
		if err = drafts.Scan(&id, &raw); err != nil {
			drafts.Close()
			return nil, err
		}
		var d AccountRecord
		if json.Unmarshal([]byte(raw), &d) != nil || d == nil {
			drafts.Close()
			return nil, errors.New("下書きの使用量帰属を確認できません")
		}
		issues := filed(d)
		if len(issues) > 0 {
			i := issues[0]
			targets[id] = draftTarget{fleet.Text(i["project"]), int64(fleet.Number(i["number"])), len(issues) > 1}
		}
	}
	err = drafts.Err()
	drafts.Close()
	if err != nil {
		return nil, err
	}
	return targets, nil
}

// attributeDraftUsage adds the consultation usage recorded before filing to the
// issue it filed, up to the issue's merge. It returns how many rows belong to
// consultations that filed several issues.
func attributeDraftUsage(ctx context.Context, tx *sql.Tx, data *ReworkRows, targets map[string]draftTarget) (int, error) {
	jobs := map[string]AccountRecord{}
	repos := []string{}
	seen := map[string]bool{}
	for _, j := range data.Jobs {
		jobs[accountKey(j)] = j
		repo := fleet.Text(j["repo"])
		if !seen[repo] {
			seen[repo] = true
			repos = append(repos, repo)
		}
	}
	split := 0
	if len(repos) > 0 {
		var latest any
		for _, j := range data.Jobs {
			if latest == nil || accountAt(j["merged_at"]).After(accountAt(latest)) {
				latest = j["merged_at"]
			}
		}
		rows, e := tx.QueryContext(ctx, `SELECT key,json FROM usage_rows WHERE issue IS NULL AND source='intake' AND repo=ANY($1::text[]) AND at<=$2 ORDER BY at,key`, repos, accountAt(latest))
		if e != nil {
			return 0, e
		}
		for rows.Next() {
			var key, raw string
			if e = rows.Scan(&key, &raw); e != nil {
				rows.Close()
				return 0, e
			}
			var row AccountRecord
			if json.Unmarshal([]byte(raw), &row) != nil || row == nil {
				data.Skipped++
				continue
			}
			target, ok := targets[fleet.Text(row["draft"])]
			if !ok {
				continue
			}
			row["repo"] = target.repo
			row["issue"] = float64(target.issue)
			job, ok := jobs[accountKey(row)]
			if !ok {
				continue
			}
			if accountAt(row["at"]).IsZero() {
				data.Skipped++
				continue
			}
			if accountAt(row["at"]).After(accountAt(job["merged_at"])) {
				continue
			}
			if target.split {
				split++
			}
			data.Usage = append(data.Usage, row)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return 0, e
		}
	}
	return split, nil
}
