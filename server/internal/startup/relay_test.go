package startup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

type relayTestKeys struct {
	mu   sync.Mutex
	keys map[string]time.Time
}

func (k *relayTestKeys) ConsumeCLIKey(_ context.Context, hash string, now time.Time) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	expires, ok := k.keys[hash]
	if !ok || expires.Before(now) {
		return false, nil
	}
	delete(k.keys, hash)
	return true, nil
}

func (k *relayTestKeys) AddCLIKey(_ context.Context, hash string, expires time.Time) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys[hash] = expires
	return nil
}

type relayPeer struct {
	server *httptest.Server
	active atomic.Bool
	mu     sync.Mutex
	seen   []*http.Request
	bodies []string
}

func newRelayPeer(t *testing.T) *relayPeer {
	t.Helper()
	p := &relayPeer{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			role := "standby"
			if p.active.Load() {
				role = "active"
			}
			io.WriteString(w, `{"ok":true,"role":"`+role+`"}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.seen, p.bodies = append(p.seen, r.Clone(context.Background())), append(p.bodies, string(body))
		p.mu.Unlock()
		w.Header().Set("X-Writer", "peer")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"saved":true}`)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func testRelay(t *testing.T, peer string, keys relayKeys, stop <-chan struct{}) *Relay {
	t.Helper()
	x, err := NewRelay(peer, 8787, keys, stop)
	if err != nil {
		t.Fatal(err)
	}
	x.interval = 5 * time.Millisecond
	return x
}

func never() bool { return false }

var relayUnused = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("自色で処理した") })

func notAccepted(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != 503 || w.Header().Get(contract.NotAcceptedHeader) != "1" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("未受理の503ではない: %d %v %s", w.Code, w.Header(), w.Body)
	}
}

func TestRelayWithoutPeerRejectsAsNotAccepted(t *testing.T) {
	x, err := NewRelay("", 8787, nil, nil)
	if err != nil || x != nil {
		t.Fatal(x, err)
	}
	w := httptest.NewRecorder()
	x.Serve(w, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/issues", strings.NewReader("{}")), never, relayUnused, "Serverは待機中です。")
	notAccepted(t, w)
	if !strings.Contains(w.Body.String(), "Serverは待機中です。") {
		t.Fatal(w.Body.String())
	}
	for _, bad := range []string{"https://peer:8787", "http://peer:8787/api", "http://user@peer:8787", "peer:8787", "http://peer:8787?x=1"} {
		if _, err := NewRelay(bad, 8787, nil, nil); err == nil {
			t.Fatal("不正な相手色を受け付けた", bad)
		}
	}
}

func TestRelayHoldsPostUntilPeerIsWriterAndDeliversOnce(t *testing.T) {
	peer := newRelayPeer(t)
	x := testRelay(t, peer.server.URL, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "http://desk.example/api/issues?x=1", strings.NewReader(`{"id":"a"}`))
	r.Header.Set("Authorization", "Bearer token")
	r.Header.Set("X-Forwarded-For", "100.64.0.9")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); x.Serve(w, r, never, relayUnused, "交代中") }()
	select {
	case <-done:
		t.Fatal("書き手が居ないのに応答した", w.Code)
	case <-time.After(60 * time.Millisecond):
	}
	peer.mu.Lock()
	early := len(peer.seen)
	peer.mu.Unlock()
	if early != 0 {
		t.Fatal("待機中の相手色へ業務要求を送った")
	}
	peer.active.Store(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("書き手の交代後に届かない")
	}
	if w.Code != http.StatusCreated || w.Body.String() != `{"saved":true}` || w.Header().Get("X-Writer") != "peer" {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	if len(peer.seen) != 1 || peer.bodies[0] != `{"id":"a"}` {
		t.Fatal("本文が1回だけ届いていない", peer.bodies)
	}
	got := peer.seen[0]
	if got.Host != "desk.example" || got.URL.RequestURI() != "/api/issues?x=1" || got.Header.Get("Authorization") != "Bearer token" ||
		got.Header.Get("X-Forwarded-For") != "100.64.0.9" || got.Header.Get(relayedHeader) != "1" || got.Header.Get(auth.RelayKeyHeader) != "" {
		t.Fatal(got.Host, got.URL, got.Header)
	}
}

