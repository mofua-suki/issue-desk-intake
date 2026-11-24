package statistics

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"time"
)

func (s *Service) Overview(ctx context.Context, values url.Values, repos []string) (any, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	rangeName, days, err := overviewRange(values)
	if err != nil {
		return nil, err
	}
	q, err := Parse(url.Values{"project": values["project"]}, now, repos)
	if err != nil {
		return nil, err
	}
	date := now.In(JST)
	to := time.Date(date.Year(), date.Month(), date.Day()+1, 0, 0, 0, 0, JST)
	p := Period{to.Add(-time.Duration(days) * 24 * time.Hour), to}
	records, err := s.read(ctx, q.Project, p.To)
	if err != nil {
		return nil, err
	}
	tally := newOverviewTally(p, records.Oldest)
	spans := newOverviewSpans(p)
	drafts, unavailable, err := s.overviewDrafts(ctx)
	if err != nil {
		return nil, err
	}
	excluded := tally.addJobs(records, p, consultationStarts(drafts), spans)
	rows, _, err := (&usage.Store{DB: s.DB}).Read(ctx, q.Project, p.From, p.To)
	if err != nil {
		return nil, err
	}
	rows = canonicalUsageRows(rows)
	spans.addReviews(rows)
	durations := spans.durations(excluded)
	if s.Drafts == nil {
		durations["consultToFiled"] = nil
	}
	tokens, models := s.overviewTokens(rows)
	tally.addTokens(rows, s.Weights)
	out := p.Dates()
	out["range"], out["project"] = rangeName, q.Project
	out["implementations"] = map[string]any{"byHost": overviewBreakdown(tally.byHost), "byProject": overviewBreakdown(tally.byProject)}
	out["implementationTiers"], out["durations"], out["daily"] = tally.tierList(), durations, tally.daily
	out["tokens"] = tokens
	out["models"] = models
	out["consultTiers"], out["roundTrips"] = consultOverview(drafts, rows, p, q.Project)
	out["unavailable"] = unavailable
	return out, nil
}

// overviewRange reads the range parameter as a number of days.
func overviewRange(values url.Values) (string, int, error) {
	rangeName := values.Get("range")
	if rangeName == "" {
		rangeName = "7d"
	}
	days := 7
	switch rangeName {
	case "today":
		days = 1
	case "7d":
	case "30d":
		days = 30
	default:
		return "", 0, contract.NewProblem(400, "rangeはtoday・7d・30dです。", "")
	}
	return rangeName, days, nil
}

// overviewDrafts reads the consultation records, or reports them unavailable
// when no reader is configured.
func (s *Service) overviewDrafts(ctx context.Context) ([]map[string]any, []any, error) {
	unavailable := []any{}
	if s.Drafts == nil {
		return nil, append(unavailable, "相談の記録を読めませんでした。"), nil
	}
	drafts, err := s.Drafts(ctx)
	if err != nil {
		return nil, nil, err
	}
	return drafts, unavailable, nil
}

// consultationStarts maps each filed issue to when its consultation began.
func consultationStarts(drafts []map[string]any) map[string]time.Time {
	consultations := map[string]time.Time{}
	for _, draft := range drafts {
		issues := fleet.List(draft["issues"])
		if len(issues) == 0 {
			issues = []any{draft["issue"]}
		}
		for _, raw := range issues {
			issue := fleet.Object(raw)
			repo := fleet.Text(issue["project"])
			if repo == "" {
				repo = fleet.Text(draft["project"])
			}
			at, _ := time.Parse(time.RFC3339Nano, fleet.Text(draft["createdAt"]))
			consultations[identity(repo, issue["number"])] = at
		}
	}
	return consultations
}

// overviewSpans collects the durations between the stages of the work that end within the period.
type overviewSpans struct {
	period  Period
	spans   map[string][]float64
	missing map[string]int
}

func newOverviewSpans(p Period) *overviewSpans {
	o := &overviewSpans{period: p, spans: map[string][]float64{}, missing: map[string]int{}}
	for _, name := range []string{"consultToFiled", "filedToLeased", "leasedToPr", "review", "prToMerged", "mergedToDeployed"} {
		o.spans[name] = []float64{}
	}
	return o
}

