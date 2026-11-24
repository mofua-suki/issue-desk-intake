package jobs

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"testing"
)

// readTransfers reads every job as the unscoped reference for scoped graph reads.
func readTransfers(ctx context.Context, q transferQuery) ([]*transferJob, error) {
	return readTransferRows(ctx, q, `SELECT j.id,j.repo,j.issue,j.title,j.status,coalesce(j.provider,'codex'),j.view,j.result,array_to_json(ARRAY(SELECT depends_on FROM job_deps WHERE job_id=j.id))FROM jobs j`)
}

func TestTransferGraphReadScopedToOpenJobs(t *testing.T) {
	db, ctx := testPostgres(t)
	_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,hold_reason,view) VALUES
 (1,'r',1,'queued',NULL,'{"dependsOnIssues":[{"repo":"R","issue":2}]}'),
 (2,'r',2,'merged',NULL,'{"unresolvedDependencies":[{"repo":"r","issue":3}]}'),
 (3,'r',3,'closed',NULL,'{}'), (4,'r',4,'held','question','{}'), (5,'r',5,'running',NULL,'{}')`)
	if err != nil {
		t.Fatal(err)
	}
	before, err := readScopedTransferGraph(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 5 {
		t.Fatalf("initial graph has %d rows, want 5", len(before))
	}
	_, err = db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status) SELECT n,'history',n,'closed' FROM generate_series(100,599) n`)
	if err != nil {
		t.Fatal(err)
	}
	after, err := readScopedTransferGraph(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("graph grew from %d to %d rows", len(before), len(after))
	}
}

func TestTransferGraphKeepsCompletedSourceReferences(t *testing.T) {
	db, ctx := testPostgres(t)
	source, destination, consumer := transferFixture()
	initial := newTransferIndex([]*transferJob{source, destination, consumer}).plan(consumer)
	key := fleet.Text(fleet.Object(initial.Checks[0])["key"])
	consumer.View["acceptanceTransferChecks"] = map[string]any{key: map[string]any{"relation": "required", "evidence": "checked"}}
	for _, j := range []*transferJob{source, destination, consumer} {
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view,result) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb)`, j.ID, j.Repo, j.Issue, j.Title, j.Status, transferJSON(j.View), transferJSON(j.Result)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(3,1)`); err != nil {
		t.Fatal(err)
	}
	scoped, err := readScopedTransferGraph(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	full, err := readTransfers(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var selected *transferJob
	ids := map[int64]bool{}
	for _, j := range scoped {
		ids[j.ID] = true
		if j.ID == 3 {
			selected = j
		}
	}
	if !ids[1] || !ids[2] || selected == nil {
		t.Fatalf("missing referenced jobs: %v", ids)
	}
	var original *transferJob
	for _, j := range full {
		if j.ID == 3 {
			original = j
		}
	}
	a, b := newTransferIndex(scoped).plan(selected), newTransferIndex(full).plan(original)
	if len(a.Checks) != 1 || len(a.Waits) != 1 || len(a.Targets) != 1 {
		t.Fatalf("expected required transfer: %#v", a)
	}
	if !reflect.DeepEqual(a.Checks, b.Checks) || !reflect.DeepEqual(a.Waits, b.Waits) || !reflect.DeepEqual(a.Targets, b.Targets) {
		t.Fatalf("scoped plan differs: %#v vs %#v", a, b)
	}
}

func TestPlanTransfersIndexBuiltOncePerRefresh(t *testing.T) {
	db, ctx := testPostgres(t)
	source, destination, consumer := transferFixture()
	another := *consumer
	another.ID = 4
	another.Issue = 4
	for _, j := range []*transferJob{source, destination, consumer, &another} {
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view,result) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb)`, j.ID, j.Repo, j.Issue, j.Title, j.Status, transferJSON(j.View), transferJSON(j.Result)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) VALUES(3,1),(4,1)`); err != nil {
		t.Fatal(err)
	}
	all, err := readScopedTransferGraph(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	builds := 0
	build := func(jobs []*transferJob) *transferIndex { builds++; return newTransferIndex(jobs) }
	s := &Store{}
	prepared := s.prepareTransferGraphWithIndexBuilder(context.Background(), all, build)
	if builds != 1 || len(prepared) != 2 {
		t.Fatalf("prepare: builds=%d prepared=%d", builds, len(prepared))
	}
	builds = 0
	err = applyTransferChangesWithIndexBuilder(ctx, db, all, prepared, "2026-01-01T00:00:00.000Z", build)
	if err != nil || builds != 1 {
		t.Fatalf("apply: builds=%d err=%v", builds, err)
	}
	var checked int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE id IN (3,4) AND jsonb_array_length(view->'acceptanceTransferWait'->'checks')=1`).Scan(&checked); err != nil || checked != 2 {
		t.Fatalf("checks written=%d err=%v", checked, err)
	}
}
