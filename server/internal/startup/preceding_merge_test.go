package startup

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/operations"
)

func TestClosedPredecessorChecksDirectMergeEvidenceAndCurrentBase(t *testing.T) {
	a := reviewNotesApp(t)
	ctx := t.Context()
	var err error
	a.Jobs, err = jobs.New(a.DB, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Jobs.Create(ctx, jobs.NewJob{Repo: "owner/app", Issue: 1, Title: "predecessor"}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.SQLite.DB().ExecContext(ctx, "CREATE TABLE IF NOT EXISTS close_reasons(id TEXT PRIMARY KEY,json TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.SQLite.DB().ExecContext(ctx, `INSERT INTO close_reasons(id,json) VALUES('owner/app#1','{"reason":"other_route","route":{"repo":"owner/app","pr":12}}')`); err != nil {
		t.Fatal(err)
	}
	base := "trunk"
	status := 200
	a.Gitea = reviewAPI(t, func(r *http.Request) (*http.Response, error) {
		body := `{"default_branch":"trunk"}`
		if strings.HasSuffix(r.URL.Path, "/pulls/10") {
			body = `{"merged":false}`
		}
		if strings.HasSuffix(r.URL.Path, "/pulls/12") {
			body = fmt.Sprintf(`{"merged":true,"merge_commit_sha":"%s","base":{"ref":%q,"repo":{"full_name":"owner/app"}}}`, strings.Repeat("a", 40), base)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
	})
	a.Operations = func() (*operations.Service, error) {
		return &operations.Service{DB: a.SQLite.DB(), PostgreSQL: a.DB}, nil
	}
	refused := func() {
		t.Helper()
		var problem *contract.Problem
		if err := a.precedingMerged(ctx, "owner/app", 1); !errors.As(err, &problem) || problem.Status != 409 {
			t.Fatalf("wanted 409, got %v", err)
		}
	}
	refused()
	if _, err = a.DB.ExecContext(ctx, `INSERT INTO requests(id,kind,repo,payload,sender,status,result) VALUES('predecessor-test','merge_manual','owner/app','{"number":12}','pc','done','{"status":"merged"}')`); err != nil {
		t.Fatal(err)
	}
	if err = a.precedingMerged(ctx, "owner/app", 1); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.ExecContext(ctx, "UPDATE jobs SET pr=10 WHERE repo=$1 AND issue=1", "owner/app"); err != nil {
		t.Fatal(err)
	}
	if err = a.precedingMerged(ctx, "owner/app", 1); err != nil {
		t.Fatalf("unmerged former worker PR hid the direct merge: %v", err)
	}
	base = "wrong-base"
	refused()
	base = "trunk"
	status = 503
	if err = a.precedingMerged(ctx, "owner/app", 1); !errors.Is(err, filing.ErrFilingReadUnavailable) {
		t.Fatalf("temporary lookup should be retried: %v", err)
	}
}
