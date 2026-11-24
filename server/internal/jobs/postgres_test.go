package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/store"
)

func testPostgres(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN is not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("tests require an explicitly named issue_desk_test database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var database string
	if err = admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&database); err != nil || !strings.HasPrefix(database, "issue_desk_test") {
		admin.Close()
		t.Fatal("test database verification failed")
	}
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := "jobs_test_" + hex.EncodeToString(id[:])
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	params := u.Query()
	params.Set("search_path", schema)
	u.RawQuery = params.Encode()
	db, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if _, err := admin.ExecContext(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("test schema cleanup failed: %v", err)
		}
		admin.Close()
	})
	if _, err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

func seedJob(t *testing.T, ctx context.Context, db *sql.DB, repo, host string, issue, epoch int64) int64 {
	t.Helper()
	var id int64
	err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at,view) VALUES($1,$2,'running',$3,$4,$5,'{"instructions":"server","reviewHolds":2,"reviewHoldsLowerBound":2,"reviewHoldsUnknown":false}') RETURNING id`, repo, issue, host, epoch, now.Add(time.Hour)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPostgresFencedReportsAndIdempotentReceipt(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 7)
	s, err := New(db, Options{Now: func() time.Time { return now }, OnMerged: func(ctx context.Context, tx *sql.Tx, job *Job, result map[string]any) error {
		return addEvent(ctx, tx, job.ID, "nas", "info", map[string]any{"mergedHook": true}, now)
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Renew(ctx, id, "other", 7, nil)
	code(t, err, 409, "lease_lost")
	_, _, err = s.Renew(ctx, id, "nas", 6, nil)
	code(t, err, 409, "lease_lost")
	job, lease, err := s.Renew(ctx, id, "nas", 7, map[string]any{"phase": "review"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Epoch != 7 || lease.ExpiresAt != iso(now.Add(15*time.Minute)) || job.ProgressAt == nil || job.View["instructions"] != "server" {
		t.Fatalf("renewal changed unrelated state: %+v", job)
	}
	who := TurnRequest{Host: "nas", JobID: &id, Epoch: 7}
	turn, err := s.AcquireTurn(ctx, "owner/repo", who)
	if err != nil || !turn.Granted {
		t.Fatalf("acquire: %v %+v", err, turn)
	}
	again, err := s.AcquireTurn(ctx, "owner/repo", who)
	if err != nil || again.Turn.Epoch != turn.Turn.Epoch || again.Turn.ExpiresAt != turn.Turn.ExpiresAt {
		t.Fatal("same-holder acquire changed turn")
	}
	pr := int64(42)
	report := Report{Epoch: 7, Status: "merged", PR: Optional[int64]{Present: true, Value: &pr}, View: map[string]any{"instructions": "overwrite", "reviewHolds": float64(1)}, Result: map[string]any{"mergeCommit": strings.Repeat("a", 40)}}
	job, err = s.Report(ctx, id, "nas", report)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "merged" || job.LeaseExpiresAt != nil || job.LeaseHost == nil || *job.LeaseHost != "nas" || job.View["instructions"] != "server" || job.ReviewHolds != int64(2) {
		t.Fatalf("invalid final state: %+v", job)
	}
	if _, err = s.Report(ctx, id, "nas", report); err != nil {
		t.Fatalf("identical terminal resend failed: %v", err)
	}
	var states, prs, hooks int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FILTER(WHERE kind='state'),count(*) FILTER(WHERE kind='pr_created'),count(*) FILTER(WHERE kind='info') FROM job_events WHERE job_id=$1", id).Scan(&states, &prs, &hooks); err != nil {
		t.Fatal(err)
	}
	if states != 1 || prs != 1 || hooks != 1 {
		t.Fatalf("receipt repeated side effects: states=%d prs=%d hooks=%d", states, prs, hooks)
	}
	var holder *string
	if err = db.QueryRowContext(ctx, "SELECT holder_host FROM merge_turns WHERE repo='owner/repo'").Scan(&holder); err != nil || holder != nil {
		t.Fatalf("terminal report did not release turn: %v", err)
	}
	report.Result = map[string]any{"mergeCommit": strings.Repeat("b", 40)}
	_, err = s.Report(ctx, id, "nas", report)
	code(t, err, 409, "lease_lost")
	_, _, err = s.Renew(ctx, id, "nas", 7, nil)
	code(t, err, 409, "lease_lost")
}

func TestPostgresTurnEpochAndConcurrentHolders(t *testing.T) {
	db, ctx := testPostgres(t)
	a := seedJob(t, ctx, db, "owner/repo", "pc-a", 1, 5)
	b := seedJob(t, ctx, db, "owner/repo", "pc-b", 2, 8)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	wa := TurnRequest{Host: "pc-a", JobID: &a, Epoch: 5}
	wb := TurnRequest{Host: "pc-b", JobID: &b, Epoch: 8}
	first, err := s.AcquireTurn(ctx, "owner/repo", wa)
	if err != nil || !first.Granted {
		t.Fatal(err)
	}
	blocked, err := s.AcquireTurn(ctx, "owner/repo", wb)
	if err != nil || blocked.Granted || blocked.Holder.Host != "pc-a" {
		t.Fatalf("two holders admitted: %v %+v", err, blocked)
	}
	wb.TurnEpoch = first.Turn.Epoch
	_, err = s.RenewTurn(ctx, "owner/repo", wb)
	code(t, err, 409, "turn_lost")
	wa.TurnEpoch = first.Turn.Epoch + 1
	code(t, s.ReleaseTurn(ctx, "owner/repo", wa), 409, "turn_lost")
	wa.TurnEpoch = first.Turn.Epoch
	if err = s.ReleaseTurn(ctx, "owner/repo", wa); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseTurn(ctx, "owner/repo", wa); err != nil {
		t.Fatalf("idempotent release failed: %v", err)
	}
	second, err := s.AcquireTurn(ctx, "owner/repo", wb)
	if err != nil || !second.Granted || second.Turn.Epoch != first.Turn.Epoch+1 {
		t.Fatalf("new holder did not get new epoch: %v %+v", err, second)
	}
	code(t, s.ReleaseTurn(ctx, "owner/repo", wa), 409, "turn_lost")
	wb.TurnEpoch = second.Turn.Epoch
	if err = s.ReleaseTurn(ctx, "owner/repo", wb); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan *TurnResult, 2)
	errors := make(chan error, 2)
	for _, who := range []TurnRequest{wa, wb} {
		wg.Add(1)
		go func(who TurnRequest) {
			defer wg.Done()
			result, err := s.AcquireTurn(ctx, "owner/repo", who)
			results <- result
			errors <- err
		}(who)
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	granted := 0
	for result := range results {
		if result.Granted {
			granted++
		}
	}
	if granted != 1 {
		t.Fatalf("concurrent acquires admitted %d holders", granted)
	}
}

func TestPostgresReportRollbackAndLeaseExpiry(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 3)
	sentinel := errors.New("hook failed")
	s, err := New(db, Options{Now: func() time.Time { return now }, OnMerged: func(context.Context, *sql.Tx, *Job, map[string]any) error { return sentinel }})
	if err != nil {
		t.Fatal(err)
	}
	who := TurnRequest{Host: "nas", JobID: &id, Epoch: 3}
	turn, err := s.AcquireTurn(ctx, "owner/repo", who)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Report(ctx, id, "nas", Report{Epoch: 3, Status: "merged", View: map[string]any{"reviewHolds": float64(2)}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("merged hook error lost: %v", err)
	}
	job, err := s.Get(ctx, id)
	if err != nil || job.Status != "running" || job.LeaseExpiresAt == nil {
		t.Fatalf("failed transaction changed job: %+v %v", job, err)
	}
	who.TurnEpoch = turn.Turn.Epoch
	if _, err = s.RenewTurn(ctx, "owner/repo", who); err != nil {
		t.Fatalf("failed transaction released turn: %v", err)
	}
	late, err := New(db, Options{Now: func() time.Time { return now.Add(2 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = late.Renew(ctx, id, "nas", 3, nil)
	code(t, err, 409, "lease_lost")
	_, err = late.AcquireTurn(ctx, "owner/repo", who)
	code(t, err, 409, "lease_lost")
}

func TestPostgresTurnExpiryAndClaimedRequest(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "nas", 1, 3)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	who := TurnRequest{Host: "nas", JobID: &id, Epoch: 3}
	first, err := s.AcquireTurn(ctx, "owner/repo", who)
	if err != nil {
		t.Fatal(err)
	}
	who.TurnEpoch = first.Turn.Epoch
	later, err := New(db, Options{Now: func() time.Time { return now.Add(16 * time.Minute) }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = later.RenewTurn(ctx, "owner/repo", who)
	code(t, err, 409, "turn_lost")
	second, err := later.AcquireTurn(ctx, "owner/repo", who)
	if err != nil || !second.Granted || second.Turn.Epoch != first.Turn.Epoch+1 {
		t.Fatalf("expired turn did not receive a new epoch: %+v %v", second, err)
	}
	code(t, later.ReleaseTurn(ctx, "owner/repo", who), 409, "turn_lost")
	_, err = db.ExecContext(ctx, `INSERT INTO requests(id,kind,repo,payload,sender,status,claimed_by) VALUES('manual-request','merge_manual','owner/manual','{}','chat','claimed','nas')`)
	if err != nil {
		t.Fatal(err)
	}
	requestID := "manual-request"
	request := TurnRequest{Host: "other", RequestID: &requestID}
	_, err = s.AcquireTurn(ctx, "owner/manual", request)
	code(t, err, 409, "turn_lost")
	request.Host = "nas"
	claimed, err := s.AcquireTurn(ctx, "owner/manual", request)
	if err != nil || !claimed.Granted {
		t.Fatalf("claimed manual merge denied: %+v %v", claimed, err)
	}
	request.TurnEpoch = claimed.Turn.Epoch
	if err = s.ReleaseTurn(ctx, "owner/manual", request); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, "UPDATE requests SET status='done' WHERE id=$1", requestID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AcquireTurn(ctx, "owner/manual", request)
	code(t, err, 409, "turn_lost")
}

func TestPostgresJobListPagination(t *testing.T) {
	db, ctx := testPostgres(t)
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,status,priority,filed_at) SELECT CASE WHEN n<=205 THEN 'owner/repo' ELSE 'other/repo' END,n,CASE WHEN n%2=0 THEN 'queued' ELSE 'running' END,(n/2)%2, '2025-01-01'::timestamptz + n * interval '1 second' FROM generate_series(1,210) n`)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	first, total, err := s.List(ctx, "owner/repo", nil, 200, 0)
	if err != nil || total != 205 || len(first) != 200 {
		t.Fatalf("first: %d %d %v", len(first), total, err)
	}
	var expected []int64
	rows, err := db.QueryContext(ctx, `SELECT id FROM jobs WHERE lower(repo)=lower('owner/repo') ORDER BY priority DESC,filed_at,id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for i, job := range first {
		if job.ID != expected[i] {
			t.Fatalf("order at %d", i)
		}
	}
	var got []int64
	for offset := 0; offset < 205; offset += 37 {
		page, count, err := s.List(ctx, "OWNER/REPO", nil, 37, offset)
		if err != nil || count != 205 {
			t.Fatalf("page: %d %v", count, err)
		}
		for _, j := range page {
			got = append(got, j.ID)
		}
	}
	if len(got) != len(expected) {
		t.Fatalf("count %d", len(got))
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Fatalf("page order at %d", i)
		}
	}
	filtered, count, err := s.List(ctx, "owner/repo", []string{"queued"}, 200, 0)
	if err != nil || count != 102 || len(filtered) != 102 {
		t.Fatalf("filter: %d %d %v", len(filtered), count, err)
	}
	empty, count, err := s.List(ctx, "owner/repo", nil, 200, 400)
	if err != nil || count != 205 || len(empty) != 0 {
		t.Fatalf("outside: %d %d %v", len(empty), count, err)
	}
}

func TestPostgresSweepExpiryResult(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, view, result string
		hasError           bool
	}{
		{"with error and prior result", `{"lastExecutionError":{"kind":"timeout","at":"2026-11-05T00:00:00Z"}}`, `{"status":"completed","summary":"old","keep":42}`, true},
		{"without error", `{}`, `{}`, false},
		{"stale stage", `{}`, `{"lastStage":"old","keep":42}`, false},
		{"empty stage", `{"phase":""}`, `{"lastStage":"old"}`, false},
		{"non-string stage", `{"phase":7}`, `{"lastStage":"old"}`, false},
		{"null result", `{}`, `null`, false},
		{"non-object error", `{"lastExecutionError":"bad"}`, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var id int64
			if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at,view,result) VALUES($1,1,'running','pc-last',1,$2,$3::jsonb,NULLIF($4,'null')::jsonb) RETURNING id`, "owner/"+strings.ReplaceAll(tc.name, " ", "-"), now.Add(-time.Minute), tc.view, tc.result).Scan(&id); err != nil {
				t.Fatal(err)
			}
			var original map[string]any
			if err := json.Unmarshal([]byte(tc.result), &original); err != nil {
				t.Fatal(err)
			}
			for n := 1; n <= 3; n++ {
				if n > 1 {
					if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_expires_at=$2 WHERE id=$1`, id, now.Add(-time.Minute)); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Sweep(ctx, nil); err != nil {
					t.Fatal(err)
				}
				detail, err := s.Detail(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				job := detail["job"].(*Job)
				if n < 3 {
					if job.Status != "queued" || !reflect.DeepEqual(job.Result, original) {
						t.Fatalf("expiry %d: %+v", n, job)
					}
				} else {
					want := "lease の期限切れが同じ世代で3回続いたため失敗にしました。最後のPC: pc-last"
					if job.Status != "failed" || job.Result["status"] != "failed" || job.Result["summary"] != want || job.Result["leaseExpiredCount"] != float64(3) || job.Result["lastHost"] != "pc-last" {
						t.Fatalf("failure: %+v", job)
					}
					if original["keep"] != nil && job.Result["keep"] != original["keep"] {
						t.Fatalf("lost existing result: %v", job.Result)
					}
					got, ok := job.Result["lastExecutionError"].(map[string]any)
					if ok != tc.hasError || ok && (got["kind"] != "timeout" || got["at"] != "2026-11-05T00:00:00Z") {
						t.Fatalf("execution error: %v", job.Result)
					}
					if _, ok := job.Result["lastStage"]; ok {
						t.Fatalf("unexpected stage: %v", job.Result)
					}
				}
				var events int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='lease_expired' AND data->>'generation'='0' AND data->>'to'=$2`, id, job.Status).Scan(&events); err != nil || events != n && n < 3 || n == 3 && events != 1 {
					t.Fatalf("events %d: %d %v", n, events, err)
				}
			}
		})
	}
}

