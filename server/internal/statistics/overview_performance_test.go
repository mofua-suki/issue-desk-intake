package statistics

import (
	"context"
	"database/sql"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/usage"
	"net/url"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func (s *Service) legacyOverview(ctx context.Context, values url.Values, repos []string) (any, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
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
		return nil, contract.NewProblem(400, "rangeはtoday・7d・30dです。", "")
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
	within := func(t time.Time) bool { return !t.Before(p.From) && t.Before(p.To) }
	byHost, byProject := map[string]map[string]any{}, map[string]map[string]any{}
	tiers := map[string]map[string]any{}
	for _, tier := range []string{"light", "standard", "heavy", "unknown"} {
		tiers[tier] = map[string]any{"tier": tier, "count": 0, "completed": 0, "failed": 0, "held": 0}
	}
	daily := []any{}
	byDay := map[string]map[string]any{}
	for at := p.From; at.Before(p.To); at = at.Add(24 * time.Hour) {
		var completed, failed any
		if !records.Oldest.IsZero() && !at.Add(24*time.Hour).Before(records.Oldest) {
			completed, failed = 0, 0
		}
		day := map[string]any{"date": at.In(JST).Format("2006-01-02"), "completed": completed, "failed": failed, "tokens": nil}
		byDay[day["date"].(string)] = day
		daily = append(daily, day)
	}
	spans := map[string][]float64{}
	missing := map[string]int{}
	excluded := 0
	for _, name := range []string{"consultToFiled", "filedToLeased", "leasedToPr", "review", "prToMerged", "mergedToDeployed"} {
		spans[name] = []float64{}
	}
	span := func(name string, start, end, anchor time.Time) {
		at := end
		if at.IsZero() {
			at = start
		}
		if at.IsZero() {
			at = anchor
		}
		if !within(at) {
			return
		}
		if start.IsZero() || end.IsZero() || end.Before(start) {
			missing[name]++
		} else {
			spans[name] = append(spans[name], float64(end.Sub(start).Milliseconds()))
		}
	}
	var drafts []map[string]any
	unavailable := []any{}
	if s.Drafts != nil {
		drafts, err = s.Drafts(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		unavailable = append(unavailable, "相談の記録を読めませんでした。")
	}
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
	for id, j := range records.Jobs {
		var leased, pr, merged time.Time
		tier := tiers[j.Tier]
		if tier == nil {
			tier = tiers["unknown"]
		}
		seen := map[string]bool{}
		started := false
		for _, e := range records.Events {
			if e.Job != id {
				continue
			}
			if e.Kind == "leased" && leased.IsZero() {
				leased = e.At
				started = within(e.At)
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
			if e.Kind != "state" || !within(e.At) {
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
			}{{byHost, e.Host, "host"}, {byProject, j.Repo, "project"}} {
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
				day := byDay[e.At.In(JST).Format("2006-01-02")]
				if day != nil && day[outcome] != nil {
					day[outcome] = day[outcome].(int) + 1
				}
			}
		}
		if start, ok := consultations[identity(j.Repo, float64(j.Issue))]; ok {
			span("consultToFiled", start, j.Filed, j.Filed)
		}
		span("filedToLeased", j.Filed, leased, j.Filed)
		span("leasedToPr", leased, pr, j.Filed)
		span("prToMerged", pr, merged, j.Filed)
		openReview := map[string]time.Time{}
		reviews := 0
		for _, event := range records.Events {
			if event.Job != id || (event.Kind != "step_start" && event.Kind != "step_end") || event.Data["label"] != "レビュー" {
				continue
			}
			reviews++
			key := identity(event.Host, event.Data["epoch"])
			if event.Kind == "step_start" {
				if old, ok := openReview[key]; ok {
					span("review", old, time.Time{}, j.Filed)
				}
				openReview[key] = event.At
			} else {
				span("review", openReview[key], event.At, j.Filed)
				delete(openReview, key)
			}
		}
		for _, at := range openReview {
			span("review", at, time.Time{}, j.Filed)
		}
		if reviews == 0 {
			anchor := pr
			if anchor.IsZero() {
				anchor = leased
			}
			if anchor.IsZero() {
				anchor = j.Filed
			}
			span("review", time.Time{}, time.Time{}, anchor)
		}
		deployRepo := false
		var deployed time.Time
		for _, deployment := range records.Deployments {
			if deployment.Repo != j.Repo {
				continue
			}
			deployRepo = true
			if deployment.Commit == fleet.Text(j.Result["mergeCommit"]) && deployment.Status == "deployed" && deployment.Finished.Valid {
				deployed = deployment.Finished.Time
				break
			}
		}
		if !deployRepo {
			if !merged.IsZero() && within(merged) {
				excluded++
			}
		} else {
			span("mergedToDeployed", merged, deployed, j.Filed)
		}
	}
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
	if s.Drafts == nil {
		durations["consultToFiled"] = nil
	}
	rows, _, err := (&usage.Store{DB: s.DB}).Read(ctx, q.Project, p.From, p.To)
	if err != nil {
		return nil, err
	}
	rows = canonicalUsageRows(rows)
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
	for _, row := range rows {
		at, _ := time.Parse(time.RFC3339Nano, fleet.Text(row["at"]))
		day := byDay[at.In(JST).Format("2006-01-02")]
		if n, ok := usage.Weighted(row, s.Weights)["weighted"].(float64); ok && day != nil {
			day["tokens"] = fleet.Number(day["tokens"]) + n
		}
	}
	breakdown := func(groups map[string]map[string]any) []any {
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
	tierList := []any{}
	for _, name := range []string{"light", "standard", "heavy", "unknown"} {
		tierList = append(tierList, tiers[name])
	}
	out := p.Dates()
	out["range"], out["project"] = rangeName, q.Project
	out["implementations"] = map[string]any{"byHost": breakdown(byHost), "byProject": breakdown(byProject)}
	out["implementationTiers"], out["durations"], out["daily"] = tierList, durations, daily
	out["tokens"] = map[string]any{"weighted": total["weighted"], "unknown": fleet.Object(total["unknown"])["weighted"], "steps": steps}
	out["models"] = models
	out["consultTiers"], out["roundTrips"] = legacyConsultOverview(drafts, rows, p, q.Project)
	out["unavailable"] = unavailable
	return out, nil
}
func legacyConsultOverview(drafts, rows []map[string]any, p Period, project string) (any, any) {
	if drafts == nil {
		return nil, nil
	}
	tiers := map[string]map[string]any{}
	for _, name := range []string{"light", "standard", "heavy", "記録なし"} {
		tiers[name] = map[string]any{"tier": name, "count": 0, "raised": 0}
	}
	distribution := []any{}
	for _, name := range []string{"1", "2", "3", "4", "5〜9", "10以上"} {
		distribution = append(distribution, map[string]any{"rounds": name, "count": 0})
	}
	count, total, missing, zero, many := 0, 0, 0, 0, 0
	for _, draft := range drafts {
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(draft["createdAt"]))
		if e != nil || at.Before(p.From) || !at.Before(p.To) || draft["parentSplit"] != nil || draft["proposal"] != nil || project != "" && draft["project"] != project {
			continue
		}

		tier := "記録なし"
		var lastRow map[string]any
		for _, r := range rows {
			if r["source"] == "intake" && usage.CanonicalStep(fleet.Text(r["step"])) == usage.ConsultationStep && r["draft"] == draft["id"] && (lastRow == nil || fleet.Text(r["at"]) >= fleet.Text(lastRow["at"])) {
				lastRow = r
			}
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
	list := []any{}
	for _, name := range []string{"light", "standard", "heavy", "記録なし"} {
		list = append(list, tiers[name])
	}
	var average any
	if count > 0 {
		average = float64(total) / float64(count)
	}
	return list, map[string]any{"count": count, "missing": missing, "average": average, "distribution": distribution, "zero": zero, "many": many, "threshold": 5}
}

// withoutRedefinedDurations drops the spans whose sources changed after the
// legacy copy: the review now comes from usage rows and merge-to-deploy follows
// later merges, so only the other output is compared with the legacy path.
func withoutRedefinedDurations(values ...any) {
	for _, value := range values {
		durations := fleet.Object(fleet.Object(value)["durations"])
		delete(durations, "review")
		delete(durations, "mergedToDeployed")
	}
}

// TestOverviewIndexedMatchesLegacy exercises the real read and response path with ordered database rows.
func TestOverviewIndexedMatchesLegacy(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC)
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,status,filed_at,tier,result,view) SELECT CASE WHEN n%3=0 THEN 'o/other' ELSE 'o/r' END,n,'merged',$1::timestamptz - interval '2 hours','light',jsonb_build_object('mergeCommit',lpad(to_hex(n),40,'0')),'{}' FROM generate_series(1,24) n`, at)
	if err != nil {
		t.Fatal(err)
	}
	// The first valid deployment in the database's returned order must win;
	// invalid candidates still establish that the repository has history.
	_, err = db.ExecContext(ctx, `INSERT INTO app_deployments(repo,commit_sha,deploy_host,status,finished_at) VALUES ('o/other',lpad(to_hex(2),40,'0'),'nas','failed',$1)`, at)
	if err != nil {
		t.Fatal(err)
	}
	// First lease before the period, two same-time state changes, interrupted
	// reviews on different hosts, and events exactly at the excluded end.
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) SELECT id,'pc',v.kind,v.at,v.data::jsonb FROM jobs CROSS JOIN (VALUES ('leased',$1::timestamptz - interval '1 minute','{}'),('step_start',$1::timestamptz - interval '1 minute','{"label":"レビュー","epoch":9}'),('step_end',$1::timestamptz + interval '1 minute','{"label":"レビュー","epoch":9}'),('step_start',$1::timestamptz + interval '2 minutes','{"label":"レビュー","epoch":10}'),('step_start',$1::timestamptz + interval '3 minutes','{"label":"レビュー","epoch":10}'),('step_end',$1::timestamptz + interval '4 minutes','{"label":"レビュー","epoch":10}'),('state',$1::timestamptz + interval '7 minutes','{"to":"failed"}'),('state',$1::timestamptz + interval '7 minutes','{"to":"merged"}'),('state',$1::timestamptz + interval '24 hours','{"to":"held"}')) AS v(kind,at,data) WHERE issue=1`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) SELECT id,'pc',v.kind,$1::timestamptz + interval '8 minutes','{"label":"レビュー","epoch":11}'::jsonb FROM jobs CROSS JOIN (VALUES ('step_end'),('step_start')) AS v(kind) WHERE issue=1`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) SELECT id,'nas','leased',$1::timestamptz - interval '1 hour','{}'::jsonb FROM jobs UNION ALL SELECT id,'pc','leased',$1::timestamptz,'{}' FROM jobs UNION ALL SELECT id,'pc','step_start',$1::timestamptz,'{"label":"レビュー","epoch":1}' FROM jobs UNION ALL SELECT id,'pc','step_end',$1::timestamptz + interval '1 minute','{"label":"レビュー","epoch":1}' FROM jobs UNION ALL SELECT id,'nas','step_start',$1::timestamptz + interval '2 minutes','{"label":"レビュー","epoch":2}' FROM jobs UNION ALL SELECT id,'nas','step_end',$1::timestamptz + interval '3 minutes','{"label":"レビュー","epoch":2}' FROM jobs WHERE issue%2=0 UNION ALL SELECT id,'pc','pr_created',$1::timestamptz + interval '4 minutes','{}' FROM jobs UNION ALL SELECT id,'pc','state',$1::timestamptz + interval '5 minutes','{"to":"merged"}' FROM jobs`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO app_deployments(repo,commit_sha,deploy_host,status,finished_at) SELECT 'o/r',lpad(to_hex(n),40,'0'),'nas','deployed',$1::timestamptz + interval '6 minutes' FROM generate_series(1,24) n`, at)
	if err != nil {
		t.Fatal(err)
	}
	drafts := []map[string]any{}
	for i := 1; i <= 24; i++ {
		drafts = append(drafts, map[string]any{"id": fmt.Sprint(i), "project": "o/r", "createdAt": iso(at), "issue": map[string]any{"number": float64(i)}, "messages": []any{map[string]any{"role": "assistant"}}})
	}
	drafts = append(drafts, map[string]any{"id": []any{"bad"}, "createdAt": iso(at)}, map[string]any{"id": map[string]any{"bad": true}, "createdAt": iso(at)})
	for i := 1; i <= 12; i++ {
		for k := 0; k < 2; k++ {
			row := fmt.Sprintf(`{"at":%q,"source":"intake","repo":"o/r","draft":%q,"step":"受付の相談","tier":%q}`, iso(at.Add(time.Minute)), fmt.Sprint(i), []string{"light", "heavy"}[k])
			_, err = db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,host,source,repo,step,json,sender) VALUES($1,$2,'nas','intake','o/r','受付の相談',$3,'test')`, fmt.Sprintf("%d-%d", i, k), at.Add(time.Minute), row)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	s := &Service{DB: db, AppDB: pgtest.New(t), Now: func() time.Time { return at.Add(time.Hour) }, Drafts: func(context.Context) ([]map[string]any, error) { return drafts, nil }}
	for _, project := range []string{"", "o/r", "o/other"} {
		values := url.Values{"range": {"today"}}
		if project != "" {
			values.Set("project", project)
		}
		old, e := s.legacyOverview(ctx, values, []string{"o/r", "o/other"})
		if e != nil {
			t.Fatal(e)
		}
		got, e := s.Overview(ctx, values, []string{"o/r", "o/other"})
		if e != nil {
			t.Fatal(e)
		}
		withoutRedefinedDurations(old, got)
		if !reflect.DeepEqual(old, got) {
			t.Fatalf("project %s: legacy=%v indexed=%v", project, old, got)
		}
	}
	for _, size := range []int{24, 240} {
		if size == 240 {
			_, err = db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,status,filed_at,tier,result,view) SELECT 'o/r',n,'merged',$1::timestamptz,'light','{}','{}' FROM generate_series(25,240) n`, at)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) SELECT id,'pc','leased',$1::timestamptz,'{}'::jsonb FROM jobs WHERE issue>24 UNION ALL SELECT id,'pc','state',$1::timestamptz + interval '1 minute','{"to":"merged"}'::jsonb FROM jobs WHERE issue>24`, at)
			if err != nil {
				t.Fatal(err)
			}
			for i := 25; i <= 240; i++ {
				drafts = append(drafts, map[string]any{"id": fmt.Sprint(i), "project": "o/r", "createdAt": iso(at), "messages": []any{}})
			}
		}
		start := time.Now()
		rec, e := s.read(ctx, "", at.Add(24*time.Hour))
		if e != nil {
			t.Fatal(e)
		}
		t.Logf("jobs=%d events=%d deployments=%d read_ms=%.3f", size, len(rec.Events), len(rec.Deployments), float64(time.Since(start))/float64(time.Millisecond))
		var results [2]any
		for i, legacy := range []bool{true, false} {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start = time.Now()
			if legacy {
				results[i], e = s.legacyOverview(ctx, url.Values{"range": {"today"}}, []string{"o/r", "o/other"})
			} else {
				results[i], e = s.Overview(ctx, url.Values{"range": {"today"}}, []string{"o/r", "o/other"})
			}
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("jobs=%d drafts=%d legacy=%t api_ms=%.3f allocated_bytes=%d (includes DB reads)", size, len(drafts), legacy, float64(elapsed)/float64(time.Millisecond), after.TotalAlloc-before.TotalAlloc)
		}
		withoutRedefinedDurations(results[0], results[1])
		if !reflect.DeepEqual(results[0], results[1]) {
			t.Fatal("scaled overview differs")
		}
	}

}

