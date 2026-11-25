package filingcheck

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/gitea"
)

func TestCheckStopsForAllUnavailableNamedMaterials(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New("https://gitea.invalid", token, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	s := &Service{options: Options{
		Gitea:  client,
		Config: func(context.Context) (map[string]any, error) { return map[string]any{"repositories": []any{}}, nil },
		Material: func(context.Context, Record) (Record, error) {
			return Record{"target": Record{}, "requests": []any{Record{"number": 1}}}, nil
		},
		Answer: func(context.Context, map[string]any) (map[string]any, error) { called = true; return nil, nil },
	}}
	d := Record{"id": "draft", "project": "owner/app", "body": "本文", "messages": []any{Record{"role": "user", "text": "owner/one:docs/a.md:1 owner/two:docs/b.md:2"}}}
	err = s.Check(context.Background(), d)
	if err == nil || called {
		t.Fatal(err, called)
	}
	for _, want := range []string{"owner/one:docs/a.md:1", "owner/two:docs/b.md:2", "許可"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatal(err)
		}
	}
}
