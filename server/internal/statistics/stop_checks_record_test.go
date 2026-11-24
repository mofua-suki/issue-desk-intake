package statistics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestCountedStopChecksLoadedOnce(t *testing.T) {
	t.Parallel()
	var ledger []struct {
		ID      string `json:"id"`
		Counted bool   `json:"counted"`
	}
	if err := json.Unmarshal(stopChecksJSON, &ledger); err != nil || stopChecksLoadErr != nil {
		t.Fatalf("ledger: %v, initialization: %v", err, stopChecksLoadErr)
	}
	for _, entry := range ledger {
		_, found := countedStopChecks[entry.ID]
		if found != (entry.Counted && entry.ID != "") {
			t.Errorf("%s: counted=%v, found=%v", entry.ID, entry.Counted, found)
		}
	}
	// Loading owns its result: changing the input afterwards cannot change it.
	input := append([]byte(nil), stopChecksJSON...)
	_, loaded, err := loadStopCheckLedger(input)
	if err != nil {
		t.Fatal(err)
	}
	for i := range input {
		input[i] = 'x'
	}
	if _, found := loaded["unasked-send"]; !found {
		t.Fatal("loaded ledger changed with its input")
	}
	err = WriteStopCheckRecords(context.Background(), nil, []StopCheckRecord{{CheckID: "unasked-send"}})
	if err == nil || err.Error() != "invalid record ID" {
		t.Fatalf("cached ledger was not used: %v", err)
	}
}

func TestStopCheckValidationParallel(t *testing.T) {
	for _, tc := range []struct{ name, checkID, want string }{
		{"counted", "unasked-send", "invalid record ID"},
		{"uncounted guard", "duplicate-issue-guard", "unknown stop check ID"},
		{"unknown", "not-in-ledger", "unknown stop check ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := WriteStopCheckRecords(context.Background(), nil, []StopCheckRecord{{CheckID: tc.checkID}})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("check %q: got %v, want %q", tc.checkID, err, tc.want)
			}
		})
	}
}

func TestPostgresStopCheckRecord(t *testing.T) {
	db, ctx := testPostgres(t)
	record := StopCheckRecord{ID: "123e4567-e89b-12d3-a456-426614174000", CheckID: "unasked-send", Result: "block", OccurredAt: "2026-11-05T12:34:56Z", Project: "project-1", Host: "pc-1"}
	if err := WriteStopCheckRecords(ctx, db, []StopCheckRecord{record}); err != nil {
		t.Fatal(err)
	}
	// A retry cannot replace the first row, even if its fields differ.
	retry := record
	retry.Result = "warn"
	retry.Project = "other"
	if err := WriteStopCheckRecords(ctx, db, []StopCheckRecord{retry}); err != nil {
		t.Fatal(err)
	}
	var count int
	var row string
	if err := db.QueryRowContext(ctx, `SELECT count(*), min(row_to_json(r)::text) FROM stop_check_records r`).Scan(&count, &row); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("got %d rows", count)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(row), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 7 || fields["result"] != "block" || fields["project"] != "project-1" || fields["host"] != "pc-1" {
		t.Fatalf("unexpected stored row: %s", row)
	}
	for _, forbidden := range []string{"body", "reason", "path"} {
		if _, ok := fields[forbidden]; ok {
			t.Fatalf("unexpected field %s", forbidden)
		}
	}
	bad := record
	bad.ID = "123e4567-e89b-12d3-a456-426614174001"
	bad.CheckID = "not-in-ledger"
	if err := WriteStopCheckRecords(ctx, db, []StopCheckRecord{bad}); err == nil {
		t.Fatal("unknown check accepted")
	}
	bad.CheckID = "duplicate-issue-guard"
	if err := WriteStopCheckRecords(ctx, db, []StopCheckRecord{bad}); err == nil || err.Error() != "unknown stop check ID" {
		t.Fatalf("uncounted guard: got %v, want unknown stop check ID", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM stop_check_records`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("guard changed row count to %d: %v", count, err)
	}
	bad.CheckID = record.CheckID
	bad.Result = "pass"
	if err := WriteStopCheckRecords(ctx, db, []StopCheckRecord{bad}); err == nil {
		t.Fatal("invalid result accepted")
	}
	bad.Result = "warn"
	bad.OccurredAt = "2026-11-05"
	if err := WriteStopCheckRecords(ctx, db, []StopCheckRecord{bad}); err == nil {
		t.Fatal("invalid time accepted")
	}
	bad.OccurredAt = "2026-11-05T12:34:56+24:00"
	if err := WriteStopCheckRecords(ctx, db, []StopCheckRecord{bad}); err == nil {
		t.Fatal("invalid timezone accepted")
	}
	bad.OccurredAt = record.OccurredAt
	bad.Host = "/private/path"
	bad.Project = "/private/path"
	if err := WriteStopCheckRecords(ctx, db, []StopCheckRecord{bad}); err != nil {
		t.Fatal(err)
	}
	var project, host string
	if err := db.QueryRowContext(ctx, `SELECT project, host FROM stop_check_records WHERE id=$1`, bad.ID).Scan(&project, &host); err != nil || project != "other" || host != "other" {
		t.Fatalf("project %q host %q: %v", project, host, err)
	}
	bad.ID = "bad"
	if err := WriteStopCheckRecords(ctx, db, []StopCheckRecord{bad}); err == nil {
		t.Fatal("invalid ID accepted")
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM stop_check_records`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("count %d: %v", count, err)
	}
}

type countingStopExecutor struct {
	StopCheckExecutor
	calls int
}

func (c *countingStopExecutor) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	c.calls++
	return c.StopCheckExecutor.ExecContext(ctx, q, args...)
}

