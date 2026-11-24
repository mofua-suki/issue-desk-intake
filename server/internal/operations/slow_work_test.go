package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"sort"
	"testing"
	"time"
)

func seedSlowJob(t *testing.T, s *Service, issue int, tier, status string, start, end time.Time) int64 {
	t.Helper()
	var id int64
	err := s.PostgreSQL.QueryRow(`INSERT INTO jobs(repo,issue,title,tier,status,view) VALUES('owner/repo',$1,'work',$2,$3,jsonb_build_object('tierHistory',jsonb_build_array(jsonb_build_object('from',$2::text,'to',$2::text)))) RETURNING id`, issue, tier, status).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostgreSQL.Exec(`INSERT INTO job_events(job_id,kind,at) VALUES($1,'leased',$2)`, id, start); err != nil {
		t.Fatal(err)
	}
	if !end.IsZero() {
		if _, err = s.PostgreSQL.Exec(`INSERT INTO job_events(job_id,kind,at) VALUES($1,'pr_created',$2)`, id, end); err != nil {
			t.Fatal(err)
		}
	}
	return id
}
func seedSlowRequest(t *testing.T, s *Service, id, kind, status, result string, start, end time.Time) {
	t.Helper()
	table := "requests"
	if kind == "full_test" {
		table = "main_test_requests"
	}
	if kind == "pr_close" {
		table = "pr_close_requests"
	}
	var finished any
	if !end.IsZero() {
		finished = end
	}
	_, err := s.PostgreSQL.Exec(`INSERT INTO `+table+`(id,kind,repo,payload,sender,status,claimed_by,claimed_at,result,finished_at) VALUES($1,$2,'owner/repo','{}','sender',$3,'pc',$4,jsonb_build_object('status',$5::text),$6)`, id, kind, status, start, result, finished)
	if err != nil {
		t.Fatal(err)
	}
}
func TestSlowWorkAverages(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		seedSlowJob(t, s, i+1, "light", "merged", now.Add(-2*time.Hour), now.Add(-time.Hour))
		for _, k := range []string{"pr_create", "merge_manual", "full_test", "pr_close"} {
			if i == 9 && k == "pr_create" {
				continue
			}
			result := map[string]string{"pr_create": "created", "merge_manual": "merged", "full_test": "passed", "pr_close": "closed"}[k]
			seedSlowRequest(t, s, fmt.Sprintf("%s-%d", k, i), k, "done", result, now.Add(-2*time.Hour), now.Add(-time.Hour))
		}
	}
	for i := 0; i < 9; i++ {
		seedSlowJob(t, s, 100+i, "standard", "merged", now.Add(-2*time.Hour), now.Add(-time.Hour))
	}
	for i, result := range []string{"cancelled", "refused", "failed", "interrupted", "skipped"} {
		seedSlowRequest(t, s, fmt.Sprintf("bad-%d", i), "pr_close", "done", result, now.Add(-10*time.Hour), now.Add(-time.Hour))
	}
	seedSlowRequest(t, s, "bad-time", "pr_close", "done", "closed", now.Add(-time.Hour), now.Add(-2*time.Hour))
	seedSlowRequest(t, s, "old", "pr_close", "done", "closed", now.Add(-40*24*time.Hour), now.Add(-31*24*time.Hour))
	for i := 0; i < 10; i++ {
		id := seedSlowJob(t, s, 400+i, "heavy", "merged", now.Add(-2*time.Hour), now.Add(-time.Hour))
		if _, err := s.PostgreSQL.Exec(`UPDATE jobs SET view='{}' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}

	seedSlowJob(t, s, 200, "heavy", "failed", now.Add(-3*time.Hour), now.Add(-time.Hour))
	rows, err := s.slowWorkRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	avgs := slowWorkAverages(rows, now)
	if _, ok := avgs["request-pr_create"]; ok {
		t.Fatal("nine request samples accepted")
	}
	for _, group := range []string{"implementation-light", "request-merge_manual", "request-full_test", "request-pr_close"} {
		if avgs[group] != 3600 {
			t.Fatalf("%s: %v", group, avgs)
		}
	}
	if _, ok := avgs["implementation-standard"]; ok {
		t.Fatal("nine samples accepted")
	}
	if _, ok := avgs["implementation-heavy"]; ok {
		t.Fatal("failed sample accepted")
	}
	for i := 0; i < 10; i++ {
		seedSlowJob(t, s, 500+i, "heavy", "merged", now.Add(-2*time.Hour), now.Add(-time.Hour))
	}
	seedSlowRequest(t, s, "pr_create-9", "pr_create", "done", "created", now.Add(-2*time.Hour), now.Add(-time.Hour))
	job := seedSlowJob(t, s, 300, "standard", "merged", now.Add(-2*time.Hour), now.Add(-time.Hour))
	if _, err = s.PostgreSQL.Exec(`UPDATE jobs SET view='{}' WHERE id=$1`, job); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostgreSQL.Exec(`INSERT INTO usage_rows(key,at,host,source,repo,issue,step,json,sender) VALUES('tier-row',$1,'pc','worker','owner/repo',300,'実装','{"generation":1,"tier":"standard"}','pc')`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostgreSQL.Exec(`INSERT INTO pr_close_requests(id,kind,repo,payload,sender,status,claimed_at,finished_at,result) VALUES('bad-infinite','pr_close','owner/repo','{}','pc','done','infinity',$1,'{"status":"closed"}'),('bad-null','pr_close','owner/repo','{}','pc','done',$1,$1,NULL)`, now); err != nil {
		t.Fatal(err)
	}
	rows, err = s.slowWorkRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	avgs = slowWorkAverages(rows, now)
	for _, group := range []string{"implementation-standard", "implementation-heavy", "request-pr_create", "request-pr_close"} {
		if avgs[group] != 3600 {
			t.Fatalf("%s: %v", group, avgs)
		}
	}
}
func TestSlowWorkDetection(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		seedSlowRequest(t, s, fmt.Sprintf("sample-%d", i), "full_test", "done", "passed", now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	}
	seedSlowRequest(t, s, "active", "full_test", "claimed", "", now.Add(-2*time.Hour+time.Minute), time.Time{})
	monitor := IncidentMonitor{Service: s, Sources: s.SlowWorkSources}
	read := func() []any {
		v, e := s.OperationalSetting(ctx, "incident-deadline")
		if e != nil {
			t.Fatal(e)
		}
		return fleet.List(v)
	}
	if err := monitor.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(read()) != 0 {
		t.Fatal("detected before threshold")
	}
	now = now.Add(time.Minute)
	if err := monitor.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rows := read()
	if len(rows) != 1 || fleet.Object(rows[0])["kind"] != "slow" {
		t.Fatal(rows)
	}
	now = now.Add(time.Minute)
	if err := monitor.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(read()) != 1 {
		t.Fatal("duplicate")
	}
	id := fleet.Text(fleet.Object(rows[0])["id"])
	if err := s.ResolveSlowWorkNoProblem(ctx, id); err != nil {
		t.Fatal(err)
	}
	monitor = IncidentMonitor{Service: s, Sources: s.SlowWorkSources}
	now = now.Add(time.Minute)
	if err := monitor.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(read()) != 1 {
		t.Fatal("judged work redetected")
	}
	if _, err := s.PostgreSQL.Exec(`UPDATE main_test_requests SET status='done',result='{"status":"failed"}',finished_at=$1 WHERE id='active'`, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := monitor.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	row := fleet.Object(read()[0])
	if row["endedAt"] == nil || row["state"] != "resolved" {
		t.Fatal(row)
	}
	for _, terminal := range []string{"completed", "failed", "closed"} {
		t.Run(terminal, func(t *testing.T) {
			s := fixture(t)
			s.PostgreSQL = postgres(t)
			at := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
			s.Now = func() time.Time { return at }
			for i := 0; i < 10; i++ {
				seedSlowJob(t, s, i+1, "light", "merged", at.Add(-3*time.Hour), at.Add(-2*time.Hour))
			}
			id := seedSlowJob(t, s, 50, "light", "running", at.Add(-2*time.Hour), time.Time{})
			m := IncidentMonitor{Service: s, Sources: s.SlowWorkSources, Actions: &IncidentActions{Service: s, Judge: func(context.Context, []any) (map[string]any, error) {
				t.Fatal("generic incident action invoked for slow work")
				return nil, nil
			}}}
			if err := m.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			alerts, err := s.incidentAlerts(ctx)
			if err != nil || len(alerts) != 1 || fleet.Object(alerts[0])["kindName"] != "作業の遅延" {
				t.Fatalf("slow alert: %v %v", alerts, err)
			}
			at = at.Add(16 * time.Minute)
			if err := m.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if terminal == "completed" {
				if _, err := s.PostgreSQL.Exec(`INSERT INTO job_events(job_id,kind,at) VALUES($1,'pr_created',$2)`, id, at); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.PostgreSQL.Exec(`UPDATE jobs SET status=$1 WHERE id=$2`, terminal, id); err != nil {
					t.Fatal(err)
				}
			}
			at = at.Add(time.Minute)
			if err := m.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			alerts, err = s.incidentAlerts(ctx)
			if err != nil || len(alerts) != 0 {
				t.Fatalf("%s: alerts=%v err=%v", terminal, alerts, err)
			}
			saved, err := s.OperationalSetting(ctx, "incident-deadline")
			if err != nil || len(fleet.List(saved)) != 1 || fleet.Object(fleet.List(saved)[0])["endedAt"] == nil {
				t.Fatalf("history: %v %v", saved, err)
			}
		})
	}

}

func TestSlowWorkRowsExcludeOldEnded(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := context.Background()
	cutoff := now.Add(-slowWorkWindow)
	for i, end := range []time.Time{cutoff, cutoff.Add(time.Second), now.Add(time.Hour), {}} {
		status := "merged"
		if end.IsZero() {
			status = "running"
		}
		seedSlowJob(t, s, 100+i, "light", status, cutoff.Add(-time.Hour), end)
		for _, kind := range []string{"pr_create", "full_test", "pr_close"} {
			state := "done"
			if end.IsZero() {
				state = "claimed"
			}
			seedSlowRequest(t, s, fmt.Sprintf("%s-%d", kind, i), kind, state, "passed", cutoff.Add(-time.Hour), end)
		}
	}
	before, err := s.slowWorkRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 16 {
		t.Fatalf("retained rows: %d", len(before))
	}
	seedSlowJob(t, s, 200, "light", "merged", cutoff.Add(-time.Hour), cutoff.Add(-time.Second))
	for _, kind := range []string{"pr_create", "full_test", "pr_close"} {
		seedSlowRequest(t, s, "old-"+kind, kind, "done", "passed", cutoff.Add(-time.Hour), cutoff.Add(-time.Second))
	}
	after, err := s.slowWorkRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("old rows retained: %d -> %d", len(before), len(after))
	}
}

func TestSlowWorkResultUnchangedByCutoff(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	current := now
	s.Now = func() time.Time { return current }
	cutoff := now.Add(-slowWorkWindow)
	ctx := context.Background()
	for i := 0; i < 9; i++ {
		seedSlowJob(t, s, i+1, "light", "merged", now.Add(-2*time.Hour), now.Add(-time.Hour))
	}
	seedSlowJob(t, s, 20, "light", "merged", cutoff.Add(-2*time.Hour), cutoff)
	seedSlowJob(t, s, 21, "light", "merged", cutoff.Add(-11*time.Hour-time.Second), cutoff.Add(-time.Second))
	seedSlowJob(t, s, 22, "light", "running", now.Add(-3*time.Hour), time.Time{})
	// A sufficiently early cutoff reads every seeded interval, while the evaluation time stays fixed.
	current = now.Add(-100 * 24 * time.Hour)
	all, err := s.slowWorkRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	current = now
	filtered, err := s.slowWorkRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(filtered)+1 {
		t.Fatalf("all=%d filtered=%d", len(all), len(filtered))
	}
	reference := slowWorkAverages(all, now)
	actual := slowWorkAverages(filtered, now)
	if !reflect.DeepEqual(reference, actual) || actual["implementation-light"] != 3960 {
		t.Fatalf("averages: all=%v filtered=%v", reference, actual)
	}
	var expected []IncidentSource
	for _, w := range all {
		avg, ok := reference[w.group]
		if !ok || w.end != nil || w.start.IsZero() || w.start.After(now) {
			continue
		}
		elapsed := now.Sub(w.start).Seconds()
		if elapsed < 2*avg {
			continue
		}
		expected = append(expected, IncidentSource{Key: "slow-" + w.key, Kind: "slow", Title: "作業の遅延: " + w.title, Target: map[string]any{"project": w.repo, "number": w.number, "workKey": w.key, "workType": w.group, "host": w.host, "startedAt": iso(w.start), "elapsedSeconds": elapsed, "averageSeconds": avg}})
	}
	got, err := s.SlowWorkSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(expected, func(i, j int) bool { return expected[i].Key < expected[j].Key })
	sort.Slice(got, func(i, j int) bool { return got[i].Key < got[j].Key })
	if len(expected) != 1 || !reflect.DeepEqual(expected, got) {
		t.Fatalf("sources: all=%v filtered=%v", expected, got)
	}
}