func TestOverviewUncomparableDraftIDs(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p := Period{at, at.Add(24 * time.Hour)}
	drafts := []map[string]any{{"id": []any{"x"}, "createdAt": iso(at)}, {"id": map[string]any{"x": true}, "createdAt": iso(at)}}
	for _, rows := range [][]map[string]any{nil, {{"draft": "x", "source": "intake", "step": "受付の相談", "at": iso(at)}}} {
		a, b := legacyConsultOverview(drafts, rows, p, "")
		c, d := consultOverview(drafts, rows, p, "")
		if !reflect.DeepEqual([]any{a, b}, []any{c, d}) {
			t.Fatal("invalid draft IDs changed output")
		}
	}
}

func TestOverviewDeploymentInputOrder(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rows := []deployment{{Repo: "r", Commit: "c", Status: "failed", Finished: sql.NullTime{Time: at, Valid: true}}, {Repo: "r", Commit: "c", Status: "deployed"}, {Repo: "r", Commit: "c", Status: "deployed", Finished: sql.NullTime{Time: at.Add(time.Minute), Valid: true}}, {Repo: "r", Commit: "c", Status: "deployed", Finished: sql.NullTime{Time: at.Add(2 * time.Minute), Valid: true}}, {Repo: "empty", Commit: "x", Status: "failed"}}
	repos, byKey := indexOverviewDeployments(rows)
	if !repos["empty"] || repos["absent"] || !byKey[[2]string{"r", "c"}].Equal(at.Add(time.Minute)) {
		t.Fatal("deployment input order or repository presence changed")
	}
}

