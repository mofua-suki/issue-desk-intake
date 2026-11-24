package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestPostgresFilingReferencesSharedGraphAndStableNoWrites(t *testing.T) {
	db, ctx := testPostgres(t)
	s, _ := New(db, Options{})
	for i := 1; i <= 4; i++ {
		view := `{"dependsOnIssues":[{"repo":"owner/app","issue":1}]}`
		if i == 1 {
			view = `{}`
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view,result) VALUES($1::bigint,'owner/app',$1::integer,'test','running',$2::jsonb,'{}')`, i, view); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(2,1),(3,1),(4,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE update_count(n int); CREATE FUNCTION count_filing_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO update_count VALUES (1); RETURN NEW; END $$; CREATE TRIGGER count_filing_update AFTER UPDATE ON jobs FOR EACH ROW EXECUTE FUNCTION count_filing_update()`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `LOCK TABLE jobs,job_deps IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	reads := 0
	reader := func(ctx context.Context, q transferQuery) ([]*transferJob, error) {
		reads++
		return readTransferGraph(ctx, q)
	}
	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	builds := 0
	builder := func(all []*transferJob) *dependencyIndex { builds++; return newDependencyIndex(all) }
	if err = s.resolveFilingReferencesWithIndexBuilder(short, reader, builder); err != nil {
		t.Fatalf("stable cycle under lock: %v", err)
	}
	var updates int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM update_count`).Scan(&updates); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || updates != 0 || builds != 1 {
		t.Fatalf("reads=%d updates=%d builds=%d", reads, updates, builds)
	}
}

func TestPostgresFilingReferencesOwnedAndLockedReread(t *testing.T) {
	db, ctx := testPostgres(t)
	s, _ := New(db, Options{})
	for i := 1; i <= 3; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view,result) VALUES($1::bigint,'owner/app',$1::integer,'test','running','{}','{}')`, i); err != nil {
			t.Fatal(err)
		}
	}
	view := `{"dependsOnIssues":[{"repo":"owner/app","issue":1}],"acceptanceTransferWait":{"ownedTargets":{"1":["key"]}}}`
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=$1::jsonb WHERE id=3`, view); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(3,1)`); err != nil {
		t.Fatal(err)
	}
	reads, locked := 0, 0
	reader := func(ctx context.Context, q transferQuery) ([]*transferJob, error) {
		reads++
		if _, ok := q.(*sql.Tx); ok {
			locked++
		}
		return readTransferGraph(ctx, q)
	}
	if err := s.resolveFilingReferencesWithReader(ctx, reader); err != nil {
		t.Fatal(err)
	}
	var owned bool
	if err := db.QueryRowContext(ctx, `SELECT view->'acceptanceTransferWait'->'ownedTargets' ? '1' FROM jobs WHERE id=3`).Scan(&owned); err != nil || owned {
		t.Fatalf("owned=%v err=%v", owned, err)
	}
	if reads != 2 || locked != 1 {
		t.Fatalf("reads=%d locked=%d", reads, locked)
	}
	// Change the reference after the unlocked snapshot. The locked read must win.
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=$1::jsonb WHERE id=3`, view); err != nil {
		t.Fatal(err)
	}
	reads = 0
	locked = 0
	reader = func(ctx context.Context, q transferQuery) ([]*transferJob, error) {
		reads++
		if _, ok := q.(*sql.Tx); ok {
			locked++
		}
		all, err := readTransferGraph(ctx, q)
		if err == nil && reads == 1 {
			_, err = db.ExecContext(ctx, `UPDATE jobs SET view='{"dependsOnIssues":[{"repo":"owner/app","issue":2}]}' WHERE id=3`)
		}
		return all, err
	}
	if err := s.resolveFilingReferencesWithReader(ctx, reader); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_deps WHERE job_id=3 AND depends_on=2)`).Scan(&exists); err != nil || !exists {
		t.Fatalf("fresh edge=%v err=%v reads=%s", exists, err, fmt.Sprint(reads))
	}
}

func TestPostgresFilingProbePreservesSnapshot(t *testing.T) {
	db, ctx := testPostgres(t)
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view,result) VALUES (1,'owner/app',1,'test','running','{}','{}'), (2,'owner/app',2,'test','running','{"dependsOnIssues":[{"repo":"owner/app","issue":1}],"unresolvedDependencies":[{"repo":"owner/app","issue":1}],"acceptanceTransferWait":{"ownedTargets":{"1":["key"]}}}','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	all, err := readTransferGraph(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := New(db, Options{})
	// Abort immediately after the probe signals a write, before the locked reread.
	reads := 0
	reader := func(ctx context.Context, q transferQuery) ([]*transferJob, error) {
		reads++
		if reads == 1 {
			return all, nil
		}
		return nil, errors.New("stop after probe")
	}
	if err := s.resolveFilingReferencesWithReader(ctx, reader); err == nil || err.Error() != "stop after probe" {
		t.Fatalf("expected write probe, got %v", err)
	}
	after, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot changed: %s -> %s", before, after)
	}
}

func TestPostgresFilingReferencesResolveAcrossCycles(t *testing.T) {
	db, ctx := testPostgres(t)
	s, _ := New(db, Options{})
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view,result) VALUES (2,'owner/app',2,'test','running','{"dependsOnIssues":[{"repo":"owner/app","issue":1}]}','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveFilingReferences(ctx); err != nil {
		t.Fatal(err)
	}
	var pending bool
	if err := db.QueryRowContext(ctx, `SELECT view->'unresolvedDependencies' @> '[{"repo":"owner/app","issue":1}]'::jsonb FROM jobs WHERE id=2`).Scan(&pending); err != nil || !pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view,result) VALUES (1,'owner/app',1,'test','running','{}','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveFilingReferences(ctx); err != nil {
		t.Fatal(err)
	}
	var edge, unresolved bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_deps WHERE job_id=2 AND depends_on=1), view ? 'unresolvedDependencies' FROM jobs WHERE id=2`).Scan(&edge, &unresolved); err != nil || !edge || unresolved {
		t.Fatalf("edge=%v unresolved=%v err=%v", edge, unresolved, err)
	}
}
