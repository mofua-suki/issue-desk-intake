package startup

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/instructionacceptance"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
)

func (a *App) referenceSettings() (map[string]any, bool) {
	if a.Fleet != nil {
		values, e := a.Values()
		if e != nil {
			return nil, false
		}
		return fleet.Object(values["assignments"]), true
	}
	if a.Config != nil {
		return fleet.Object(a.Config.Values["assignments"]), false
	}
	return nil, false
}
func (a *App) referenceAcceptance(ctx context.Context, repo string, issue map[string]any, settings map[string]any, configReadable bool) map[string]any {
	var job *jobs.Job
	readable := configReadable && a.Jobs != nil
	number, valid := positiveNumber(issue["number"])
	if !valid {
		readable = false
	}
	if readable {
		var e error
		job, e = a.Jobs.ByIssue(ctx, repo, number)
		if e != nil {
			readable = false
		}
	}
	attention := map[string]any(nil)
	if a.SQLite != nil {
		var exists int
		if e := a.SQLite.DB().QueryRowContext(ctx, referenceTableExistsSQL(), "attention_cases").Scan(&exists); e != nil {
			readable = false
		} else if exists > 0 {
			var raw string
			e := a.SQLite.DB().QueryRowContext(ctx, referenceAttentionSQL(), repo, number).Scan(&raw)
			if e != nil && e != sql.ErrNoRows {
				readable = false
			} else if e == nil && json.Unmarshal([]byte(raw), &attention) != nil {
				readable = false
			}
		}
	}
	return instructionacceptance.Check(fleet.Text(issue["state"]), fleet.Text(issue["body"]), job, attention, settings, readable)
}

// attachOpenRequestState loads one snapshot for the current listing. Individual
// reference reads and fallbacks retain their existing failure handling.
func (a *App) attachOpenRequestState(ctx context.Context, repo string, issues []map[string]any) {
	if len(issues) == 0 {
		return
	}
	settings, configReadable := a.referenceSettings()
	deskIssues := make([]numbers.Issue, 0, len(issues))
	jobIssues := make([]jobs.Issue, 0, len(issues))
	issueNumbers := make([]int64, 0, len(issues))
	for _, issue := range issues {
		deskIssues = append(deskIssues, numbers.Issue{Project: repo, Number: int64(fleet.Number(issue["number"]))})
		if number, valid := positiveNumber(issue["number"]); valid {
			jobIssues = append(jobIssues, jobs.Issue{Repo: repo, Number: number})
			issueNumbers = append(issueNumbers, number)
		}
	}
	deskNumbers, deskErr := a.Numbers.ForIssues(ctx, deskIssues)
	jobsByIssue := map[int64]*jobs.Job{}
	var jobsErr error
	jobsReadable := configReadable && a.Jobs != nil
	if jobsReadable {
		var all []*jobs.Job
		all, jobsErr = a.Jobs.ForIssues(ctx, jobIssues)
		for _, job := range all {
			if job.Repo == repo {
				jobsByIssue[job.Issue] = job
			}
		}
	}
	attention, attentionErr := a.referenceAttentionForIssues(ctx, repo, issueNumbers)
	for i, issue := range issues {
		issue["desk"] = nil
		if deskErr != nil {
			issue["desk"] = a.DeskNumber(repo, deskIssues[i].Number)
		} else if number, ok := deskNumbers[deskIssues[i]]; ok {
			issue["desk"] = number
		}
		number, valid := positiveNumber(issue["number"])
		if !valid || jobsErr != nil || attentionErr != nil {
			issue["acceptance"] = a.referenceAcceptance(ctx, repo, issue, settings, configReadable)
			continue
		}
		issue["acceptance"] = instructionacceptance.Check(fleet.Text(issue["state"]), fleet.Text(issue["body"]), jobsByIssue[number], attention[number], settings, jobsReadable)
	}
}

func (a *App) referenceAttentionForIssues(ctx context.Context, repo string, issues []int64) (map[int64]map[string]any, error) {
	result := map[int64]map[string]any{}
	if a.SQLite == nil || len(issues) == 0 {
		return result, nil
	}
	db := a.SQLite.DB()
	var exists int
	if err := db.QueryRowContext(ctx, referenceTableExistsSQL(), "attention_cases").Scan(&exists); err != nil || exists == 0 {
		return result, err
	}
	wanted, err := json.Marshal(issues)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, referenceAttentionForIssuesSQL(), repo, string(wanted))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var number int64
		var raw string
		if err := rows.Scan(&number, &raw); err != nil {
			return nil, err
		}
		var attention map[string]any
		if err := json.Unmarshal([]byte(raw), &attention); err != nil {
			return nil, err
		}
		result[number] = attention
	}
	return result, rows.Err()
}
