package projects

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"sync/atomic"
	"testing"
)

type executionKey struct{}

func TestCreationGateRejectsBeforeExternalWorkAndPropagatesToFiling(t *testing.T) {
	f := newFixture(t)
	var draining atomic.Bool
	draining.Store(true)
	var acquired, released atomic.Int64
	f.options.Acquire = func(ctx context.Context, kind string) (context.Context, func(), error) {
		if kind != "project" {
			t.Errorf("unexpected work kind %s", kind)
		}
		if draining.Load() && ctx.Value(executionKey{}) != true {
			return nil, nil, problem(503, "更新のため新しい処理を停止中です。")
		}
		acquired.Add(1)
		return context.WithValue(ctx, executionKey{}, true), func() { released.Add(1) }, nil
	}
	var registered atomic.Bool
	f.options.Repositories = func() []string {
		if registered.Load() {
			return []string{"owner/demo"}
		}
		return []string{}
	}
	var submitted atomic.Bool
	f.options.Submit = func(ctx context.Context, d Object) ([]Object, error) {
		if ctx.Value(executionKey{}) != true {
			t.Error("execution context was dropped before filing")
		}
		if ctx.Err() != nil {
			t.Error("creation release cancelled filing")
		}
		submitted.Store(true)
		return []Object{{"number": 1}}, nil
	}
	f.options.Registered = func(ctx context.Context, _ Object) error {
		if ctx.Value(executionKey{}) != true {
			t.Error("execution context was dropped before registration")
		}
		registered.Store(true)
		draining.Store(true)
		return nil
	}
	f.restart()
	_, _ = f.call("/api/projects", Object{"id": id, "text": "新規プロジェクト"})
	p := f.wait("ready")
	code, _ := f.call("/api/projects/"+id+"/approve", Object{"fingerprint": p["fingerprint"]})
	if code != 503 {
		t.Fatalf("drain status %d", code)
	}
	p, _ = f.s.Snapshot(context.Background(), id)
	if p["status"] == "creating" || f.git.createCalls != 0 || acquired.Load() != 0 {
		t.Fatal("drain started external creation")
	}
	draining.Store(false)
	code, _ = f.call("/api/projects/"+id+"/create", Object{})
	if code != 202 {
		t.Fatalf("create status %d", code)
	}
	f.wait("registered")
	f.s.wg.Wait()
	// 登録の直後は起票が終わっていないことがあるので、起票の完了を待ってから最新の状態で比べる。
	p, _ = f.s.Snapshot(context.Background(), id)
	if !submitted.Load() || fleet.ObjectOrEmpty(p["filing"])["status"] != "filed" || acquired.Load() != 2 || released.Load() != 2 {
		t.Fatalf("execution did not finish: %v acquired=%d released=%d", p, acquired.Load(), released.Load())
	}
}
