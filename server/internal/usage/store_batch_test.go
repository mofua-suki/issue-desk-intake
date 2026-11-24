package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestBatchMixedDuplicatesAndRollback(t *testing.T) {
	db := migratedUsageDB(t)
	s := Store{DB: db}
	ctx := context.Background()
	lists := map[string][]map[string]any{}
	for i := 0; i < 250; i++ {
		at := fmt.Sprintf("2026-11-01T00:%02d:%02d.000Z", i/60, i%60)
		lists["usage"] = append(lists["usage"], map[string]any{"at": at, "host": "nas", "source": "intake", "inputTokens": i})
		lists["limits"] = append(lists["limits"], map[string]any{"at": at, "host": "nas", "source": "intake"})
		lists["fixedTests"] = append(lists["fixedTests"], map[string]any{"runId": fmt.Sprint(i), "startedAt": at, "host": "nas", "repo": "r", "scope": "full", "status": "passed", "elapsedMs": 0, "timeoutSeconds": 1, "percent": 0, "implementingAtStart": nil, "loadavg1AtStart": nil})
		lists["logLines"] = append(lists["logLines"], map[string]any{"at": at, "line": fmt.Sprint(i)})
	}
	records := map[string]json.RawMessage{}
	encode := func() {
		for kind, rows := range lists {
			raw, err := json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			records[kind] = raw
		}
	}
	encode()
	start := time.Now()
	got, err := s.Add(ctx, "sender-a", records)
	t.Logf("1000 mixed rows: %s", time.Since(start))
	if err != nil || got != (Counts{Added: 1000}) {
		t.Fatalf("mixed add: %+v %v", got, err)
	}
	got, err = s.Add(ctx, "sender-a", records)
	if err != nil || got != (Counts{Duplicate: 1000}) {
		t.Fatalf("resend: %+v %v", got, err)
	}
	// Same fixed-test identity with different content retains the first record, including its sender.
	fixed := lists["fixedTests"][0]
	fixedCopy := map[string]any{}
	for k, v := range fixed {
		fixedCopy[k] = v
	}
	fixedCopy["repo"] = "later"
	usage := make([]map[string]any, 202)
	for i := range usage {
		usage[i] = map[string]any{"at": "2026-11-02T00:00:00.000Z", "host": fmt.Sprint(i), "source": "intake"}
	}
	usage[201] = usage[0]
	usage[200] = map[string]any{"invalid": true}
	lists = map[string][]map[string]any{"usage": usage, "fixedTests": {fixedCopy, fixedCopy}}
	encode()
	delete(records, "limits")
	delete(records, "logLines")
	got, err = s.Add(ctx, "sender-b", records)
	if err != nil || got != (Counts{Added: 200, Duplicate: 3, Rejected: 1}) {
		t.Fatalf("duplicates: %+v %v", got, err)
	}
	var repo, sender string
	if err = db.QueryRowContext(ctx, "SELECT repo,sender FROM fixed_test_usage WHERE key=$1", hash("[\"nas\",\"0\"]")).Scan(&repo, &sender); err != nil || repo != "r" || sender != "sender-a" {
		t.Fatalf("first row: %q %q %v", repo, sender, err)
	}
	if _, err = db.ExecContext(ctx, "ALTER TABLE runner_log_lines ADD CONSTRAINT reject_test_line CHECK (line <> 'reject-db')"); err != nil {
		t.Fatal(err)
	}
	lists = map[string][]map[string]any{"usage": {{"at": "2026-11-03T00:00:00.000Z", "host": "rollback", "source": "intake"}}, "logLines": {{"at": "2026-11-03T00:00:00Z", "line": "reject-db"}}}
	records = map[string]json.RawMessage{}
	encode()
	if got, err = s.Add(ctx, "sender", records); err == nil || got != (Counts{}) {
		t.Fatalf("failure: %+v %v", got, err)
	}
	var n int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM usage_rows WHERE host='rollback'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rollback: %d %v", n, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err = s.Add(canceled, "sender", records); err == nil || got != (Counts{}) {
		t.Fatalf("cancel: %+v %v", got, err)
	}
}

func TestBatchInterleavedCallIdentity(t *testing.T) {
	db := migratedUsageDB(t)
	s := Store{DB: db}
	rows := make([]map[string]any, 201)
	for i := range rows {
		rows[i] = map[string]any{"at": "2026-11-01T00:00:00.000Z", "host": fmt.Sprint(i), "source": "intake"}
	}
	first := map[string]any{"at": "2026-11-01T00:00:00.000Z", "host": "nas", "source": "intake", "callId": "11111111-1111-4111-8111-111111111111"}
	second := map[string]any{"at": first["at"], "host": "nas", "source": "intake", "callId": "22222222-2222-4222-8222-222222222222"}
	conflict := map[string]any{"at": first["at"], "host": "nas", "source": "intake", "callId": first["callId"], "outputTokens": 7}
	rows = append(rows[:200], append([]map[string]any{first, first, conflict, second}, rows[200:]...)...)
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]json.RawMessage{"usage": raw}
	for i, want := range []Counts{{Added: 203, Duplicate: 1, Rejected: 1}, {Duplicate: 204, Rejected: 1}} {
		got, err := s.Add(t.Context(), "sender", records)
		if err != nil || got != want {
			t.Fatalf("attempt %d: got %+v, want %+v: %v", i, got, want, err)
		}
	}
	stored, skipped, err := s.Read(t.Context(), "", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || skipped != 0 || len(stored) != 203 {
		t.Fatalf("read: rows=%d skipped=%d err=%v", len(stored), skipped, err)
	}
	seen := map[string]bool{}
	for _, row := range stored {
		if id, ok := row["callId"].(string); ok {
			seen[id] = true
			if _, changed := row["outputTokens"]; changed {
				t.Fatalf("conflicting row replaced original: %v", row)
			}
		}
	}
	if len(seen) != 2 || !seen[first["callId"].(string)] || !seen[second["callId"].(string)] {
		t.Fatalf("call IDs: %v", seen)
	}
}