func TestRelayDoesNotSendARelayedRequestBack(t *testing.T) {
	peer := newRelayPeer(t)
	peer.active.Store(true)
	x := testRelay(t, peer.server.URL, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/issues", strings.NewReader("{}"))
	r.Header.Set(relayedHeader, "1")
	w := httptest.NewRecorder()
	x.Serve(w, r, never, relayUnused, "交代中")
	notAccepted(t, w)
	if len(peer.seen) != 0 {
		t.Fatal("転送済みの要求を送り返した")
	}
}

func TestRelayServesLocallyWhenThisColorBecomesWriter(t *testing.T) {
	peer := newRelayPeer(t)
	x := testRelay(t, peer.server.URL, nil, nil)
	var ready atomic.Bool
	served := 0
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	})
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		x.Serve(w, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/todos", strings.NewReader("mine")), ready.Load, local, "待機中")
	}()
	time.Sleep(30 * time.Millisecond)
	ready.Store(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("自色が書き手になっても処理しない")
	}
	if served != 1 || w.Body.String() != "mine" || len(peer.seen) != 0 {
		t.Fatal(served, w.Body.String(), len(peer.seen))
	}
}

func TestRelayEndsWaitAsNotAcceptedOnDeadlineAndShutdown(t *testing.T) {
	peer := newRelayPeer(t)
	x := testRelay(t, peer.server.URL, nil, nil)
	x.hold = 40 * time.Millisecond
	w := httptest.NewRecorder()
	x.Serve(w, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/issues", strings.NewReader("{}")), never, relayUnused, "交代中")
	notAccepted(t, w)
	stop := make(chan struct{})
	x = testRelay(t, peer.server.URL, nil, stop)
	w = httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		x.Serve(w, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/issues", strings.NewReader("{}")), never, relayUnused, "交代中")
	}()
	time.Sleep(20 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("停止時に待ちが終わらない")
	}
	notAccepted(t, w)
	if len(peer.seen) != 0 {
		t.Fatal("未受理の要求が届いた")
	}
}

func TestRelayReplacesLocalOneTimeKeyWithTicket(t *testing.T) {
	peer := newRelayPeer(t)
	keys := &relayTestKeys{keys: map[string]time.Time{}}
	x := testRelay(t, peer.server.URL, keys, nil)
	key := strings.Repeat("k", 43)
	send := func(remote string, forwarded bool) *httptest.ResponseRecorder {
		keys.AddCLIKey(context.Background(), auth.Hash(key), time.Now().Add(time.Minute))
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/issues", strings.NewReader("{}"))
		r.RemoteAddr = remote
		r.Header.Set("X-Desk-Key", key)
		r.Header.Set(auth.RelayKeyHeader, strings.Repeat("f", 43))
		if forwarded {
			r.Header.Set("X-Forwarded-For", "100.64.0.9")
		}
		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { defer close(done); x.Serve(w, r, never, relayUnused, "交代中") }()
		// 一時キーの期限（1分）より長い交代でも届くよう、待つ前に消費する。
		time.Sleep(30 * time.Millisecond)
		peer.active.Store(true)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("届かない")
		}
		peer.active.Store(false)
		x.mu.Lock()
		x.checked = time.Time{}
		x.mu.Unlock()
		return w
	}
	if w := send("127.0.0.1:5000", false); w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}
	ticket := peer.seen[0].Header.Get(auth.RelayKeyHeader)
	if !relayKeyPattern.MatchString(ticket) || ticket == strings.Repeat("f", 43) {
		t.Fatal("チケットを発行していない", ticket)
	}
	if _, ok := keys.keys[auth.Hash(key)]; ok {
		t.Fatal("一時キーを消費していない")
	}
	if expires, ok := keys.keys[auth.RelayKeyHash(ticket)]; !ok || time.Until(expires) > time.Minute {
		t.Fatal("チケットを共有の保存先へ保存していない")
	}
	// 自色でローカルと確かめられない要求には、チケットを発行せず、持ち込まれたチケットも渡さない。
	for _, c := range []struct {
		remote    string
		forwarded bool
	}{{"172.18.0.9:5000", false}, {"127.0.0.1:5000", true}} {
		send(c.remote, c.forwarded)
		got := peer.seen[len(peer.seen)-1]
		if got.Header.Get(auth.RelayKeyHeader) != "" || len(keys.keys) != 2 {
			t.Fatal("ローカルでない要求にチケットを渡した", c, got.Header)
		}
		if _, ok := keys.keys[auth.Hash(key)]; !ok {
			t.Fatal("ローカルでない要求が一時キーを消費した", c)
		}
	}
}

