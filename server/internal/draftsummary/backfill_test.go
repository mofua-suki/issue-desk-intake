package draftsummary_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/draftsummary"
	"issue-desk-intake/server/internal/pgtest"
)

func TestDraftSummaryOldSchemaRecovery(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	summaryExec(t, env.DB, `DROP TRIGGER drafts_screen_summary ON drafts;
 DROP FUNCTION `+env.Schema+`.drafts_set_screen_summary();
 DROP FUNCTION `+env.Schema+`.drafts_screen_summary(text);
 ALTER TABLE drafts DROP COLUMN screen_summary`)
	fixtures := summaryFixtures()
	for _, f := range fixtures {
		summaryExec(t, env.DB, `INSERT INTO drafts(id,json) VALUES($1,$2)`, f.id, f.raw)
	}
	if err := draftsummary.EnsureDraftSummary(ctx, env.DB, env.Schema); err != nil {
		t.Fatal(err)
	}
	// Backfill must update only screen_summary, never fire UPDATE OF json.
	summaryExec(t, env.DB, `CREATE FUNCTION `+env.Schema+`.reject_json_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'json must remain untouched'; END $$;
 CREATE TRIGGER reject_json_update BEFORE UPDATE OF json ON drafts FOR EACH ROW EXECUTE FUNCTION `+env.Schema+`.reject_json_update()`)
	n, err := draftsummary.BackfillDraftSummary(ctx, env.DB, env.Schema, 3)
	if err != nil || n != int64(len(fixtures)) {
		t.Fatalf("backfill: %d %v", n, err)
	}
	var nulls int64
	for _, f := range fixtures {
		assertSummaryFixture(t, env, f)
		if f.nullSummary {
			nulls++
		}
	}
	before := summaryVersions(t, env.DB)
	n, err = draftsummary.BackfillDraftSummary(ctx, env.DB, env.Schema, 2)
	if err != nil || n != nulls {
		t.Fatalf("rerun: %d want %d, %v", n, nulls, err)
	}
	after := summaryVersions(t, env.DB)
	for id, version := range before {
		if after[id] != version {
			t.Fatalf("already filled row %s rewritten", id)
		}
	}
	for _, f := range fixtures {
		assertSummaryFixture(t, env, f)
	}
}

func summaryVersions(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT id,xmin::text||':'||ctid::text FROM drafts WHERE screen_summary IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	versions := map[string]string{}
	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			t.Fatal(err)
		}
		versions[id] = version
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return versions
}

// The first batch is really committed; cancellation occurs at the next public
// BeginTx boundary, avoiding timing-dependent sleeps or mock SQL execution.
type cancelSummaryBeginner struct {
	db     *sql.DB
	cancel context.CancelFunc
	calls  int
}

func (b *cancelSummaryBeginner) BeginTx(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
	b.calls++
	if b.calls == 2 {
		b.cancel()
	}
	return b.db.BeginTx(ctx, options)
}

