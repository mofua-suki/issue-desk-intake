package filingcheck

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

func TestMaterialMatchesLegacyFullRead(t *testing.T) {
	ctx := t.Context()
	env := pgtest.NewIsolated(t)
	db := env.DB
	jobSchema := env.Schema + "_jobs"
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+jobSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), "DROP SCHEMA "+jobSchema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", jobSchema)
	u.RawQuery = query.Encode()
	jobDB, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobDB.Close() })
	if _, err := store.Migrate(ctx, jobDB); err != nil {
		t.Fatal(err)
	}
	records, err := consultations.OpenRecords(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobDB, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int64{8, 9, 11, 12, 13} {
		view := map[string]any{}
		if n == 12 {
			view["body"] = "古いジョブ本文 old.go"
		}
		if _, err := queue.Create(ctx, jobs.NewJob{Repo: "owner/repo", Issue: n, Title: "job", Scope: []string{"file.go"}, View: view}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := jobDB.ExecContext(ctx, "UPDATE jobs SET status='held', hold_reason='question' WHERE issue IN (11, 12, 13)"); err != nil {
		t.Fatal(err)
	}
	failIssues := false
	dependencyState := "open"
	dependencyPR := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			if failIssues {
				http.Error(w, "fixture failure", http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode([]any{Record{"number": 8, "title": "dependency", "state": "open", "body": "dependency body", "user": Record{}}, Record{"number": 9, "title": "replaced", "state": "open", "body": "replacement body", "user": Record{}}, Record{"number": 10, "title": "other", "state": "open", "body": "other body", "user": Record{}}, Record{"number": 11, "state": "closed", "body": "共有目的 file.go", "user": Record{}}, Record{"number": 12, "state": "open", "body": "共有目的 file.go", "user": Record{}}})
		case "/api/v1/repos/owner/repo/issues/8":
			issue := Record{"number": 8, "title": "dependency", "state": dependencyState}
			if dependencyPR {
				issue["pull_request"] = Record{}
			}
			json.NewEncoder(w).Encode(issue)
		case "/api/v1/repos/owner/repo/pulls/8":
			json.NewEncoder(w).Encode(Record{"state": "closed", "merged": true})
		case "/api/v1/repos/owner/repo/issues/9":
			json.NewEncoder(w).Encode(Record{"number": 9, "title": "replaced", "state": "open"})
		case "/api/v1/repos/owner/repo":
			json.NewEncoder(w).Encode(Record{"default_branch": "main"})
		case "/api/v1/repos/owner/repo/branches/main":
			json.NewEncoder(w).Encode(Record{"commit": Record{"id": strings.Repeat("a", 40)}})
		case "/api/v1/repos/owner/repo/git/trees/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":
			json.NewEncoder(w).Encode(Record{"tree": []any{}, "truncated": false})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "test-token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New(server.URL, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{options: Options{Records: records, Jobs: queue, Gitea: client, Project: func(string) error { return nil }, Own: func(map[string]any) bool { return true }, DeskNumber: func(project string, n int64) any {
		if project == "owner/repo" && n == 8 {
			return int64(5435)
		}
		return nil
	}, Config: func(context.Context) (map[string]any, error) { return map[string]any{}, nil }}}
	rows := []Record{
		{"id": "grand", "project": "owner/repo", "createdAt": "1", "title": "grand", "body": "ancestor body", "messages": []any{Record{"role": "user", "text": "ancestor message"}}},
		{"id": "parent", "parentConsultation": "grand", "project": "owner/repo", "createdAt": "2", "body": "parent body", "splitChildren": []any{"sibling", "target", "unfiled"}, "splitIssues": []any{Record{}, Record{}, Record{}}},
		{"id": "target", "parentSplit": "parent", "project": "owner/repo", "createdAt": "4", "status": "submitting", "body": "target body", "changeScope": Record{"paths": []any{"file.go"}}, "replaceIssues": []any{9}, "dependencies": []any{8}, "dependsOnIssues": []any{Record{"source": "gitea", "number": 8}}},
		{"id": "sibling", "parentSplit": "parent", "project": "owner/repo", "createdAt": "3", "status": "auto_ready", "issue": Record{"project": "owner/repo", "number": 8}, "changeScope": Record{"paths": []any{"file.go"}}},
		{"id": "unfiled", "parentSplit": "parent", "project": "owner/repo", "status": "draft", "title": "未起票", "body": "未起票の本文"},
		{"id": "prior", "project": "owner/repo", "createdAt": "0", "status": "draft", "title": "prior", "body": "prior body", "messages": []any{Record{"text": "message"}}},
		{"id": "later", "project": "owner/repo", "createdAt": "9", "status": "draft", "body": "later body"},
		{"id": "inactive", "project": "owner/repo", "createdAt": "0", "status": "closed", "body": "inactive body"},
		{"id": "other", "project": "elsewhere/repo", "createdAt": "0", "status": "draft", "body": "other body"},
	}
	for _, row := range rows {
		id := fleet.Text(row["id"])
		if _, err := records.Change(ctx, "drafts", id, func(*sql.Tx, consultations.Record) (consultations.Record, error) { return row, nil }); err != nil {
			t.Fatal(err)
		}
	}
	target := rows[2]
	old, err := s.legacyMaterial(ctx, target)
	if err != nil {
		t.Fatalf("legacy: %v", err)
	}
	got, err := s.material(ctx, target)
	if err != nil {
		t.Fatalf("material: %v", err)
	}
	if !reflect.DeepEqual(old, got) {
		t.Fatalf("legacy %s\nnew %s", raw(old), raw(got))
	}
	siblings := fleet.Elements(obj(obj(got["target"])["decisionContext"])["splitSiblings"])
	if len(siblings) != 2 || fleet.WholeNumber(obj(siblings[0])["proposalNumber"]) != 1 || fleet.WholeNumber(obj(siblings[0])["giteaNumber"]) != 8 || fleet.WholeNumber(obj(siblings[0])["commonNumber"]) != 5435 || obj(siblings[0])["state"] != "open" || obj(siblings[1])["body"] != "未起票の本文" {
		t.Fatalf("siblings: %v", siblings)
	}
	if len(fleet.Elements(obj(got["target"])["dependsOnIssues"])) != 1 {
		t.Fatalf("duplicate dependencies: %v", got)
	}
	for _, r := range fleet.Elements(got["requests"]) {
		if fleet.WholeNumber(obj(r)["number"]) == 8 {
			t.Fatal("sibling is a duplicate candidate")
		}
	}
	if len(fleet.Elements(got["replacements"])) != 1 || fleet.WholeNumber(obj(fleet.Elements(got["replacements"])[0])["number"]) != 9 {
		t.Fatalf("replacement missing: %v", got)
	}
	if len(fleet.Elements(got["consultations"])) != 1 || fleet.Text(obj(fleet.Elements(got["consultations"])[0])["id"]) != "prior" {
		t.Fatalf("prior missing: %v", got)
	}
	if len(fleet.Elements(obj(obj(got["target"])["decisionContext"])["references"])) != 2 || len(fleet.Elements(got["explicitDependencies"])) != 1 {
		t.Fatalf("references/dependency missing: %v", got)
	}
	fixture := Record{"id": "fixture", "project": "owner/repo", "body": "共有目的 file.go", "changeScope": Record{"paths": []any{"file.go"}}}
	material, err := s.materialWithDrafts(ctx, fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidateRows := candidates(material)
	if candidateRows["job:11"] != nil || candidateRows["request:11"] != nil {
		t.Fatalf("closed issue remains a candidate: %v", candidateRows)
	}
	if candidateRows["job:13"] == nil {
		t.Fatal("job with unconfirmed issue was excluded")
	}
	if candidateRows["request:12"] == nil || fleet.Text(candidateRows["job:12"]["body"]) != "共有目的 file.go" {
		t.Fatalf("open issue body missing from job alias: %v", candidateRows)
	}
	for _, row := range fleet.Elements(material["jobs"]) {
		if fleet.WholeNumber(obj(row)["issue"]) == 12 {
			t.Fatal("open job duplicated as standalone candidate")
		}
	}
	expanded, err := s.expand(ctx, fixture, material, []any{Record{"candidateId": "job:12", "kind": "body"}, Record{"candidateId": "job:11", "kind": "body"}})
	if err != nil || !hasFullDuplicateBody(expanded, "job:12") || len(fleet.Elements(expanded["evidenceOutsideCandidates"])) != 1 {
		t.Fatalf("body expansion: %v, %v", expanded, err)
	}
	if fleet.Text(obj(fleet.Elements(expanded["evidence"])[0])["body"]) != "共有目的 file.go" {
		t.Fatalf("expanded job alias did not use issue body: %v", expanded["evidence"])
	}
	decision := duplicateVerdict(Record{"proceed": false, "duplicate": Record{"candidateId": "job:12", "targetPurposeQuote": "共有目的", "candidatePurposeQuote": "共有目的", "commonTargets": []any{Record{"kind": "path", "value": "file.go"}}}}, expanded)
	if decision["proceed"] != false || decision["duplicate"] == nil {
		t.Fatalf("open issue duplicate not retained: %v", decision)
	}
	// A bound sibling dependency reaches the same explicit status lookup even without a pre-existing declaration.
	bound := clone(target)
	bound["dependsOnIssues"] = []any{}
	bound["changeScope"] = nil
	boundMaterial, err := s.material(ctx, bound)
	if err != nil {
		t.Fatal(err)
	}
	boundDeps := fleet.Elements(obj(boundMaterial["target"])["dependsOnIssues"])
	boundStates := fleet.Elements(boundMaterial["explicitDependencies"])
	if len(boundDeps) != 1 || fleet.WholeNumber(obj(boundDeps[0])["number"]) != 8 || len(boundStates) != 1 || fleet.WholeNumber(obj(boundStates[0])["number"]) != 8 || obj(boundStates[0])["satisfied"] != false {
		t.Fatalf("bound sibling: %v", boundMaterial)
	}
	if _, err := jobDB.ExecContext(ctx, "UPDATE jobs SET status='merged' WHERE issue=8"); err != nil {
		t.Fatal(err)
	}
	for _, pr := range []bool{false, true} {
		dependencyPR = pr
		for _, state := range []string{"", "unknown", "open", "closed"} {
			dependencyState = state
			m, err := s.material(ctx, bound)
			if err != nil {
				t.Fatal(err)
			}
			got := obj(fleet.Elements(m["explicitDependencies"])[0])["satisfied"]
			if got != (state == "open" || state == "closed") {
				t.Fatalf("pr=%v state=%q satisfied=%v", pr, state, got)
			}
		}
	}
	dependencyState, dependencyPR = "open", false
	// Removing the dependency makes the earlier sibling's missing path a conflict.
	noDep := clone(target)
	noDep["dependsOnIssues"] = []any{}
	noDep["dependencies"] = []any{}
	_, oldErr := s.legacyMaterial(ctx, noDep)
	_, newErr := s.material(ctx, noDep)
	if oldErr == nil || newErr == nil || oldErr.Error() != newErr.Error() || !strings.Contains(newErr.Error(), "sibling") {
		t.Fatalf("overlap differs: %v / %v", oldErr, newErr)
	}
	// A split proposal dependency also exempts the sibling from the conflict.
	parentRow := clone(rows[1])
	parentRow["splitIssues"] = []any{Record{}, Record{"dependsOn": []any{1}}, Record{}}
	if _, err := records.Change(ctx, "drafts", "parent", func(*sql.Tx, consultations.Record) (consultations.Record, error) { return parentRow, nil }); err != nil {
		t.Fatal(err)
	}
	old, oldErr = s.legacyMaterial(ctx, noDep)
	got, newErr = s.material(ctx, noDep)
	if oldErr != nil || newErr != nil || !reflect.DeepEqual(old, got) {
		t.Fatalf("split dependency differs: %v / %v\nold %s\nnew %s", oldErr, newErr, raw(old), raw(got))
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES ($1,$2)", "legacy-surrogate", `{"id":"legacy-surrogate","project":"unrelated/repo","status":"draft","title":"\ud800"}`); err != nil {
		t.Fatal(err)
	}
	old, oldErr = s.legacyMaterial(ctx, target)
	got, newErr = s.material(ctx, target)
	if oldErr != nil || newErr != nil || !reflect.DeepEqual(old, got) {
		t.Fatalf("surrogate fallback differs: %v / %v\nold %s\nnew %s", oldErr, newErr, raw(old), raw(got))
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES ($1,$2)", "invalid-projection", "{"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.material(ctx, target); err == nil {
		t.Fatal("material swallowed projection failure")
	}
	// A fresh client avoids the issue-history cache when simulating a read failure.
	failIssues = true
	failedClient, err := gitea.New(server.URL, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.options.Gitea = failedClient
	if _, err := s.materialWithDrafts(ctx, fixture, nil); err == nil {
		t.Fatal("issue-list failure was ignored")
	}
	// Run the actual material entry point and legacyMaterial over the same
	// boundary fixtures as the driver-level volume test, including siblings.
	failIssues = false
	s.options.Gitea = client
	for _, tc := range ancestorBoundaries() {
		t.Run("ancestor-"+tc.name, func(t *testing.T) {
			target, _ := seedAncestorBoundary(t, records, tc)
			compare := func(wantError bool) {
				t.Helper()
				old, oldErr := s.legacyMaterial(ctx, target)
				got, newErr := s.material(ctx, target)
				if (oldErr != nil) != wantError || (newErr != nil) != wantError || !reflect.DeepEqual(old, got) {
					t.Fatalf("boundary differs: %v / %v\nold %s\nnew %s", oldErr, newErr, raw(old), raw(got))
				}
				if wantError && oldErr.Error() != newErr.Error() {
					t.Fatalf("errors differ: %v / %v", oldErr, newErr)
				}
			}
			compare(false)
			// Genuine PostgreSQL projection failure takes the compatibility path.
			if _, err := db.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES('boundary-exception',$1)", `{"title":"\ud800"}`); err != nil {
				t.Fatal(err)
			}
			compare(false)
			if _, err := db.ExecContext(ctx, "UPDATE drafts SET json='{' WHERE id='boundary-exception'"); err != nil {
				t.Fatal(err)
			}
			compare(true)
		})
	}
}
