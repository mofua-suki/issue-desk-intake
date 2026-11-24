// Package live sends bounded change identifiers to authenticated browser sessions.
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

type client struct {
	owner   string
	pending map[string]map[string]any
	wake    chan struct{}
}
type Events struct {
	Auth    *auth.Authenticator
	mu      sync.Mutex
	clients map[*client]bool
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
}

var uuid = regexp.MustCompile(`^[a-f0-9-]{36}$`)
var repo = regexp.MustCompile(`^[^/\s]{1,100}/[^/\s]{1,100}$`)

func New(ctx context.Context, a *auth.Authenticator) *Events {
	lifecycle, cancel := context.WithCancel(ctx)
	return &Events{Auth: a, clients: map[*client]bool{}, ctx: lifecycle, cancel: cancel}
}
func event(value map[string]any) map[string]any {
	kind, _ := value["type"].(string)
	switch kind {
	case "list":
		return map[string]any{"type": "list"}
	case "draft", "todo", "project", "closeRequest":
		id, _ := value["id"].(string)
		if uuid.MatchString(id) {
			return map[string]any{"type": kind, "id": id}
		}
	case "issue":
		project, _ := value["project"].(string)
		var number int64
		switch n := value["number"].(type) {
		case int64:
			number = n
		case int:
			number = int64(n)
		case json.Number:
			number, _ = n.Int64()
		case float64:
			if n == float64(int64(n)) {
				number = int64(n)
			}
		}
		if repo.MatchString(project) && number > 0 && number <= 9007199254740991 {
			return map[string]any{"type": kind, "project": project, "number": number}
		}
	}
	return nil
}
func (e *Events) Publish(raw map[string]any) {
	value := event(raw)
	if value == nil {
		return
	}
	data, _ := json.Marshal(value)
	key := string(data)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	for c := range e.clients {
		if _, all := c.pending[`{"type":"list"}`]; !all {
			c.pending[key] = value
			if len(c.pending) > 50 {
				c.pending = map[string]map[string]any{`{"type":"list"}`: {"type": "list"}}
			}
		}
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}
func (e *Events) Close() { e.mu.Lock(); e.closed = true; e.cancel(); e.mu.Unlock() }
func (e *Events) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		contract.WriteProblem(w, contract.NewProblem(405, "このメソッドは利用できません。", ""))
		return
	}
	p, problem := e.Auth.Authenticate(r)
	if problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	if problem = auth.Authorize(p, auth.Policy{Screen: true}); problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	cookie, err := r.Cookie("desk_session")
	if err != nil {
		contract.WriteProblem(w, contract.NewProblem(401, "受付への接続が必要です。", ""))
		return
	}
	c := &client{owner: auth.Hash(cookie.Value), pending: map[string]map[string]any{}, wake: make(chan struct{}, 1)}
	if !e.register(c) {
		contract.WriteProblem(w, contract.NewProblem(503, "即時通知の接続数が上限です。定期取得で更新します。", ""))
		return
	}
	defer func() { e.mu.Lock(); delete(e.clients, c); e.mu.Unlock() }()
	e.stream(w, r, c)
}

// register は接続の上限（全体8・同じ利用者4）に収まるときだけ通知の宛先に加える。
func (e *Events) register(c *client) bool {
	e.mu.Lock()
	owners := 0
	for other := range e.clients {
		if other.owner == c.owner {
			owners++
		}
	}
	if e.closed || len(e.clients) >= 8 || owners >= 4 {
		e.mu.Unlock()
		return false
	}
	e.clients[c] = true
	e.mu.Unlock()
	return true
}

// stream は通知を送り続ける。25秒ごとに接続を確かめ、溜まった通知をまとめて送る。
func (e *Events) stream(w http.ResponseWriter, r *http.Request, c *client) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set(contract.Header, contract.Version)
	controller := http.NewResponseController(w)
	write := func(chunk string) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprint(w, chunk); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !write("retry: 3000\n\n") {
		return
	}
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-e.ctx.Done():
			return
		case <-heartbeat.C:
			if _, failure := e.Auth.Authenticate(r); failure != nil {
				return
			}
			if !write(": keepalive\n\n") {
				return
			}
		case <-c.wake:
			timer := time.NewTimer(150 * time.Millisecond)
			select {
			case <-timer.C:
			case <-r.Context().Done():
				timer.Stop()
				return
			case <-e.ctx.Done():
				timer.Stop()
				return
			}
			e.mu.Lock()
			pending := c.pending
			c.pending = map[string]map[string]any{}
			e.mu.Unlock()
			for _, item := range pending {
				raw, _ := json.Marshal(item)
				if !write("data: " + string(raw) + "\n\n") {
					return
				}
			}
		}
	}
}
