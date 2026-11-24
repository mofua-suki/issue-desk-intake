package statistics

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/timedcache"
	"issue-desk-intake/server/internal/usage"
	"maps"
	"sort"
	"time"
)

type Service struct {
	Summary         bool
	TokenNames      func(context.Context) ([]string, error)
	draftOutcomes   map[string]map[string]any
	reviewCache     *timedcache.Cache[reviewJudgment]
	Assignments     *fleet.Assignments
	UsageReviewHost string
	RuntimeDir      string
	Risk            map[string]any
	AppDB           *sql.DB
	Now             func() time.Time
	Drafts          func(context.Context) ([]map[string]any, error)
	DB              *sql.DB
	Weights         map[string]any
	Desk            func(string, int64) any
	DraftIssue      func(string) (string, int64)
}

func (s *Service) Usage(ctx context.Context, q Query) (any, error) {
	from, to := q.Before.From, q.Before.To
	if q.After.From.Before(from) {
		from = q.After.From
	}
	if q.After.To.After(to) {
		to = q.After.To
	}
	store := usage.Store{DB: s.DB}
	rows, skipped, err := store.Read(ctx, "", from, to)
	if err != nil {
		return nil, err
	}
	history, _, err := store.Read(ctx, "", from.Add(-30*24*time.Hour), from)
	if err != nil {
		return nil, err
	}
	rows, status := usage.CorrectCosts(rows, history)
	service := *s
	if !s.Summary {
		service.draftOutcomes = s.draftUsageOutcomes(ctx)
	}
	report := map[string]any{"project": q.Project, "reason": nil, "skipped": skipped, "before": service.usagePeriod(rows, status, q.Before, q.Project), "after": service.usagePeriod(rows, status, q.After, q.Project)}
	hosts := []string{}
	if s.Assignments != nil {
		for host := range s.Assignments.Hosts {
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	senders := []string{}
	if s.TokenNames != nil {
		if values, e := s.TokenNames(ctx); e == nil {
			senders = values
		}
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	if coverage, e := store.Coverage(ctx, from, to, now, hosts, senders); e == nil {
		report["coverage"] = coverage
	} else {
		report["coverage"] = map[string]any{"failures": []string{"使用量の到着状況を読み取れませんでした"}}
	}
	return report, nil
}
func groupRows(rows []map[string]any, keys ...string) [][]map[string]any {
	groups := map[string][]map[string]any{}
	order := []string{}
	for _, r := range rows {
		values := []any{}
		for _, key := range keys {
			values = append(values, r[key])
		}
		raw, _ := json.Marshal(values)
		key := string(raw)
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], r)
	}
	out := [][]map[string]any{}
	for _, key := range order {
		out = append(out, groups[key])
	}
	return out
}
func (s *Service) axis(rows []map[string]any, keys ...string) []any {
	out := []any{}
	for _, group := range groupRows(rows, keys...) {
		t := usage.Totals(group, s.Weights)
		attachDetail(t, group)
		for _, key := range keys {
			t[key] = group[0][key]
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := fleet.Object(out[i]), fleet.Object(out[j])
		av, bv := fleet.Number(a["weighted"]), fleet.Number(b["weighted"])
		if av != bv {
			return av > bv
		}
		return a["count"].(int) > b["count"].(int)
	})
	return out
}
func (s *Service) usagePeriod(rows []map[string]any, status []string, p Period, project string) map[string]any {
	rows = canonicalUsageRows(rows)
	mine, costCorrection := selectUsagePeriod(rows, status, p, project)
	issues, omitted := s.usageIssues(mine)
	out := p.Dates()
	out["total"] = usage.Totals(mine, s.Weights)
	attachDetail(fleet.Object(out["total"]), mine)
	out["days"] = s.usageDays(mine, p)
	out["steps"] = s.axis(mine, "step")
	out["models"] = s.axis(mine, "provider", "model")
	out["byBilling"] = s.usageByBilling(mine)
	out["byStepProvider"] = s.axis(mine, "step", "provider")
	out["hosts"] = s.axis(mine, "host")
	out["kinds"] = s.axis(mine, "kind")
	out["repos"] = s.usageRepos(mine)
	out["issues"], out["omittedIssues"] = issues, omitted
	out["costCorrection"] = costCorrection
	for k, v := range usageDetails(mine, periodRows(rows, p)) {
		out[k] = v
	}
	if !s.Summary {
		for k, v := range s.usageExtras(mine) {
			out[k] = v
		}
	}
	out["intakeCache"] = intakeCacheSummary(mine, p)
	out["intakeTiming"] = intakeTiming(mine)
	return out
}

// selectUsagePeriod selects the period's rows of the project and counts how their costs were corrected.
func selectUsagePeriod(rows []map[string]any, status []string, p Period, project string) ([]map[string]any, map[string]any) {
	mine := []map[string]any{}
	corrected, filled, uncorrectable := 0, 0, 0
	var firstUnknown, lastUnknown time.Time
	for i, r := range rows {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
		if e != nil || at.Before(p.From) || !at.Before(p.To) || project != "" && r["repo"] != project {
			continue
		}
		mine = append(mine, r)
		switch status[i] {
		case "corrected":
			corrected++
		case "filled":
			filled++
		case "uncorrectable":
			uncorrectable++
			if firstUnknown.IsZero() || at.Before(firstUnknown) {
				firstUnknown = at
			}
			if lastUnknown.IsZero() || at.After(lastUnknown) {
				lastUnknown = at
			}
		}
	}
	var fromUnknown, toUnknown any
	if !firstUnknown.IsZero() {
		fromUnknown = firstUnknown.In(JST).Format("2006-01-02")
		toUnknown = lastUnknown.In(JST).Format("2006-01-02")
	}
	return mine, map[string]any{"corrected": corrected, "filled": filled, "uncorrectable": uncorrectable, "from": fromUnknown, "to": toUnknown}
}

// usageDays totals the rows of each day.
func (s *Service) usageDays(mine []map[string]any, p Period) []any {
	days := []any{}
	day := p.From.In(JST)
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, JST)
	for at := start; at.Before(p.To); at = at.Add(24 * time.Hour) {
		list := []map[string]any{}
		for _, r := range mine {
			t, _ := time.Parse(time.RFC3339Nano, fleet.Text(r["at"]))
			if t.In(JST).Format("2006-01-02") == at.Format("2006-01-02") {
				list = append(list, r)
			}
		}
		totals := usage.Totals(list, s.Weights)
		attachDetail(totals, list)
		totals["date"] = at.Format("2006-01-02")
		days = append(days, totals)
	}

	return days
}