// A relayed event stream must not keep the old color from stopping.
func TestRelayEndsRelayedReadWhenThisColorStops(t *testing.T) {
	started := make(chan struct{})
	var streams atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			io.WriteString(w, `{"ok":true,"role":"active"}`)
			return
		}
		n := streams.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: stream-%d\n\n", n)
		w.(http.Flusher).Flush()
		if n == 1 {
			close(started)
			<-r.Context().Done()
		}
	}))
	defer peer.Close()
	stop := make(chan struct{})
	x := testRelay(t, peer.URL, nil, stop)
	x.stopGrace = 100 * time.Millisecond
	var switched atomic.Bool
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if switched.Load() {
			http.Redirect(w, r, peer.URL+r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		x.Serve(w, r, never, http.NotFoundHandler(), "待機中")
	}))
	defer front.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(front.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: stream-1\n" {
		t.Fatalf("initial stream: %q %v", line, err)
	}
	<-started
	switched.Store(true)
	close(stop)
	_, err = io.ReadAll(reader)
	resp.Body.Close()
	if err == nil {
		t.Fatal("stopped stream was not interrupted")
	}
	resp, err = client.Get(front.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err = bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || resp.StatusCode != http.StatusOK || line != "data: stream-2\n" || streams.Load() != 2 {
		t.Fatalf("reconnected stream: %d %q %v", resp.StatusCode, line, err)
	}
}

// A caller that gave up while its POST was held must not have it delivered later.
func TestRelayDropsHeldPostAfterCallerDisconnects(t *testing.T) {
	peer := newRelayPeer(t)
	x := testRelay(t, peer.server.URL, nil, nil)
	returned := make(chan struct{}, 1)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.Serve(w, r, never, http.NotFoundHandler(), "待機中")
		returned <- struct{}{}
	}))
	defer front.Close()
	ctx, cancel := context.WithCancel(t.Context())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/api/comment", strings.NewReader(`{"text":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	failed := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(request)
		if err == nil {
			response.Body.Close()
		}
		failed <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-failed; err == nil {
		t.Fatal("held request answered before a writer existed")
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("held request kept waiting after the caller disconnected")
	}
	peer.active.Store(true)
	time.Sleep(3 * x.interval)
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if len(peer.seen) != 0 {
		t.Fatal("abandoned request reached the writer")
	}
}

func TestRelayDoesNotHoldOversizedBody(t *testing.T) {
	peer := newRelayPeer(t)
	peer.active.Store(true)
	x := testRelay(t, peer.server.URL, nil, nil)
	w := httptest.NewRecorder()
	x.Serve(w, httptest.NewRequest(http.MethodPost, "http://issue-desk.example/api/comment", strings.NewReader(strings.Repeat("a", relayBodyLimit+1))), never, http.NotFoundHandler(), "待機中")
	notAccepted(t, w)
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if len(peer.seen) != 0 {
		t.Fatal("oversized request reached the writer")
	}
}