func TestPostgresStopCheckBatch(t *testing.T) {
	db, ctx := testPostgres(t)
	records := make([]StopCheckRecord, 1000)
	for i := range records {
		records[i] = StopCheckRecord{ID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i), CheckID: "unasked-send", Result: "block", OccurredAt: "2026-11-05T12:34:56Z", Project: "p", Host: "h"}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	counter := &countingStopExecutor{StopCheckExecutor: tx}
	// Existing, same-chunk and cross-chunk duplicates retain the earliest value.
	if err := WriteStopCheckRecords(ctx, tx, []StopCheckRecord{records[0]}); err != nil {
		t.Fatal(err)
	}
	records[1].ID = records[0].ID
	records[1].Result = "warn"
	records[201].ID = records[2].ID
	records[201].Result = "warn"
	records[3].ID = records[2].ID
	records[3].Result = "warn"
	if err := WriteStopCheckRecords(ctx, counter, records); err != nil {
		t.Fatal(err)
	}
	if counter.calls != 5 {
		t.Fatalf("SQL calls = %d", counter.calls)
	}
	var count int
	var result string
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM stop_check_records`).Scan(&count); err != nil || count != 997 {
		t.Fatalf("count %d: %v", count, err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT result FROM stop_check_records WHERE id=$1`, records[2].ID).Scan(&result); err != nil || result != "block" {
		t.Fatalf("first row %q: %v", result, err)
	}
	invalid := append([]StopCheckRecord(nil), records...)
	invalid[500].CheckID = "unknown"
	counter.calls = 0
	if err := WriteStopCheckRecords(ctx, counter, invalid); !errors.Is(err, ErrInvalidStopCheck) || counter.calls != 0 {
		t.Fatalf("invalid %v calls %d", err, counter.calls)
	}
	if err := WriteStopCheckRecords(ctx, counter, nil); err != nil || counter.calls != 0 {
		t.Fatalf("empty %v calls %d", err, counter.calls)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// A database error in the second chunk must leave no rows after rollback.
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE stop_check_records ADD CONSTRAINT reject_test_project CHECK (project <> 'fail')`); err != nil {
		t.Fatal(err)
	}
	records[250].Project = "fail"
	counter = &countingStopExecutor{StopCheckExecutor: tx}
	if err := WriteStopCheckRecords(ctx, counter, records); err == nil || counter.calls != 2 {
		t.Fatalf("failure %v calls %d", err, counter.calls)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM stop_check_records`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback count %d: %v", count, err)
	}
}

func TestPostgresStopCheckComparison(t *testing.T) {
	db, ctx := testPostgres(t)
	for _, n := range []int{1, 1000} {
		records := make([]StopCheckRecord, n)
		for i := range records {
			records[i] = StopCheckRecord{ID: fmt.Sprintf("10000000-0000-0000-0000-%012d", i), CheckID: "unasked-send", Result: "block", OccurredAt: "2026-11-05T12:34:56Z", Project: "project-1", Host: "pc-1"}
		}
		for _, batch := range []bool{false, true} {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			c := &countingStopExecutor{StopCheckExecutor: tx}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			if batch {
				err = WriteStopCheckRecords(ctx, c, records)
			} else {
				for _, r := range records {
					if err = WriteStopCheckRecords(ctx, c, []StopCheckRecord{r}); err != nil {
						break
					}
				}
			}
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)
			if err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			t.Logf("rows=%d batch=%t SQL=%d elapsed=%s allocated=%d bytes", n, batch, c.calls, elapsed, after.TotalAlloc-before.TotalAlloc)
		}
	}
}
