package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"testing"
	"time"

	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/pgtest"
)

func recoveryFixture(t *testing.T) *JobRecovery {
	t.Helper()
	return &JobRecovery{Service: &Service{DB: pgtest.New(t), PostgreSQL: postgres(t), Repositories: func() ([]string, error) { return []string{"owner/repo"}, nil }}, UsageLimit: 100, Limit: 100}
}
func recoveryExec(t *testing.T, r *JobRecovery, q string, args ...any) {
	t.Helper()
	if _, e := r.Service.PostgreSQL.Exec(q, args...); e != nil {
		t.Fatal(e)
	}
}
func recoveryInsert(t *testing.T, r *JobRecovery, n int, status string, hold any, kind, repo, view, result string, generation int, host any, age string) {
	t.Helper()
	recoveryExec(t, r, `INSERT INTO jobs(repo,issue,status,hold_reason,kind,view,result,generation,lease_host,updated_at) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8,$9,now()-$10::interval)`, repo, n, status, hold, kind, view, result, generation, host, age)
}

func TestRecoveryCandidateExclusionsPostgres(t *testing.T) {
	r := recoveryFixture(t)
	ctx := context.Background()
	rows := []struct {
		name, status             string
		hold                     any
		kind, repo, view, result string
		generation               int
		host                     any
		age                      string
	}{
		{"valid failed", "failed", nil, "normal", "owner/repo", `{}`, `{"error":"broken"}`, 0, nil, "1 hour"},
		{"valid question", "held", "question", "normal", "owner/repo", `{}`, `{"questions":["why?"]}`, 0, nil, "1 hour"},
		{"valid review", "held", "review_held", "normal", "owner/repo", `{"reviewHolds":0}`, `{"summary":"review","review":{"findings":["fix"]}}`, 0, nil, "1 hour"},
		{"recent", "failed", nil, "normal", "owner/repo", `{}`, `{"error":"x"}`, 0, nil, "1 minute"},
		{"stopped", "stopped", nil, "normal", "owner/repo", `{}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"stop requested", "failed", nil, "normal", "owner/repo", `{"stopRequested":true}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"stop queued", "failed", nil, "normal", "owner/repo", `{}`, `{"error":"x"}`, 0, "nas", "1 hour"},
		{"stop acked", "failed", nil, "normal", "owner/repo", `{}`, `{"error":"x"}`, 0, "nas", "1 hour"},
		{"investigation kind", "failed", nil, "investigation", "owner/repo", `{}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"investigation workType", "failed", nil, "normal", "owner/repo", `{"workType":"investigation"}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"prerequisite", "held", "question", "normal", "owner/repo", `{"prerequisiteMissingGenerations":[1]}`, `{"status":"failed","questions":[],"prerequisiteMissing":{"reason":"missing","artifacts":[{"artifact":"API","repo":"owner/repo","issue":1}]} }`, 1, nil, "1 hour"},
		{"transfer", "failed", nil, "normal", "owner/repo", `{"acceptanceTransferWait":{"blocking":true}}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"run limit", "failed", nil, "normal", "owner/repo", `{"workerExecution":{"implementationRuns":4}}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"run limit held", "held", "question", "normal", "owner/repo", `{"workerExecution":{"implementationRuns":4}}`, `{"questions":["x"]}`, 0, nil, "1 hour"},
		{"usage attention", "failed", nil, "normal", "owner/repo", `{}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"permission hold", "held", "review_held", "normal", "owner/repo", `{"permissionHold":{"v":1}}`, `{"summary":"x"}`, 0, nil, "1 hour"},
		{"permission text", "held", "review_held", "normal", "owner/repo", `{}`, `{"error":"権限の範囲を定めるファイルの変更","summary":"x"}`, 0, nil, "1 hour"},
		{"deletion hold", "held", "deletions", "normal", "owner/repo", `{"deletionApproval":{"head":"abc"}}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"deletion approval", "failed", nil, "normal", "owner/repo", `{"deletionApproval":{}}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"foreign", "failed", nil, "normal", "owner/other", `{}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"empty failed", "failed", nil, "normal", "owner/repo", `{}`, `{"error":" "}`, 0, nil, "1 hour"},
		{"empty question", "held", "question", "normal", "owner/repo", `{}`, `{"questions":[]}`, 0, nil, "1 hour"},
		{"empty review", "held", "review_held", "normal", "owner/repo", `{}`, `{"summary":"","review":{"findings":[]}}`, 0, nil, "1 hour"},
		{"usage text", "failed", nil, "normal", "owner/repo", `{}`, `{"error":"利用上限です"}`, 0, nil, "1 hour"},
		{"usage unknown", "failed", nil, "normal", "owner/repo", `{}`, `{"error":"x"}`, 1, nil, "1 hour"},
		{"retry marker is not limit", "failed", nil, "normal", "owner/repo", `{"usageLimitRetry":{"generation":1,"by":"test"}}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"usage exceeded", "failed", nil, "normal", "owner/repo", `{}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"retry without usage marker", "failed", nil, "normal", "owner/repo", `{}`, `{"error":"x"}`, 0, nil, "1 hour"},
		{"empty review metadata", "held", "review_held", "normal", "owner/repo", `{}`, `{"summary":"","review":{"decision":"hold","findings":[]}}`, 0, nil, "1 hour"},
	}
	for i, v := range rows {
		recoveryInsert(t, r, i+1, v.status, v.hold, v.kind, v.repo, v.view, v.result, v.generation, v.host, v.age)
	}
	for _, v := range []struct {
		n     int
		state string
	}{{7, "queued"}, {8, "acked"}} {
		recoveryExec(t, r, `INSERT INTO job_commands(id,repo,issue,kind,payload,target_host,status) VALUES($1,'owner/repo',$2,'stop','{}','nas',$3)`, fmt.Sprintf("00000000-0000-4000-8000-%012d", v.n), v.n, v.state)
	}
	if _, e := r.Service.DB.Exec(`INSERT INTO attention_cases(key,json) VALUES(?,?)`, "usage", `{"kind":"failed","state":"usage_wait","project":"owner/repo","number":15,"usageLimitRetry":{"provider":"codex","at":123}}`); e != nil {
		t.Fatal(e)
	}
	recoveryExec(t, r, `INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES('u',now(),'nas','owner/repo',27,$1,'test')`, `{"repo":"owner/repo","issue":27,"provider":"codex","inputTokens":101,"cacheReadTokens":0,"outputTokens":0,"step":"実装"}`)
	if _, e := r.Service.DB.Exec(`INSERT INTO attention_cases(key,json) VALUES(?,?)`, "normal-retry", `{"kind":"failed","state":"retrying","project":"owner/repo","number":28}`); e != nil {
		t.Fatal(e)
	}
	got, e := r.Candidates(ctx)
	if e != nil {
		t.Fatal(e)
	}
	want := []int64{1, 2, 3, 26, 28}
	if len(got) != len(want) {
		t.Fatalf("got %#v", got)
	}
	for i, c := range got {
		if c.Job.Issue != want[i] {
			t.Fatalf("got issue %d want %d", c.Job.Issue, want[i])
		}
	}
	records, e := r.records(ctx)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range records {
		if v.Issue == 11 {
			t.Fatal("prerequisite wait recorded as missing cause")
		}
	}
	for _, n := range []int64{21, 22, 23, 29} {
		found := false
		for _, v := range records {
			if v.Issue == n && v.ExclusionReason == "原因なし" && !v.ExclusionAt.IsZero() {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing cause not recorded: %d", n)
		}
	}
	r.Limit = 2
	got, e = r.Candidates(ctx)
	if e != nil || len(got) != 2 || got[0].Job.Issue != 1 || got[1].Job.Issue != 2 {
		t.Fatal(got, e)
	}
}

func TestRecoveryOutcomesAndReservationsPostgres(t *testing.T) {
	for _, state := range []string{"sent", "send_unknown", "send_rejected"} {
		t.Run(state, func(t *testing.T) {
			r := recoveryFixture(t)
			ctx := context.Background()
			c := RecoveryCandidate{Job: jobs.Job{ID: 42, Repo: "owner/repo", Issue: 1}, Cause: "A"}
			action, id, e := r.Reserve(ctx, c)
			if e != nil || action != "proceed" || !regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-5[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`).MatchString(id) {
				t.Fatal(action, id, e)
			}
			other := c
			other.Cause = "B"
			other.Job.Generation++
			action, _, e = r.Reserve(ctx, other)
			if e != nil || action != "already_reserved" {
				t.Fatal(action, e)
			}
			if e = r.Finish(ctx, c, id, state); e != nil {
				t.Fatal(e)
			}
			action, id2, e := r.Reserve(ctx, other)
			if e != nil || action != "proceed" {
				t.Fatal(action, e)
			}
			if e = r.Finish(ctx, other, id2, "skipped"); e != nil {
				t.Fatal(e)
			}
			c.Job.Generation += 2
			action, _, e = r.Reserve(ctx, c)
			if e != nil || action != "abandon" {
				t.Fatal(action, e)
			}
			before, e := r.Service.OperationalSetting(ctx, "job-recovery")
			if e != nil {
				t.Fatal(e)
			}
			a, ok, e := abandonedCause(ctx, r, "owner/repo", 1)
			if e != nil || !ok || a.CauseHash != "A" || a.PreviousCause != "B" || a.At.IsZero() || a.SendCount != 1 || a.LastResult != state || !a.Abandoned {
				t.Fatal(a, ok, e)
			}
			after, e := r.Service.OperationalSetting(ctx, "job-recovery")
			if e != nil {
				t.Fatal(e)
			}
			b1, _ := json.Marshal(before)
			b2, _ := json.Marshal(after)
			if string(b1) != string(b2) {
				t.Fatal("read modified ledger")
			}
			if _, ok, e = abandonedCause(ctx, r, "owner/repo", 999); e != nil || ok {
				t.Fatal(ok, e)
			}
			recoveryInsert(t, r, 1, "failed", nil, "normal", "owner/repo", `{}`, `{"error":"x"}`, 0, nil, "1 hour")
			cause := recoveryCause(&jobs.Job{Status: "failed", Result: map[string]any{"error": "x"}})
			if e = r.ledger(ctx, func(rows *[]RecoveryRecord) error {
				(*rows)[0].Causes[cause] = RecoveryCause{State: "abandoned", At: time.Now()}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			got, e := r.Candidates(ctx)
			if e != nil || len(got) != 0 {
				t.Fatal(got, e)
			}
		})
	}
}

func TestRecoverySkippedAndDeterministicIDPostgres(t *testing.T) {
	r := recoveryFixture(t)
	ctx := context.Background()
	c := RecoveryCandidate{Job: jobs.Job{ID: 42, Repo: "owner/repo", Issue: 1}, Cause: "A"}
	act, id, e := r.Reserve(ctx, c)
	if e != nil || act != "proceed" {
		t.Fatal(act, e)
	}
	if e = r.Finish(ctx, c, id, "skipped"); e != nil {
		t.Fatal(e)
	}
	act, _, e = r.Reserve(ctx, c)
	if e != nil || act != "already_reserved" {
		t.Fatal(act, e)
	}
	if _, ok, e := abandonedCause(ctx, r, "owner/repo", 1); e != nil || ok {
		t.Fatal(ok, e)
	}
	c.Job.Generation++
	act, next, e := r.Reserve(ctx, c)
	if e != nil || act != "proceed" || id == next {
		t.Fatal(act, next, e)
	}
	// Independently recreating the same reservation gives the same ID.
	r2 := recoveryFixture(t)
	act, again, e := r2.Reserve(ctx, c)
	if e != nil || act != "proceed" || next != again {
		t.Fatal(act, again, e)
	}
}

func TestRecoveryRecheckChangesPostgres(t *testing.T) {
	r := recoveryFixture(t)
	ctx := context.Background()
	recoveryInsert(t, r, 1, "failed", nil, "normal", "owner/repo", `{}`, `{"error":"x"}`, 0, "nas", "1 hour")
	list, e := r.Candidates(ctx)
	if e != nil || len(list) != 1 {
		t.Fatal(list, e)
	}
	c := list[0]
	checks := []string{`UPDATE jobs SET status='queued'`, `UPDATE jobs SET generation=1`, `UPDATE jobs SET result='{"error":"different"}'`, `UPDATE jobs SET view='{"stopRequested":true}'`, `UPDATE jobs SET view='{"permissionHold":{"reason":"approval"}}'`, `UPDATE jobs SET view='{"deletionApproval":{}}'`, `UPDATE jobs SET kind='investigation'`}
	for _, q := range checks {
		recoveryExec(t, r, q)
		ok, e := r.Recheck(ctx, c)
		if e != nil || ok {
			t.Fatal(q, ok, e)
		}
		recoveryExec(t, r, `UPDATE jobs SET kind='normal',status='failed',generation=0,result='{"error":"x"}',view='{}'`)
	}
	for i, state := range []string{"queued", "acked", "done"} {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		recoveryExec(t, r, `INSERT INTO job_commands(id,repo,issue,kind,payload,target_host,status) VALUES($1,'owner/repo',1,'stop','{}','nas',$2)`, id, state)
		ok, e := r.Recheck(ctx, c)
		if e != nil || ok != (state == "done") {
			t.Fatal(state, ok, e)
		}
		recoveryExec(t, r, `DELETE FROM job_commands WHERE id=$1`, id)
	}
}

func TestRecoveryLedgerCapacityPostgres(t *testing.T) {
	r := recoveryFixture(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour)
	records := make([]RecoveryRecord, 200)
	for i := range records {
		state := "skipped"
		if i == 0 {
			state = "reserved"
		}
		records[i] = RecoveryRecord{Repo: "owner/repo", Issue: int64(i + 1), Cause: "A", Causes: map[string]RecoveryCause{"A": {State: state, At: at.Add(time.Duration(i) * time.Second)}}}
		recoveryInsert(t, r, i+1, "closed", nil, "normal", "owner/repo", `{}`, `{}`, 0, nil, "1 hour")
	}
	b, e := json.Marshal(records)
	if e != nil {
		t.Fatal(e)
	}
	recoveryExec(t, r, `INSERT INTO settings(key,value) VALUES('job-recovery',$1::jsonb)`, string(b))
	c := RecoveryCandidate{Job: jobs.Job{ID: 999, Repo: "owner/repo", Issue: 999}, Cause: "B"}
	act, _, e := r.Reserve(ctx, c)
	if e != nil || act != "proceed" {
		t.Fatal(act, e)
	}
	got, e := r.records(ctx)
	if e != nil || len(got) != 200 {
		t.Fatal(len(got), e)
	}
	present := map[int64]bool{}
	for _, v := range got {
		present[v.Issue] = true
	}
	if !present[1] || present[2] || !present[999] {
		t.Fatal(present[1], present[2], present[999])
	}
}

func TestRecoveryCauseNormalization(t *testing.T) {
	base := jobs.Job{Status: "failed", Result: map[string]any{"error": "error 2026-01-01T00:00:00Z aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}}
	other := jobs.Job{Status: "failed", Result: map[string]any{"error": "error 2026-02-02T12:30:00Z bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}}
	if recoveryCause(&base) == "" || recoveryCause(&base) != recoveryCause(&other) {
		t.Fatal("normalization mismatch")
	}
	review := "review_held"
	j := jobs.Job{Status: "held", HoldReason: &review, Result: map[string]any{"summary": "summary", "review": map[string]any{"findings": []any{"first"}}}}
	old := recoveryCause(&j)
	j.Result["review"] = map[string]any{"findings": []any{"second"}}
	if old == recoveryCause(&j) {
		t.Fatal("review findings ignored")
	}
	j.Result["summary"] = ""
	j.Result["review"] = map[string]any{"decision": "hold", "findings": []any{}}
	if recoveryCause(&j) != "" {
		t.Fatal("metadata is not a cause")
	}
}

func TestRecoveryEmptyCausePreservesReservationPostgres(t *testing.T) {
	r := recoveryFixture(t)
	ctx := context.Background()
	recoveryInsert(t, r, 1, "held", "question", "normal", "owner/repo", `{}`, `{"questions":[]}`, 0, nil, "1 hour")
	c := RecoveryCandidate{Job: jobs.Job{ID: 1, Repo: "owner/repo", Issue: 1}, Cause: "earlier"}
	act, id, e := r.Reserve(ctx, c)
	if e != nil || act != "proceed" {
		t.Fatal(act, e)
	}
	list, e := r.Candidates(ctx)
	if e != nil || len(list) != 0 {
		t.Fatal(list, e)
	}
	rows, e := r.records(ctx)
	if e != nil || len(rows) != 1 || rows[0].ExclusionReason != "原因なし" || rows[0].Causes["earlier"].ReservationID != id || rows[0].Causes["earlier"].State != "reserved" {
		t.Fatal(rows, e)
	}
}

func TestRecoveryPrerequisiteRecheckPostgres(t *testing.T) {
	r := recoveryFixture(t)
	ctx := context.Background()
	recoveryInsert(t, r, 1, "held", "question", "normal", "owner/repo", `{"prerequisiteMissingGenerations":[1]}`, `{"status":"failed","questions":["why?"],"prerequisiteMissing":{"reason":"missing","artifacts":[]}}`, 0, nil, "1 hour")
	list, err := r.Candidates(ctx)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	recoveryExec(t, r, `UPDATE jobs SET view='{"prerequisiteMissingGenerations":[0]}'`)
	ok, err := r.Recheck(ctx, list[0])
	if err != nil || ok {
		t.Fatal(ok, err)
	}
	list, err = r.Candidates(ctx)
	if err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
}

func TestRecoveryFindingBodiesAndExclusionJSON(t *testing.T) {
	review := "review_held"
	j := jobs.Job{Status: "held", HoldReason: &review, Result: map[string]any{"review": map[string]any{"findingSpecs": []any{map[string]any{"problem": "broken", "expected": "works", "test": "verify"}}}}}
	original := recoveryCause(&j)
	for _, key := range []string{"problem", "expected", "test"} {
		findings := j.Result["review"].(map[string]any)["findingSpecs"].([]any)[0].(map[string]any)
		previous := findings[key]
		findings[key] = "changed " + key
		if recoveryCause(&j) == original {
			t.Fatalf("%s ignored", key)
		}
		findings[key] = previous
	}
	b, err := json.Marshal(RecoveryRecord{})
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(b, &v); err != nil || v["exclusionAt"] != "0001-01-01T00:00:00Z" {
		t.Fatal(string(b), err)
	}
	var restored RecoveryRecord
	if err = json.Unmarshal(b, &restored); err != nil || !restored.ExclusionAt.IsZero() {
		t.Fatal(restored, err)
	}
}

func TestRecoveryCandidatesCapacityAndMalformedWaitPostgres(t *testing.T) {
	for _, scenario := range []string{"full", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			r := recoveryFixture(t)
			ctx := context.Background()
			recoveryInsert(t, r, 1000, "held", "question", "normal", "owner/repo", `{}`, `{"questions":[]}`, 0, nil, "1 hour")
			recoveryInsert(t, r, 1001, "failed", nil, "normal", "owner/repo", `{}`, `{"error":"failure"}`, 0, nil, "1 hour")
			if scenario == "full" {
				records := make([]RecoveryRecord, 201)
				for i := range records {
					records[i] = RecoveryRecord{Repo: "owner/repo", Issue: int64(i + 1), Causes: map[string]RecoveryCause{"x": {State: "sent"}}}
				}
				b, err := json.Marshal(records)
				if err != nil {
					t.Fatal(err)
				}
				recoveryExec(t, r, `INSERT INTO settings(key,value) VALUES('job-recovery',$1::jsonb)`, string(b))
			} else if _, err := r.Service.DB.Exec(`INSERT INTO attention_cases(key,json) VALUES(?,?)`, "bad", `{"state":"usage_wait"}`); err != nil {
				t.Fatal(err)
			}
			candidates, err := r.Candidates(ctx)
			if err != nil || len(candidates) != 1 || candidates[0].Job.Issue != 1001 {
				t.Fatal(candidates, err)
			}
		})
	}
}

func TestRecoveryMissingCauseDoesNotRewritePostgres(t *testing.T) {
	r := recoveryFixture(t)
	ctx := context.Background()
	recoveryInsert(t, r, 1, "held", "question", "normal", "owner/repo", `{}`, `{"questions":[]}`, 0, nil, "1 hour")
	if _, err := r.Candidates(ctx); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := r.Service.PostgreSQL.QueryRow(`SELECT updated_at::text FROM settings WHERE key='job-recovery'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Candidates(ctx); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := r.Service.PostgreSQL.QueryRow(`SELECT updated_at::text FROM settings WHERE key='job-recovery'`).Scan(&after); err != nil || before != after {
		t.Fatal(before, after, err)
	}
}
