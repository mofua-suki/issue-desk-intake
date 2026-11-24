package statistics

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
)

// TestStatsTimingFixture measures the actual service entry points with a reproducible database.
func TestStatsTimingFixture(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,status,filed_at,tier,result,view) SELECT 'o/r',n,'merged',$1::timestamptz+(n%28)*interval '1 day','light','{}','{}' FROM generate_series(1,300) n`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) SELECT id,'nas','leased',filed_at+CASE WHEN issue=2 THEN interval '0' ELSE interval '1 minute' END,'{}'::jsonb FROM jobs WHERE issue<>1 UNION ALL SELECT id,'nas','state',filed_at+interval '5 minutes','{"to":"merged"}'::jsonb FROM jobs WHERE issue<>1`)
	if err != nil {
		t.Fatal(err)
	}
	now := at.Add(32 * 24 * time.Hour)
	s := &Service{DB: db, AppDB: pgtest.New(t), Summary: true, Now: func() time.Time { return now }, Assignments: &fleet.Assignments{}, Drafts: func(context.Context) ([]map[string]any, error) { return nil, nil }}
	q := Query{Before: Period{at, at.Add(14 * 24 * time.Hour)}, After: Period{at.Add(14 * 24 * time.Hour), at.Add(28 * 24 * time.Hour)}}
	measure := func(name string, f func() (any, error)) {
		start := time.Now()
		_, err := f()
		t.Logf("%s %.3f ms error=%v", name, float64(time.Since(start))/float64(time.Millisecond), err)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	measure("waits DB read", func() (any, error) { return s.readWait(ctx, q.After.To) })
	all, err := s.readWait(ctx, q.After.To)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Period{q.Before, q.After} {
		var reports [2]any
		for n, legacy := range []bool{true, false} {
			cache := newWaitScopeCache(s.Assignments)
			cache.legacyCuts = legacy
			started := time.Now()
			start, publish, e := waitEntries(ctx, all, p, q.Project, s.Assignments, cache)
			if e != nil {
				t.Fatal(e)
			}
			if p == q.Before {
				missing, zero := false, false
				for _, entry := range start {
					if entry.Duration == nil && entry.Missing {
						missing = true
					}
					if entry.Duration != nil && *entry.Duration == 0 {
						zero = true
					}
				}
				if !missing || !zero {
					t.Fatal("欠測とゼロの試験データがありません")
				}
			}
			reports[n] = []any{waitReport(start, strings.Fields("dependency scopeOverlap slotFull idle unknown")), waitReport(publish, strings.Fields("review other unknown"))}
			t.Logf("wait aggregation legacy=%t %.3f ms", legacy, float64(time.Since(started))/float64(time.Millisecond))
		}
		if !reflect.DeepEqual(reports[0], reports[1]) {
			t.Fatal("waits の改善前後の結果が異なります")
		}
	}
	old, err := s.waitsWithLegacyCuts(ctx, q, true)
	if err != nil {
		t.Fatal(err)
	}
	newValue, err := s.Waits(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old, newValue) {
		t.Fatal("waits の応答が変わりました")
	}
	measure("work", func() (any, error) { return s.Work(ctx, q, []string{"o/r"}) })
	for _, name := range strings.Fields("inputBudget implementation tiers usage usageLimits fixedTestUsage rework reviewNotes followups usageReview priorityReview antigravity scope sizeEstimates workTypes testSelection stalls inventory issueBody waits throughput filingCheck consultationBudget giteaReads") {
		name := name
		measure(name, func() (any, error) {
			switch name {
			case "usage":
				return s.Usage(ctx, q)
			case "rework":
				return s.Rework(ctx, q)
			case "implementation":
				return s.Implementation(ctx, q)
			case "scope":
				return s.Scope(ctx, q)
			case "fixedTestUsage":
				return s.FixedTests(ctx, q)
			default:
				return s.Saved(ctx, name, q)
			}
		})
	}
}
