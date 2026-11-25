package consultationai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
)

func TestFilingMaterialsNamedRangesAndFailures(t *testing.T) {
	const source = "first\nsecond\nthird\n"
	revision := strings.Repeat("a", 40)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New("https://gitea.invalid", token, transportFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		switch {
		case strings.HasSuffix(r.URL.Path, "/branches/main"):
			_ = json.NewEncoder(w).Encode(Record{"commit": Record{"id": revision}})
		case strings.Contains(r.URL.Path, "/git/trees/"):
			_ = json.NewEncoder(w).Encode(Record{"tree": []any{Record{"path": "docs/a.md", "type": "blob", "mode": "100644", "sha": fixtureSHA(source), "size": len(source)}}, "truncated": false})
		case strings.Contains(r.URL.Path, "/git/blobs/"):
			_ = json.NewEncoder(w).Encode(Record{"encoding": "base64", "content": encoded(source)})
		default:
			_ = json.NewEncoder(w).Encode(Record{"default_branch": "main"})
		}
		return w.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"repositories": []any{Record{"name": "owner/one"}, Record{"name": "owner/two"}}}
	draft := Record{"project": "owner/app", "body": "確定本文", "messages": []any{Record{"role": "user", "text": "owner/one:docs/a.md:1-2 と owner/two:docs/a.md:3"}}}
	docs, problem, err := FilingMaterials(context.Background(), client, config, draft)
	if err != nil || problem != "" || len(docs) != 2 {
		t.Fatalf("docs=%v problem=%s err=%v", docs, problem, err)
	}
	if fleet.Text(fleet.Object(docs[0])["text"]) != "1: first\n2: second" || fleet.Text(fleet.Object(docs[1])["text"]) != "3: third" {
		t.Fatal(docs)
	}
	draft["messages"] = []any{Record{"role": "user", "text": "owner/one:docs/missing.md:1 owner/two:docs/a.md:9 owner/no:docs/a.md:1"}}
	docs, problem, err = FilingMaterials(context.Background(), client, config, draft)
	if err != nil || len(docs) != 0 {
		t.Fatal(docs, problem, err)
	}
	for _, want := range []string{"owner/one:docs/missing.md:1", "ありません", "owner/two:docs/a.md:9", "範囲外", "owner/no:docs/a.md:1", "許可"} {
		if !strings.Contains(problem, want) {
			t.Fatalf("missing %q: %s", want, problem)
		}
	}
}