func TestPostgresSweepPreservesPhaseAndRejectsLateReport(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/late", "nas", 1, 7)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "running", View: map[string]any{"phase": "publishing"}}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_expires_at=$2 WHERE id=$1`, id, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err = s.Sweep(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != "failed" || before.Result["lastStage"] != "publishing" {
		t.Fatalf("failure: %+v", before)
	}
	_, err = s.Report(ctx, id, "nas", Report{Epoch: 7, Status: "failed", Result: map[string]any{"summary": "late result"}})
	code(t, err, 409, "lease_lost")
	after, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "failed" || !reflect.DeepEqual(before.Result, after.Result) {
		t.Fatalf("late report changed result: before=%v after=%v", before.Result, after.Result)
	}
}

func TestPostgresSweepUnknownHost(t *testing.T) {
	for _, host := range []any{nil, ""} {
		t.Run(fmt.Sprint(host), func(t *testing.T) {
			db, ctx := testPostgres(t)
			s, err := New(db, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			var id int64
			if err = db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at,view) VALUES('owner/unknown',1,'running',$1,1,$2,'{}') RETURNING id`, host, now.Add(-time.Minute)).Scan(&id); err != nil {
				t.Fatal(err)
			}
			for n := 0; n < 3; n++ {
				if n > 0 {
					if _, err = db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_expires_at=$2 WHERE id=$1`, id, now.Add(-time.Minute)); err != nil {
						t.Fatal(err)
					}
				}
				if err = s.Sweep(ctx, nil); err != nil {
					t.Fatal(err)
				}
			}
			job, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if job.Status != "failed" || job.Result["lastHost"] != "不明" || job.Result["summary"] != "lease の期限切れが同じ世代で3回続いたため失敗にしました。最後のPC: 不明" {
				t.Fatalf("failure: %+v", job)
			}
		})
	}
}

