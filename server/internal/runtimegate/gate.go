// Package runtimegate coordinates AI capacity and the NAS deployment drain.
package runtimegate

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/contract"
	"sync"
	"time"
)

type acceptedKey struct{}
type accepted struct {
	gate   *Gate
	kind   string
	ticket *bool
}
type Options struct {
	RuntimeDir, Revision string
	Capacity             int
	Now                  func() time.Time
}
type Gate struct {
	options  Options
	mu       sync.Mutex
	changed  chan struct{}
	active   map[string]int
	until    int64
	closed   bool
	handoff  bool
	lastTime int64
	ioMu     sync.Mutex
}

func New(o Options) (*Gate, error) {
	if o.Capacity < 1 || o.Capacity > 8 || o.RuntimeDir == "" {
		return nil, errors.New("AI枠と保存先の設定が不正です")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	g := &Gate{options: o, changed: make(chan struct{}), active: map[string]int{"ai": 0, "filing": 0, "project": 0}}
	if err := g.prepare(); err != nil {
		return nil, err
	}
	return g, nil
}
func (g *Gate) notify() { close(g.changed); g.changed = make(chan struct{}) }
func (g *Gate) Acquire(ctx context.Context, kind string) (context.Context, func(), error) {
	if kind != "ai" && kind != "filing" && kind != "project" {
		return nil, nil, errors.New("処理枠の種類が不正です")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return nil, nil, errors.New("Serverを停止中です")
		}
		prior, _ := ctx.Value(acceptedKey{}).(accepted)
		continuing := prior.gate == g && prior.ticket != nil && *prior.ticket && (prior.kind == "filing" || prior.kind == "project")
		if g.handoff && !continuing {
			g.mu.Unlock()
			return nil, nil, contract.NewProblem(503, "Serverの入れ替えのため新しい処理を止めています。", "SERVER_DRAINING")
		}
		if g.until != 0 && !continuing && kind != "ai" {
			g.mu.Unlock()
			return nil, nil, contract.NewProblem(503, "Serverの入れ替えのため新しい起票・作成を止めています。保存済みの依頼は入れ替え後に続けます。", "SERVER_DRAINING")
		}
		if (g.until == 0 || continuing) && (kind != "ai" || g.active["ai"] < g.options.Capacity) {
			g.active[kind]++
			g.notify()
			g.mu.Unlock()
			once := &sync.Once{}
			ticket := true
			release := func() { once.Do(func() { g.mu.Lock(); g.active[kind]--; ticket = false; g.notify(); g.mu.Unlock() }) }
			if kind != "ai" {
				ctx = context.WithValue(ctx, acceptedKey{}, accepted{g, kind, &ticket})
			}
			return ctx, release, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}
func (g *Gate) Close() error {
	g.ioMu.Lock()
	defer g.ioMu.Unlock()
	g.mu.Lock()
	g.closed = true
	g.until = 0
	g.notify()
	last := g.lastTime
	g.mu.Unlock()
	if last != 0 {
		if err := g.writeHeartbeat(last, false); err != nil {
			return err
		}
	}
	return g.removeArtifacts(false)
}
