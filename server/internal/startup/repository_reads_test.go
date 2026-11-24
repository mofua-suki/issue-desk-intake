package startup

import (
	"context"
	"io"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/operations"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStartupRepositoryReadWarning(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	api, err := gitea.New("http://gitea.invalid", token, deployPollTransport(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		status, body := 200, `[]`
		if strings.Contains(r.URL.Path, "/owner/gone/") {
			status, body = 404, `{"message":"not found"}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	logs := []string{}
	a := &App{Gitea: api, Config: &Config{Repositories: []string{"owner/repo", "owner/gone"}}, RepositoryReads: &operations.RepositoryReads{}, Log: func(s string) { logs = append(logs, s) }}
	if err := a.checkRepositoryReads(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || !strings.Contains(paths[0], "/owner/repo/issues") || !strings.Contains(paths[0], "limit=1") {
		t.Fatalf("reads: %v", paths)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "owner/gone") || !strings.Contains(logs[0], "HTTP 404") {
		t.Fatalf("warning: %v", logs)
	}
	alerts, err := a.systemAlerts(context.Background())
	if err != nil || len(alerts) != 1 {
		t.Fatalf("attention: %v %v", alerts, err)
	}
	if row, _ := alerts[0].(map[string]any); row["repo"] != "owner/gone" || row["kind"] != "intake_repository_unreadable" {
		t.Fatalf("attention row: %v", row)
	}
}

// The check is periodic and reads the current candidates each time: a Gitea
// that starts late clears the attention items on the next check, and a
// candidate removed from the configuration loses its record.
func TestRepositoryReadsRecheckCurrentCandidates(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	down := true
	api, err := gitea.New("http://gitea.invalid", token, deployPollTransport(func(r *http.Request) (*http.Response, error) {
		status, body := 200, `[]`
		if down || strings.Contains(r.URL.Path, "/owner/gone/") {
			status, body = 404, `{"message":"not found"}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Gitea: api, Config: &Config{Repositories: []string{"owner/repo", "owner/gone"}}, RepositoryReads: &operations.RepositoryReads{}, Log: func(string) {}}
	repos := func() []string {
		t.Helper()
		if err := a.checkRepositoryReads(context.Background()); err != nil {
			t.Fatal(err)
		}
		alerts, err := a.systemAlerts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, row := range alerts {
			out = append(out, row.(map[string]any)["repo"].(string))
		}
		return out
	}
	if got := repos(); len(got) != 2 {
		t.Fatalf("Gitea down: %v", got)
	}
	down = false
	if got := repos(); len(got) != 1 || got[0] != "owner/gone" {
		t.Fatalf("Gitea up: %v", got)
	}
	a.mu.Lock()
	a.Config.Repositories = []string{"owner/repo"}
	a.mu.Unlock()
	if got := repos(); len(got) != 0 {
		t.Fatalf("candidate removed: %v", got)
	}
}

func TestRepositoryReadsArePeriodic(t *testing.T) {
	app := openLifecycleApp(t, lifecycleGitea, nil)
	app.backgroundMu.Lock()
	names := append([]string(nil), app.periodicNames...)
	app.backgroundMu.Unlock()
	if !slices.Contains(names, "repository-reads") {
		t.Fatalf("repository reads not periodic: %v", names)
	}
}