// The reported state outranks stale failure data, unknown ends carry their
// diagnosis, and a resend of the same terminal report records nothing new.
func TestPostgresReportSavesImplementationEndReason(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stale := map[string]any{"error": "固定テストが不合格", "testFailures": map[string]any{"complete": true}}
	cases := []struct {
		name, status, hold string
		result, view       map[string]any
		want               map[string]any
	}{
		{"merged over stale failure", "merged", "", stale, map[string]any{"implementationUsageStop": true}, map[string]any{"reason": "success", "usageUnknown": false}},
		{"stopped over stale failure", "stopped", "", stale, nil, map[string]any{"reason": "stopped", "usageUnknown": false}},
		{"question", "held", "question", stale, nil, map[string]any{"reason": "question"}},
		{"review hold", "held", "review_held", nil, nil, map[string]any{"reason": "review-hold"}},
		{"scope wait", "held", "scope_wait", stale, nil, nil},
		{"deletions", "held", "deletions", stale, nil, nil},
		{"usage unknown guard", "failed", "", map[string]any{"error": "累計使用量が未知です。利用上限"}, nil, map[string]any{"reason": "guard", "usageUnknown": true}},
		{"usage limit before timeout", "failed", "", map[string]any{"testTimeout": map[string]any{"complete": true}}, map[string]any{"implementationUsageStop": true}, map[string]any{"reason": "usage-limit", "usageUnknown": false}},
		{"timeout before fixed test", "failed", "", map[string]any{"testTimeout": map[string]any{"complete": true}, "testFailures": map[string]any{"complete": true}}, nil, map[string]any{"reason": "timeout"}},
		{"fixed test before checks", "failed", "", map[string]any{"error": "固定テスト不合格。lint failed"}, nil, map[string]any{"reason": "fixed-test"}},
		{"checks before prerequisite", "failed", "", map[string]any{"error": "typecheck failed"}, map[string]any{"codexDepsUnavailable": true}, map[string]any{"reason": "checks"}},
		{"prerequisite before guard", "failed", "", map[string]any{"error": "環境が不足しています。ガードが停止"}, nil, map[string]any{"reason": "prerequisite"}},
		{"other guard", "failed", "", map[string]any{"error": "guard rejected"}, nil, map[string]any{"reason": "guard", "usageUnknown": false}},
		{"no inputs", "failed", "", nil, nil, map[string]any{"reason": "unknown", "unknownCause": "missing", "missingFields": []any{"result.error", "result.testTimeout", "result.testFailures", "view.implementationUsageStop", "view.codexDepsUnavailable"}}},
		{"present but unmatched", "failed", "", map[string]any{"error": "落ちました", "testTimeout": map[string]any{"complete": false}, "testFailures": nil}, map[string]any{"implementationUsageStop": false, "codexDepsUnavailable": float64(0)}, map[string]any{"reason": "unknown", "unknownCause": "unclassified", "missingFields": []any{"result.testFailures"}}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := seedJob(t, ctx, db, "owner/end-reason", "nas", int64(i+1), 3)
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET generation=5 WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			report := Report{Epoch: 3, Status: tc.status, View: tc.view, Result: tc.result}
			if tc.hold != "" {
				hold := tc.hold
				report.HoldReason = &hold
			}
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := s.Report(ctx, id, "nas", report); err != nil {
					t.Fatalf("attempt %d: %v", attempt, err)
				}
			}
			rows, err := db.QueryContext(ctx, `SELECT data FROM job_events WHERE job_id=$1 AND kind='state'`, id)
			if err != nil {
				t.Fatal(err)
			}
			events := []map[string]any{}
			for rows.Next() {
				var raw []byte
				var event map[string]any
				if err := rows.Scan(&raw); err != nil || json.Unmarshal(raw, &event) != nil {
					t.Fatal(err)
				}
				events = append(events, event)
			}
			rows.Close()
			if len(events) != 1 {
				t.Fatalf("state events after resend: %v", events)
			}
			end, recorded := events[0]["implementationEnd"].(map[string]any)
			if tc.want == nil {
				if recorded {
					t.Fatalf("hold outside implementation recorded an end: %v", end)
				}
				return
			}
			if !recorded || end["version"] != float64(1) || end["generation"] != float64(5) {
				t.Fatalf("end=%v", end)
			}
			if _, copied := end["error"]; copied {
				t.Fatalf("error text was copied: %v", end)
			}
			for key, want := range tc.want {
				if !reflect.DeepEqual(end[key], want) {
					t.Fatalf("%s=%v want %v (end=%v)", key, end[key], want, end)
				}
			}
			if _, has := end["unknownCause"]; has != (end["reason"] == "unknown") {
				t.Fatalf("unknown diagnosis on a classified end: %v", end)
			}
		})
	}
}
