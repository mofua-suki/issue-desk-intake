package jobs

import (
	"fmt"
	"testing"
	"time"
)

func TestPostgresScopeLimitBoundariesAndHistory(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		baseline, files, limit   int
		narrowFirst, narrowAfter bool
	}{
		{"four", 4, 8, 8, false, false},
		{"two", 2, 8, 8, false, false},
		{"five", 5, 8, 10, false, false},
		{"explicit", 4, 12, 12, false, false},
		{"small_explicit", 2, 3, 4, false, false},
		{"narrow_before_first_add", 5, 8, 10, true, false},
		{"narrow_after_add", 5, 8, 10, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now()
			s, _ := New(db, Options{Now: func() time.Time { return at }})
			paths := make([]string, tc.baseline)
			for i := range paths {
				paths[i] = fmt.Sprintf("file%d", i)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at) VALUES(1,'owner/repo',1,'running',$1::text[],'declared','worker',1,$2)`, paths, at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if tc.narrowFirst {
				for _, actual := range [][]string{paths[:2], paths[:1]} {
					if _, err := s.Narrow(ctx, 1, "worker", 1, actual); err != nil {
						t.Fatal(err)
					}
				}
				paths = paths[:1]
			}
			if tc.narrowAfter {
				if _, err := s.Widen(ctx, 1, "worker", 1, []string{"interim"}, nil, tc.files); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Narrow(ctx, 1, "worker", 1, paths[:1]); err != nil {
					t.Fatal(err)
				}
				paths = paths[:1]
			}
			// Each step adds one production file; the baseline must never grow.
			for total := len(paths) + 1; total <= tc.limit; total++ {
				got, err := s.Widen(ctx, 1, "worker", 1, []string{fmt.Sprintf("add%d", total)}, nil, tc.files)
				if err != nil || got.Result != "granted" || got.Job.Status != "running" {
					t.Fatalf("total=%d got=%+v err=%v", total, got, err)
				}
			}
			got, err := s.Widen(ctx, 1, "worker", 1, []string{"too-many"}, nil, tc.files)
			code(t, err, 409, "lease_lost")
			if got != nil {
				t.Fatal(got)
			}
			var baseline, limit, count int
			if err := db.QueryRowContext(ctx, `SELECT (data->>'baseline')::int,(data->>'limit')::int,(data->>'count')::int FROM job_events WHERE job_id=1 AND kind='scope_over_limit'`).Scan(&baseline, &limit, &count); err != nil || baseline != tc.baseline || limit != tc.limit || count != tc.limit+1 {
				t.Fatalf("baseline=%d limit=%d count=%d err=%v", baseline, limit, count, err)
			}
		})
	}
}
