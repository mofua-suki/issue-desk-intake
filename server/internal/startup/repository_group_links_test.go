package startup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
)

func TestRepositoryGroupIssueLinksAdapter(t *testing.T) {
	status := http.StatusOK
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/owner/repo/issues/7" {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"body":"old","content_version":4}`))
		case http.MethodPatch:
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["body"] != "new" || payload["content_version"] != float64(4) {
				t.Errorf("unexpected payload %v: %v", payload, err)
			}
			w.WriteHeader(status)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer fixture.Close()
	token := filepath.Join(t.TempDir(), "fixture-token")
	if err := os.WriteFile(token, []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New(fixture.URL, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter := repositoryGroupLinksAdapter{client: client}
	issue, err := adapter.Get(context.Background(), "owner/repo", 7)
	if err != nil || issue["body"] != "old" {
		t.Fatalf("get: %v, %v", issue, err)
	}
	if err := adapter.Update(context.Background(), "owner/repo", 7, "new", issue["content_version"]); err != nil {
		t.Fatal(err)
	}
	status = http.StatusConflict
	err = adapter.Update(context.Background(), "owner/repo", 7, "new", issue["content_version"])
	var giteaErr *gitea.Error
	if !errors.As(err, &giteaErr) || giteaErr.Status != 409 {
		t.Fatalf("conflict: %v", err)
	}
	fixture.Close()
	if err = adapter.Update(context.Background(), "owner/repo", 7, "new", issue["content_version"]); err == nil {
		t.Fatal("transport failure accepted")
	}
}

func TestRepositoryGroupIssueLinksShippedConfiguration(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	app := &App{ctx: context.Background(), Config: &Config{Values: map[string]any{}, Repositories: []string{"mofua-suki/sample-expense-server", "mofua-suki/sample-expense-client"}}, Fleet: &fleet.Service{FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json")}}
	groups, err := app.filingRepositoryGroups(context.Background())
	if err != nil || len(groups) != 1 || len(groups[0]) != 2 || groups[0][0] != "mofua-suki/sample-expense-server" || groups[0][1] != "mofua-suki/sample-expense-client" {
		t.Fatalf("shipped groups: %v %v", groups, err)
	}
}