// add records the span from start to end, dated by end, start or anchor
// whichever is known first. A span without both ends is counted as missing.
func (o *overviewSpans) add(name string, start, end, anchor time.Time) {
	at := end
	if at.IsZero() {
		at = start
	}
	if at.IsZero() {
		at = anchor
	}
	if !o.period.Contains(at) {
		return
	}
	if start.IsZero() || end.IsZero() || end.Before(start) {
		o.missing[name]++
	} else {
		o.spans[name] = append(o.spans[name], float64(end.Sub(start).Milliseconds()))
	}
}

// addReviews records the independent review durations from the usage rows.
func (o *overviewSpans) addReviews(rows []map[string]any) {
	spans, missing := o.spans, o.missing
	// 独立レビューは使用量の1行を1回とし、その行の実行時間を所要時間にする。
	for _, row := range rows {
		if row["step"] != "独立レビュー" {
			continue
		}
		if ms, ok := row["elapsedMs"].(float64); ok && ms > 0 {
			spans["review"] = append(spans["review"], ms)
		} else {
			missing["review"]++
		}
	}
}

// durations summarizes each span's count, average, median and missing count.
func (o *overviewSpans) durations(excluded int) map[string]any {
	spans, missing := o.spans, o.missing
	durations := map[string]any{}
	for name, list := range spans {
		var average any
		if len(list) > 0 {
			sum := 0.0
			for _, n := range list {
				sum += n
			}
			average = sum / float64(len(list))
		}
		durations[name] = map[string]any{"count": len(list), "averageMs": average, "medianMs": percentile(list, 0.5), "missing": missing[name]}
	}
	fleet.Object(durations["mergedToDeployed"])["excluded"] = excluded
	return durations
}

// overviewTally counts implementation outcomes by host, project, tier and day.
type overviewTally struct {
	byHost, byProject map[string]map[string]any
	tiers             map[string]map[string]any
	daily             []any
	byDay             map[string]map[string]any
}

func newOverviewTally(p Period, oldest time.Time) *overviewTally {
	t := &overviewTally{byHost: map[string]map[string]any{}, byProject: map[string]map[string]any{}, tiers: map[string]map[string]any{}, daily: []any{}, byDay: map[string]map[string]any{}}
	for _, tier := range []string{"light", "standard", "heavy", "unknown"} {
		t.tiers[tier] = map[string]any{"tier": tier, "count": 0, "completed": 0, "failed": 0, "held": 0}
	}
	for at := p.From; at.Before(p.To); at = at.Add(24 * time.Hour) {
		var completed, failed any
		if !oldest.IsZero() && !at.Add(24*time.Hour).Before(oldest) {
			completed, failed = 0, 0
		}
		day := map[string]any{"date": at.In(JST).Format("2006-01-02"), "completed": completed, "failed": failed, "tokens": nil}
		t.byDay[day["date"].(string)] = day
		t.daily = append(t.daily, day)
	}
	return t
}