// usageIssues totals the rows by issue, resolving a consultation to the issue it
// filed, and keeps the ten largest.
func (s *Service) usageIssues(mine []map[string]any) ([]any, int) {
	issues := []any{}
	issueGroups := map[string][]map[string]any{}
	issueFields := map[string]map[string]any{}
	resolved := map[string]struct {
		Repo   string
		Number int64
	}{}
	for _, r := range mine {
		repo, number := fleet.Text(r["repo"]), int64(fleet.Number(r["issue"]))
		if number == 0 && s.DraftIssue != nil {
			draft := fleet.Text(r["draft"])
			value, exists := resolved[draft]
			if !exists {
				value.Repo, value.Number = s.DraftIssue(draft)
				resolved[draft] = value
			}
			repo, number = value.Repo, value.Number
		}
		fields := map[string]any{"repo": nil, "number": nil, "desk": nil, "bucket": nil}
		key := ""
		if repo != "" && number > 0 {
			fields["repo"], fields["number"] = repo, number
			if s.Desk != nil {
				fields["desk"] = s.Desk(repo, number)
			}
			key = identity(repo, number)
		} else {
			bucket := "none"
			if r["source"] == "intake" {
				bucket = "intake"
			}
			fields["bucket"] = bucket
			key = ":" + bucket
		}
		issueGroups[key] = append(issueGroups[key], r)
		issueFields[key] = fields
	}
	for key, group := range issueGroups {
		t := usage.Totals(group, s.Weights)
		for k, v := range issueFields[key] {
			t[k] = v
		}
		issues = append(issues, t)
	}
	sort.SliceStable(issues, func(i, j int) bool {
		return fleet.Number(fleet.Object(issues[i])["weighted"]) > fleet.Number(fleet.Object(issues[j])["weighted"])
	})
	omitted := 0
	if len(issues) > 10 {
		omitted = len(issues) - 10
		issues = issues[:10]
	}
	return issues, omitted
}

// usageByBilling totals the subscription and API credit rows.
func (s *Service) usageByBilling(mine []map[string]any) []any {
	billingRows := map[string][]map[string]any{}
	for _, row := range mine {
		billing := "subscription"
		if row["billing"] == "api_credit" {
			billing = "api_credit"
		}
		billingRows[billing] = append(billingRows[billing], row)
	}
	byBilling := []any{}
	for _, billing := range []string{"subscription", "api_credit"} {
		if group := billingRows[billing]; len(group) > 0 {
			totals := usage.Totals(group, s.Weights)
			totals["billing"] = billing
			byBilling = append(byBilling, totals)
		}
	}
	return byBilling
}

// usageRepos totals the rows by repository, largest first.
func (s *Service) usageRepos(mine []map[string]any) []any {
	repoRows := []any{}
	for _, group := range groupRows(mine, "repo") {
		t := usage.Totals(group, s.Weights)
		t["repo"], t["steps"] = group[0]["repo"], s.axis(group, "step")
		repoRows = append(repoRows, t)
	}
	sort.SliceStable(repoRows, func(i, j int) bool {
		return fleet.Number(fleet.Object(repoRows[i])["weighted"]) > fleet.Number(fleet.Object(repoRows[j])["weighted"])
	})
	return repoRows
}

// correctedUsageCosts corrects the recorded costs and clears the ones that cannot be corrected.
func correctedUsageCosts(all []map[string]any) []map[string]any {
	all, statuses := usage.CorrectCosts(all, []map[string]any{})
	for i, status := range statuses {
		if status == "uncorrectable" {
			all[i] = maps.Clone(all[i])
			all[i]["costUsd"] = nil
		}
	}
	return all
}