func TestDraftSummaryBackfillCancellationAndErrors(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	// Reverse insertion order and the empty key distinguish primary-key traversal
	// from source_rowid order. The invalid row consumes a committed batch slot.
	for _, id := range []string{"d", "c", "b", "a", ""} {
		raw := `{"title":"` + id + `"}`
		if id == "a" {
			raw = `{`
		}
		summaryExec(t, env.DB, `INSERT INTO drafts(id,json) VALUES($1,$2)`, id, raw)
	}
	summaryExec(t, env.DB, `UPDATE drafts SET screen_summary=NULL`)
	canceled, cancel := context.WithCancel(ctx)
	defer cancel()
	beginner := &cancelSummaryBeginner{db: env.DB, cancel: cancel}
	n, err := draftsummary.BackfillDraftSummary(canceled, beginner, env.Schema, 2)
	if n != 2 || !errors.Is(err, context.Canceled) {
		t.Fatalf("partial cancellation: %d %v", n, err)
	}
	versions := summaryVersions(t, env.DB)
	if len(versions) != 1 || versions[""] == "" {
		t.Fatalf("first batch not primary-key ordered: %v", versions)
	}
	n, err = draftsummary.BackfillDraftSummary(ctx, env.DB, env.Schema, 2)
	if n != 4 || err != nil {
		t.Fatalf("resume: %d %v", n, err)
	}
	for _, id := range []string{"", "b", "c", "d"} {
		assertSummaryFixture(t, env, summaryFixture{id, `{"title":"` + id + `"}`, false})
	}
	assertSummaryFixture(t, env, summaryFixture{"a", `{`, true})
	n, err = draftsummary.BackfillDraftSummary(ctx, env.DB, env.Schema, 2)
	if n != 1 || err != nil {
		t.Fatalf("NULL-only rerun: %d %v", n, err)
	}
	// A database error in batch two rolls back that whole batch and returns only
	// the two rows committed by batch one, including its unprojectable row.
	summaryExec(t, env.DB, `UPDATE drafts SET screen_summary=NULL;
 CREATE FUNCTION `+env.Schema+`.fail_summary_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='c' THEN RAISE EXCEPTION 'test batch failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER fail_summary_update BEFORE UPDATE OF screen_summary ON drafts FOR EACH ROW EXECUTE FUNCTION `+env.Schema+`.fail_summary_update()`)
	n, err = draftsummary.BackfillDraftSummary(ctx, env.DB, env.Schema, 2)
	if n != 2 || err == nil {
		t.Fatalf("partial DB failure: %d %v", n, err)
	}
	versions = summaryVersions(t, env.DB)
	if len(versions) != 1 || versions[""] == "" {
		t.Fatalf("failed batch partially survived: %v", versions)
	}
	summaryExec(t, env.DB, `DROP TRIGGER fail_summary_update ON drafts`)
	n, err = draftsummary.BackfillDraftSummary(ctx, env.DB, env.Schema, 2)
	if n != 4 || err != nil {
		t.Fatalf("DB failure recovery: %d %v", n, err)
	}
	canceled, cancel = context.WithCancel(ctx)
	cancel()
	n, err = draftsummary.BackfillDraftSummary(canceled, env.DB, env.Schema, 2)
	if n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("initial cancellation: %d %v", n, err)
	}
	for _, size := range []int{0, -1} {
		if n, err := draftsummary.BackfillDraftSummary(ctx, env.DB, env.Schema, size); n != 0 || err == nil {
			t.Fatalf("invalid batch size %d: %d %v", size, n, err)
		}
	}
	if _, err := draftsummary.BackfillDraftSummary(ctx, env.DB, "bad.schema", 1); err == nil {
		t.Fatal("invalid schema accepted")
	}
	if _, err := draftsummary.BackfillDraftSummary(ctx, env.DB, "missing_summary_schema", 1); err == nil {
		t.Fatal("missing table error suppressed")
	}
}

