package ai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type LiveCall struct {
	ID        string    `json:"id"`
	Draft     string    `json:"draft"`
	Project   string    `json:"repo"`
	Step      string    `json:"step"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	StartedAt time.Time `json:"startedAt"`
}

var liveCalls = struct {
	sync.Mutex
	byRunner map[*Runner]map[string]LiveCall
}{byRunner: map[*Runner]map[string]LiveCall{}}

func (r *Runner) live(request Request) func() {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	liveCalls.Lock()
	if liveCalls.byRunner[r] == nil {
		liveCalls.byRunner[r] = map[string]LiveCall{}
	}
	liveCalls.byRunner[r][id] = LiveCall{id, request.Draft, request.Project, request.Step, request.Provider, request.Model, time.Now()}
	liveCalls.Unlock()
	return func() {
		liveCalls.Lock()
		delete(liveCalls.byRunner[r], id)
		if len(liveCalls.byRunner[r]) == 0 {
			delete(liveCalls.byRunner, r)
		}
		liveCalls.Unlock()
	}
}
func (r *Runner) Calls(ctx context.Context) ([]LiveCall, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	liveCalls.Lock()
	defer liveCalls.Unlock()
	out := []LiveCall{}
	for _, call := range liveCalls.byRunner[r] {
		out = append(out, call)
	}
	return out, nil
}
