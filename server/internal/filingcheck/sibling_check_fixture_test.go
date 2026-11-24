package filingcheck

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

type siblingCheckFixture struct {
	s                         *Service
	d, parent, sibling, other Record
	jobDB                     *sql.DB
	issues                    map[int64]Record
	listing                   []any
	sent                      []Record
	answer                    func(Record) map[string]any
	failIssue                 bool
	pr                        Record
	statusReads               int
}

func newSiblingCheckFixture(t *testing.T) *siblingCheckFixture {
	t.Helper()
	ctx := t.Context()
	env := pgtest.NewIsolated(t)
	jobSchema := env.Schema + "_jobs"
	if _, err := env.DB.ExecContext(ctx, "CREATE SCHEMA "+jobSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := env.DB.ExecContext(context.Background(), "DROP SCHEMA "+jobSchema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", jobSchema)
	u.RawQuery = q.Encode()
	jobDB, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobDB.Close() })
	if _, err := store.Migrate(ctx, jobDB); err != nil {
		t.Fatal(err)
	}
	records, err := consultations.OpenRecords(ctx, env.DB)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobDB, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int64{483, 484} {
		if _, err := queue.Create(ctx, jobs.NewJob{Repo: "owner/repo", Issue: n, Title: "先行", Scope: []string{"src/contract.go"}}); err != nil {
			t.Fatal(err)
		}
	}
	f := &siblingCheckFixture{jobDB: jobDB, issues: map[int64]Record{}}
	for _, n := range []int64{483, 484, 900} {
		row := Record{"number": n, "title": fmt.Sprint("依頼", n), "state": "open", "body": "別の目的", "user": Record{}}
		f.issues[n] = row
		f.listing = append(f.listing, row)
	}
	parent := Record{"id": "parent", "project": "owner/repo", "createdAt": "1", "body": "分割元", "splitChildren": []any{"one", "other", "target"}, "splitIssues": []any{
		Record{"title": "契約", "purpose": "契約を定義", "acceptance": []any{"契約が利用可能"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"api/types.ts"}}},
		Record{"title": "独立", "purpose": "独立した処理", "acceptance": []any{"処理が動く"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"src/other.go"}}},
		Record{"title": "後続", "purpose": "先行契約を使う", "acceptance": []any{"利用できる"}, "dependsOn": []any{1}, "changeScope": Record{"known": true, "paths": []any{"src/target.go"}}},
	}}
	plan, err := filing.PlanSplit(parent)
	if err != nil {
		t.Fatal(err)
	}
	f.parent = parent
	f.sibling = plan[0]
	f.sibling["id"], f.sibling["status"], f.sibling["issue"] = "one", "submitted", Record{"project": "owner/repo", "number": 483}
	f.other = plan[1]
	f.other["id"], f.other["status"], f.other["issues"] = "other", "submitted", []any{Record{"project": "owner/repo", "number": 484}}
	f.d = plan[2]
	f.d["id"], f.d["status"], f.d["revision"], f.d["createdAt"] = "target", "submitting", 1, "2"
	f.save(t, f.parent, f.sibling, f.other, f.d, records)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			json.NewEncoder(w).Encode(f.listing)
		case "/api/v1/repos/owner/repo":
			json.NewEncoder(w).Encode(Record{"default_branch": "main"})
		case "/api/v1/repos/owner/repo/branches/main":
			json.NewEncoder(w).Encode(Record{"commit": Record{"id": strings.Repeat("a", 40)}})
		case "/api/v1/repos/owner/repo/git/trees/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":
			json.NewEncoder(w).Encode(Record{"tree": []any{}, "truncated": false})
		case "/api/v1/repos/owner/repo/pulls/483":
			json.NewEncoder(w).Encode(f.pr)
		default:
			var n int64
			if _, err := fmt.Sscanf(r.URL.Path, "/api/v1/repos/owner/repo/issues/%d", &n); err != nil || f.issues[n] == nil {
				t.Errorf("unexpected route %s", r.URL.Path)
				http.Error(w, "missing fixture", 404)
				return
			}
			f.statusReads++
			if f.failIssue {
				http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(f.issues[n])
		}
	}))
	t.Cleanup(server.Close)
	token := filepath.Join(t.TempDir(), "fixture-token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New(server.URL, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := filepath.Abs("../../schemas")
	if err != nil {
		t.Fatal(err)
	}
	f.s, err = New(Options{Records: records, Jobs: queue, Gitea: client, AppDB: env.DB,
		Project: func(string) error { return nil }, Own: func(map[string]any) bool { return true },
		Config: func(context.Context) (map[string]any, error) { return map[string]any{}, nil },
		DeskNumber: func(_ string, n int64) any {
			if n == 483 {
				return 5435
			}
			return nil
		},
		SchemaDir: schema, Revision: strings.Repeat("a", 40),
		Answer: func(_ context.Context, call map[string]any) (map[string]any, error) {
			input := fleet.Text(call["input"])
			if len(input)+len(fleet.Text(call["schema"])) > 160000 {
				t.Fatalf("sent input exceeds budget: %d", len(input)+len(fleet.Text(call["schema"])))
			}
			parts := strings.SplitN(input, materialSeparator, 2)
			if len(parts) != 2 {
				t.Fatal("material missing")
			}
			var m Record
			if err := json.Unmarshal([]byte(parts[1]), &m); err != nil {
				t.Fatal(err)
			}
			f.sent = append(f.sent, m)
			if f.answer != nil {
				return f.answer(m), nil
			}
			return allowed(), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *siblingCheckFixture) save(t *testing.T, parent, sibling, other, d Record, records *consultations.Records) {
	t.Helper()
	for _, row := range []Record{parent, sibling, other, d} {
		if _, err := records.Change(t.Context(), "drafts", fleet.Text(row["id"]), func(*sql.Tx, Record) (Record, error) { return clone(row), nil }); err != nil {
			t.Fatal(err)
		}
	}
}
func (f *siblingCheckFixture) persist(t *testing.T) {
	t.Helper()
	f.save(t, f.parent, f.sibling, f.other, f.d, f.s.options.Records)
}
func (f *siblingCheckFixture) saved(t *testing.T) Record {
	t.Helper()
	d, ok, err := f.s.options.Records.Get(t.Context(), "drafts", fleet.Text(f.d["id"]))
	if err != nil || !ok {
		t.Fatalf("saved draft: %v %v", ok, err)
	}
	return d
}
func assertSiblingCheckAllowed(t *testing.T, f *siblingCheckFixture, numbers ...int64) {
	t.Helper()
	if err := f.s.Check(t.Context(), f.d); err != nil {
		t.Fatal(err)
	}
	saved := f.saved(t)
	deps := fleet.Elements(saved["dependsOnIssues"])
	if len(deps) != len(numbers) || len(f.sent) != 1 || obj(saved["filingCheckCalls"])["outcome"] != "allowed" {
		t.Fatalf("saved=%v calls=%d", saved, len(f.sent))
	}
	for _, n := range numbers {
		found := false
		for _, dep := range deps {
			found = found || fleet.WholeNumber(obj(dep)["number"]) == n && obj(dep)["source"] == "gitea"
		}
		if !found {
			t.Fatalf("missing dependency %d: %v", n, deps)
		}
	}
	checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
	if len(checks) != 1 || obj(checks[0])["result"] != "allowed" {
		t.Fatal(checks)
	}
}