type relayBrokenKeys struct{}

func (relayBrokenKeys) ConsumeCLIKey(context.Context, string, time.Time) (bool, error) {
	return false, context.DeadlineExceeded
}
func (relayBrokenKeys) AddCLIKey(context.Context, string, time.Time) error {
	return context.DeadlineExceeded
}

// An unreadable key store must not turn a valid local key into an authentication failure.
func TestRelayAnswersNotAcceptedWhenKeyStoreFails(t *testing.T) {
	peer := newRelayPeer(t)
	peer.active.Store(true)
	x := testRelay(t, peer.server.URL, relayBrokenKeys{}, nil)
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/comment", strings.NewReader("{}"))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("X-Desk-Key", strings.Repeat("k", 43))
	w := httptest.NewRecorder()
	x.Serve(w, r, never, http.NotFoundHandler(), "待機中")
	notAccepted(t, w)
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if len(peer.seen) != 0 {
		t.Fatal("request without its key reached the writer")
	}
}

// A delivered long poll is cut after the grace so the old color can stop.
func TestRelayCutsDeliveredWriteAfterStopGrace(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			io.WriteString(w, `{"ok":true,"role":"active"}`)
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer peer.Close()
	defer close(release)
	stop := make(chan struct{})
	x := testRelay(t, peer.URL, nil, stop)
	x.stopGrace = 300 * time.Millisecond
	done := make(chan time.Time, 1)
	w := httptest.NewRecorder()
	go func() {
		x.Serve(w, httptest.NewRequest(http.MethodPost, "http://issue-desk.example/api/v1/jobs/next", strings.NewReader("{}")), never, http.NotFoundHandler(), "待機中")
		done <- time.Now()
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("long poll did not reach the writer")
	}
	stopped := time.Now()
	close(stop)
	select {
	case ended := <-done:
		if ended.Sub(stopped) < x.stopGrace {
			t.Fatal("delivered write was cut before the grace", ended.Sub(stopped))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delivered write kept the stopping color waiting")
	}
	// The writer may have handled it: the answer must not claim "not accepted".
	if w.Code != http.StatusBadGateway || w.Header().Get(contract.NotAcceptedHeader) != "" {
		t.Fatal(w.Code, w.Header())
	}
}

// After the handoff boundary closed, the old color hands new requests to the
// relay; requests admitted before the boundary are unaffected and not counted twice.
func TestHandoffBoundaryPassesLateRequestsToRelay(t *testing.T) {
	peer := newRelayPeer(t)
	peer.active.Store(true)
	x := testRelay(t, peer.server.URL, nil, nil)
	boundary := &HandoffHTTP{}
	local := 0
	var handler http.Handler
	boundary.Unavailable = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.Serve(w, r, func() bool { return !boundary.Stopped() }, handler, "交代中")
	})
	handler = boundary.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { local++; w.WriteHeader(http.StatusNoContent) }))
	send := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "http://issue-desk.example/api/comment", strings.NewReader(`{"text":"a"}`)))
		return w
	}
	if w := send(); w.Code != http.StatusNoContent || local != 1 {
		t.Fatal(w.Code, local)
	}
	boundary.Begin()
	if w := send(); w.Code != http.StatusCreated || w.Header().Get("X-Writer") != "peer" || local != 1 {
		t.Fatal(w.Code, w.Header(), local)
	}
	if err := boundary.Wait(t.Context()); err != nil {
		t.Fatal("late request held the handoff", err)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if len(peer.bodies) != 1 || peer.bodies[0] != `{"text":"a"}` {
		t.Fatal(peer.bodies)
	}
}