// addJobs counts each job's outcomes and records its stage durations. It
// returns how many merges in the period could not be measured to deployment.
func (t *overviewTally) addJobs(records records, p Period, consultations map[string]time.Time, spans *overviewSpans) int {
	excluded := 0
	eventsByJob := make(map[int64][]event)
	for _, e := range records.Events {
		eventsByJob[e.Job] = append(eventsByJob[e.Job], e)
	}
	deployRepos, deployedByKey := indexOverviewDeployments(records.Deployments)
	var mergedJobs []mergedJob
	selfRecordStart := map[string]time.Time{}
	for _, d := range records.Deployments {
		if fleet.SelfDeploymentRepository(d.Repo) && d.Status == "deployed" && d.Finished.Valid {
			if old, ok := selfRecordStart[d.Repo]; !ok || d.Finished.Time.Before(old) {
				selfRecordStart[d.Repo] = d.Finished.Time
			}
		}
	}
	for id, j := range records.Jobs {
		var leased, pr, merged time.Time
		tier := t.tiers[j.Tier]
		if tier == nil {
			tier = t.tiers["unknown"]
		}
		seen := map[string]bool{}
		started := false
		for _, e := range eventsByJob[id] {
			if e.Kind == "leased" && leased.IsZero() {
				leased = e.At
				started = p.Contains(e.At)
				if started {
					tier["count"] = tier["count"].(int) + 1
				}
			}
			if e.Kind == "pr_created" && pr.IsZero() {
				pr = e.At
			}
			if e.Kind == "state" && e.Data["to"] == "merged" && merged.IsZero() {
				merged = e.At
			}
			if e.Kind != "state" || !p.Contains(e.At) {
				continue
			}
			outcome := map[string]string{"merged": "completed", "failed": "failed", "held": "held"}[fleet.Text(e.Data["to"])]
			if outcome == "" || seen[outcome] {
				continue
			}
			seen[outcome] = true
			for _, bucket := range []struct {
				m     map[string]map[string]any
				k     string
				field string
			}{{t.byHost, e.Host, "host"}, {t.byProject, j.Repo, "project"}} {
				if bucket.m[bucket.k] == nil {
					bucket.m[bucket.k] = map[string]any{bucket.field: bucket.k, "completed": 0, "failed": 0, "held": 0}
				}
				b := bucket.m[bucket.k]
				b[outcome] = b[outcome].(int) + 1
			}
			if started {
				tier[outcome] = tier[outcome].(int) + 1
			}
			if outcome != "held" {
				day := t.byDay[e.At.In(JST).Format("2006-01-02")]
				if day != nil && day[outcome] != nil {
					day[outcome] = day[outcome].(int) + 1
				}
			}
		}
		if start, ok := consultations[identity(j.Repo, float64(j.Issue))]; ok {
			spans.add("consultToFiled", start, j.Filed, j.Filed)
		}
		spans.add("filedToLeased", j.Filed, leased, j.Filed)
		spans.add("leasedToPr", leased, pr, j.Filed)
		spans.add("prToMerged", pr, merged, j.Filed)
		// Self-updating repositories are sampled from when their records began; an
		// earlier merge may have been deployed unrecorded, so it cannot be measured.
		if start, ok := selfRecordStart[j.Repo]; !deployRepos[j.Repo] || ok && !merged.IsZero() && merged.Before(start) {
			if !merged.IsZero() && p.Contains(merged) {
				excluded++
			}
		} else {
			mergedJobs = append(mergedJobs, mergedJob{j, merged})
		}
	}
	deployedAfter := indexDeployedAfter(mergedJobs, deployedByKey)
	for _, m := range mergedJobs {
		spans.add("mergedToDeployed", m.at, deployedAfter(m), m.job.Filed)
	}
	return excluded
}

// addTokens adds each usage row's weighted tokens to its day.
func (t *overviewTally) addTokens(rows []map[string]any, weights map[string]any) {
	for _, row := range rows {
		at, _ := time.Parse(time.RFC3339Nano, fleet.Text(row["at"]))
		day := t.byDay[at.In(JST).Format("2006-01-02")]
		if n, ok := usage.Weighted(row, weights)["weighted"].(float64); ok && day != nil {
			day["tokens"] = fleet.Number(day["tokens"]) + n
		}
	}
}

func (t *overviewTally) tierList() []any {
	tierList := []any{}
	for _, name := range []string{"light", "standard", "heavy", "unknown"} {
		tierList = append(tierList, t.tiers[name])
	}
	return tierList
}

// overviewBreakdown lists the groups in name order.
func overviewBreakdown(groups map[string]map[string]any) []any {
	out := []any{}
	names := []string{}
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, groups[name])
	}
	return out
}

