package startup

import (
	"context"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/runtimegate"
)

type gatedRunner struct {
	runner *ai.Runner
	app    *App
}

// Gate changes and acquisition must use these methods. Gate remains exported for
// existing lifecycle code, which accesses it only when no replacement is running.
func (a *App) setGate(gate *runtimegate.Gate) {
	a.gateMu.Lock()
	defer a.gateMu.Unlock()
	a.Gate = gate
}

func (a *App) currentGate() *runtimegate.Gate {
	a.gateMu.RLock()
	defer a.gateMu.RUnlock()
	return a.Gate
}

func (a *App) acquireGate(ctx context.Context, kind string) (context.Context, func(), error) {
	gate := a.currentGate()
	if gate == nil {
		return nil, nil, contract.NewProblem(503, "Serverの準備中のため新しい処理を受け付けられません。", "SERVER_DRAINING")
	}
	return gate.Acquire(ctx, kind)
}

func (r gatedRunner) Call(ctx context.Context, request ai.Request) (ai.Answer, error) {
	ctx, release, err := r.app.acquireGate(ctx, "ai")
	if err != nil {
		return ai.Answer{}, err
	}
	defer release()
	return r.runner.Call(ctx, request)
}
func (a *App) startGate() error {
	if err := a.Gate.Tick(); err != nil {
		return err
	}
	a.periodically("gate-tick", time.Second, func(context.Context) error { return a.Gate.Tick() })
	return nil
}
