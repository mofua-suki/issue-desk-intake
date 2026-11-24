package jobs

import (
	"database/sql"
	"testing"
	"time"

	"issue-desk-intake/server/internal/store"
)

// A report without a result stores the JSON value null, which is not SQL NULL: `'null'::jsonb || object`
// builds an array, and one array in jobs.result stops every read of the jobs table.
func TestPostgresSweepExpiryKeepsResultAnObjectAfterJSONNull(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at,view,result,generation) VALUES('owner/json-null',1,'running','pc-last',1,$1,'{"phase":"implementation"}','null'::jsonb,1) RETURNING id`, now.Add(-time.Minute)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		if n == 2 {
			if _, err := db.ExecContext(ctx, `UPDATE jobs SET view='{}' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_expires_at=$2 WHERE id=$1`, id, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.Sweep(ctx, nil); err != nil {
			t.Fatal(err)
		}
		var host, expires, to, eventHost string
		var phase sql.NullString
		var phasePresent bool
		var epoch, generation int64
		if err := db.QueryRowContext(ctx, `SELECT host,data->>'leaseHost',(data->>'epoch')::bigint,data->>'expiresAt',data->>'phase',data ? 'phase',(data->>'generation')::bigint,data->>'to' FROM job_events WHERE job_id=$1 AND kind='lease_expired' ORDER BY seq DESC LIMIT 1`, id).Scan(&eventHost, &host, &epoch, &expires, &phase, &phasePresent, &generation, &to); err != nil {
			t.Fatal(err)
		}
		wantStatus := "queued"
		if n == 3 {
			wantStatus = "failed"
		}
		if eventHost != "" || host != "pc-last" || epoch != 1 || expires != iso(now.Add(-time.Minute)) || (n == 1 && (!phasePresent || phase.String != "implementation")) || (n > 1 && phasePresent) || generation != 1 || to != wantStatus {
			t.Fatalf("expired host=%q leaseHost=%q epoch=%d expires=%q phase=%v present=%v generation=%d to=%q", eventHost, host, epoch, expires, phase, phasePresent, generation, to)
		}
	}
	var kind string
	if err := db.QueryRowContext(ctx, `SELECT jsonb_typeof(result) FROM jobs WHERE id=$1`, id).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "object" {
		t.Fatalf("result is a %s", kind)
	}
	detail, err := s.Detail(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job := detail["job"].(*Job); job.Status != "failed" || job.Result["leaseExpiredCount"] != float64(3) {
		t.Fatalf("failure: %+v", job)
	}
}

// The rows the bug above already wrote must be readable again once the repair migration has run.
func TestPostgresRepairMigrationRestoresArrayResults(t *testing.T) {
	db, ctx := testPostgres(t)
	list, err := store.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	repair := ""
	for _, migration := range list {
		if migration.Name == "repair_job_result_arrays" {
			repair = migration.SQL
		}
	}
	if repair == "" {
		t.Fatal("repair migration is missing")
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE jobs DROP CONSTRAINT jobs_result_object_only`); err != nil {
		t.Fatal(err)
	}
	seed := func(repo, result string) (id int64) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,view,result) VALUES($1,1,'failed','{}',$2::jsonb) RETURNING id`, repo, result).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	wrapped := seed("owner/wrapped", `[null,{"status":"failed","leaseExpiredCount":3}]`)
	empty := seed("owner/empty", `[null,1]`)
	object := seed("owner/object", `{"keep":42}`)
	if _, err := db.ExecContext(ctx, repair); err != nil {
		t.Fatal(err)
	}
	read := func(id int64) string {
		t.Helper()
		var value sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT result::text FROM jobs WHERE id=$1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value.String
	}
	if got := read(wrapped); got != `{"status": "failed", "leaseExpiredCount": 3}` {
		t.Fatalf("wrapped: %s", got)
	}
	if got := read(empty); got != "" {
		t.Fatalf("empty: %s", got)
	}
	if got := read(object); got != `{"keep": 42}` {
		t.Fatalf("object: %s", got)
	}
}

// One bad row must not stop job acquisition for every PC, the job list, or the job's own detail.
func TestPostgresResultOfAnotherTypeDoesNotStopReads(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE jobs DROP CONSTRAINT jobs_result_object_only`); err != nil {
		t.Fatal(err)
	}
	var bad int64
	if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,view,result) VALUES('owner/bad',1,'failed','{}','[null,{"status":"failed"}]'::jsonb) RETURNING id`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if _, err := readTransfers(ctx, db); err != nil {
		t.Fatalf("transfers: %v", err)
	}
	if _, err := s.prepareTransfers(ctx); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	detail, err := s.Detail(ctx, bad)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if job := detail["job"].(*Job); job.Status != "failed" || job.Result != nil {
		t.Fatalf("job: %+v", job)
	}
}

// The column rejects anything but an object, so a future writer fails alone instead of stopping every read.
func TestPostgresResultColumnRejectsOtherTypes(t *testing.T) {
	db, ctx := testPostgres(t)
	var id int64
	if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,view) VALUES('owner/check',1,'failed','{}') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`[null,{"a":1}]`, `"text"`, `1`, `true`} {
		if _, err := db.ExecContext(ctx, `UPDATE jobs SET result=$2::jsonb WHERE id=$1`, id, value); err == nil {
			t.Fatalf("%s was saved", value)
		}
	}
	for _, value := range []string{`{"a":1}`, `null`} {
		if _, err := db.ExecContext(ctx, `UPDATE jobs SET result=$2::jsonb WHERE id=$1`, id, value); err != nil {
			t.Fatalf("%s: %v", value, err)
		}
	}
}

func TestStoredResultIgnoresOtherTypes(t *testing.T) {
	for _, raw := range []string{``, `null`, `[null,{"a":1}]`, `"text"`, `1`, `true`, `{broken`} {
		if got := StoredResult(1, []byte(raw)); got != nil {
			t.Fatalf("%q: %v", raw, got)
		}
	}
	if got := StoredResult(1, []byte(`{"a":1}`)); got["a"] != float64(1) {
		t.Fatalf("object: %v", got)
	}
}
