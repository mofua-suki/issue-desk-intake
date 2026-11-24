package startup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/processactivity"
)

func TestPeriodicActivity(t *testing.T) {
	c := &processactivity.Collector{}
	old := processactivity.Process
	processactivity.Process = c
	defer func() { processactivity.Process = old }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	app := &App{backgroundCtx: ctx, Log: func(string) { close(done) }}
	app.periodically("test-task", time.Hour, func(context.Context) error { return errors.New("failed") })
	<-done
	cancel()
	app.StopBackground()
	app.StopBackground()
	rows := c.Report()[0].Rows
	if len(rows) != 1 || rows[0].Name != "test-task" || rows[0].Failures != 1 {
		t.Fatalf("rows: %+v", rows)
	}
}

func TestPeriodicCancellationIsNotFailure(t *testing.T) {
	c := &processactivity.Collector{}
	old := processactivity.Process
	processactivity.Process = c
	defer func() { processactivity.Process = old }()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	app := &App{backgroundCtx: ctx, Log: func(s string) { t.Errorf("unexpected log: %s", s) }}
	app.periodically("cancelled-task", time.Hour, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	<-started
	cancel()
	app.StopBackground()
	app.StopBackground()
	rows := c.Report()[0].Rows
	if len(rows) != 1 || rows[0].Name != "cancelled-task" || rows[0].Count != 1 || rows[0].Failures != 0 {
		t.Fatalf("rows: %+v", rows)
	}
}

func TestRegisteredPeriodicNames(t *testing.T) {
	app := openLifecycleApp(t, lifecycleGitea, nil)
	app.backgroundMu.Lock()
	names := append([]string(nil), app.periodicNames...)
	app.backgroundMu.Unlock()
	if len(names) == 0 {
		t.Fatal("no periodic tasks registered")
	}
	seen := make(map[string]bool)
	for _, name := range names {
		if strings.TrimSpace(name) == "" || seen[name] {
			t.Fatalf("empty or duplicate periodic name: %q (all: %v)", name, names)
		}
		seen[name] = true
	}
}
func TestPeriodicFailureLogIncludesTaskName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	logs := make(chan string, 1)
	app := &App{backgroundCtx: ctx, Log: func(s string) { logs <- s }}
	app.periodically("failing-task", time.Hour, func(context.Context) error { return errors.New("原因です\nBearer secretsecret") })
	msg := <-logs
	cancel()
	app.StopBackground()
	app.StopBackground()
	if !strings.Contains(msg, "周期処理（failing-task）を完了できません。") || !strings.Contains(msg, "原因です") || strings.Contains(msg, "secretsecret") || strings.Contains(msg, "\n") {
		t.Fatal(msg)
	}
}
func TestHTTPActivity(t *testing.T) {
	c := &processactivity.Collector{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); _, _ = w.Write([]byte("ok")) })
	h := c.Wrap(next)
	for _, p := range []string{"/api/issues/search", "/api/issues/123?ignored=yes"} {
		r := httptest.NewRequest("GET", p, nil)
		r.Header.Set("X-Desk-Key", "key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 201 || w.Body.String() != "ok" {
			t.Fatalf("response: %d %s", w.Code, w.Body.String())
		}
	}
	rows := c.Report()[0].Rows
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Count != 1 || r.Credential != "cli" {
			t.Fatalf("row: %+v", r)
		}
		seen[r.Name] = true
	}
	if !seen["/api/issues/search"] || !seen["/api/issues/{n}"] {
		t.Fatal(seen)
	}
}
