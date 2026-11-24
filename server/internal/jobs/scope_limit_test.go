package jobs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestScopeFileCount(t *testing.T) {
	count, unknown := scopeFileCount([]string{"src/A.go", "src/a.go", "a_test.go", "x.test.ts", "x.spec.js", "test_x.py", "x_test.py", "test/a", "tests/a", "testdata/a", "__tests__/a", "src/", "*"})
	if count != 1 || len(unknown) != 2 {
		t.Fatalf("count=%d uncounted=%v", count, unknown)
	}
}

func TestPostgresScopeLimitAndResume(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now()
	s, _ := New(db, Options{Now: func() time.Time { return at }})
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at,result) VALUES (1,'owner/repo',1,'running',ARRAY['a','b','c','d'],'declared','worker',1,$1,'{"keep":true}'), (2,'owner/repo',2,'running',ARRAY['e'],'declared','other',1,$1,'{}')`, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET generation=7, view='{"phase":"implementation"}' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO merge_turns(repo,holder_host,job_id,expires_at) VALUES('owner/repo','worker',1,$1)`, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Widen(ctx, 1, "worker", 1, []string{"e", "f", "g", "h", "i", "tests/a", "dir/"}, nil)
	code(t, err, 409, "lease_lost")
	stopped, readErr := s.Get(ctx, 1)
	if got != nil || readErr != nil || stopped.Status != "stopped" || stopped.HoldReason != nil || stopped.LeaseExpiresAt != nil || len(stopped.Scope) != 4 {
		t.Fatalf("stop: %+v %v", got, err)
	}
	var released bool
	if err = db.QueryRowContext(ctx, `SELECT holder_host IS NULL AND job_id IS NULL AND expires_at IS NULL FROM merge_turns WHERE repo='owner/repo'`).Scan(&released); err != nil || !released {
		t.Fatalf("merge turn: %v %v", released, err)
	}
	var raw []byte
	if err = db.QueryRowContext(ctx, `SELECT data FROM job_events WHERE job_id=1 AND kind='scope_over_limit'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if json.Unmarshal(raw, &event) != nil || event["count"] != float64(9) || event["limit"] != float64(8) || event["baseline"] != float64(4) || len(event["uncounted"].([]any)) != 1 {
		t.Fatalf("event: %s", raw)
	}
	assertScopeTransition(t, event, before, "stopped")
	if !reflect.DeepEqual(event["add"], []any{"e", "f", "g", "h", "i", "tests/a", "dir/"}) || !reflect.DeepEqual(event["uncounted"], []any{"dir/"}) {
		t.Fatalf("event: %s", raw)
	}
	if stopped.Result["keep"] != true || !strings.Contains(stopped.Result["error"].(string), "理由を付けた再開") {
		t.Fatal(stopped.Result)
	}
	if err = s.Sweep(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id=1`).Scan(&status); err != nil || status != "stopped" {
		t.Fatalf("sweep %s %v", status, err)
	}
	for i, reason := range []string{"", "継続する"} {
		input := ControlInput{ID: []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}[i], Action: "resume", By: "issue-screen", Text: reason}
		if _, err = s.Control(ctx, 1, input, ControlOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_host='worker',lease_expires_at=$1 WHERE id=1`, at.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		got, err = s.Widen(ctx, 1, "worker", 1, []string{"f", "g", "h", "i", "j"}, nil)
		if i == 0 {
			code(t, err, 409, "lease_lost")
			if got != nil {
				t.Fatal(got)
			}
			continue
		}
		if err != nil || got.Result != "granted" {
			t.Fatalf("resume %d: %+v", i, got)
		}
	}
	got, err = s.Widen(ctx, 1, "worker", 1, []string{"k"}, nil)
	if err != nil || got.Result != "granted" || got.Job.Status != "running" {
		t.Fatalf("subsequent widening: %+v %v", got, err)
	}
	var approved int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=1 AND kind='scope_over_limit_approved'`).Scan(&approved); err != nil || approved != 1 {
		t.Fatalf("approval %d %v", approved, err)
	}
}

