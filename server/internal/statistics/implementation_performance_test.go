package statistics

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/jobs"
	"reflect"
	"testing"
	"time"
)

func TestPostgresImplementationBoundaries(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := at.Add(72 * time.Hour)
	for i := 1; i <= 10; i++ {
		status := "merged"
		if i == 7 || i == 8 {
			status = "running"
		}
		expires := now.Add(time.Hour)
		if i == 8 {
			expires = now.Add(-time.Hour)
		}
		var id int64
		if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,filed_at,view,result,lease_host,lease_epoch,lease_expires_at) VALUES('o/r',$1,$2,$3,'{}','{}','host',1,$4) RETURNING id`, i, status, at.Add(-48*time.Hour), expires).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if i == 7 {
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET generation=2 WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
		}
		if i == 10 {
			continue
		}
		leaseTime := at.Add(-time.Hour)
		if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES($1,'host','leased',$2,'{"epoch":1,"generation":1}')`, id, leaseTime); err != nil {
			t.Fatal(err)
		}
		if i == 4 || i == 5 {
			when := leaseTime
			if i == 5 {
				when = at.Add(-30 * time.Minute)
			}
			epoch := 2
			if i == 5 {
				epoch = 1
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES($1,'host','leased',$2,jsonb_build_object('epoch',$3::int,'generation',1))`, id, when, epoch); err != nil {
				t.Fatal(err)
			}
		}
		addUsage := func(key string, when time.Time, host string, generation int, step string) {
			t.Helper()
			row := map[string]any{"repo": "o/r", "issue": i, "at": when.Format(time.RFC3339Nano), "host": host, "generation": generation, "step": step, "outcome": "completed"}
			raw, _ := json.Marshal(row)
			if _, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,step,json,host,sender) VALUES($1,'o/r',$2,$3,$4,$5,'host','test')`, key, i, when, step, string(raw)); err != nil {
				t.Fatal(err)
			}
		}
		host := "host"
		if i == 3 {
			host = "different"
		}
		gen := 1
		if i == 2 {
			gen = 0
		}
		when := at
		if i == 6 {
			when = at.Add(24 * time.Hour)
		}
		if i == 9 {
			when = at.Add(48 * time.Hour)
		}
		addUsage(fmt.Sprintf("row-%d", i), when, host, gen, "実装")
		if i == 1 {
			addUsage("cross-period", at.Add(25*time.Hour), host, gen, "実装")
			addUsage("review", at.Add(time.Hour), host, gen, "独立レビュー")
		}
		if i == 1 || i == 6 {
			if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES($1,'host','state',$2,'{"epoch":1,"to":"merged"}')`, id, at.Add(26*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Two valid leases and generations exercise a real retry on the period boundary.
	var retryID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,filed_at,view,result,generation) VALUES('o/r',11,'merged',$1,'{}','{}',1) RETURNING id`, at.Add(-48*time.Hour)).Scan(&retryID); err != nil {
		t.Fatal(err)
	}
	for generation := 1; generation <= 2; generation++ {
		when := at.Add(time.Duration(generation-1) * 24 * time.Hour)
		if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES($1,'host','leased',$2,jsonb_build_object('epoch',$3::int,'generation',$3::int)),($1,'host','state',$4,jsonb_build_object('epoch',$3::int,'to','merged'))`, retryID, when.Add(-time.Minute), generation, when.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		row := map[string]any{"repo": "o/r", "issue": 11, "at": when.Format(time.RFC3339Nano), "host": "host", "generation": generation, "step": "実装", "outcome": "completed"}
		raw, _ := json.Marshal(row)
		if _, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,step,json,host,sender) VALUES($1,'o/r',11,$2,'実装',$3,'host','test')`, fmt.Sprintf("retry-%d", generation), when, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for issue, terminal := range map[int]string{12: `{"epoch":1,"to":"closed","implementationEnd":{"version":1,"generation":1,"reason":"fixed-test","usageUnknown":false}}`, 13: `{"epoch":1,"to":"held","holdReason":"question"}`, 14: `{"epoch":1,"to":"closed","implementationEnd":{"version":1,"generation":2,"reason":"checks","usageUnknown":false}}`} {
		var id int64
		if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,filed_at,view,result) VALUES('o/r',$1,'merged',$2,'{}','{}') RETURNING id`, issue, at.Add(-48*time.Hour)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) VALUES($1,'host','leased',$2,'{"epoch":1,"generation":1}'),($1,'host','state',$3,$4::jsonb)`, id, at.Add(-time.Hour), at.Add(time.Hour), terminal); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"repo": "o/r", "issue": issue, "at": at.Format(time.RFC3339Nano), "host": "host", "generation": 1, "step": "実装", "outcome": "completed"})
		if _, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,step,json,host,sender) VALUES($1,'o/r',$2,$3,'実装',$4,'host','test')`, fmt.Sprintf("reason-%d", issue), issue, at, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,at,step,json,host,sender) VALUES('broken','o/r',$1,'実装','{','host','test')`, at); err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: db, Now: func() time.Time { return now }}
	q := Query{Before: Period{at, at.Add(24 * time.Hour)}, After: Period{at.Add(24 * time.Hour), at.Add(48 * time.Hour)}}
	got, err := s.Implementation(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	before := got.(map[string]any)["before"].(map[string]any)
	after := got.(map[string]any)["after"].(map[string]any)
	for key, want := range map[string]int{"runs": 9, "unknownGenerations": 4, "unknownIssues": 4, "noImplementationRecord": 2, "skipped": 1, "implementationMismatchIssues": 2} {
		if before[key] != want {
			t.Errorf("before %s=%v, want %d", key, before[key], want)
		}
	}
	for key, want := range map[string]int{"generationUnidentified": 4, "inProgress": 0, "reasonNotRecorded": 1, "unclassified": 1} {
		if before["unknownDetails"].(map[string]any)[key] != want {
			t.Errorf("unknown %s", key)
		}
	}
	if after["runs"] != 3 || after["reasons"].(map[string]any)["success"] != 3 || after["skipped"] != 1 {
		t.Fatalf("after: %v", after)
	}
	for _, issue := range before["issues"].([]any) {
		item := issue.(map[string]any)
		if item["key"] == "o/r#2" && item["implementations"] != nil {
			t.Fatal("unknown generation must be null")
		}
	}
	if !reflect.DeepEqual(before["distribution"], []any{map[string]any{"implementations": int64(1), "issues": 6}}) || !reflect.DeepEqual(before["uc1Distribution"], []any{map[string]any{"implementations": int64(1), "issues": 8}, map[string]any{"implementations": int64(2), "issues": 1}, map[string]any{"implementations": int64(3), "issues": 1}}) {
		t.Fatalf("unexpected distributions: %v %v", before["distribution"], before["uc1Distribution"])
	}
	if !reflect.DeepEqual(after["distribution"], []any{map[string]any{"implementations": int64(1), "issues": 2}, map[string]any{"implementations": int64(2), "issues": 1}}) {
		t.Fatalf("after distribution: %v", after["distribution"])
	}
	if reasons := before["reasons"].(map[string]any); reasons["success"] != 1 || reasons["fixed-test"] != 1 || reasons["question"] != 1 || reasons["unknown"] != 6 {
		t.Fatalf("reasons: %v", reasons)
	}
	if before["reasons"].(map[string]any)["checks"] != 0 {
		t.Fatal("zero reason count missing")
	}

	// Completing the active lease adds one success to the public result.
	if _, err := db.ExecContext(ctx, `INSERT INTO job_events(job_id,host,kind,at,data) SELECT id,'host','state',$1,'{"epoch":1,"to":"merged"}'::jsonb FROM jobs WHERE repo='o/r' AND issue=7`, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='merged' WHERE repo='o/r' AND issue=7`); err != nil {
		t.Fatal(err)
	}
	finished, err := s.Implementation(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	finalBefore := finished.(map[string]any)["before"].(map[string]any)
	if finalBefore["runs"] != 10 || finalBefore["reasons"].(map[string]any)["success"] != 2 || finalBefore["reasons"].(map[string]any)["unknown"] != 6 {
		t.Fatalf("finished before: %v", finalBefore)
	}
	if finalBefore["days"].([]any)[0].(map[string]any)["runs"] != 10 || finalBefore["unknownDetails"].(map[string]any)["inProgress"] != 0 {
		t.Fatalf("finished daily or unknown details: %v", finalBefore)
	}

}

func TestImplementationUsesSweepLeaseExpiredHost(t *testing.T) {
	db, ctx := testPostgres(t)
	now := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	store, err := jobs.New(db, jobs.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	err = db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,filed_at,lease_host,lease_epoch,lease_expires_at,generation,view,result) VALUES('owner/expiry',1,'running',$1,'nas',7,$2,1,'{}','{}') RETURNING id`, now.Add(-24*time.Hour), now.Add(-time.Minute)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,host,data) VALUES($1,'leased',$2,'nas','{"epoch":7,"generation":1}')`, id, now.Add(-3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	row := `{"repo":"owner/expiry","issue":1,"host":"nas","generation":1,"step":"実装","at":"2026-11-01T01:00:00Z","outcome":"completed"}`
	_, err = db.ExecContext(ctx, `INSERT INTO usage_rows(key,repo,issue,at,step,json,host,sender) VALUES('expiry','owner/expiry',1,$1,'実装',$2,'nas','nas')`, now.Add(-2*time.Hour), row)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Sweep(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// A later terminal record must not override a matching expiry, but remains
	// eligible if that expiry belongs to a different lease.
	_, err = db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,host,data) VALUES($1,'state',$2,'nas','{"epoch":7,"to":"held","holdReason":"question"}')`, id, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: db, Now: func() time.Time { return now.Add(time.Hour) }}
	q := Query{Before: Period{now.Add(-24 * time.Hour), now.Add(24 * time.Hour)}, After: Period{now.Add(24 * time.Hour), now.Add(48 * time.Hour)}}
	check := func(wantExpired, wantQuestion int) {
		t.Helper()
		value, err := s.Implementation(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		before := value.(map[string]any)["before"].(map[string]any)
		reasons := before["reasons"].(map[string]any)
		if before["runs"] != 1 || reasons["unknown"] != 0 || reasons["lease-expired"] != wantExpired || reasons["question"] != wantQuestion {
			t.Fatalf("unexpected implementation result: %v", before)
		}
	}
	check(1, 0)
	for _, update := range []string{
		`UPDATE job_events SET data=jsonb_set(data,'{leaseHost}','"other"') WHERE job_id=$1 AND kind='lease_expired'`,
		`UPDATE job_events SET data=jsonb_set(jsonb_set(data,'{leaseHost}','"nas"'),'{epoch}','8') WHERE job_id=$1 AND kind='lease_expired'`,
	} {
		if _, err = db.ExecContext(ctx, update, id); err != nil {
			t.Fatal(err)
		}
		check(0, 1)
	}
}
