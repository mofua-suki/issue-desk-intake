package statistics

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestPostgresWaitScopeHistoryAcrossNarrowing(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,scope,filed_at) VALUES
 (1,'o/r',1,'running','running',ARRAY['other'], $1),
 (2,'o/r',2,'waiting','running',ARRAY['src/a'], $1)`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES
 (1,'nas','leased',$1,'{}'),
 (1,'nas','scope_narrowed',$1::timestamptz + interval '5 seconds','{"before":["src/","other"],"after":["other"],"actual":["other"]}'),
 (2,'nas','leased',$1::timestamptz + interval '10 seconds','{}')`, at)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{DB: db, Assignments: &fleet.Assignments{Implementers: map[string][]string{"o/r": {"nas"}}, Hosts: map[string]*fleet.Host{"nas": {Max: 2}}}}
	result, err := svc.Waits(ctx, Query{Project: "o/r", Before: Period{at, at.Add(20 * time.Second)}, After: Period{at.Add(20 * time.Second), at.Add(30 * time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	start := result.(map[string]any)["before"].(map[string]any)["startWait"].(map[string]any)
	causes := start["causes"].(map[string]any)
	if causes["scopeOverlap"].(map[string]any)["ms"] != float64(5000) || causes["idle"].(map[string]any)["ms"] != float64(5000) {
		t.Fatalf("history: %v", causes)
	}
	// Place the lease inside the shorter report while narrowing remains in the future.
	_, err = db.ExecContext(ctx, `UPDATE job_events SET at=$1::timestamptz + interval '3 seconds' WHERE job_id=2 AND kind='leased'`, at)
	if err != nil {
		t.Fatal(err)
	}
	// The narrowing can occur after the report's end; before must still be read.
	result, err = svc.Waits(ctx, Query{Project: "o/r", Before: Period{at, at.Add(4 * time.Second)}, After: Period{at, at.Add(4 * time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, period := range []string{"before", "after"} {
		causes := result.(map[string]any)[period].(map[string]any)["startWait"].(map[string]any)["causes"].(map[string]any)
		if causes["scopeOverlap"].(map[string]any)["ms"] != float64(3000) || causes["idle"].(map[string]any)["ms"] != float64(0) {
			t.Fatalf("%s: %v", period, causes)
		}
	}
	all, err := svc.readWait(ctx, at.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	versions := buildWaitScopeHistory(all[1])
	if got := versions[scopeVersionAt(versions, at)].scope; len(got) != 2 || got[0] != "src/" {
		t.Fatalf("future narrowing: %v", got)
	}
}

func TestPostgresWaitReportIgnoresFutureScopeWaitState(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,scope,filed_at) VALUES
 (1,'o/r',1,'unknown','queued',ARRAY['src/'], $1),
 (2,'o/r',2,'waiting','running',ARRAY['src/a'], $1)`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES
 (2,'nas','leased',$1::timestamptz + interval '10 seconds','{}')`, at)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{DB: db, Assignments: &fleet.Assignments{Implementers: map[string][]string{"o/r": {"nas"}}, Hosts: map[string]*fleet.Host{"nas": {Max: 2}}}}
	p := Period{at, at.Add(20 * time.Second)}
	q := Query{Project: "o/r", Before: p, After: p}
	read := func() map[string]any {
		t.Helper()
		result, err := svc.Waits(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return result.(map[string]any)
	}
	before := read()
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES
 (1,'nas','leased',$1::timestamptz + interval '20 seconds','{}'),
 (1,'nas','scope_wait',$1::timestamptz + interval '21 seconds','{"add":["other"]}')`, at)
	if err != nil {
		t.Fatal(err)
	}
	after := read()
	for _, period := range []string{"before", "after"} {
		causes := func(report map[string]any) map[string]any {
			return report[period].(map[string]any)["startWait"].(map[string]any)["causes"].(map[string]any)
		}
		if !reflect.DeepEqual(causes(before), causes(after)) {
			t.Fatalf("%s causes changed: %v -> %v", period, causes(before), causes(after))
		}
		if causes(after)["unknown"].(map[string]any)["ms"] != float64(10000) {
			t.Fatalf("%s: %v", period, causes(after))
		}
	}
}

func TestWaitScopeHistoryNormalizesAdd(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	j := &waitJob{ScopeEvents: []event{
		{Kind: "scope_narrowed", At: at, Data: map[string]any{"before": []string{"a", "b"}, "after": []string{"a"}}},
		{Kind: "scope_widened", At: at.Add(time.Second), Data: map[string]any{"add": []string{"a", "b", "b"}}},
		{Kind: "scope_wait", At: at.Add(2 * time.Second), Data: map[string]any{"add": []string{"*", "c"}}},
		{Kind: "scope_widened", At: at.Add(3 * time.Second), Data: map[string]any{"add": []string{"d"}}},
	}}
	versions := buildWaitScopeHistory(j)
	for _, tc := range []struct {
		seconds int
		want    []string
	}{{0, []string{"a"}}, {1, []string{"a", "b"}}, {2, []string{"*"}}, {3, []string{"*"}}} {
		if got := versions[scopeVersionAt(versions, at.Add(time.Duration(tc.seconds)*time.Second))].scope; !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%d: %v", tc.seconds, got)
		}
	}
}

func TestWaitScopeHistoryCacheAndCalculationLoad(t *testing.T) {
	all, period, assignments := waitLoadJobs(20, 8)
	at := period.From
	for _, j := range all {
		before := append([]string{}, j.Scope...)
		for n := 1; n <= 30; n++ {
			when := at.Add(2*time.Second + time.Duration(n)*100*time.Millisecond)
			narrow := event{Kind: "scope_narrowed", At: when, Data: map[string]any{"before": before, "after": before[:1]}}
			widen := event{Kind: "scope_widened", At: when.Add(time.Microsecond), Data: map[string]any{"add": before[1:]}}
			j.ScopeEvents = append(j.ScopeEvents, narrow, widen)
			j.Events = append(j.Events, narrow, widen)
		}
		sort.Slice(j.Events, func(a, b int) bool { return j.Events[a].At.Before(j.Events[b].At) })
	}
	cache := newWaitScopeCache(assignments)
	start := time.Now()
	starts, _, err := waitEntries(context.Background(), all, period, "o/r", assignments, cache)
	if err != nil || len(starts) != len(all) {
		t.Fatalf("entries: %d %v", len(starts), err)
	}
	t.Logf("20 jobs, 60 scope events each: calculation %s, overlap recomputations %d", time.Since(start), cache.recomputations)
	// The old whole-cache invalidation repeatedly recomputes unrelated pairs.
	if cache.recomputations > 4000 {
		t.Fatalf("waitEntries recomputed too many overlaps: %d", cache.recomputations)
	}
	// Count actual cache misses by checking whether each requested pair is present.
	cache = newWaitScopeCache(assignments)
	jobs := []*waitJob{all[1], all[2], all[3], all[4]}
	misses := 0
	check := func(a, b *waitJob, version int) {
		cache.setVersion(a.ID, version)
		cache.setVersion(b.ID, 0)
		pair := waitScopePair{Left: a.ID, Right: b.ID, LeftVersion: version}
		if _, ok := cache.pairs[pair]; !ok {
			misses++
		}
		if _, err := cache.overlap(context.Background(), a, b); err != nil {
			t.Fatal(err)
		}
	}
	check(jobs[0], jobs[1], 0)
	check(jobs[2], jobs[3], 0)
	for n := 1; n <= 30; n++ {
		check(jobs[0], jobs[1], n)
		check(jobs[2], jobs[3], 0)
	}
	t.Logf("overlap recomputations: %d", misses)
	if misses != 32 {
		t.Fatalf("unrelated pair was recomputed: %d", misses)
	}
}

func TestWaitScopeCacheVersionsAndFIFO(t *testing.T) {
	ctx := context.Background()
	c := newWaitScopeCache(nil)
	a := &waitJob{ID: 1, Scope: []string{"a"}}
	b := &waitJob{ID: 2, Scope: []string{"a"}}
	check := func(want bool) {
		t.Helper()
		got, err := c.overlap(ctx, a, b)
		if err != nil || got != want {
			t.Fatalf("overlap = %v, %v; want %v", got, err, want)
		}
	}
	check(true)
	// Change the version without calling setVersion: prepared keys must follow it.
	c.versions[a.ID] = 1
	a.Scope = []string{"b"}
	check(false)
	if c.recomputations != 2 {
		t.Fatalf("recomputations = %d", c.recomputations)
	}
	c.setVersion(a.ID, 0)
	a.Scope = []string{"a"}
	check(true)
	if c.recomputations != 2 || len(c.order) != 2 {
		t.Fatalf("old version not reused: %d, %d", c.recomputations, len(c.order))
	}
	// Fill and wrap the ring, then ensure changing a version does not compact it.
	for id := int64(3); id <= waitScopePairLimit+2; id++ {
		if _, err := c.overlap(ctx, a, &waitJob{ID: id, Scope: []string{"z"}}); err != nil {
			t.Fatal(err)
		}
	}
	next, first := c.next, c.order[c.next]
	c.setVersion(a.ID, 2)
	if len(c.order) != waitScopePairLimit || c.next != next || c.order[c.next] != first {
		t.Fatal("version change altered FIFO")
	}
	if _, ok := c.pairs[first]; !ok {
		t.Fatal("version change removed oldest pair")
	}
	if _, err := c.overlap(ctx, a, &waitJob{ID: waitScopePairLimit + 3}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.pairs[first]; ok {
		t.Fatal("FIFO did not evict oldest pair")
	}
}
