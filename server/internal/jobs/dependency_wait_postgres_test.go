package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/store"
)

func waitExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
func dependencyWaitJob(t *testing.T, db *sql.DB, issue int, status string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`INSERT INTO jobs(repo,issue,status) VALUES('test/repo',$1,$2) RETURNING id`, issue, status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func waitValue(t *testing.T, db *sql.DB, job, pred int64) (sql.NullTime, bool) {
	t.Helper()
	var ts sql.NullTime
	var unknown bool
	if err := db.QueryRow(`SELECT wait_started_at,wait_unknown FROM job_deps WHERE job_id=$1 AND depends_on=$2`, job, pred).Scan(&ts, &unknown); err != nil {
		t.Fatal(err)
	}
	return ts, unknown
}
func waitEdge(t *testing.T, db *sql.DB, job, pred int64) {
	t.Helper()
	waitExec(t, db, `INSERT INTO job_deps(job_id,depends_on) VALUES($1,$2)`, job, pred)
}
func assertWait(t *testing.T, db *sql.DB, job, pred int64, timed, unknown bool) sql.NullTime {
	t.Helper()
	ts, u := waitValue(t, db, job, pred)
	if ts.Valid != timed || u != unknown {
		t.Fatalf("wait=%v unknown=%v; want timed=%v unknown=%v", ts, u, timed, unknown)
	}
	return ts
}

func TestDependencyWaitRecordedOnEnteringWait(t *testing.T) {
	db, _ := testPostgres(t)
	p := dependencyWaitJob(t, db, 1, "running")
	ready := dependencyWaitJob(t, db, 2, "merged")
	j := dependencyWaitJob(t, db, 3, "running")
	waitEdge(t, db, j, p)
	waitEdge(t, db, j, ready)
	assertWait(t, db, j, p, false, false)
	waitExec(t, db, `UPDATE jobs SET status='queued' WHERE id=$1`, j)
	assertWait(t, db, j, p, true, false)
	assertWait(t, db, j, ready, false, false)
}
func TestDependencyWaitClearedAndRerecorded(t *testing.T) {
	db, _ := testPostgres(t)
	p := dependencyWaitJob(t, db, 1, "running")
	j := dependencyWaitJob(t, db, 2, "queued")
	waitEdge(t, db, j, p)
	assertWait(t, db, j, p, true, false)
	// Use a deliberately old timestamp to avoid relying on clock resolution or scheduling.
	waitExec(t, db, `UPDATE job_deps SET wait_started_at='2000-01-01' WHERE job_id=$1 AND depends_on=$2`, j, p)
	first, _ := waitValue(t, db, j, p)
	waitExec(t, db, `UPDATE jobs SET status='merged' WHERE id=$1`, p)
	assertWait(t, db, j, p, false, false)
	waitExec(t, db, `UPDATE jobs SET status='running' WHERE id=$1`, p)
	second := assertWait(t, db, j, p, true, false)
	if !second.Time.After(first.Time) {
		t.Fatal("time moved backwards")
	}
	waitExec(t, db, `UPDATE job_deps SET wait_started_at='2000-01-01' WHERE job_id=$1 AND depends_on=$2`, j, p)
	waitExec(t, db, `DELETE FROM job_deps WHERE job_id=$1 AND depends_on=$2`, j, p)
	waitEdge(t, db, j, p)
	third := assertWait(t, db, j, p, true, false)
	if !third.Time.After(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("reinsertion did not record a new time")
	}
}
func TestDependencyWaitMatchesReleasePredicate(t *testing.T) {
	db, _ := testPostgres(t)
	p := dependencyWaitJob(t, db, 1, "running")
	j := dependencyWaitJob(t, db, 2, "queued")
	waitEdge(t, db, j, p)
	cases := []struct {
		name, status, view string
		ready              bool
	}{
		{"pending", "running", `{}`, false}, {"merged", "merged", `{}`, true}, {"closed without evidence", "closed", `{}`, false},
		{"owned", "closed", fmt.Sprintf(`{"acceptanceTransferWait":{"ownedTargets":{"%d":{}}}}`, p), true},
		{"merge", "closed", fmt.Sprintf(`{"dependencyMerges":{"%d":{"pr":1,"mergeCommit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`, p), true},
		{"legacy unrelated", "closed", fmt.Sprintf(`{"dependencyRelations":{"%d":{"relation":"unrelated","evidence":"proof"}}}`, p), false},
		{"unrelated", "closed", fmt.Sprintf(`{"dependencyReviews":{"test/repo#1":{"relation":"unrelated","evidence":"proof","consultationId":"%s","at":"2026-11-06T00:00:00Z"}}}`, TransferConsultationID(j, "test/repo#1")), true},
		{"invalid consultation", "closed", `{"dependencyReviews":{"test/repo#1":{"relation":"unrelated","evidence":"proof","consultationId":"invalid","at":"2026-11-06T00:00:00Z"}}}`, false},
		{"required", "closed", fmt.Sprintf(`{"dependencyReviews":{"test/repo#1":{"relation":"required","evidence":"proof","consultationId":"%s","at":"2026-11-06T00:00:00Z"}}}`, TransferConsultationID(j, "test/repo#1")), false},
		{"invalid timestamp", "closed", fmt.Sprintf(`{"dependencyReviews":{"test/repo#1":{"relation":"unrelated","evidence":"proof","consultationId":"%s","at":"2026-02-30T00:00:00Z"}}}`, TransferConsultationID(j, "test/repo#1")), false},
		{"long evidence", "closed", fmt.Sprintf(`{"dependencyReviews":{"test/repo#1":{"relation":"unrelated","evidence":"%s","consultationId":"%s","at":"2026-11-06T00:00:00Z"}}}`, strings.Repeat("😀", 2001), TransferConsultationID(j, "test/repo#1")), false},
		{"empty evidence", "closed", fmt.Sprintf(`{"dependencyRelations":{"%d":{"relation":"unrelated","evidence":" "}}}`, p), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			waitExec(t, db, `UPDATE jobs SET status=$1 WHERE id=$2`, c.status, p)
			waitExec(t, db, `UPDATE jobs SET view=$1::jsonb WHERE id=$2`, c.view, j)
			var ready bool
			if err := db.QueryRow(`SELECT `+DependencyPairReadySQL+` FROM job_deps d JOIN jobs p ON p.id=d.depends_on JOIN jobs j ON j.id=d.job_id WHERE d.job_id=$1 AND d.depends_on=$2`, j, p).Scan(&ready); err != nil {
				t.Fatal(err)
			}
			if ready != c.ready {
				t.Fatalf("predicate=%v", ready)
			}
			assertWait(t, db, j, p, !ready, false)
		})
	}
}
func beforeWaitMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	waitExec(t, db, `DROP TRIGGER dependency_wait_job_update ON jobs; DROP TRIGGER dependency_wait_edge_insert ON job_deps; DROP FUNCTION dependency_wait_job(); DROP FUNCTION dependency_wait_edge(); DROP FUNCTION dependency_waiting(bigint,bigint); ALTER TABLE job_deps DROP COLUMN wait_started_at, DROP COLUMN wait_unknown; DROP TABLE native_test_requests; DROP TABLE server_takeovers; DELETE FROM schema_migrations WHERE version>=36`)
}
func TestDependencyWaitReviewMigrationReconcilesExistingEdges(t *testing.T) {
	db, ctx := testPostgres(t)
	beforeWaitMigration(t, db)
	migrations, err := store.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	waitExec(t, db, migrations[35].SQL)
	waitExec(t, db, `INSERT INTO schema_migrations(version,name) VALUES(36,'dependency_wait_started')`)
	p := dependencyWaitJob(t, db, 1, "closed")
	legacy := dependencyWaitJob(t, db, 2, "queued")
	valid := dependencyWaitJob(t, db, 3, "queued")
	pending := dependencyWaitJob(t, db, 4, "queued")
	waitExec(t, db, `UPDATE jobs SET view=$1::jsonb WHERE id=$2`, fmt.Sprintf(`{"dependencyRelations":{"%d":{"relation":"unrelated","evidence":"proof"}}}`, p), legacy)
	waitExec(t, db, `UPDATE jobs SET view=$1::jsonb WHERE id=$2`, fmt.Sprintf(`{"dependencyReviews":{"test/repo#1":{"relation":"unrelated","evidence":"proof","consultationId":"%s","at":"2026-11-06T00:00:00Z"}}}`, TransferConsultationID(valid, "test/repo#1")), valid)
	waitEdge(t, db, legacy, p)
	waitEdge(t, db, valid, p)
	waitEdge(t, db, pending, p)
	waitExec(t, db, `UPDATE job_deps SET wait_started_at='2000-01-01T00:00:00Z' WHERE job_id=$1`, pending)
	if _, err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertWait(t, db, legacy, p, false, true)
	assertWait(t, db, valid, p, false, false)
	if got := assertWait(t, db, pending, p, true, false); !got.Time.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("existing wait time changed: %v", got.Time)
	}
}
func TestDependencyWaitUnknownForPreMigrationWaits(t *testing.T) {
	db, ctx := testPostgres(t)
	beforeWaitMigration(t, db)
	p := dependencyWaitJob(t, db, 1, "running")
	j := dependencyWaitJob(t, db, 2, "queued")
	waitEdge(t, db, j, p)
	waitExec(t, db, `UPDATE jobs SET updated_at='2000-01-01' WHERE id=$1`, j)
	if _, err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertWait(t, db, j, p, false, true)
	waitExec(t, db, `UPDATE jobs SET status='merged' WHERE id=$1`, p)
	assertWait(t, db, j, p, false, false)
	waitExec(t, db, `UPDATE jobs SET status='running' WHERE id=$1`, p)
	assertWait(t, db, j, p, true, false)
}