// An ordinary delivered write is not cut by the long-poll grace: it finishes.
func TestRelayLetsDeliveredOrdinaryWriteFinishAfterStop(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			io.WriteString(w, `{"ok":true,"role":"active"}`)
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer peer.Close()
	stop := make(chan struct{})
	x := testRelay(t, peer.URL, nil, stop)
	x.stopGrace = 20 * time.Millisecond
	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() {
		defer close(done)
		x.Serve(w, httptest.NewRequest(http.MethodPost, "http://issue-desk.example/api/comment", strings.NewReader("{}")), never, http.NotFoundHandler(), "待機中")
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not reach the writer")
	}
	close(stop)
	select {
	case <-done:
		t.Fatal("ordinary write was cut when this color stopped", w.Code)
	case <-time.After(10 * x.stopGrace):
	}
	close(release)
	<-done
	if w.Code != http.StatusCreated {
		t.Fatal(w.Code)
	}
}

func TestRelayAnswersNotAcceptedWhenBodyStalls(t *testing.T) {
	peer := newRelayPeer(t)
	x := testRelay(t, peer.server.URL, nil, nil)
	x.bodyWait = 200 * time.Millisecond
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.Serve(w, r, never, http.NotFoundHandler(), "Serverは待機中です。")
	}))
	defer front.Close()
	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The caller announces 100 bytes, sends 10 and stalls.
	if _, err = io.WriteString(conn, "POST /api/comment HTTP/1.1\r\nHost: 127.0.0.1:8787\r\nContent-Length: 100\r\n\r\n0123456789"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("本文が止まった要求に未受理の応答が届きません: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get(contract.NotAcceptedHeader) != "1" {
		t.Fatalf("status=%d header=%q", response.StatusCode, response.Header.Get(contract.NotAcceptedHeader))
	}
}

func TestRelayShortReadsSurviveStop(t *testing.T) {
	for _, phase := range []string{"held", "delivered"} {
		for _, path := range []string{"/", "/api/projects"} {
			t.Run(phase+path, func(t *testing.T) {
				var active atomic.Bool
				probed := make(chan struct{}, 1)
				entered := make(chan struct{}, 1)
				release := make(chan struct{})
				var count atomic.Int32
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/healthz" {
						if active.Load() {
							io.WriteString(w, `{"ok":true,"role":"active"}`)
						} else {
							select {
							case probed <- struct{}{}:
							default:
							}
							io.WriteString(w, `{"ok":true,"role":"standby"}`)
						}
						return
					}
					count.Add(1)
					entered <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					io.WriteString(w, "response "+r.URL.Path)
				}))
				defer peer.Close()
				stop := make(chan struct{})
				x := testRelay(t, peer.URL, nil, stop)
				x.stopGrace = time.Second
				front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { x.Serve(w, r, never, relayUnused, "交代中") }))
				defer front.Close()
				result := make(chan string, 1)
				go func() {
					c := &http.Client{Timeout: 3 * time.Second}
					resp, err := c.Get(front.URL + path)
					if err != nil {
						result <- err.Error()
						return
					}
					defer resp.Body.Close()
					b, err := io.ReadAll(resp.Body)
					result <- fmt.Sprintf("%d %s %v", resp.StatusCode, b, err)
				}()
				select {
				case <-probed:
				case <-time.After(3 * time.Second):
					t.Fatal("not held")
				}
				active.Store(true)
				if phase == "held" {
					close(stop)
				}
				select {
				case <-entered:
				case got := <-result:
					close(release)
					t.Fatalf("before delivery: %s", got)
				case <-time.After(3 * time.Second):
					close(release)
					t.Fatal("not delivered")
				}
				if phase == "delivered" {
					close(stop)
					select {
					case got := <-result:
						close(release)
						t.Fatalf("stopped during delivery: %s", got)
					case <-time.After(50 * time.Millisecond):
					}
				}
				close(release)
				select {
				case got := <-result:
					if got != "200 response "+path+" <nil>" || count.Load() != 1 {
						t.Fatalf("result=%s count=%d", got, count.Load())
					}
				case <-time.After(3 * time.Second):
					t.Fatal("no response")
				}
			})
		}
	}
}

