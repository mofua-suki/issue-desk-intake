package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/jobs"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/fleet"
)

func insertAttentionPrerequisite(t *testing.T, s *Service, issue int, generation int, view any) {
	t.Helper()
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	result := `{"status":"failed","questions":[],"prerequisiteMissing":{"reason":"成果物が不足","artifacts":[{"artifact":"成果物","repo":"owner/repo","issue":99}]}}`
	_, err = s.PostgreSQL.Exec(`INSERT INTO jobs(repo,issue,title,status,hold_reason,generation,view,result) VALUES('owner/repo',$1,'test','held','question',$2,$3::jsonb,$4::jsonb)`, issue, generation, string(raw), result)
	if err != nil {
		t.Fatal(err)
	}
}

func TestJobAttentionListsStoredQuestionPrerequisite(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	insertAttentionPrerequisite(t, s, 1, 2, map[string]any{"prerequisiteMissingGenerations": []int{1, 2}})
	insertAttentionPrerequisite(t, s, 2, 3, map[string]any{"prerequisiteMissingGenerations": []int{3}})
	insertAttentionPrerequisite(t, s, 3, 3, map[string]any{"prerequisiteMissingGenerations": []int{2}})
	insertAttentionPrerequisite(t, s, 4, 3, map[string]any{})
	rows, err := s.jobAttention(context.Background(), map[string]bool{"owner/repo": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected two prerequisite waits: %v", rows)
	}
	for i, raw := range rows {
		row := fleet.Object(raw)
		if row["kind"] != "prerequisite_missing" || row["number"] != int64(i+1) {
			t.Fatalf("unexpected row: %v", row)
		}
	}
}

func TestJobAttentionShowsResolutionState(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	cases := []struct{ state, reason, want string }{
		{"rejected", "実装の回数の上限（4回）に達しました。", "実装の回数の上限（4回）に達しました。"},
		{"limit_reached", "", "上限（3回）"}, {"cycle", "", "循環"}, {"out_of_scope", "", "other/outside-repo"},
		{"connected", "", "依存を接続"}, {"retried", "", "再試行しました"},
	}
	for i, tc := range cases {
		view := map[string]any{"prerequisiteMissingGenerations": []int{1}, "prerequisiteResolution": map[string]any{"hash": map[string]any{"state": tc.state, "reason": tc.reason, "artifact": map[string]any{"artifact": "成果物", "repo": "other/outside-repo"}}}}
		insertAttentionPrerequisite(t, s, i+1, 1, view)
	}
	insertAttentionPrerequisite(t, s, len(cases)+1, 1, map[string]any{"prerequisiteMissingGenerations": []int{1}})
	rows, err := s.jobAttention(context.Background(), map[string]bool{"owner/repo": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(cases)+1 {
		t.Fatalf("unexpected rows: %v", rows)
	}
	for i, raw := range rows {
		row := fleet.Object(raw)
		reason := fleet.Text(row["reason"])
		if row["kind"] != "prerequisite_missing" || row["number"] != int64(i+1) || !strings.Contains(reason, "成果物が不足") {
			t.Fatalf("row %d: %v", i, row)
		}
		if i < len(cases) && (!strings.Contains(reason, cases[i].want) || !strings.Contains(reason, cases[i].state)) {
			t.Errorf("state %s: %s", cases[i].state, reason)
		}
		if i == len(cases) && !strings.Contains(reason, "成果物を確認してから") {
			t.Fatalf("missing traditional guidance: %s", reason)
		}
	}
}

func resolverFixture(t *testing.T, producerStatus string, artifactIssue int) (*PrerequisiteResolver, *jobs.Store) {
	t.Helper()
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	at := s.now()
	raw := map[string]any{}
	if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1},"owner/prior":{"rank":1}}}},"repositories":{}}`), &raw); err != nil {
		t.Fatal(err)
	}
	a, err := fleet.NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	s.Assignments = a
	store, err := jobs.New(s.PostgreSQL, jobs.Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostgreSQL.Exec(`INSERT INTO hosts(host,ready,heartbeat_at,capacity) VALUES('nas',true,$1,1)`, at); err != nil {
		t.Fatal(err)
	}
	result := fmt.Sprintf(`{"status":"failed","questions":[],"prerequisiteMissing":{"reason":"成果物が不足","artifacts":[{"artifact":"成果物","repo":"owner/prior","issue":%d}]}}`, artifactIssue)
	_, err = s.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,title,status,hold_reason,generation,view,result) VALUES(1,'owner/repo',1,'wait','held','question',1,'{"prerequisiteMissingGenerations":[1]}',$1::jsonb),(2,'owner/prior',2,'producer',$2,CASE WHEN $2='held' THEN 'question' ELSE NULL END,0,'{}','{}'),(3,'owner/prior',3,'existing','merged',NULL,0,'{}','{}')`, result, producerStatus)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostgreSQL.Exec(`INSERT INTO job_deps(job_id,depends_on) VALUES(1,3)`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostgreSQL.Exec(`UPDATE jobs SET status='running',hold_reason=NULL,view='{}',result='{}',lease_host='nas',lease_epoch=1,lease_expires_at=$1 WHERE id=1`, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var resultObject map[string]any
	if err = json.Unmarshal([]byte(result), &resultObject); err != nil {
		t.Fatal(err)
	}
	reason := "prerequisite_missing"
	if _, err = store.Report(t.Context(), 1, "nas", jobs.Report{Epoch: 1, Status: "held", HoldReason: &reason, Result: resultObject}); err != nil {
		t.Fatal(err)
	}
	return &PrerequisiteResolver{Service: s, Store: store, Control: jobs.ControlOptions{RunLimit: 4, UsageLimit: 100}}, store
}

func resolutionSnapshot(t *testing.T, r *PrerequisiteResolver) (string, int64, []int64, map[string]any) {
	t.Helper()
	var status string
	var generation int64
	var raw []byte
	if err := r.Service.PostgreSQL.QueryRow(`SELECT status,generation,view FROM jobs WHERE id=1`).Scan(&status, &generation, &raw); err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	rows, err := r.Service.PostgreSQL.Query(`SELECT depends_on FROM job_deps WHERE job_id=1 ORDER BY depends_on`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var deps []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		deps = append(deps, id)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	resolutions := fleet.Object(view["prerequisiteResolution"])
	if len(resolutions) != 1 {
		t.Fatalf("resolution=%v", resolutions)
	}
	for _, v := range resolutions {
		return status, generation, deps, fleet.Object(v)
	}
	panic("unreachable")
}

func TestPrerequisiteResolverConnectsOpenProducer(t *testing.T) {
	for _, status := range []string{"queued", "running", "held", "failed"} {
		t.Run(status, func(t *testing.T) {
			r, store := resolverFixture(t, status, 42)
			if _, err := r.Service.DB.Exec(`INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES(42,'owner/prior',2,'now')`); err != nil {
				t.Fatal(err)
			}
			if err := r.Resolve(t.Context()); err != nil {
				t.Fatal(err)
			}
			got, gen, deps, record := resolutionSnapshot(t, r)
			if got != "queued" || gen != 2 || !reflect.DeepEqual(deps, []int64{2, 3}) || record["state"] != "connected" {
				t.Fatalf("%s %d %v %v", got, gen, deps, record)
			}
			job, _, err := store.Next(t.Context(), "nas", 1, r.Service.Assignments, []string{"owner/repo"})
			if err != nil || job != nil {
				t.Fatalf("premature dispatch: %v %v", job, err)
			}
			if _, err = r.Service.PostgreSQL.Exec(`UPDATE jobs SET status='merged',hold_reason=NULL WHERE id=2`); err != nil {
				t.Fatal(err)
			}
			job, _, err = store.Next(t.Context(), "nas", 1, r.Service.Assignments, []string{"owner/repo"})
			if err != nil || job == nil || job.ID != 1 {
				t.Fatalf("dispatch after merge: %v %v", job, err)
			}
		})
	}
}

func TestPrerequisiteResolverRetriesWhenProducerMerged(t *testing.T) {
	r, _ := resolverFixture(t, "merged", 2)
	if err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, gen, deps, record := resolutionSnapshot(t, r)
	if status != "queued" || gen != 2 || !reflect.DeepEqual(deps, []int64{3}) || record["state"] != "retried" || fleet.Number(record["attempts"]) != 1 {
		t.Fatalf("%s %d %v %v", status, gen, deps, record)
	}
}

func TestPrerequisiteResolverRejectsCycleAndKeepsDeps(t *testing.T) {
	r, _ := resolverFixture(t, "queued", 2)
	if _, err := r.Service.PostgreSQL.Exec(`INSERT INTO job_deps(job_id,depends_on) VALUES(2,1)`); err != nil {
		t.Fatal(err)
	}
	err := r.Resolve(t.Context())
	var p *contract.Problem
	if !errors.As(err, &p) || p.Status != 409 || p.Code != "acceptance_transfer_cycle" {
		t.Fatalf("cycle: %v", err)
	}
	status, gen, deps, record := resolutionSnapshot(t, r)
	if status != "held" || gen != 1 || !reflect.DeepEqual(deps, []int64{3}) || record["state"] != "cycle" {
		t.Fatalf("%s %d %v %v", status, gen, deps, record)
	}
	var edges int
	if err = r.Service.PostgreSQL.QueryRow(`SELECT count(*) FROM job_deps`).Scan(&edges); err != nil || edges != 2 {
		t.Fatalf("edges=%d err=%v", edges, err)
	}
	rows, err := r.Service.jobAttention(t.Context(), map[string]bool{"owner/repo": true})
	if err != nil || len(rows) != 1 || !strings.Contains(fleet.Text(fleet.Object(rows[0])["reason"]), "循環") {
		t.Fatalf("attention=%v err=%v", rows, err)
	}
}

func TestPrerequisiteResolverAmbiguousNumbersStayHeld(t *testing.T) {
	r, _ := resolverFixture(t, "queued", 2)
	if _, err := r.Service.DB.Exec(`INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES(2,'owner/prior',3,'now')`); err != nil {
		t.Fatal(err)
	}
	if err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	var status string
	var gen int64
	if err := r.Service.PostgreSQL.QueryRow(`SELECT status,generation FROM jobs WHERE id=1`).Scan(&status, &gen); err != nil || status != "queued" || gen != 2 {
		t.Fatalf("%s %d %v", status, gen, err)
	}
}

func TestPrerequisiteResolverHonorsRetryLimits(t *testing.T) {
	t.Run("run limit", func(t *testing.T) {
		r, _ := resolverFixture(t, "merged", 2)
		if _, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET generation=5,view='{"prerequisiteMissingGenerations":[5]}' WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		if err := r.Resolve(t.Context()); err != nil {
			t.Fatal(err)
		}
		status, gen, _, record := resolutionSnapshot(t, r)
		if status != "held" || gen != 5 || record["state"] != "rejected" || fleet.Number(record["attempts"]) != 0 || !strings.Contains(fleet.Text(record["reason"]), "上限") {
			t.Fatalf("%s %d %v", status, gen, record)
		}
	})
	t.Run("same missing limit", func(t *testing.T) {
		r, _ := resolverFixture(t, "merged", 2)
		for attempt := 1; attempt <= 3; attempt++ {
			if err := r.Resolve(t.Context()); err != nil {
				t.Fatal(err)
			}
			status, gen, _, record := resolutionSnapshot(t, r)
			if status != "queued" || gen != int64(attempt+1) || fleet.Number(record["attempts"]) != float64(attempt) {
				t.Fatalf("%s %d %v", status, gen, record)
			}
			if _, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET status='held',hold_reason='question',view=jsonb_set(view,'{prerequisiteMissingGenerations}',(view->'prerequisiteMissingGenerations') || to_jsonb(generation)) WHERE id=1`); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Resolve(t.Context()); err != nil {
			t.Fatal(err)
		}
		status, gen, _, record := resolutionSnapshot(t, r)
		if status != "held" || gen != 4 || record["state"] != "limit_reached" || fleet.Number(record["attempts"]) != 3 {
			t.Fatalf("%s %d %v", status, gen, record)
		}
	})
}

func TestPrerequisiteResolverPrioritizesOpenProducer(t *testing.T) {
	r, _ := resolverFixture(t, "queued", 2)
	if _, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET result='{"status":"failed","questions":[],"prerequisiteMissing":{"artifacts":[{"artifact":"a","repo":"owner/prior","issue":3},{"artifact":"b","repo":"owner/prior","issue":2}]}}' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, gen, deps, record := resolutionSnapshot(t, r)
	if status != "queued" || gen != 2 || !reflect.DeepEqual(deps, []int64{2, 3}) || record["state"] != "connected" {
		t.Fatalf("%s %d %v %v", status, gen, deps, record)
	}
}

func TestPrerequisiteResolverIsIdempotentAcrossCycles(t *testing.T) {
	for _, tc := range []struct {
		producer, state string
		deps            []int64
	}{
		{"queued", "connected", []int64{2, 3}}, {"merged", "retried", []int64{3}},
	} {
		t.Run(tc.producer, func(t *testing.T) {
			r, _ := resolverFixture(t, tc.producer, 2)
			errs := make(chan error, 2)
			for i := 0; i < 2; i++ {
				go func() { errs <- r.Resolve(context.Background()) }()
			}
			for i := 0; i < 2; i++ {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
			}
			reopened := &PrerequisiteResolver{Service: r.Service, Store: r.Store, Control: r.Control}
			if err := reopened.Resolve(t.Context()); err != nil {
				t.Fatal(err)
			}
			status, gen, deps, record := resolutionSnapshot(t, r)
			if status != "queued" || gen != 2 || !reflect.DeepEqual(deps, tc.deps) || record["state"] != tc.state || fleet.Number(record["attempts"]) != 1 {
				t.Fatalf("%s %d %v %v", status, gen, deps, record)
			}
		})
	}
}

func TestPrerequisiteResolverRespectsApprovalAndScope(t *testing.T) {
	for _, tc := range []struct{ name, view, result string }{
		{"approval", `{"prerequisiteMissingGenerations":[1],"permissionHold":{"reason":"approval"}}`, ``},
		{"stop", `{"prerequisiteMissingGenerations":[1],"stopRequested":true}`, ``},
		{"outside", `{"prerequisiteMissingGenerations":[1]}`, `{"status":"failed","questions":[],"prerequisiteMissing":{"reason":"missing","artifacts":[{"artifact":"x","repo":"outside/repo","issue":2}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := resolverFixture(t, "queued", 2)
			if _, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET view=$1::jsonb,result=CASE WHEN $2='' THEN result ELSE $2::jsonb END WHERE id=1`, tc.view, tc.result); err != nil {
				t.Fatal(err)
			}
			if err := r.Resolve(t.Context()); err != nil {
				t.Fatal(err)
			}
			var status string
			var generation int64
			var deps int
			if err := r.Service.PostgreSQL.QueryRow(`SELECT status,generation,(SELECT count(*) FROM job_deps WHERE job_id=1) FROM jobs WHERE id=1`).Scan(&status, &generation, &deps); err != nil || status != "held" || generation != 1 || deps != 1 {
				t.Fatalf("%s %d %d %v", status, generation, deps, err)
			}
		})
	}
}

func TestPrerequisiteResolverKeepsAttemptsWhenArtifactsReordered(t *testing.T) {
	r, _ := resolverFixture(t, "merged", 2)
	first := `{"status":"failed","questions":[],"prerequisiteMissing":{"reason":"missing","artifacts":[{"artifact":"a","repo":"owner/prior","issue":2},{"artifact":"b","repo":"owner/prior","issue":3}]}}`
	second := `{"status":"failed","questions":[],"prerequisiteMissing":{"reason":"missing","artifacts":[{"artifact":"b","repo":"owner/prior","issue":3},{"artifact":"a","repo":"owner/prior","issue":2}]}}`
	if _, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET result=$1::jsonb WHERE id=1`, first); err != nil {
		t.Fatal(err)
	}
	if err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET status='held',hold_reason='question',result=$1::jsonb,view=jsonb_set(view,'{prerequisiteMissingGenerations}',(view->'prerequisiteMissingGenerations') || to_jsonb(generation)) WHERE id=1`, second); err != nil {
		t.Fatal(err)
	}
	if err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, gen, _, record := resolutionSnapshot(t, r)
	if status != "queued" || gen != 3 || fleet.Number(record["attempts"]) != 2 {
		t.Fatalf("%s %d %v", status, gen, record)
	}
}

func TestPrerequisiteResolverKeepsAttemptsWhenArtifactTextChanges(t *testing.T) {
	r, _ := resolverFixture(t, "merged", 2)
	result := func(text string) string {
		return `{"status":"failed","questions":[],"prerequisiteMissing":{"reason":"missing","artifacts":[{"artifact":"` + text + `","repo":"owner/prior","issue":2}]}}`
	}
	if _, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET result=$1::jsonb WHERE id=1`, result("first wording")); err != nil {
		t.Fatal(err)
	}
	if err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET status='held',hold_reason='question',result=$1::jsonb,view=jsonb_set(view,'{prerequisiteMissingGenerations}',(view->'prerequisiteMissingGenerations') || to_jsonb(generation)) WHERE id=1`, result("second wording")); err != nil {
		t.Fatal(err)
	}
	if err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, gen, _, record := resolutionSnapshot(t, r)
	if status != "queued" || gen != 3 || fleet.Number(record["attempts"]) != 2 {
		t.Fatalf("%s %d %v", status, gen, record)
	}
}

func TestPrerequisiteResolverAmbiguousRepoCaseStaysHeld(t *testing.T) {
	r, _ := resolverFixture(t, "queued", 2)
	if _, err := r.Service.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,title,status) VALUES(4,'OWNER/PRIOR',2,'duplicate','queued')`); err != nil {
		t.Fatal(err)
	}
	if err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	var status string
	var generation int64
	if err := r.Service.PostgreSQL.QueryRow(`SELECT status,generation FROM jobs WHERE id=1`).Scan(&status, &generation); err != nil || status != "queued" || generation != 2 {
		t.Fatalf("%s %d %v", status, generation, err)
	}
}

func holdAgain(t *testing.T, r *PrerequisiteResolver) {
	t.Helper()
	_, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET status='held',hold_reason='question',view=jsonb_set(view,'{prerequisiteMissingGenerations}',(view->'prerequisiteMissingGenerations') || to_jsonb(generation)) WHERE id=1`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrerequisiteResolverSelfArtifactResumesThenFromBase(t *testing.T) {
	r, _ := resolverFixture(t, "merged", 2)
	_, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET result=jsonb_set(result,'{prerequisiteMissing,artifacts}', '[{"artifact":"x","repo":"owner/repo","issue":1}]'), view=view || '{"continuation":{"branch":"work"}}'::jsonb WHERE id=1`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if err := r.Resolve(t.Context()); err != nil {
			t.Fatal(err)
		}
		status, gen, _, rec := resolutionSnapshot(t, r)
		var view map[string]any
		var raw []byte
		if err := r.Service.PostgreSQL.QueryRow(`SELECT view FROM jobs WHERE id=1`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &view); err != nil {
			t.Fatal(err)
		}
		if status != "queued" || gen != int64(i+1) || fleet.Number(rec["attempts"]) != float64(i) {
			t.Fatalf("%s %d %v", status, gen, rec)
		}
		if i == 1 && (view["retryFromBase"] != nil || view["continuation"] == nil) {
			t.Fatal(view)
		}
		if i == 2 && (view["retryFromBase"] == nil || len(fleet.List(view["instructions"])) == 0) {
			t.Fatal(view)
		}
		var instruction bool
		if err := r.Service.PostgreSQL.QueryRow(`SELECT (data->>'instruction')::boolean FROM job_events WHERE job_id=1 AND kind='control' AND (data->>'generation')::bigint=$1`, gen).Scan(&instruction); err != nil {
			t.Fatal(err)
		}
		if instruction != (i == 2) {
			t.Fatalf("resume %d: instruction = %t", i, instruction)
		}
		if i == 1 {
			holdAgain(t, r)
		}
	}
}

func TestPrerequisiteResolverUnknownProducerBoundedRetries(t *testing.T) {
	for _, status := range []string{"absent", "closed", "stopped", "ambiguous"} {
		t.Run(status, func(t *testing.T) {
			r, _ := resolverFixture(t, "merged", 2)
			switch status {
			case "absent":
				_, _ = r.Service.PostgreSQL.Exec(`DELETE FROM jobs WHERE id=2`)
			case "closed", "stopped":
				_, _ = r.Service.PostgreSQL.Exec(`UPDATE jobs SET status=$1 WHERE id=2`, status)
			case "ambiguous":
				_, _ = r.Service.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,title,status) VALUES(4,'OWNER/PRIOR',2,'duplicate','queued')`)
			}
			calls := 0
			r.retry = func(ctx context.Context, tx *sql.Tx, id, gen int64, opt jobs.ControlOptions, base ...bool) error {
				calls++
				return r.Store.RetryPrerequisiteTx(ctx, tx, id, gen, opt, base...)
			}
			at := r.Service.now()
			r.Service.Now = func() time.Time { return at }
			for i := 1; i <= 4; i++ {
				if err := r.Resolve(t.Context()); err != nil {
					t.Fatal(err)
				}
				st, gen, _, rec := resolutionSnapshot(t, r)
				expected := i
				if expected > 3 {
					expected = 3
				}
				if calls != expected || gen != int64(1+expected) || fleet.Number(rec["attempts"]) != float64(expected) || (i == 4 && (st != "held" || rec["state"] != "limit_reached")) {
					t.Fatalf("%s %d %v", st, gen, rec)
				}
				if i < 4 {
					holdAgain(t, r)
					at = at.Add(29*time.Minute + 59*time.Second)
					if err := r.Resolve(t.Context()); err != nil {
						t.Fatal(err)
					}
					_, g, _, v := resolutionSnapshot(t, r)
					if calls != i || g != gen || fleet.Number(v["attempts"]) != float64(i) {
						t.Fatal(g, v)
					}
					at = at.Add(time.Second)
				}
			}
		})
	}
	t.Run("outside", func(t *testing.T) {
		r, _ := resolverFixture(t, "merged", 2)
		lookups := 0
		r.lookupProducer = func(ctx context.Context, tx *sql.Tx, a prerequisiteArtifact) (*prerequisiteProducer, error) {
			lookups++
			return r.producer(ctx, tx, a)
		}
		_, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET result=jsonb_set(result,'{prerequisiteMissing,artifacts}', '[{"artifact":"x","repo":"other/outside-repo","issue":2}]') WHERE id=1`)
		if err != nil {
			t.Fatal(err)
		}
		if err = r.Resolve(t.Context()); err != nil {
			t.Fatal(err)
		}
		st, g, _, rec := resolutionSnapshot(t, r)
		rows, err := r.Service.jobAttention(t.Context(), map[string]bool{"owner/repo": true})
		if lookups != 0 || err != nil || st != "held" || g != 1 || rec["state"] != "out_of_scope" || len(rows) != 1 || !strings.Contains(fleet.Text(fleet.Object(rows[0])["reason"]), "other/outside-repo") {
			t.Fatal(st, g, rec, rows, err)
		}
	})
}

func TestPrerequisiteResolverLimitKeepsImplementationRuns(t *testing.T) {
	r, _ := resolverFixture(t, "merged", 2)
	for i := 0; i < 3; i++ {
		if err := r.Resolve(t.Context()); err != nil {
			t.Fatal(err)
		}
		holdAgain(t, r)
	}
	calls := 0
	r.retry = func(ctx context.Context, tx *sql.Tx, id, gen int64, opt jobs.ControlOptions, base ...bool) error {
		calls++
		return r.Store.RetryPrerequisiteTx(ctx, tx, id, gen, opt, base...)
	}
	before, err := r.Store.Get(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := r.Store.Get(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || jobs.ImplementationRuns(before) != jobs.ImplementationRuns(after) {
		t.Fatal(before, after)
	}
}

func TestPrerequisiteResolverControlRefusalSurfacesInAttention(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup string
		limit float64
		want  string
	}{
		{"runs", `UPDATE jobs SET generation=5,view='{"prerequisiteMissingGenerations":[5]}' WHERE id=1`, 100, "実装の回数の上限（4回）に達しました。"},
		{"usage", `INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES('u',now(),'nas','owner/repo',1,'{"repo":"owner/repo","issue":1,"provider":"codex","inputTokens":101,"cacheReadTokens":0,"outputTokens":0,"step":"実装"}','test')`, 100, "累計使用量"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := resolverFixture(t, "merged", 2)
			r.Control.UsageLimit = tc.limit
			at := r.Service.now()
			r.Service.Now = func() time.Time { return at }
			calls := 0
			r.retry = func(ctx context.Context, tx *sql.Tx, id, gen int64, opt jobs.ControlOptions, base ...bool) error {
				calls++
				err := r.Store.RetryPrerequisiteTx(ctx, tx, id, gen, opt, base...)
				var p *contract.Problem
				if calls == 1 && (!errors.As(err, &p) || p.Status != 409) {
					t.Errorf("retry expected 409: %v", err)
				}
				return err
			}
			if _, err := r.Service.PostgreSQL.Exec(tc.setup); err != nil {
				t.Fatal(err)
			}
			if tc.name == "runs" {
				if _, err := r.Service.PostgreSQL.Exec(`INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES('u',now(),'nas','owner/repo',1,'{"repo":"owner/repo","issue":1,"provider":"codex","inputTokens":1,"cacheReadTokens":0,"outputTokens":0,"step":"実装"}','test')`); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if err := r.Resolve(t.Context()); err != nil {
					t.Fatal(err)
				}
				st, gen, _, rec := resolutionSnapshot(t, r)
				expected := int64(1)
				if tc.name == "runs" {
					expected = 5
				}
				rows, err := r.Service.jobAttention(t.Context(), map[string]bool{"owner/repo": true})
				if calls != 1 || err != nil || st != "held" || gen != expected || rec["state"] != "rejected" || fleet.Number(rec["attempts"]) != 0 || len(rows) != 1 || !strings.Contains(fleet.Text(fleet.Object(rows[0])["reason"]), tc.want) {
					t.Fatal(st, gen, rec, rows, err)
				}
			}
			// A later check picks up a changed admission limit without consuming an attempt on refusal.
			at = at.Add(30 * time.Minute)
			r.Control.RunLimit = 10
			r.Control.UsageLimit = 1000
			if err := r.Resolve(t.Context()); err != nil {
				t.Fatal(err)
			}
			st, _, _, rec := resolutionSnapshot(t, r)
			if calls != 2 || st != "queued" || rec["state"] != "retried" || fleet.Number(rec["attempts"]) != 1 {
				t.Fatal(st, rec, calls)
			}
		})
	}
}

func TestPrerequisiteResolverAppliesToExistingHeldJobs(t *testing.T) {
	r, _ := resolverFixture(t, "merged", 2)
	var present bool
	if err := r.Service.PostgreSQL.QueryRow(`SELECT view ? 'prerequisiteResolution' FROM jobs WHERE id=1`).Scan(&present); err != nil || present {
		t.Fatalf("record exists=%v err=%v", present, err)
	}
	if err := r.Resolve(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, gen, _, record := resolutionSnapshot(t, r)
	if status != "queued" || gen != 2 || record["state"] != "retried" || fleet.Number(record["attempts"]) != 1 {
		t.Fatalf("%s %d %v", status, gen, record)
	}
}