func TestDependencyWaitSurvivesDumpRestore(t *testing.T) {
	bin := os.Getenv("ISSUE_DESK_TEST_POSTGRES_BIN")
	for _, name := range []string{"pg_dump", "pg_restore", "psql"} {
		tool := name
		if bin != "" {
			tool = filepath.Join(bin, name)
		}
		if _, err := exec.LookPath(tool); err != nil {
			if bin != "" {
				t.Fatalf("PostgreSQL backup tool %s: %v", tool, err)
			}
			t.Skip("PostgreSQL backup tools are not configured")
		}
	}
	db, ctx := testPostgres(t)
	beforeWaitMigration(t, db)
	p := dependencyWaitJob(t, db, 1, "running")
	j := dependencyWaitJob(t, db, 2, "queued")
	waitEdge(t, db, j, p)
	if _, err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	// A claimed native request must remain resumable after the same backup restore.
	waitExec(t, db, `INSERT INTO native_test_requests(id,kind,repo,payload,sender,status,claimed_by,claimed_at) VALUES('1794530000009-aabbccdd','native_test','owner/repo',jsonb_build_object('host','mac-test','head',repeat('a',40),'base',repeat('b',40),'tree',repeat('c',40)),'nas','claimed','mac-test',now())`)
	p2 := dependencyWaitJob(t, db, 3, "running")
	waitEdge(t, db, j, p2)
	var schema string
	if err := db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	restored := schema + "_restored"
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := db.ExecContext(c, `DROP SCHEMA IF EXISTS `+restored+` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	dump := filepath.Join(t.TempDir(), "backup.dump")
	cmd := exec.Command(filepath.Join(bin, "pg_dump"), "--format=custom", "--schema="+schema, "--file="+dump, dsn)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pg_dump: %v: %s", err, output)
	}
	cmd = exec.Command(filepath.Join(bin, "pg_restore"), "--no-owner", "--no-privileges", "--file=-", dump)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pg_restore: %v: %s", err, output)
	}
	script := strings.ReplaceAll(string(output), schema, restored)
	cmd = exec.Command(filepath.Join(bin, "psql"), "--set=ON_ERROR_STOP=1", dsn)
	cmd.Stdin = strings.NewReader(script)
	if output, err = cmd.CombinedOutput(); err != nil {
		t.Fatalf("restore SQL: %v: %s", err, output)
	}
	rows := func(table string) string {
		t.Helper()
		var value string
		if err := db.QueryRow(`SELECT COALESCE(jsonb_agg(jsonb_build_array(job_id,depends_on,wait_started_at,wait_unknown) ORDER BY job_id,depends_on),'[]'::jsonb)::text FROM ` + table).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	original := rows(schema + `.job_deps`)
	if got := rows(restored + `.job_deps`); got != original || got == "[]" {
		t.Fatalf("restored %s, original %s", got, original)
	}
	nativeRow := func(schema string) string {
		t.Helper()
		var value string
		if err := db.QueryRow(`SELECT to_jsonb(r)::text FROM ` + schema + `.native_test_requests r WHERE id='1794530000009-aabbccdd' AND status='claimed' AND claimed_by='mac-test'`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if before, after := nativeRow(schema), nativeRow(restored); before != after {
		t.Fatalf("Mac検証要求の復旧で状態・対象が変わりました: %s %s", before, after)
	}
	assertWait(t, db, j, p, false, true)
	assertWait(t, db, j, p2, true, false)
}

func TestSharedDependencyReadinessUpgrade(t *testing.T) {
	db, ctx := testPostgres(t)
	beforeWaitMigration(t, db)
	waitExec(t, db, `DROP FUNCTION dependency_pair_ready(jobs,jobs)`)
	migrations, err := store.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[35:37] {
		waitExec(t, db, m.SQL)
		waitExec(t, db, `INSERT INTO schema_migrations(version,name) VALUES($1,$2)`, m.Version, m.Name)
	}
	p := dependencyWaitJob(t, db, 1, "closed")
	j := dependencyWaitJob(t, db, 2, "queued")
	waitEdge(t, db, j, p)
	waitExec(t, db, `DROP FUNCTION dependency_pair_ready(jobs,jobs)`)
	result, err := store.Migrate(ctx, db)
	if err != nil || len(result.Applied) != len(migrations)-37 || result.Applied[0] != 38 {
		t.Fatalf("upgrade: %+v %v", result, err)
	}
	for _, tc := range []struct {
		status string
		ready  bool
	}{{"closed", false}, {"merged", true}, {"running", false}} {
		waitExec(t, db, `UPDATE jobs SET status=$1 WHERE id=$2`, tc.status, p)
		ready, err := DependenciesReady(ctx, db, j, map[string]any{"dependsOnIssues": []any{map[string]any{"repo": "test/repo", "issue": float64(1)}}})
		if err != nil || ready != tc.ready {
			t.Fatalf("status %s: ready=%v error=%v", tc.status, ready, err)
		}
		assertWait(t, db, j, p, !tc.ready, false)
	}
}
