package startup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
)

type providerFixture struct {
	app             *App
	calls           [2]atomic.Int64
	active, maximum atomic.Int64
	ready           atomic.Bool
	started         chan string
	hook            func(context.Context, ai.ProcessRequest, int, int64) error
}

func newProviderFixture(t *testing.T) *providerFixture {
	t.Helper()
	dir := t.TempDir()
	executables := [2]string{}
	for i, name := range providerNames {
		executables[i] = filepath.Join(dir, name)
		if err := os.WriteFile(executables[i], []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &providerFixture{started: make(chan string, 100)}
	f.ready.Store(true)
	f.app = &App{Config: &Config{RuntimeDir: dir, Codex: executables[0], Claude: executables[1], Values: map[string]any{"fixture": true}}, ctx: ctx, cancel: cancel}
	f.app.Runner = &ai.Runner{Codex: executables[0], Claude: executables[1], Execute: func(ctx context.Context, req ai.ProcessRequest) (ai.ProcessResult, error) {
		index := 0
		if req.Arguments[0] == "auth" {
			index = 1
		}
		count := f.calls[index].Add(1)
		active := f.active.Add(1)
		defer f.active.Add(-1)
		for old := f.maximum.Load(); active > old; old = f.maximum.Load() {
			if f.maximum.CompareAndSwap(old, active) {
				break
			}
		}
		f.started <- providerNames[index]
		if f.hook != nil {
			if err := f.hook(ctx, req, index, count); err != nil {
				return ai.ProcessResult{}, err
			}
		}
		if !f.ready.Load() {
			return ai.ProcessResult{Code: 1}, nil
		}
		return authenticatedProviderResult(index), nil
	}}
	t.Cleanup(f.app.Close)
	return f
}

func authenticatedProviderResult(index int) ai.ProcessResult {
	if index == 0 {
		return ai.ProcessResult{Stderr: []byte("Logged in using ChatGPT")}
	}
	return ai.ProcessResult{Stdout: []byte(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"oauth_token"}`)}
}

type providerResult struct {
	rows []map[string]any
	err  error
}

func requestProviders(app *App, ctx context.Context) <-chan providerResult {
	done := make(chan providerResult, 1)
	go func() { rows, err := app.Providers(ctx); done <- providerResult{rows, err} }()
	return done
}
func receiveProviderResult(t *testing.T, done <-chan providerResult) providerResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("provider request did not finish")
		return providerResult{}
	}
}
func assertProviderRows(t *testing.T, result providerResult, ready bool) {
	t.Helper()
	if result.err != nil {
		t.Fatal(result.err)
	}
	if len(result.rows) != 2 {
		t.Fatalf("rows: %v", result.rows)
	}
	reason := "authentication"
	if ready {
		reason = "ready"
	}
	for i, row := range result.rows {
		if row["provider"] != providerNames[i] || row["ready"] != ready || row["reason"] != reason {
			t.Fatalf("row %d: %v", i, row)
		}
	}
}
func waitProviderStarts(t *testing.T, f *providerFixture, count int) {
	t.Helper()
	for range count {
		select {
		case <-f.started:
		case <-time.After(10 * time.Second):
			t.Fatal("independent providers did not start")
		}
	}
}
func waitProviderCondition(t *testing.T, f *providerFixture, condition func(*providerCache) bool) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		f.app.providers.mu.Lock()
		ok := condition(&f.app.providers)
		f.app.providers.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("provider synchronization did not complete")
		default:
			runtime.Gosched()
		}
	}
}
func assertProviderCalls(t *testing.T, f *providerFixture, codex, claude int64) {
	t.Helper()
	if a, b := f.calls[0].Load(), f.calls[1].Load(); a != codex || b != claude {
		t.Fatalf("launches: codex=%d claude=%d, want %d/%d", a, b, codex, claude)
	}
}
func setProviderClock(f *providerFixture) *atomic.Int64 {
	clock := &atomic.Int64{}
	clock.Store(time.Unix(100, 0).UnixNano())
	f.app.providers.now = func() time.Time { return time.Unix(0, clock.Load()) }
	return clock
}

func TestProvidersParallelSharedComparison(t *testing.T) {
	f := newProviderFixture(t)
	const delay = 40 * time.Millisecond
	var gate atomic.Pointer[chan struct{}]
	f.hook = func(ctx context.Context, _ ai.ProcessRequest, _ int, _ int64) error {
		if p := gate.Load(); p != nil {
			select {
			case <-*p:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Reproduce helpers.go at adaaa788593badca5bdea79fac5344d2fe1195c8:
	// the two Runner.Status calls were sequential, with CurrentConfig per call.
	before := time.Now()
	sequential := []map[string]any{}
	for _, p := range []string{"codex", "claude"} {
		state := f.app.Runner.Status(context.Background(), p, f.app.CurrentConfig().RuntimeDir)
		sequential = append(sequential, map[string]any{"provider": p, "ready": state.Ready, "reason": state.Reason})
	}
	baseline := time.Since(before)
	assertProviderRows(t, providerResult{rows: sequential}, true)
	waitProviderStarts(t, f, 2)
	release := make(chan struct{})
	gate.Store(&release)
	before = time.Now()
	cold := requestProviders(f.app, context.Background())
	waitProviderStarts(t, f, 2) // Both Execute calls entered before either was released.
	close(release)
	assertProviderRows(t, receiveProviderResult(t, cold), true)
	coldElapsed := time.Since(before)
	assertProviderCalls(t, f, 2, 2)
	before = time.Now()
	rows, err := f.app.Providers(context.Background())
	warmElapsed := time.Since(before)
	assertProviderRows(t, providerResult{rows, err}, true)
	assertProviderCalls(t, f, 2, 2)
	// Force expiration without depending on wall-clock TTL sleeps.
	f.app.providers.mu.Lock()
	for _, entry := range f.app.providers.current {
		entry.expires = time.Time{}
	}
	f.app.providers.mu.Unlock()
	release = make(chan struct{})
	gate.Store(&release)
	const clients = 24
	requests := make([]<-chan providerResult, clients)
	before = time.Now()
	for i := range requests {
		requests[i] = requestProviders(f.app, context.Background())
	}
	waitProviderStarts(t, f, 2)
	waitProviderCondition(t, f, func(c *providerCache) bool { return c.current[0].waiters == clients && c.current[1].waiters == clients })
	assertProviderCalls(t, f, 3, 3)
	close(release)
	for _, request := range requests {
		assertProviderRows(t, receiveProviderResult(t, request), true)
	}
	concurrentElapsed := time.Since(before)
	if f.maximum.Load() != 2 {
		t.Fatalf("screen maximum concurrency: %d", f.maximum.Load())
	}
	t.Logf("Execute delay=%s; sequential=%s launches codex=1 claude=1; cold=%s launches codex=1 claude=1; warm=%s launches codex=0 claude=0; concurrent(%d)=%s launches codex=1 claude=1", delay, baseline, coldElapsed, warmElapsed, clients, concurrentElapsed)
	// Every caller owns its array and maps; model identity must never leak into meta.
	rows[0]["provider"] = "mutated"
	again, err := f.app.Providers(context.Background())
	assertProviderRows(t, providerResult{again, err}, true)
	if _, ok := again[0]["id"]; ok {
		t.Fatal("model id leaked into screen rows")
	}
}

func TestProvidersTTLPerCompletionAndAuthenticationRecovery(t *testing.T) {
	f := newProviderFixture(t)
	clock := setProviderClock(f)
	releases := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	f.ready.Store(false)
	f.hook = func(ctx context.Context, _ ai.ProcessRequest, index int, count int64) error {
		if count != 1 {
			return nil
		}
		select {
		case <-releases[index]:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	request := requestProviders(f.app, context.Background())
	waitProviderStarts(t, f, 2)
	close(releases[0])
	waitProviderCondition(t, f, func(c *providerCache) bool { return c.current[0].finished })
	clock.Add(int64(time.Second))
	close(releases[1])
	assertProviderRows(t, receiveProviderResult(t, request), false)
	f.ready.Store(true)
	clock.Store(time.Unix(103, 0).Add(-time.Nanosecond).UnixNano())
	rows, err := f.app.Providers(context.Background())
	assertProviderRows(t, providerResult{rows, err}, false)
	assertProviderCalls(t, f, 1, 1)
	clock.Add(1) // Exactly codex completion + 3 seconds; Claude still has one second.
	rows, err = f.app.Providers(context.Background())
	if err != nil || rows[0]["ready"] != true || rows[1]["ready"] != false {
		t.Fatalf("per-provider expiry: %v %v", rows, err)
	}
	assertProviderCalls(t, f, 2, 1)
	clock.Store(time.Unix(104, 0).UnixNano())
	rows, err = f.app.Providers(context.Background())
	assertProviderRows(t, providerResult{rows, err}, true)
	assertProviderCalls(t, f, 2, 2)
}

func TestProvidersReuseConditions(t *testing.T) {
	changes := []struct {
		name   string
		change func(*testing.T, *providerFixture)
	}{
		{"config snapshot", func(t *testing.T, f *providerFixture) {
			f.app.mu.Lock()
			copy := *f.app.Config
			copy.Values = map[string]any{"fixture": "changed"}
			f.app.Config = &copy
			f.app.mu.Unlock()
		}},
		{"runtime", func(t *testing.T, f *providerFixture) {
			f.app.mu.Lock()
			copy := *f.app.Config
			copy.RuntimeDir = t.TempDir()
			f.app.Config = &copy
			f.app.mu.Unlock()
		}},
		{"runner", func(t *testing.T, f *providerFixture) {
			f.app.mu.Lock()
			copy := *f.app.Runner
			f.app.Runner = &copy
			f.app.mu.Unlock()
		}},
		{"execute closure", func(t *testing.T, f *providerFixture) {
			f.app.mu.Lock()
			old := f.app.Runner.Execute
			f.app.Runner.Execute = func(ctx context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) { return old(ctx, r) }
			f.app.mu.Unlock()
		}},
		{"specification", func(t *testing.T, f *providerFixture) {
			f.app.mu.Lock()
			f.app.Runner.Codex = filepath.Join(filepath.Dir(f.app.Runner.Codex), ".", "codex")
			f.app.Runner.Codex = filepath.Dir(f.app.Runner.Codex) + "/./codex"
			f.app.mu.Unlock()
		}},
		{"atomic executable replacement", func(t *testing.T, f *providerFixture) {
			path := f.app.Runner.Codex
			info, _ := os.Stat(path)
			replacement := path + ".new"
			if err := os.WriteFile(replacement, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"in-place executable update", func(t *testing.T, f *providerFixture) {
			path := f.app.Runner.Codex
			info, _ := os.Stat(path)
			if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			f := newProviderFixture(t)
			rows, err := f.app.Providers(context.Background())
			assertProviderRows(t, providerResult{rows, err}, true)
			change.change(t, f)
			rows, err = f.app.Providers(context.Background())
			assertProviderRows(t, providerResult{rows, err}, true)
			expectedClaude := int64(2)
			if change.name == "specification" || change.name == "atomic executable replacement" || change.name == "in-place executable update" {
				expectedClaude = 1
			}
			assertProviderCalls(t, f, 2, expectedClaude)
		})
	}
}

func TestProvidersEnvironmentResolutionAndUnknownConditions(t *testing.T) {
	for _, variable := range []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "TMPDIR", "PATH", "LANG"} {
		t.Run(variable, func(t *testing.T) {
			f := newProviderFixture(t)
			rows, err := f.app.Providers(context.Background())
			assertProviderRows(t, providerResult{rows, err}, true)
			t.Setenv(variable, t.TempDir())
			rows, err = f.app.Providers(context.Background())
			assertProviderRows(t, providerResult{rows, err}, true)
			assertProviderCalls(t, f, 2, 2)
		})
	}
	t.Run("PATH resolution", func(t *testing.T) {
		f := newProviderFixture(t)
		t.Setenv("PATH", filepath.Dir(f.app.Runner.Codex))
		f.app.Runner.Codex = "codex"
		rows, err := f.app.Providers(context.Background())
		assertProviderRows(t, providerResult{rows, err}, true)
		next := t.TempDir()
		if err := os.WriteFile(filepath.Join(next, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", next)
		rows, err = f.app.Providers(context.Background())
		assertProviderRows(t, providerResult{rows, err}, true)
		assertProviderCalls(t, f, 2, 2)
	})
	t.Run("symlink target", func(t *testing.T) {
		f := newProviderFixture(t)
		link := filepath.Join(t.TempDir(), "codex")
		if err := os.Symlink(f.app.Runner.Codex, link); err != nil {
			t.Fatal(err)
		}
		f.app.Runner.Codex = link
		rows, err := f.app.Providers(context.Background())
		assertProviderRows(t, providerResult{rows, err}, true)
		target := filepath.Join(t.TempDir(), "codex")
		if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		rows, err = f.app.Providers(context.Background())
		assertProviderRows(t, providerResult{rows, err}, true)
		assertProviderCalls(t, f, 2, 1)
	})
	t.Run("unknown executable never retained", func(t *testing.T) {
		f := newProviderFixture(t)
		f.app.Runner.Codex = filepath.Join(t.TempDir(), "missing")
		for range 2 {
			rows, err := f.app.Providers(context.Background())
			assertProviderRows(t, providerResult{rows, err}, true)
		}
		assertProviderCalls(t, f, 2, 1)
	})
}

func TestProvidersOldGenerationCannotOverwriteNew(t *testing.T) {
	f := newProviderFixture(t)
	release := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	f.hook = func(ctx context.Context, _ ai.ProcessRequest, index int, count int64) error {
		if count == 1 {
			select {
			case <-release[index]:
				return errors.New("old authentication failure")
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	old := requestProviders(f.app, context.Background())
	waitProviderStarts(t, f, 2)
	f.app.mu.Lock()
	copy := *f.app.Config
	copy.Values = map[string]any{"generation": 2}
	f.app.Config = &copy
	f.app.mu.Unlock()
	next := requestProviders(f.app, context.Background())
	waitProviderCondition(t, f, func(c *providerCache) bool { return len(c.live) == 4 })
	close(release[1]) // Old codex stays running while the new generation completes.
	assertProviderRows(t, receiveProviderResult(t, next), true)
	close(release[0])
	assertProviderRows(t, receiveProviderResult(t, old), false)
	rows, err := f.app.Providers(context.Background())
	assertProviderRows(t, providerResult{rows, err}, true)
	assertProviderCalls(t, f, 2, 2)
	if f.maximum.Load() != 2 {
		t.Fatalf("cross-generation concurrency=%d", f.maximum.Load())
	}
}

func TestProvidersCallerCancellationAndDeadline(t *testing.T) {
	f := newProviderFixture(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.app.Providers(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if _, err := f.app.Providers(expired); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	assertProviderCalls(t, f, 0, 0)
	release := make(chan struct{})
	f.hook = func(ctx context.Context, _ ai.ProcessRequest, _ int, _ int64) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	owner, cancelOwner := context.WithCancel(context.Background())
	first := requestProviders(f.app, owner)
	waitProviderStarts(t, f, 2)
	second := requestProviders(f.app, context.Background())
	waitProviderCondition(t, f, func(c *providerCache) bool { return c.current[0].waiters == 2 && c.current[1].waiters == 2 })
	cancelOwner()
	if err := receiveProviderResult(t, first).err; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waitProviderCondition(t, f, func(c *providerCache) bool { return c.current[0].waiters == 1 && c.current[1].waiters == 1 })
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer deadlineCancel()
	deadlineRequest := requestProviders(f.app, deadlineCtx)
	if err := receiveProviderResult(t, deadlineRequest).err; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	close(release)
	assertProviderRows(t, receiveProviderResult(t, second), true)
	assertProviderCalls(t, f, 1, 1)
	if _, err := f.app.Providers(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestProvidersAllWaitersCancelAndQueuedCancellation(t *testing.T) {
	f := newProviderFixture(t)
	stopped := make(chan struct{}, 2)
	cleanup, releaseCleanup := newProviderCleanup(t)
	f.hook = func(ctx context.Context, _ ai.ProcessRequest, _ int, count int64) error {
		if count == 1 {
			<-ctx.Done()
			stopped <- struct{}{}
			<-cleanup
			return ctx.Err()
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := requestProviders(f.app, ctx)
	waitProviderStarts(t, f, 2)
	second := requestProviders(f.app, ctx)
	waitProviderCondition(t, f, func(c *providerCache) bool { return c.current[0].waiters == 2 && c.current[1].waiters == 2 })
	cancel()
	if err := receiveProviderResult(t, first).err; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := receiveProviderResult(t, second).err; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Fatal("abandoned check not cancelled")
		}
	}
	queuedCtx, cancelQueued := context.WithCancel(context.Background())
	queued := requestProviders(f.app, queuedCtx)
	waitProviderCondition(t, f, func(c *providerCache) bool { return len(c.live) == 4 })
	cancelQueued()
	if err := receiveProviderResult(t, queued).err; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	next := requestProviders(f.app, context.Background())
	waitProviderCondition(t, f, func(c *providerCache) bool {
		return c.current[0] != nil && c.current[1] != nil && c.current[0].ctx.Err() == nil && c.current[1].ctx.Err() == nil
	})
	releaseCleanup()
	assertProviderRows(t, receiveProviderResult(t, next), true)
	assertProviderCalls(t, f, 2, 2)
	if f.maximum.Load() != 2 {
		t.Fatal(f.maximum.Load())
	}
}

func TestProvidersAppCloseAndLifecycleCancellation(t *testing.T) {
	for _, method := range []string{"Close", "parent cancellation"} {
		t.Run(method, func(t *testing.T) {
			f := newProviderFixture(t)
			f.hook = func(ctx context.Context, _ ai.ProcessRequest, _ int, _ int64) error { <-ctx.Done(); return ctx.Err() }
			request := requestProviders(f.app, context.Background())
			waitProviderStarts(t, f, 2)
			if method == "Close" {
				f.app.Close()
			} else {
				f.app.cancel()
			}
			if err := receiveProviderResult(t, request).err; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if _, err := f.app.Providers(context.Background()); !errors.Is(err, context.Canceled) {
				t.Fatalf("joined closed app: %v", err)
			}
			f.app.Close()
			if f.active.Load() != 0 {
				t.Fatal("App.Close left checks running")
			}
			assertProviderCalls(t, f, 1, 1)
		})
	}
}

func TestProvidersModelOperationsAlwaysRecheck(t *testing.T) {
	f := newProviderFixture(t)
	clock := setProviderClock(f)
	rows, err := f.app.Providers(context.Background())
	assertProviderRows(t, providerResult{rows, err}, true)
	f.ready.Store(false)
	models, err := f.app.modelProviders(context.Background())
	assertProviderRows(t, providerResult{models, err}, false)
	assertProviderCalls(t, f, 2, 2)
	for i, row := range models {
		if row["id"] != providerNames[i] || row["name"] != providerNames[i] {
			t.Fatal(row)
		}
	}
	rows, err = f.app.Providers(context.Background())
	assertProviderRows(t, providerResult{rows, err}, true)
	if _, ok := rows[0]["id"]; ok {
		t.Fatal("model id leaked into meta")
	}
	clock.Add(int64(providerTTL))
	rows, err = f.app.Providers(context.Background())
	assertProviderRows(t, providerResult{rows, err}, false)
	f.ready.Store(true)
	models, err = f.app.modelProviders(context.Background())
	assertProviderRows(t, providerResult{models, err}, true)
	assertProviderCalls(t, f, 4, 4)
	rows, err = f.app.Providers(context.Background())
	assertProviderRows(t, providerResult{rows, err}, false)
	assertProviderCalls(t, f, 4, 4)
}

func TestProvidersModelOperationsDoNotJoinScreenFlight(t *testing.T) {
	f := newProviderFixture(t)
	release := make(chan struct{})
	f.hook = func(ctx context.Context, _ ai.ProcessRequest, _ int, count int64) error {
		if count == 1 {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return errors.New("fresh authentication failure")
	}
	screen := requestProviders(f.app, context.Background())
	waitProviderStarts(t, f, 2)
	models := make(chan providerResult, 1)
	go func() { rows, err := f.app.modelProviders(context.Background()); models <- providerResult{rows, err} }()
	assertProviderRows(t, receiveProviderResult(t, models), false)
	close(release)
	assertProviderRows(t, receiveProviderResult(t, screen), true)
	assertProviderCalls(t, f, 2, 2)
}

func TestProvidersRealExecutablesAndAppIsolation(t *testing.T) {
	// Production ai.Run, clean environment, CLI arguments and auth parsers use
	// temporary executables and fixture homes, never installed credentials.
	for range 2 {
		f := newProviderFixture(t)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("CODEX_HOME", t.TempDir())
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		counter := filepath.Join(f.app.Config.RuntimeDir, "launches")
		for i, path := range []string{f.app.Runner.Codex, f.app.Runner.Claude} {
			output := "Logged in using ChatGPT"
			args := "login status"
			if i == 1 {
				output = `{"loggedIn":true,"apiProvider":"firstParty","authMethod":"claude.ai"}`
				args = "auth status --json"
			}
			script := fmt.Sprintf("#!/bin/sh\n[ \"$*\" = '%s' ] || exit 2\nprintf '%%s\\n' '%s'\nprintf 'launch\\n' >> '%s'\n", args, output, counter)
			if err := os.WriteFile(path, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
		}
		f.app.Runner.Execute = nil
		for range 2 {
			rows, err := f.app.Providers(context.Background())
			assertProviderRows(t, providerResult{rows, err}, true)
		}
		data, err := os.ReadFile(counter)
		if err != nil || string(data) != "launch\nlaunch\n" {
			t.Fatalf("production launch count %q: %v", data, err)
		}
	}
}

func TestProvidersQueuedConditionsAreRechecked(t *testing.T) {
	f := newProviderFixture(t)
	cleanup, releaseCleanup := newProviderCleanup(t)
	stopped := make(chan struct{}, 2)
	nextRuntime := t.TempDir()
	f.hook = func(ctx context.Context, req ai.ProcessRequest, _ int, count int64) error {
		if count == 1 {
			<-ctx.Done()
			stopped <- struct{}{}
			<-cleanup
			return ctx.Err()
		}
		if req.Directory != nextRuntime {
			return fmt.Errorf("executed obsolete runtime %q", req.Directory)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	abandoned := requestProviders(f.app, ctx)
	waitProviderStarts(t, f, 2)
	cancel()
	if err := receiveProviderResult(t, abandoned).err; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Fatal("cancel not delivered")
		}
	}
	queued := requestProviders(f.app, context.Background())
	waitProviderCondition(t, f, func(c *providerCache) bool { return len(c.live) == 4 })
	f.app.mu.Lock()
	copy := *f.app.Config
	copy.RuntimeDir = nextRuntime
	f.app.Config = &copy
	f.app.mu.Unlock()
	releaseCleanup()
	assertProviderRows(t, receiveProviderResult(t, queued), true)
	assertProviderCalls(t, f, 2, 2)
}