// Diagnostics must not disclose request data or transport error text, and a
// delivered write must never be retried after its result becomes uncertain.
func TestRelayFailureDiagnostics(t *testing.T) {
	for _, mode := range []string{"connect", "waiting", "body", "stop"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			old := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(old)
			var count atomic.Int32
			entered := make(chan struct{})
			stop := make(chan struct{})
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					io.WriteString(w, `{"ok":true,"role":"active"}`)
					return
				}
				count.Add(1)
				io.Copy(io.Discard, r.Body)
				if mode == "body" {
					w.Header().Set("Content-Length", "100")
					io.WriteString(w, "partial")
					return
				}
				if mode == "waiting" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						return
					}
					conn.Close()
					return
				}
				close(entered)
				<-r.Context().Done()
			}))
			defer peer.Close()
			x := testRelay(t, peer.URL, nil, stop)
			if mode == "connect" {
				x.proxy.Transport = relayTransport{base: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("raw-error-secret")}
				}}}
			}
			x.stopGrace = time.Millisecond
			path := "/api/write"
			if mode == "stop" {
				path = "/api/jobs/next"
			}
			req := httptest.NewRequest(http.MethodPost, "http://desk.example"+path+"?secret=query-secret", strings.NewReader("body-secret"))
			req.Header.Set("Authorization", "Bearer header-secret")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); x.Serve(w, req, never, relayUnused, "交代中") }()
			if mode == "stop" {
				<-entered
				close(stop)
			}
			<-done
			if w.Header().Get("X-Issue-Desk-Relay-Stop") != "" {
				t.Fatalf("unexpected stop marker: %v", w.Header())
			}
			want := map[string]string{"connect": "connect_failed", "waiting": "response_interrupted", "body": "response_interrupted", "stop": "stopped"}[mode]
			if !strings.Contains(logs.String(), "relay failure class="+want) {
				t.Fatalf("log=%q want=%s", logs.String(), want)
			}
			for _, secret := range []string{"header-secret", "query-secret", "body-secret", "raw-error-secret"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("secret in log: %q", logs.String())
				}
			}
			expected := int32(1)
			if mode == "connect" {
				expected = 0
			}
			if count.Load() != expected {
				t.Fatalf("deliveries=%d want=%d", count.Load(), expected)
			}
			switch mode {
			case "connect":
				if w.Code != http.StatusServiceUnavailable || w.Header().Get(contract.NotAcceptedHeader) != "1" || w.Header().Get("Retry-After") == "" {
					t.Fatalf("status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
				}
			case "waiting", "stop":
				if w.Code != http.StatusBadGateway || w.Header().Get(contract.NotAcceptedHeader) != "" || !strings.Contains(w.Body.String(), "結果を確かめてください") {
					t.Fatalf("status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
				}
			}
		})
	}
}

func TestRelayStoppedBeforeHeadersMarksOnlyReads(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		marked       bool
	}{
		{http.MethodGet, "/api/session", true}, {http.MethodHead, "/api/session", true},
		{http.MethodPost, "/api/jobs/next", false}, {http.MethodGet, "/api/jobs/next", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			entered := make(chan struct{})
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					io.WriteString(w, `{"ok":true,"role":"active"}`)
					return
				}
				close(entered)
				<-r.Context().Done()
			}))
			defer peer.Close()
			stop := make(chan struct{})
			x := testRelay(t, peer.URL, nil, stop)
			x.stopGrace = 10 * time.Millisecond
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				x.Serve(w, httptest.NewRequest(tc.method, "http://desk.example"+tc.path, nil), never, relayUnused, "待機中")
			}()
			<-entered
			close(stop)
			<-done
			if w.Code != http.StatusBadGateway || w.Header().Get(contract.NotAcceptedHeader) != "" || (w.Header().Get("X-Issue-Desk-Relay-Stop") == "1") != tc.marked {
				t.Fatalf("status=%d headers=%v", w.Code, w.Header())
			}
		})
	}
}
