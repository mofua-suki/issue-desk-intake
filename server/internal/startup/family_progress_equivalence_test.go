package startup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
	"issue-desk-intake/server/internal/usage"
	"strings"
)

func TestPostgresFamilyProgressFilteredMatchesFull(t *testing.T) {
	db := appPostgres(t)
	ctx := context.Background()
	store, err := jobs.New(db, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ns, err := numbers.OpenLedger(ctx, db, filepath.Join(t.TempDir(), "state-desk-numbers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,status,result,hold_reason,view) SELECT 'outside/repo',n,'queued','{}',NULL,'{}' FROM generate_series(1000,5999) n`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO desk_numbers(number,project,gitea,assigned_at) SELECT n,'outside/repo',n,'2026-01-01' FROM generate_series(1000,5999) n`)
	if err != nil {
		t.Fatal(err)
	}
	statuses := []string{"merged", "closed", "failed", "held", "stopped", "queued", "queued"}
	for i, status := range statuses {
		view := `{}`
		if i == 5 {
			view = `{"dependsOnIssues":[{"repo":"owner/repo","issue":7}]}`
		}
		if i == 6 {
			view = `{"dependsOnIssues":[{"repo":"owner/repo","issue":99}]}`
		}
		_, err = db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,status,result,hold_reason,view) VALUES('Owner/Repo',$1,$2,'{"error":"failure"}',CASE WHEN $2='held' THEN 'question' ELSE NULL END,$3::jsonb)`, i+1, status, view)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.ExecContext(ctx, `INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES($1,'Owner/Repo',$1,'2026-01-01')`, i+1)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = db.ExecContext(ctx, `INSERT INTO job_deps(job_id,depends_on) SELECT a.id,b.id FROM jobs a,jobs b WHERE a.repo='Owner/Repo' AND a.issue=6 AND b.repo='Owner/Repo' AND b.issue=7`)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Jobs: store, Numbers: ns}
	ids := []any{}
	records := []consultations.Record{{"id": "parent"}}
	for i := 1; i <= 7; i++ {
		id := fmt.Sprint(i)
		ids = append(ids, id)
		var n any = i
		if i == 2 {
			n = "2"
		}
		if i == 3 {
			n = json.Number("3")
		}
		records = append(records, consultations.Record{"id": id, "parentSplit": "parent", "project": "owner/repo", "issue": consultations.Record{"number": n}})
	}
	ids = append(ids, "finished", "missing", "noncanonical")
	records = append(records, consultations.Record{"id": "finished", "parentSplit": "parent", "status": "submitted", "finishedWithoutIssue": true}, consultations.Record{"id": "noncanonical", "parentSplit": "parent", "project": "owner/repo", "issue": consultations.Record{"number": "01"}})
	records[0]["splitChildren"] = ids
	records[0]["splitIssues"] = make([]any, len(ids))
	legacy := func(ctx context.Context, _ []consultations.FamilyIssue) ([]consultations.FamilyJob, error) {
		all, e := store.AllForIssues(ctx)
		if e != nil {
			return nil, e
		}
		waiting, e := legacyFamilyDependencies(ctx, db, all)
		if e != nil {
			return nil, e
		}
		desk, e := ns.All(ctx)
		if e != nil {
			return nil, e
		}
		result := make([]consultations.FamilyJob, 0, len(all))
		for _, j := range all {
			var n any
			if v, ok := desk[numbers.Issue{Project: j.Repo, Number: j.Issue}]; ok {
				n = v
			}
			reason := ""
			if j.Status == "held" && j.HoldReason != nil {
				reason = *j.HoldReason
			}
			if j.Status == "failed" {
				reason, _ = j.Result["error"].(string)
			}
			reason = usage.Redact(strings.Join(strings.Fields(reason), " "))
			if len([]rune(reason)) > 500 {
				reason = string([]rune(reason)[:500])
			}
			result = append(result, consultations.FamilyJob{Project: j.Repo, Number: j.Issue, Desk: n, Status: j.Status, Reason: reason, Waiting: waiting[j.ID]})
		}
		return result, nil
	}
	mk := func(reader consultations.FamilyJobs) *consultations.Service {
		s, e := consultations.New(consultations.Options{FamilyJobs: reader, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }, Records: &consultations.Records{}})
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	start := time.Now()
	before := mk(legacy).FamilyProgress(ctx, records)
	fullTime := time.Since(start)
	start = time.Now()
	after := mk(app.familyJobs).FamilyProgress(ctx, records)
	filteredTime := time.Since(start)
	if !reflect.DeepEqual(before[0]["familyProgress"], after[0]["familyProgress"]) {
		t.Fatalf("full=%v filtered=%v", before[0]["familyProgress"], after[0]["familyProgress"])
	}
	t.Logf("full jobs=5007 numbers=5007 deps=1 elapsed=%s; filtered jobs=7 numbers=7 deps=1 elapsed=%s", fullTime, filteredTime)
}

// legacyFamilyDependencies reproduces the former unfiltered dependency query.
func legacyFamilyDependencies(ctx context.Context, db *sql.DB, all []*jobs.Job) (map[int64][]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT d.job_id,p.repo,p.issue,(`+jobs.PredecessorReadySQL+`) FROM job_deps d JOIN jobs p ON p.id=d.depends_on ORDER BY p.repo,p.issue`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	waiting := map[int64][]string{}
	edges := map[int64]map[string]bool{}
	for rows.Next() {
		var id, n int64
		var repo string
		var ready bool
		if err = rows.Scan(&id, &repo, &n, &ready); err != nil {
			return nil, err
		}
		key := strings.ToLower(fmt.Sprintf("%s#%d", repo, n))
		if edges[id] == nil {
			edges[id] = map[string]bool{}
		}
		edges[id][key] = ready
		if !ready {
			waiting[id] = append(waiting[id], fmt.Sprintf("%s#%d", repo, n))
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, j := range all {
		valid, e := jobs.DependencyReferences(j.View, func(repo string, n int64) (bool, error) {
			key := strings.ToLower(fmt.Sprintf("%s#%d", repo, n))
			if _, ok := edges[j.ID][key]; !ok {
				waiting[j.ID] = append(waiting[j.ID], fmt.Sprintf("%s#%d（依存の記録なし）", repo, n))
			}
			return true, nil
		})
		if e != nil {
			return nil, e
		}
		if !valid {
			waiting[j.ID] = append(waiting[j.ID], "依存の記録が不正です")
		}
	}
	return waiting, nil
}