// overviewTokens summarizes the weighted tokens by step and the model share of each step.
func (s *Service) overviewTokens(rows []map[string]any) (map[string]any, []any) {
	total := usage.Totals(rows, s.Weights)
	steps := []any{}
	for _, raw := range s.axis(rows, "step") {
		g := fleet.Object(raw)
		var percent any
		if n, ok := g["weighted"].(float64); ok && fleet.Number(total["weighted"]) > 0 {
			percent = n / fleet.Number(total["weighted"]) * 100
		}
		steps = append(steps, map[string]any{"step": g["step"], "weighted": g["weighted"], "unknown": fleet.Object(g["unknown"])["weighted"], "count": g["count"], "percent": percent})
	}
	models := []any{}
	for _, list := range groupRows(rows, "step", "provider", "model", "effort") {
		base := list[0]
		count := 0
		for _, r := range rows {
			if r["step"] == base["step"] {
				count++
			}
		}
		models = append(models, map[string]any{"step": base["step"], "provider": base["provider"], "model": base["model"], "effort": base["effort"], "count": len(list), "percent": float64(len(list)) / float64(count) * 100})
	}
	return map[string]any{"weighted": total["weighted"], "unknown": fleet.Object(total["unknown"])["weighted"], "steps": steps}, models
}

var overviewCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

type mergedJob struct {
	job job
	at  time.Time
}

// indexDeployedAfter returns when a merge reached its deployment target. A
// deployed revision that is the merge commit of the same or a later merge on
// that repository contains the merge, because merges land on the default branch
// in order. A revision without a merge record (a direct PR, for example) cannot
// be ordered: when one was deployed before the found revision, the merge may have
// gone out with it, so the time is unknown instead of overstated.
func indexDeployedAfter(merges []mergedJob, deployedByKey map[[2]string]time.Time) func(mergedJob) time.Time {
	type candidate struct{ merged, deployed time.Time }
	firstMerge := map[[2]string]time.Time{}
	for _, m := range merges {
		commit := fleet.Text(m.job.Result["mergeCommit"])
		if !overviewCommit.MatchString(commit) {
			continue
		}
		key := [2]string{m.job.Repo, commit}
		if old, ok := firstMerge[key]; !m.at.IsZero() && (!ok || m.at.Before(old)) {
			firstMerge[key] = m.at
		}
	}
	byRepo := map[string][]candidate{}
	unordered := map[string][]time.Time{}
	for key, deployed := range deployedByKey {
		if merged, ok := firstMerge[key]; ok {
			byRepo[key[0]] = append(byRepo[key[0]], candidate{merged, deployed})
		} else {
			unordered[key[0]] = append(unordered[key[0]], deployed)
		}
	}
	for _, list := range byRepo {
		sort.Slice(list, func(a, b int) bool { return list[a].merged.Before(list[b].merged) })
		for i := len(list) - 2; i >= 0; i-- {
			if list[i+1].deployed.Before(list[i].deployed) {
				list[i].deployed = list[i+1].deployed
			}
		}
	}
	for _, list := range unordered {
		sort.Slice(list, func(a, b int) bool { return list[a].Before(list[b]) })
	}
	return func(m mergedJob) time.Time {
		if m.at.IsZero() {
			return time.Time{}
		}
		// Search from the first merge of the same commit so that a job sharing
		// the commit with an earlier job still finds that deployment.
		from := m.at
		if first, ok := firstMerge[[2]string{m.job.Repo, fleet.Text(m.job.Result["mergeCommit"])}]; ok {
			from = first
		}
		list := byRepo[m.job.Repo]
		i := sort.Search(len(list), func(i int) bool { return !list[i].merged.Before(from) })
		if i == len(list) {
			return time.Time{}
		}
		deployed := list[i].deployed
		others := unordered[m.job.Repo]
		if k := sort.Search(len(others), func(k int) bool { return others[k].After(from) }); k < len(others) && others[k].Before(deployed) {
			return time.Time{}
		}
		return deployed
	}
}
func identity(v ...any) string { raw, _ := json.Marshal(v); return string(raw) }
func indexOverviewDeployments(deployments []deployment) (map[string]bool, map[[2]string]time.Time) {
	repos := make(map[string]bool)
	byKey := make(map[[2]string]time.Time)
	for _, d := range deployments {
		repos[d.Repo] = true
		key := [2]string{d.Repo, d.Commit}
		if _, exists := byKey[key]; !exists && d.Status == "deployed" && d.Finished.Valid {
			byKey[key] = d.Finished.Time
		}
	}
	return repos, byKey
}