// TestOverviewConsultScaling compares actual allocation and time while asserting equivalence.
func TestOverviewConsultScaling(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p := Period{at, at.Add(24 * time.Hour)}
	for _, n := range []int{100, 1000} {
		drafts := make([]map[string]any, n)
		rows := make([]map[string]any, n)
		for i := 0; i < n; i++ {
			id := fmt.Sprint(i)
			drafts[i] = map[string]any{"id": id, "createdAt": iso(at), "messages": []any{}}
			rows[i] = map[string]any{"draft": id, "source": "intake", "step": "受付の相談", "at": iso(at), "tier": "light"}
		}
		a, b := legacyConsultOverview(drafts, rows, p, "")
		c, d := consultOverview(drafts, rows, p, "")
		if !reflect.DeepEqual([]any{a, b}, []any{c, d}) {
			t.Fatal("consult output differs")
		}
	}
}

func TestOverviewDeployedAfterUnorderedAndSharedCommits(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	merge := func(repo, commit string, minutes int) mergedJob {
		return mergedJob{job{Repo: repo, Result: map[string]any{"mergeCommit": commit}}, at.Add(time.Duration(minutes) * time.Minute)}
	}
	sha := func(c string) string { return strings.Repeat(c, 40) }
	a, b, x1, x2 := merge("r", sha("a"), 1), merge("r", sha("b"), 5), merge("s", sha("c"), 0), merge("s", sha("c"), 5)
	y1, y2 := merge("u", sha("e"), 0), merge("u", sha("e"), 5)
	deployedAfter := indexDeployedAfter([]mergedJob{a, b, x1, x2, y1, y2}, map[[2]string]time.Time{
		{"u", sha("f")}: at.Add(3 * time.Minute), // a direct PR between the two records of "e"
		{"u", sha("e")}: at.Add(20 * time.Minute),
		{"r", sha("d")}: at.Add(3 * time.Minute), // a direct PR without a merge record
		{"r", sha("b")}: at.Add(10 * time.Minute),
		{"s", sha("c")}: at.Add(20 * time.Minute),
	})
	// "a" may have gone out with the direct PR at 3 minutes, so it is unknown
	// rather than the 9 minutes until "b"; "b" merged after the direct PR.
	if got := deployedAfter(a); !got.IsZero() {
		t.Fatalf("a: %v", got)
	}
	if got := deployedAfter(b); !got.Equal(at.Add(10 * time.Minute)) {
		t.Fatalf("b: %v", got)
	}
	// Jobs sharing a commit agree: the direct PR after the first merge of "e"
	// makes both unknown.
	for _, m := range []mergedJob{y1, y2} {
		if got := deployedAfter(m); !got.IsZero() {
			t.Fatalf("%v: %v", m.at, got)
		}
	}
	// Both jobs with the same merge commit use that commit's deployment.
	for _, m := range []mergedJob{x1, x2} {
		if got := deployedAfter(m); !got.Equal(at.Add(20 * time.Minute)) {
			t.Fatalf("%v: %v", m.at, got)
		}
	}
}

