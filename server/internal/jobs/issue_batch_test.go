package jobs

import (
	"errors"
	"reflect"

	"issue-desk-intake/server/internal/contract"
	"testing"
)

func TestPostgresGetBatch(t *testing.T) {
	db, ctx := testPostgres(t)
	store, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := store.GetBatch(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty: %v %v", empty, err)
	}
	rows, err := db.QueryContext(ctx, `INSERT INTO jobs(repo,issue,status,view) SELECT 'batch/test',n,'running','{"reviewHolds":2}'::jsonb FROM generate_series(1,501) n RETURNING id`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var outsideID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,view) VALUES('batch/test',999,'merged','{}') RETURNING id`).Scan(&outsideID); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetBatch(ctx, append(ids, ids[0]))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[outsideID]; ok {
		t.Fatal("unrequested job returned")
	}
	if len(got) != len(ids) {
		t.Fatalf("got %d jobs", len(got))
	}
	for _, id := range ids {
		want, err := store.Get(ctx, id)
		if err != nil || !reflect.DeepEqual(got[id], want) {
			t.Fatalf("job %d differs: %v", id, err)
		}
	}
	if _, err := store.GetBatch(ctx, []int64{ids[0], -1}); err == nil {
		t.Fatal("missing job accepted")
	} else {
		var problem *contract.Problem
		if !errors.As(err, &problem) || problem.Status != 404 {
			t.Fatalf("missing job error: %v", err)
		}
	}
	// JSONB can hold a value of the wrong shape even though its syntax is valid.
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET view='[]'::jsonb WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBatch(ctx, ids[:1]); err == nil {
		t.Fatal("invalid view silently accepted")
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE jobs CASCADE"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBatch(ctx, ids[:1]); err == nil {
		t.Fatal("read error ignored")
	}
}