func consultOverview(drafts, rows []map[string]any, p Period, project string) (any, any) {
	if drafts == nil {
		return nil, nil
	}
	mine := []map[string]any{}
	for _, draft := range drafts {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(draft["createdAt"]))
		if e != nil || at.Before(p.From) || !at.Before(p.To) || draft["parentSplit"] != nil || draft["proposal"] != nil || project != "" && draft["project"] != project {
			continue
		}
		mine = append(mine, draft)
	}
	return consultTiers(mine, lastConsultationRows(rows)), consultRoundTrips(mine)
}

// lastConsultationRows finds each draft's last consultation usage row.
func lastConsultationRows(rows []map[string]any) map[any]map[string]any {
	lastByDraft := make(map[any]map[string]any)
	for _, r := range rows {
		if r["source"] == "intake" && usage.CanonicalStep(fleet.Text(r["step"])) == usage.ConsultationStep {
			id := r["draft"]
			if id != nil && !reflect.TypeOf(id).Comparable() {
				continue
			}
			if last := lastByDraft[id]; last == nil || fleet.Text(r["at"]) >= fleet.Text(last["at"]) {
				lastByDraft[id] = r
			}
		}
	}
	return lastByDraft
}

// consultTiers counts the consultations by the tier of their last call, and those whose tier was raised.
func consultTiers(drafts []map[string]any, lastByDraft map[any]map[string]any) []any {
	tiers := map[string]map[string]any{}
	for _, name := range []string{"light", "standard", "heavy", "記録なし"} {
		tiers[name] = map[string]any{"tier": name, "count": 0, "raised": 0}
	}
	for _, draft := range drafts {
		at, _ := time.Parse(time.RFC3339Nano, fleet.Text(draft["createdAt"]))
		tier := "記録なし"
		var lastRow map[string]any
		id := draft["id"]
		if id == nil || reflect.TypeOf(id).Comparable() {
			lastRow = lastByDraft[id]
		}
		if lastRow != nil {
			executed, e := time.Parse(time.RFC3339Nano, fleet.Text(lastRow["at"]))
			if e == nil && !executed.Before(at) && tiers[fleet.Text(lastRow["tier"])] != nil {
				tier = fleet.Text(lastRow["tier"])
			}
		}
		tiers[tier]["count"] = tiers[tier]["count"].(int) + 1
		rank := map[string]int{"light": 1, "standard": 2, "heavy": 3}
		for _, raw := range fleet.List(fleet.Object(draft["tier"])["history"]) {
			h := fleet.Object(raw)
			from, to := rank[fleet.Text(h["from"])], rank[fleet.Text(h["to"])]
			if from > 0 && to > from {
				tiers[tier]["raised"] = tiers[tier]["raised"].(int) + 1
				break
			}
		}
	}
	list := []any{}
	for _, name := range []string{"light", "standard", "heavy", "記録なし"} {
		list = append(list, tiers[name])
	}
	return list
}

// consultRoundTrips distributes the consultations by their number of AI answers.
func consultRoundTrips(drafts []map[string]any) map[string]any {
	distribution := []any{}
	for _, name := range []string{"1", "2", "3", "4", "5〜9", "10以上"} {
		distribution = append(distribution, map[string]any{"rounds": name, "count": 0})
	}
	count, total, missing, zero, many := 0, 0, 0, 0, 0
	for _, draft := range drafts {
		messages, ok := draft["messages"].([]any)
		if !ok {
			missing++
			continue
		}
		turns := 0
		for _, raw := range messages {
			if fleet.Object(raw)["role"] == "assistant" {
				turns++
			}
		}
		count++
		total += turns
		if turns == 0 {
			zero++
		} else {
			index := turns - 1
			if turns >= 10 {
				index = 5
			} else if turns >= 5 {
				index = 4
			}
			g := fleet.Object(distribution[index])
			g["count"] = g["count"].(int) + 1
		}
		if turns >= 5 {
			many++
		}
	}
	var average any
	if count > 0 {
		average = float64(total) / float64(count)
	}
	return map[string]any{"count": count, "missing": missing, "average": average, "distribution": distribution, "zero": zero, "many": many, "threshold": 5}
}
