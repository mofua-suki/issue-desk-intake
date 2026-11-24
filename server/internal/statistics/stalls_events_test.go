package statistics

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
)

type stallFixture struct {
	db    *sql.DB
	ctx   context.Context
	s     Service
	start time.Time
	t     *testing.T
}

func newStallFixture(t *testing.T, logs bool) *stallFixture {
	t.Helper()
	db, ctx := testPostgres(t)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	f := &stallFixture{db: db, ctx: ctx, start: at, t: t}
	f.s = Service{DB: db, AppDB: pgtest.New(t), Now: func() time.Time { return at.Add(20 * time.Minute) }}
	if logs {
		if _, err := db.ExecContext(ctx, `INSERT INTO runner_log_lines(host,at,key,line,sender) VALUES('nas',$1,'start','起動 test','nas'),('nas',$2,'end','last','nas')`, at, at.Add(20*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func (f *stallFixture) job(repo string, issue int) int64 {
	f.t.Helper()
	var id int64
	err := f.db.QueryRowContext(f.ctx, `INSERT INTO jobs(repo,issue,status,filed_at,tier,result,view) VALUES($1,$2,'queued',$3,'light','{}','{}') RETURNING id`, repo, issue, f.start).Scan(&id)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f *stallFixture) event(id int64, minute int, host, kind, data string) {
	f.t.Helper()
	_, err := f.db.ExecContext(f.ctx, `INSERT INTO job_events(job_id,host,kind,at,data)VALUES($1,$2,$3,$4,$5::jsonb)`, id, host, kind, f.start.Add(time.Duration(minute)*time.Minute), data)
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *stallFixture) leased(id int64, minute int, host, phase string, generation, epoch int, expiry int) {
	f.event(id, minute, host, "leased", fmt.Sprintf(`{"generation":%d,"epoch":%d,"phase":%q,"expiresAt":%q}`, generation, epoch, phase, f.start.Add(time.Duration(expiry)*time.Minute).Format(time.RFC3339)))
}
func (f *stallFixture) result(project string) map[string]any {
	f.t.Helper()
	p := Period{f.start, f.start.Add(20 * time.Minute)}
	result, err := f.s.Stalls(f.ctx, Query{Project: project, Before: p, After: p})
	if err != nil {
		f.t.Fatal(err)
	}
	return fleet.Object(fleet.Object(result)["after"])
}
func (f *stallFixture) transition(id int64, minute int, host, kind, to string, generation, epoch int) {
	f.event(id, minute, host, kind, fmt.Sprintf(`{"generation":%d,"epoch":%d,"leaseHost":%q,"to":%q}`, generation, epoch, host, to))
}
func TestStallsJobEventsTimeline(t *testing.T) {
	f := newStallFixture(t, true)
	id := f.job("o/r", 1)
	f.leased(id, 1, "nas", "", 1, 1, 18)
	f.transition(id, 2, "nas", "phase", "running", 1, 1)
	f.transition(id, 5, "nas", "phase", "publishing", 1, 1)
	f.transition(id, 10, "nas", "state", "merged", 1, 1)
	got := f.result("")
	if fleet.Object(got["parallel"])["activeMs"] != float64(3*60000) {
		t.Fatal(got)
	}
}
func TestStallsJobEventsHosts(t *testing.T) {
	f := newStallFixture(t, true)
	a := f.job("o/a", 1)
	b := f.job("o/b", 2)
	f.leased(a, 1, "nas", "running", 1, 1, 18)
	f.leased(b, 3, "pc", "running", 1, 1, 18)
	f.transition(a, 7, "nas", "state", "merged", 1, 1)
	f.transition(b, 9, "pc", "state", "merged", 1, 1)
	x := fleet.Object(f.result("")["parallel"])
	y := fleet.Object(f.result("o/a")["parallel"])
	if x["parallelMs"] != float64(4*60000) || y["parallelMs"] != x["parallelMs"] || x["hosts"].(map[string]float64)["nas"] != float64(6*60000) || x["hosts"].(map[string]float64)["pc"] != float64(6*60000) {
		t.Fatal(x, y)
	}
}
func TestStallsJobEventsLeaseLifecycle(t *testing.T) {
	f := newStallFixture(t, true)
	id := f.job("o/r", 1)
	f.leased(id, 1, "nas", "running", 1, 1, 18)
	f.transition(id, 3, "nas", "control", "held", 1, 1)
	f.transition(id, 3, "nas", "control", "held", 1, 1)
	f.leased(id, 5, "pc", "publishing", 2, 2, 18)
	f.transition(id, 8, "pc", "scope_wait", "held", 2, 2)
	f.leased(id, 9, "nas", "releasing", 3, 3, 18)
	f.transition(id, 11, "nas", "phase", "running", 3, 3)
	f.leased(id, 11, "nas", "running", 1, 1, 18)
	f.transition(id, 12, "nas", "state", "held", 3, 3)
	f.leased(id, 13, "nas", "running", 4, 4, 18)
	f.transition(id, 14, "nas", "scope_over_limit", "stopped", 4, 4)
	f.leased(id, 15, "pc", "running", 5, 5, 18)
	f.transition(id, 16, "pc", "lease_expired", "queued", 5, 5)
	got := fleet.Object(f.result("")["parallel"])
	if got["activeMs"] != float64(5*60000) || got["measuredMs"] != float64(19*60000) {
		t.Fatal(got)
	}
}
func TestStallsJobEventsCoverage(t *testing.T) {
	f := newStallFixture(t, true)
	id := f.job("o/r", 1)
	f.leased(id, 1, "nas", "", 1, 1, 4)
	f.transition(id, 2, "nas", "phase", "running", 1, 1)
	f.event(id, 6, "nas", "lease_renewed", fmt.Sprintf(`{"generation":2,"epoch":2,"expiresAt":%q}`, f.start.Add(19*time.Minute).Format(time.RFC3339)))
	f.transition(id, 8, "nas", "lease_expired", "queued", 1, 1)
	got := f.result("")
	if got["recordedMs"] != float64(14*60000) || got["unrecordedMs"] != float64(4*60000) || fleet.Object(got["parallel"])["activeMs"] != float64(2*60000) || len(got["daily"].([]any)) == 0 {
		t.Fatal(got)
	}
	day := fleet.Object(got["daily"].([]any)[0])
	if fleet.Object(day["measured"])["start"] != fleet.Object(got["measured"])["start"] || fleet.Object(day["measured"])["end"] != fleet.Object(got["measured"])["end"] || day["ratio"] != fleet.Object(got["parallel"])["ratio"] {
		t.Fatal(day, got)
	}
	old := newStallFixture(t, true)
	legacy := old.job("o/old", 1)
	old.event(legacy, 1, "nas", "leased", `{}`)
	old.leased(legacy, 5, "nas", "running", 2, 2, 10)
	old.event(legacy, 8, "nas", "lease_renewed", fmt.Sprintf(`{"generation":2,"epoch":2,"expiresAt":%q}`, old.start.Add(15*time.Minute).Format(time.RFC3339)))
	old.transition(legacy, 12, "nas", "state", "merged", 2, 2)
	v := old.result("")
	if v["recordedMs"] != float64(15*60000) || fleet.Object(v["parallel"])["activeMs"] != float64(7*60000) {
		t.Fatal(v)
	}
}
func TestStallsIndependentSources(t *testing.T) {
	f := newStallFixture(t, false)
	id := f.job("o/r", 1)
	f.leased(id, 1, "nas", "running", 1, 1, 18)
	fv := f.result("")
	if fv["measured"] != nil {
		t.Fatal(fv)
	}
	g := newStallFixture(t, true)
	g.job("o/r", 2)
	gv := g.result("")
	if gv["measured"] != nil || fv["reason"] == nil || gv["reason"] == nil || fv["reason"] == gv["reason"] {
		t.Fatal(fv, gv)
	}
}
