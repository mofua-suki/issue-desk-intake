package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"
)

func TestPostgresTransferGateRefreshesEvidenceAndOwnsOnlyItsEdges(t *testing.T) {
	db, ctx := testPostgres(t)
	db.SetMaxOpenConns(1)
	source, _, _ := transferFixture()
	records := source.View["acceptanceTransferRecords"]
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,title,status,view)VALUES(1,'owner/repo',1,'source','merged',jsonb_build_object('acceptanceTransferRecords',$1::jsonb)),(2,'owner/repo',2,'producer','closed','{}'),(3,'owner/repo',3,'use artifact','queued','{}')`, transferJSON(records)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO job_deps(job_id,depends_on)VALUES(3,1)"); err != nil {
		t.Fatal(err)
	}
	body := "current body"
	s, err := New(db, Options{ReadIssue: func(ctx context.Context, repo string, number int64) (map[string]any, error) {
		check, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var value int
		if err := db.QueryRowContext(check, "SELECT 42").Scan(&value); err != nil {
			return nil, err
		}
		return map[string]any{"number": json.Number("3"), "title": "use artifact", "body": body}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	refresh := func() map[string]any {
		t.Helper()
		prepared, err := s.prepareTransfers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.transaction(ctx, func(tx *sql.Tx) error { return refreshTransfers(ctx, tx, prepared, iso(time.Now())) }); err != nil {
			t.Fatal(err)
		}
		job, err := s.Get(ctx, 3)
		if err != nil {
			t.Fatal(err)
		}
		return fleet.Object(job.View["acceptanceTransferWait"])
	}
	wait := refresh()
	if wait["blocking"] != true {
		t.Fatal("unknown transfer released")
	}
	setRelation := func(wait map[string]any, relation string) {
		t.Helper()
		key := fleet.Text(fleet.Object(fleet.List(wait["checks"])[0])["key"])
		if _, err := db.ExecContext(ctx, "UPDATE jobs SET view=jsonb_set(view,'{acceptanceTransferChecks}',$1::jsonb)WHERE id=3", transferJSON(map[string]any{key: map[string]any{"relation": relation, "evidence": "checked current body"}})); err != nil {
			t.Fatal(err)
		}
	}
	setRelation(wait, "required")
	wait = refresh()
	if wait["blocking"] != true {
		t.Fatal("closed destination released")
	}
	var edge int
	if err = db.QueryRowContext(ctx, "SELECT count(*)FROM job_deps WHERE job_id=3 AND depends_on=2").Scan(&edge); err != nil || edge != 1 {
		t.Fatalf("required edge %d %v", edge, err)
	}
	if _, err = db.ExecContext(ctx, "UPDATE jobs SET status='merged',result=$1::jsonb WHERE id=2", transferJSON(map[string]any{"mergeCommit": strings.Repeat("a", 40)})); err != nil {
		t.Fatal(err)
	}
	if refresh()["blocking"] != false {
		t.Fatal("verified merged destination still blocked")
	}
	body = "edited request"
	wait = refresh()
	if wait["blocking"] != true {
		t.Fatal("request edit reused stale relation")
	}
	setRelation(wait, "unrelated")
	wait = refresh()
	if wait["blocking"] != false {
		t.Fatal("unrelated relation not released")
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*)FROM job_deps WHERE job_id=3 AND depends_on=2").Scan(&edge); err != nil || edge != 0 {
		t.Fatalf("owned edge retained %d %v", edge, err)
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*)FROM job_deps WHERE job_id=3 AND depends_on=1").Scan(&edge); err != nil || edge != 1 {
		t.Fatalf("ordinary edge removed %d %v", edge, err)
	}
}
