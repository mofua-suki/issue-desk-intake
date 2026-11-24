package jobs

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"testing"
	"time"
)

func TestTestOnlyOverlapDoesNotBlockProductionScopes(t *testing.T) {
	for _, tc := range []struct {
		left, right []string
		want        bool
	}{
		{[]string{"src/a_test.go"}, []string{"src/"}, false},
		{[]string{"tests/"}, []string{"tests/helper.go"}, false},
		{[]string{"src/a.test.ts"}, []string{"src/a.test.ts"}, false},
		{[]string{"test_a.py", "a_test.py"}, []string{"*"}, false},
		{[]string{"__tests__/fixture.go"}, []string{"__tests__/fixture.go"}, false},
		{[]string{"src/a.go", "src/a_test.go"}, []string{"src/b.go", "src/a_test.go"}, false},
		{[]string{"src/a.go", "src/a_test.go"}, []string{"src/a.go"}, true},
		{[]string{"src/"}, []string{"src/a.go"}, true},
		{[]string{"*"}, []string{"src/"}, true},
		{[]string{"testimony.go"}, []string{"testimony.go"}, true},
		{[]string{"src/a.go.test/"}, []string{"src/a.go.test/production.go"}, true},
	} {
		if got := ScopeOverlap(tc.left, tc.right, nil); got != tc.want {
			t.Errorf("%v / %v = %v", tc.left, tc.right, got)
		}
		if got := ScopeOverlap(tc.right, tc.left, nil); got != tc.want {
			t.Errorf("reverse %v / %v = %v", tc.left, tc.right, got)
		}
	}
}

func TestPostgresTestOverlapDispatchAndWidenKeepDeclaration(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now()
	_, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,2)`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at,filed_at,view) VALUES
 (1,'owner/repo',1,'running',ARRAY['src/production.go','tests/shared.go','src/production_test.go','testdata/shared.json'],'declared','other',1,$1,$2,'{}'),
 (2,'owner/repo',2,'queued',ARRAY['src/independent.go','tests/shared.go'],'declared',NULL,0,NULL,$2,'{}')`, at.Add(time.Hour), at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":2,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw)
	a, err := fleet.NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := New(db, Options{Now: func() time.Time { return at }})
	job, _, err := s.Next(ctx, "nas", 2, a)
	if err != nil || job == nil || job.ID != 2 {
		t.Fatalf("dispatch: %v %v", job, err)
	}
	if !reflect.DeepEqual(job.Scope, []string{"src/independent.go", "tests/shared.go"}) {
		t.Fatal("declaration lost", job.Scope)
	}
	got, err := s.Widen(ctx, job.ID, "nas", job.LeaseEpoch, []string{"src/production_test.go", "testdata/shared.json"}, nil)
	if err != nil || got.Result != "granted" || got.Job.Status != "running" {
		t.Fatalf("test widening: %+v %v", got, err)
	}
	if !reflect.DeepEqual(got.Job.Scope, []string{"src/independent.go", "tests/shared.go", "src/production_test.go", "testdata/shared.json"}) {
		t.Fatal("widened declaration lost", got.Job.Scope)
	}
	got, err = s.Widen(ctx, job.ID, "nas", job.LeaseEpoch, []string{"src/production.go"}, nil)
	if err != nil || got.Result != "wait" || len(got.Blockers) != 1 {
		t.Fatalf("production widening: %+v %v", got, err)
	}
}