func TestIntakeDeploymentsSkipSwitchedBackTakeovers(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	takeover := func(revision string, minutes int) serverTakeover {
		return serverTakeover{revision, at.Add(time.Duration(minutes) * time.Minute)}
	}
	// "n" was switched back to "o" twice and deployed on the third try; restarts
	// of the same revision and the later "p" keep their first good takeover.
	got := map[string]time.Time{}
	for _, d := range intakeDeployments([]serverTakeover{takeover("o", 0), takeover("n", 10), takeover("o", 11), takeover("n", 20), takeover("o", 21), takeover("n", 30), takeover("n", 35), takeover("p", 40)}) {
		got[d.Commit] = d.Finished.Time
	}
	want := map[string]time.Time{"o": at, "n": at.Add(30 * time.Minute), "p": at.Add(40 * time.Minute)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestOverviewDeployedAfterIgnoresMissingMergeCommits(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	early := mergedJob{job{Repo: "r", Result: map[string]any{}}, at}
	late := mergedJob{job{Repo: "r", Result: map[string]any{}}, at.Add(10 * time.Minute)}
	deployedAfter := indexDeployedAfter([]mergedJob{early, late}, map[[2]string]time.Time{{"r", ""}: at.Add(5 * time.Minute)})
	// A merge without its commit is not matched to another one's deployment.
	if got := deployedAfter(late); !got.IsZero() {
		t.Fatal(got)
	}
}
