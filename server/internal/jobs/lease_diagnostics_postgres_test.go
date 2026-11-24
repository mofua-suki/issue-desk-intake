package jobs

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPostgresMissingJobReportDoesNotLogRejection(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logger := log.Default()
	original := logger.Writer()
	logger.SetOutput(&output)
	t.Cleanup(func() { logger.SetOutput(original) })
	_, err = s.Report(ctx, -1, "nas", Report{Epoch: 1, Status: "running"})
	code(t, err, 404, "")
	if strings.Contains(output.String(), "failed to record report rejection") {
		t.Fatalf("unexpected rejection failure log: %s", output.String())
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='report_rejected'`, -1).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejection events: %d, %v", count, err)
	}
}

func TestLeaseDiagnosticsPostgres(t *testing.T) {
	db, ctx := testPostgres(t)
	clock := now
	s, err := New(db, Options{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                string
		rejects, renew, old bool
	}{{"recorded", true, true, false}, {"missing", false, false, false}, {"previous generation", true, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			id := seedJob(t, ctx, db, "diagnostics/"+tc.name, "nas", 1, 7)
			if tc.rejects {
				for i := 0; i < 2; i++ {
					_, err := s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "held"})
					code(t, err, 400, "")
				}
			}
			if tc.rejects && !tc.old {
				_, err = s.Report(ctx, id, "other", Report{Epoch: 7, Status: "held"})
				code(t, err, 400, "")
				_, err = s.Report(ctx, id, "nas", Report{Epoch: 8, Status: "held"})
				code(t, err, 400, "")
			}
			if tc.old {
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET generation=generation+1 WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			if tc.renew {
				_, first, err := s.Renew(ctx, id, "nas", 7, nil)
				if err != nil {
					t.Fatal(err)
				}
				clock = clock.Add(time.Second)
				_, lease, renewErr := s.Renew(ctx, id, "nas", 7, nil)
				if renewErr != nil {
					t.Fatal(renewErr)
				}
				var expires string
				if err = db.QueryRowContext(ctx, `SELECT data->>'expiresAt' FROM job_events WHERE job_id=$1 AND kind='lease_renewed'`, id).Scan(&expires); err != nil || expires != lease.ExpiresAt || lease.ExpiresAt == first.ExpiresAt || lease.ExpiresAt != iso(clock.Add(15*time.Minute)) {
					t.Fatalf("renewal expiry=%q lease=%+v err=%v", expires, lease, err)
				}
				if _, _, err = s.Renew(ctx, id, "nas", 8, nil); err == nil {
					t.Fatal("mismatched epoch renewal succeeded")
				}
				var events int
				if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='lease_renewed'`, id).Scan(&events); err != nil || events != 1 {
					t.Fatalf("renewal events: %d %v", events, err)
				}
			}
			for n := 0; n < 3; n++ {
				if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_expires_at=$2 WHERE id=$1`, id, clock.Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
				if err = s.Sweep(ctx, nil); err != nil {
					t.Fatal(err)
				}
			}
			job, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			d, ok := job.Result["leaseDiagnostics"].(map[string]any)
			if !ok {
				t.Fatalf("diagnostics: %v", job.Result)
			}
			if tc.rejects && !tc.old {
				r, ok := d["lastReportRejection"].(map[string]any)
				if !ok || r["count"] != float64(2) || r["reportKind"] != "held" || r["message"] != "held には正しい holdReason が必要です。" {
					t.Fatalf("rejection: %v", d)
				}
			} else if d["lastReportRejection"] != nil || d["lastReportRejectionMissing"] == nil {
				t.Fatalf("missing rejection: %v", d)
			}
			if tc.renew {
				r, ok := d["lastRenewal"].(map[string]any)
				if !ok || r["at"] != iso(clock) || r["epoch"] != float64(7) {
					t.Fatalf("renewal: %v", d)
				}
			} else if d["lastRenewal"] != nil || d["lastRenewalMissing"] == nil {
				t.Fatalf("missing renewal: %v", d)
			}
		})
	}
	t.Run("legacy and another epoch", func(t *testing.T) {
		id := seedJob(t, ctx, db, "diagnostics/legacy", "nas", 1, 7)
		if _, _, err := s.Renew(ctx, id, "nas", 7, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES($1,'nas','report_rejected',$2,'{"code":"REQUEST_REJECTED","epoch":7,"status":400}')`, id, clock); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_epoch=8,lease_expires_at=$2 WHERE id=$1`, id, clock.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := s.Sweep(ctx, nil); err != nil {
				t.Fatal(err)
			}
		}
		job, err := s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		d := job.Result["leaseDiagnostics"].(map[string]any)
		if d["lastRenewal"] != nil || d["lastRenewalMissing"] == nil || d["lastReportRejection"] != nil || d["lastReportRejectionMissing"] == nil {
			t.Fatalf("legacy or different epoch attributed: %v", d)
		}
	})

	t.Run("concurrent rejections", func(t *testing.T) {
		id := seedJob(t, ctx, db, "diagnostics/concurrent", "nas", 1, 7)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "held"})
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			code(t, err, 400, "")
		}
		for i := 0; i < 3; i++ {
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_expires_at=$2 WHERE id=$1`, id, clock.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := s.Sweep(ctx, nil); err != nil {
				t.Fatal(err)
			}
		}
		job, err := s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		d := job.Result["leaseDiagnostics"].(map[string]any)
		r, ok := d["lastReportRejection"].(map[string]any)
		if !ok || r["count"] != float64(8) {
			t.Fatalf("concurrent rejection: %v", d)
		}
	})

}
