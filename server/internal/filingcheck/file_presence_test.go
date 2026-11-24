package filingcheck

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/gitea"
)

func TestFilePresenceChecksExactPathsOnDefaultBranch(t *testing.T) {
	const existing = "server/internal/consultationai/context_documents.go"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo":
			json.NewEncoder(w).Encode(Record{"default_branch": "main"})
		case "/api/v1/repos/owner/repo/branches/main":
			json.NewEncoder(w).Encode(Record{"commit": Record{"id": strings.Repeat("a", 40)}})
		case "/api/v1/repos/owner/repo/git/trees/" + strings.Repeat("a", 40):
			json.NewEncoder(w).Encode(Record{"tree": []any{Record{"path": existing, "type": "blob", "mode": "100644"}, Record{"path": "private/hidden.go", "type": "blob", "mode": "100644"}}, "truncated": false})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New(server.URL, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{options: Options{Gitea: client}}
	result, err := s.filePresence(t.Context(), Record{"project": "owner/repo", "body": existing + " consultationai/context_documents.go private/hidden.go"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Record{}
	for _, item := range fleet.Elements(result["files"]) {
		row := obj(item)
		got[fleet.Text(row["path"])] = row
	}
	for _, tc := range []struct {
		path             string
		exists, readable bool
	}{{existing, true, true}, {"consultationai/context_documents.go", false, false}, {"private/hidden.go", true, false}} {
		row := got[tc.path]
		if row == nil || row["exists"] != tc.exists || row["readable"] != tc.readable {
			t.Errorf("%s: %+v", tc.path, row)
		}
	}
}