// Explicit large measurement (never a timing gate):
// node scripts/with-test-postgres.ts -- sh -c 'cd server && go test ./internal/draftsummary -run ^$ -bench ^BenchmarkDraftSummaryWrites$ -benchtime=1x -count=1 -timeout=0 -v'
// Each of three paired trials writes exactly 6702 fixed rows per condition in
// one transaction. Both single-row SQL and 100-row SQL are measured separately.
// Timed interval includes BeginTx, all writes and Commit, excluding generation,
// preparation, truncate and validation. No environment-variable-only test gate.
func BenchmarkDraftSummaryWrites(b *testing.B) {
	env := pgtest.NewIsolated(b)
	ctx := context.Background()
	const count = 6702
	ids := make([]string, count)
	raw := make([]string, count)
	updatedRaw := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("draft-%05d", i)
		record := map[string]any{"id": ids[i], "status": "draft", "project": "owner/app", "body": strings.Repeat("b", 8192), "savedMaterials": strings.Repeat("s", 8192), "updatedAt": "2026-11-06T00:00:00Z"}
		if i%2 == 1 {
			record["status"] = "submitted"
			record["issue"] = map[string]any{"number": i + 1}
		}
		// 3351 parent/child pairs: each unfinished even row owns the next finished row.
		if i%2 == 0 {
			record["splitChildren"] = []string{fmt.Sprintf("draft-%05d", i+1)}
			record["splitIssues"] = []any{map[string]any{"project": "owner/app"}}
		} else {
			record["parentSplit"] = fmt.Sprintf("draft-%05d", i-1)
		}
		messages := make([]any, 4)
		for j := range messages {
			role := "assistant"
			if j%2 == 0 {
				role = "user"
			}
			messages[j] = map[string]any{"role": role, "text": strings.Repeat("m", 4096)}
		}
		record["messages"] = messages
		data, err := json.Marshal(record)
		if err != nil {
			b.Fatal(err)
		}
		raw[i] = string(data)
		updatedRaw[i] = strings.Replace(raw[i], "2026-11-06T00:00:00Z", "2026-11-07T00:00:00Z", 1)
	}
	var pgVersion, fsync, syncCommit, fullPage string
	if err := env.DB.QueryRow(`SELECT version(),current_setting('fsync'),current_setting('synchronous_commit'),current_setting('full_page_writes')`).Scan(&pgVersion, &fsync, &syncCommit, &fullPage); err != nil {
		b.Fatal(err)
	}
	b.Logf("engine=%s fsync=%s synchronous_commit=%s full_page_writes=%s; rows=%d unfinished=3351 parent_child_pairs=3351 body=8192B messages=4x4096B savedMaterials=8192B", pgVersion, fsync, syncCommit, fullPage, count)
	b.ResetTimer()
	b.StopTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		for _, batch := range []int{1, 100} {
			for _, operation := range []string{"INSERT", "UPDATE-json"} {
				var totals [2]time.Duration
				for trial := 0; trial < 3; trial++ {
					var times [2]time.Duration
					// Alternate the condition order to reduce systematic warm-cache bias.
					order := []int{trial % 2, 1 - trial%2}
					for _, enabled := range order {
						if _, err := env.DB.ExecContext(ctx, `TRUNCATE drafts`); err != nil {
							b.Fatal(err)
						}
						mode := "DISABLE"
						if enabled == 1 {
							mode = "ENABLE"
						}
						if _, err := env.DB.ExecContext(ctx, `ALTER TABLE drafts `+mode+` TRIGGER drafts_screen_summary`); err != nil {
							b.Fatal(err)
						}
						if operation == "UPDATE-json" {
							measureSummaryWrites(b, env.DB, ids, raw, batch, false)
						}
						writeRaw := raw
						if operation == "UPDATE-json" {
							writeRaw = updatedRaw
						}
						b.StartTimer()
						start := time.Now()
						affected := measureSummaryWrites(b, env.DB, ids, writeRaw, batch, operation == "UPDATE-json")
						times[enabled] = time.Since(start)
						b.StopTimer()
						totals[enabled] += times[enabled]
						if affected != count {
							b.Fatalf("affected %d, want %d", affected, count)
						}
						var stored, unfinished, parents, children, badSummary, badShape int
						err := env.DB.QueryRow(`SELECT count(*),count(*) FILTER(WHERE json::jsonb->>'status'='draft'),count(*) FILTER(WHERE jsonb_exists(json::jsonb,'splitChildren')),count(*) FILTER(WHERE jsonb_exists(json::jsonb,'parentSplit')),count(*) FILTER(WHERE screen_summary IS DISTINCT FROM CASE WHEN $1 THEN `+env.Schema+`.drafts_screen_summary(json) ELSE NULL END),count(*) FILTER(WHERE octet_length(json::jsonb->>'body')<>8192 OR octet_length(json::jsonb->>'savedMaterials')<>8192 OR jsonb_array_length(json::jsonb->'messages')<>4 OR EXISTS(SELECT 1 FROM jsonb_array_elements(json::jsonb->'messages') m WHERE octet_length(m->>'text')<>4096)) FROM drafts`, enabled == 1).Scan(&stored, &unfinished, &parents, &children, &badSummary, &badShape)
						if err != nil || stored != count || unfinished != 3351 || parents != 3351 || children != 3351 || badSummary != 0 || badShape != 0 {
							b.Fatalf("stored fixture: %d %d %d %d summaryErrors=%d shapeErrors=%d %v", stored, unfinished, parents, children, badSummary, badShape, err)
						}
					}
					b.Logf("%s rows_per_SQL=%d trial=%d rows=%d tx=1 off_total=%s on_total=%s off_us_per_row=%.3f on_us_per_row=%.3f added_us_per_row=%.3f", operation, batch, trial+1, count, times[0], times[1], float64(times[0].Nanoseconds())/count/1000, float64(times[1].Nanoseconds())/count/1000, float64((times[1]-times[0]).Nanoseconds())/count/1000)
				}
				b.Logf("aggregate %s rows_per_SQL=%d trials=3 rows_per_condition=%d off_total=%s on_total=%s added_us_per_row=%.3f", operation, batch, 3*count, totals[0], totals[1], float64((totals[1]-totals[0]).Nanoseconds())/(3*count)/1000)
			}
		}
	}
	if _, err := env.DB.ExecContext(ctx, `ALTER TABLE drafts ENABLE TRIGGER drafts_screen_summary`); err != nil {
		b.Fatal(err)
	}
}

