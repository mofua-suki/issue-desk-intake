package statistics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"time"
)

func (s *Service) measurementLead(ctx context.Context, steps []*loggedStep, p Period, project string) ([]string, error) {
	first := firstLoggedSteps(steps, project)
	created, e := s.readFiledTimes(ctx, project)
	if e != nil {
		return nil, e
	}
	lines := []string{filingLeadLine(first, created, p), "外側の工程の合計が長いIssue:"}
	lines = append(lines, longestIssueLines(steps, first, created, p, project)...)
	return lines, nil
}

// firstLoggedSteps finds each issue's first logged step.
func firstLoggedSteps(steps []*loggedStep, project string) map[string]*loggedStep {
	first := map[string]*loggedStep{}
	for _, step := range steps {
		if project != "" && step.Repo != project {
			continue
		}
		if old := first[step.Key]; old == nil || step.Start.Before(old.Start) {
			first[step.Key] = step
		}
	}
	return first
}

// readFiledTimes reads when each issue was filed.
func (s *Service) readFiledTimes(ctx context.Context, project string) (map[string]time.Time, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT repo,issue,filed_at FROM jobs WHERE ($1=''OR repo=$1)", project)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	created := map[string]time.Time{}
	for rows.Next() {
		var repo string
		var issue int64
		var at time.Time
		if e = rows.Scan(&repo, &issue, &at); e != nil {
			return nil, e
		}
		created[fmt.Sprintf("%s#%d", repo, issue)] = at
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	return created, nil
}

// filingLeadLine describes the time from filing to the first step of the issues started in the period.
func filingLeadLine(first map[string]*loggedStep, created map[string]time.Time, p Period) string {
	values := []any{}
	missing := 0
	for key, step := range first {
		if step.Start.Before(p.From) || !step.Start.Before(p.To) {
			continue
		}
		at, ok := created[key]
		if !ok || !step.Logged || at.After(step.Start) {
			missing++
			continue
		}
		values = append(values, float64(step.Start.Sub(at))/float64(time.Minute))
	}
	d := AccountDistribution(values)
	return fmt.Sprintf("起票→着手（最初の工程開始）: %v件、平均%s分、中央値%s分（欠測%d件）", d["known"], referenceRatio(d["mean"]), referenceRatio(d["median"]), missing)
}

// longestIssueLines lists the ten issues whose outer steps took longest in the period.
func longestIssueLines(steps []*loggedStep, first map[string]*loggedStep, created map[string]time.Time, p Period, project string) []string {
	lines := []string{}
	totals := map[string]float64{}
	unknown := map[string]int{}
	for _, step := range steps {
		if step.Parent != nil || project != "" && step.Repo != project || stepPlaced(step).Before(p.From) || !stepPlaced(step).Before(p.To) {
			continue
		}
		if stepMeasured(step) {
			totals[step.Key] += float64(step.End.Sub(step.Start)) / float64(time.Minute)
		} else {
			unknown[step.Key]++
		}
	}
	keys := []string{}
	for key := range totals {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if totals[keys[i]] == totals[keys[j]] {
			return keys[i] < keys[j]
		}
		return totals[keys[i]] > totals[keys[j]]
	})
	if len(keys) > 10 {
		keys = keys[:10]
	}
	for _, key := range keys {
		var lead any
		if at, ok := created[key]; ok && first[key] != nil && first[key].Logged && !at.After(first[key].Start) {
			lead = float64(first[key].Start.Sub(at)) / float64(time.Minute)
		}
		lines = append(lines, fmt.Sprintf("- %s: 合計%s分、起票→着手%s分、欠測%d件", key, referenceRatio(totals[key]), referenceRatio(lead), unknown[key]))
	}
	return lines
}

// issueConsultationRows attributes pre-filing usage to its recorded first target.
// The lifetime cutoff remains each issue's merge time, rather than the report start.
func (s *Service) issueConsultationRows(ctx context.Context, tx *sql.Tx, data *ReworkRows, cutoff time.Time) error {
	if s.DraftIssue == nil {
		return nil
	}
	merged := map[string]time.Time{}
	for _, j := range data.Jobs {
		merged[accountKey(j)] = accountAt(j["merged_at"])
	}
	if len(merged) == 0 {
		return nil
	}
	stored, e := tx.QueryContext(ctx, "SELECT json FROM usage_rows WHERE (issue IS NULL OR issue=0)AND at<$1 ORDER BY at,key", cutoff)
	if e != nil {
		return e
	}
	defer stored.Close()
	for stored.Next() {
		var raw string
		if e = stored.Scan(&raw); e != nil {
			return e
		}
		var row AccountRecord
		if json.Unmarshal([]byte(raw), &row) != nil || row == nil || accountAt(row["at"]).IsZero() {
			data.Skipped++
			continue
		}
		if fleet.Number(row["issue"]) > 0 || fleet.Text(row["draft"]) == "" {
			continue
		}
		repo, issue := s.DraftIssue(fleet.Text(row["draft"]))
		copy := AccountRecord{}
		for key, v := range row {
			copy[key] = v
		}
		copy["repo"], copy["issue"] = repo, float64(issue)
		at := accountAt(copy["at"])
		if end, ok := merged[accountKey(copy)]; ok && !at.After(end) {
			data.Usage = append(data.Usage, copy)
		}
	}
	if e = stored.Err(); e != nil {
		return e
	}
	sort.SliceStable(data.Usage, func(i, j int) bool { return accountAt(data.Usage[i]["at"]).Before(accountAt(data.Usage[j]["at"])) })
	return nil
}