func TestPostgresScopeLimitDeclaredBaseline(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now()
	s, _ := New(db, Options{Now: func() time.Time { return at }})
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at) VALUES(1,'owner/repo',1,'running',ARRAY['a','b','c','d','e'],'declared','worker',1,$1)`, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Widen(ctx, 1, "worker", 1, []string{"tests/a", "DIR/"}, nil)
	if err != nil || got.Result != "granted" {
		t.Fatalf("within: %+v %v", got, err)
	}
	got, err = s.Widen(ctx, 1, "worker", 1, []string{"f", "g", "h", "i", "j"}, nil)
	if err != nil || got.Result != "granted" {
		t.Fatalf("at limit: %+v %v", got, err)
	}
	got, err = s.Widen(ctx, 1, "worker", 1, []string{"k"}, nil)
	code(t, err, 409, "lease_lost")
	if got != nil {
		t.Fatal(got)
	}
	var baseline int
	if err = db.QueryRowContext(ctx, `SELECT (data->>'baseline')::int FROM job_events WHERE job_id=1 AND kind='scope_over_limit'`).Scan(&baseline); err != nil || baseline != 5 {
		t.Fatalf("baseline %d %v", baseline, err)
	}
}

func TestPostgresScopeLimitWildcardEvents(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now()
	s, _ := New(db, Options{Now: func() time.Time { return at }})
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at) VALUES (1,'owner/repo',1,'running',ARRAY['a'],'declared','worker',1,$1), (2,'other/repo',2,'running',ARRAY['a'],'declared','worker',1,$1), (3,'other/repo',3,'running',ARRAY['z'],'declared','other',1,$1)`, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET generation=5,view='{"phase":"review"}' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at,generation) VALUES (4,'third/repo',4,'running',ARRAY['q'],'declared','worker',1,$1,6), (5,'third/repo',5,'running',ARRAY['z'],'declared','other',1,$1,0)`, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id           int64
		kind, result string
	}{{1, "scope_widened", "granted"}, {2, "scope_wait", "wait"}, {4, "scope_wait", "wait"}} {
		before, e := s.Get(ctx, tc.id)
		if e != nil {
			t.Fatal(e)
		}
		got, e := s.Widen(ctx, tc.id, "worker", 1, []string{"*", "DIR/", "dir/"}, nil)
		if e != nil || got.Result != tc.result {
			t.Fatalf("%s: %+v %v", tc.kind, got, e)
		}
		var raw []byte
		if e = db.QueryRowContext(ctx, `SELECT data FROM job_events WHERE job_id=$1 AND kind=$2`, tc.id, tc.kind).Scan(&raw); e != nil {
			t.Fatal(e)
		}
		var event map[string]any
		if e = json.Unmarshal(raw, &event); e != nil {
			t.Fatal(e)
		}
		expected := map[string]any{"add": []any{"*"}, "baseline": float64(1), "uncounted": []any{"*", "DIR/"}, "blockers": []any{}}
		if tc.kind == "scope_wait" {
			expected["blockers"] = []any{map[string]any{"id": float64(3), "repo": "other/repo", "issue": float64(3), "host": "other"}}
			if tc.id == 4 {
				expected["blockers"] = []any{map[string]any{"id": float64(5), "repo": "third/repo", "issue": float64(5), "host": "other"}}
			}
			assertScopeTransition(t, event, before, "held")
			if got.Job.Status != "held" || got.Job.HoldReason == nil || *got.Job.HoldReason != "scope_wait" || got.Job.LeaseExpiresAt != nil {
				t.Fatalf("wait: %+v", got.Job)
			}
			for _, key := range []string{"to", "leaseHost", "generation", "epoch", "phase"} {
				delete(event, key)
			}
		}
		if !reflect.DeepEqual(event, expected) {
			t.Fatalf("%s: %s", tc.kind, raw)
		}
	}
}

func TestPostgresScopeLimitTestFilesAndOverLimitWildcard(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now()
	s, _ := New(db, Options{Now: func() time.Time { return at }})
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at) VALUES (1,'owner/repo',1,'running',ARRAY['a','b','c','d'],'declared','worker',1,$1), (2,'other/repo',2,'running',ARRAY['a','b','c','d','e'],'widened','worker',1,$1)`, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	tests := []string{"unit_test.go", "foo.test.ts", "foo.spec.js", "test_file.py", "file_test.py", "test/a.go", "tests/b.go", "testdata/c.go", "__tests__/d.go", "A"}
	got, err := s.Widen(ctx, 1, "worker", 1, tests, nil)
	if err != nil || got.Result != "granted" {
		t.Fatalf("test files: %+v %v", got, err)
	}
	before, err := s.Get(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.Widen(ctx, 2, "worker", 1, []string{"*", "dir/", "new-file", "new2", "new3", "new4"}, nil)
	code(t, err, 409, "lease_lost")
	if got != nil {
		t.Fatal(got)
	}
	var raw []byte
	if err = db.QueryRowContext(ctx, `SELECT data FROM job_events WHERE job_id=2 AND kind='scope_over_limit'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var event struct {
		Baseline  int      `json:"baseline"`
		Count     int      `json:"count"`
		Add       []string `json:"add"`
		Uncounted []string `json:"uncounted"`
	}
	if err = json.Unmarshal(raw, &event); err != nil || event.Baseline != 0 || event.Count != 9 || len(event.Add) != 6 || event.Add[2] != "new-file" || len(event.Uncounted) != 2 || event.Uncounted[0] != "*" || event.Uncounted[1] != "dir/" {
		t.Fatalf("event %s %v", raw, err)
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	assertScopeTransition(t, data, before, "stopped")
	if data["limit"] != float64(8) {
		t.Fatalf("event %s", raw)
	}
	stopped, err := s.Get(ctx, 2)
	if err != nil || stopped.Status != "stopped" || stopped.LeaseExpiresAt != nil {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
}

func assertScopeTransition(t *testing.T, data map[string]any, before *Job, to string) {
	t.Helper()
	if data["to"] != to || before.LeaseHost == nil || data["leaseHost"] != *before.LeaseHost || data["generation"] != float64(before.Generation) || data["epoch"] != float64(before.LeaseEpoch) {
		t.Fatalf("transition: %v before: %+v", data, before)
	}
	phase, present := before.View["phase"]
	saved, exists := data["phase"]
	if present != exists || !reflect.DeepEqual(saved, phase) {
		t.Fatalf("phase: %v present=%v before=%v present=%v", saved, exists, phase, present)
	}
}