// Return the number of actual writes. For UPDATE setup, the identical insert
// method runs outside the timed interval, with the same trigger condition.
func measureSummaryWrites(b *testing.B, db *sql.DB, ids, raw []string, batch int, update bool) int64 {
	b.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	var total int64
	for start := 0; start < len(ids); start += batch {
		end := min(start+batch, len(ids))
		var result sql.Result
		if batch == 1 {
			if update {
				result, err = tx.ExecContext(ctx, `UPDATE drafts SET json=$2 WHERE id=$1`, ids[start], raw[start])
			} else {
				result, err = tx.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, ids[start], raw[start])
			}
		} else {
			if update {
				result, err = tx.ExecContext(ctx, `UPDATE drafts d SET json=s.json FROM unnest($1::text[],$2::text[]) AS s(id,json) WHERE d.id=s.id`, ids[start:end], raw[start:end])
			} else {
				result, err = tx.ExecContext(ctx, `INSERT INTO drafts(id,json) SELECT * FROM unnest($1::text[],$2::text[])`, ids[start:end], raw[start:end])
			}
		}
		if err != nil {
			b.Fatal(err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			b.Fatal(err)
		}
		total += n
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return total
}

// Observed in the isolated PostgreSQL 17.11 run for this change (linux/amd64,
// i9-12900H; fsync, synchronous_commit and full_page_writes off). These are
// environment-specific observations, never pass/fail thresholds. Each total
// covers 3 trials x 6702 rows per condition; the per-row delta includes amortized
// transaction cost. A 100-row SQL run uses 67 full statements and one of 2 rows.
// SQL rows | operation   | trigger off total | trigger on total | added us/row
//        1 | INSERT      |      8.979737727s |    19.250155613s |      510.814
//        1 | UPDATE-json |      7.192626762s |    16.859823628s |      480.812
//      100 | INSERT      |      2.975716544s |     5.981734219s |      149.508
//      100 | UPDATE-json |      2.883959066s |     5.283946361s |      119.367

func TestDraftSummaryUpgradeBackfill(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	raw := `{"usageMeasurementHold":{},"consultationBudget":{"pendingCall":{},"unknownCalls":3}}`
	summaryExec(t, env.DB, `INSERT INTO drafts(id,json) VALUES('old',$1),('current',$1)`, raw)
	summaryExec(t, env.DB, `UPDATE drafts SET screen_summary='{}',screen_summary_version=NULL WHERE id='old'`)
	before := summaryVersions(t, env.DB)["current"]
	if err := draftsummary.EnsureDraftSummary(ctx, env.DB, env.Schema); err != nil {
		t.Fatal(err)
	}
	n, err := draftsummary.BackfillDraftSummary(ctx, env.DB, env.Schema, 1)
	if err != nil || n != 1 {
		t.Fatalf("upgrade: %d %v", n, err)
	}
	assertSummaryFixture(t, env, summaryFixture{"old", raw, false})
	if after := summaryVersions(t, env.DB)["current"]; before != after {
		t.Fatal("current summary rewritten")
	}
	var hold, pending bool
	var unknown int
	if err := env.DB.QueryRow(`SELECT (screen_summary::jsonb->>'usageMeasurementHold')::boolean, (screen_summary::jsonb->'consultationBudget'->>'pendingCall')::boolean, (screen_summary::jsonb->'consultationBudget'->>'unknownCalls')::int FROM drafts WHERE id='old'`).Scan(&hold, &pending, &unknown); err != nil || !hold || !pending || unknown != 3 {
		t.Fatalf("projection: %v %v %d %v", hold, pending, unknown, err)
	}
}

type pauseSummaryBeginner struct {
	db             *sql.DB
	calls          int
	paused, resume chan struct{}
}

func (b *pauseSummaryBeginner) BeginTx(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
	b.calls++
	if b.calls == 2 {
		close(b.paused)
		<-b.resume
	}
	return b.db.BeginTx(ctx, options)
}

func TestDraftSummaryBackfillRecovery(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	summaryExec(t, env.DB, `DROP TRIGGER drafts_screen_summary ON drafts; DROP FUNCTION `+env.Schema+`.drafts_set_screen_summary(); DROP FUNCTION `+env.Schema+`.drafts_screen_summary(text); ALTER TABLE drafts DROP COLUMN screen_summary`)
	fixtures := []summaryFixture{{"a", `{"title":"a"}`, false}, {"b", `{"title":"b"}`, false}, {"c", `{"title":"c"}`, false}, {"y", `{`, true}, {"z", `{"title":"\u0000"}`, true}}
	for _, f := range fixtures {
		summaryExec(t, env.DB, `INSERT INTO drafts(id,json) VALUES($1,$2)`, f.id, f.raw)
	}
	if err := draftsummary.EnsureDraftSummary(ctx, env.DB, env.Schema); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	first, err := draftsummary.BackfillDraftSummary(canceled, &cancelSummaryBeginner{db: env.DB, cancel: cancel}, env.Schema, 2)
	cancel()
	if first != 2 || !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupt: %d %v", first, err)
	}
	b := &pauseSummaryBeginner{db: env.DB, paused: make(chan struct{}), resume: make(chan struct{})}
	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	go func() { n, err := draftsummary.BackfillDraftSummary(ctx, b, env.Schema, 1); done <- result{n, err} }()
	<-b.paused
	// Update rows from both runs while the resumed backfill is active.
	summaryExec(t, env.DB, `UPDATE drafts SET json='{"title":"updated"}' WHERE id IN ('a','c')`)
	close(b.resume)
	r := <-done
	if r.err != nil || first+r.n != int64(len(fixtures)) {
		t.Fatalf("resume: %d + %d: %v", first, r.n, r.err)
	}
	for _, f := range fixtures {
		if f.id == "a" || f.id == "c" {
			f.raw = `{"title":"updated"}`
		}
		assertSummaryFixture(t, env, f)
	}
	var stale int
	if err := env.DB.QueryRow(`SELECT count(*) FROM drafts WHERE screen_summary IS NOT NULL AND screen_summary IS DISTINCT FROM ` + env.Schema + `.drafts_screen_summary(json)`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("stale: %d %v", stale, err)
	}
}
