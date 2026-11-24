package startup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/store"
)

// RelayHold bounds how long a request waits for a writer. It covers the deployer's
// handoff (HANDOFF_MS=75000) and the recovery of the old color (RECOVERY_MS=45000).
const RelayHold = 120 * time.Second

// relayedHeader marks a request that already crossed the colors once. A color
// that cannot serve it answers "not accepted" instead of sending it back.
const relayedHeader = "X-Issue-Desk-Relayed"

// relayBodyLimit matches the entry's request_buffers. A larger body is not held.
const relayBodyLimit = 1 << 20

// relayStopGrace bounds how long reads and delivered long polls may continue
// after this color was told to stop.
const relayStopGrace = 10 * time.Second

// relayBodyWait bounds reading the body of a request that is about to be held.
const relayBodyWait = 10 * time.Second

var relayKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// relayKeys is the shared one-time key store (cli_keys). Both colors read and
// write it; it does not need the writer lease (intake admin cli-key uses it too).
type relayKeys interface {
	ConsumeCLIKey(context.Context, string, time.Time) (bool, error)
	AddCLIKey(context.Context, string, time.Time) error
}

// Relay keeps a request that this color cannot serve (standby, or after its
// handoff boundary closed) and delivers it once a writer exists: to this color
// when it became the writer, otherwise to the other color. The request has not
// reached a business handler here, so nothing is processed twice.
type Relay struct {
	peer      *url.URL
	port      int
	keys      relayKeys
	proxy     *httputil.ReverseProxy
	client    *http.Client
	hold      time.Duration
	interval  time.Duration
	stopGrace time.Duration
	bodyWait  time.Duration
	stop      <-chan struct{}

	mu      sync.Mutex
	checked time.Time
	active  bool
}

type relayState struct {
	deadline time.Time
	cli      bool
	granted  bool
}

type relayStateKey struct{}

var (
	errRelayStopped = errors.New("relay stopped")
	errRelayNotSent = errors.New("relay request not sent")
)

type relayTrace struct{ connected atomic.Bool }

type relayTransport struct{ base http.RoundTripper }

func (t relayTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	trace := &relayTrace{}
	ctx := httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { trace.connected.Store(true) }})
	response, err := t.base.RoundTrip(r.WithContext(ctx))
	if err != nil {
		kind := "unknown"
		if errors.Is(context.Cause(ctx), errRelayStopped) {
			kind = "stopped"
		} else {
			var op *net.OpError
			if errors.As(err, &op) && op.Op == "dial" && !trace.connected.Load() {
				kind = "connect_failed"
				// This transport never reuses a connection. A dial failure
				// before GotConn means the writer could not receive the request.
				err = errors.Join(errRelayNotSent, err)
			} else if trace.connected.Load() && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
				kind = "response_interrupted"
			}
		}
		log.Printf("relay failure class=%s", kind)
		return nil, err
	}
	response.Body = &relayResponseBody{ReadCloser: response.Body, ctx: ctx}
	return response, nil
}

type relayResponseBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b *relayResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		kind := "response_interrupted"
		if errors.Is(context.Cause(b.ctx), errRelayStopped) {
			kind = "stopped"
		}
		log.Printf("relay failure class=%s", kind)
	}
	return n, err
}

// NewRelay returns nil when no peer is configured: the caller then rejects as before.
func NewRelay(peer string, port int, keys relayKeys, stop <-chan struct{}) (*Relay, error) {
	if peer == "" {
		return nil, nil
	}
	u, err := url.Parse(peer)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("ISSUE_DESK_PEER_URL は http://ホスト:ポート の形で指定してください")
	}
	u.Path = ""
	if port == 0 {
		port = 8787
	}
	x := &Relay{peer: u, port: port, keys: keys, hold: RelayHold, interval: 250 * time.Millisecond, stopGrace: relayStopGrace, bodyWait: relayBodyWait, stop: stop}
	dial := (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	// A stopped color must not receive a request over a reused connection.
	transport := &http.Transport{DisableKeepAlives: true, DialContext: dial}
	x.client = &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	x.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(x.peer)
			// The receiving color checks the same Host and forwarding headers as a direct request.
			pr.Out.Host = pr.In.Host
			for name, values := range pr.In.Header {
				if auth.ForwardingHeader(name) {
					pr.Out.Header[name] = values
				}
			}
			pr.Out.Header.Set(relayedHeader, "1")
		},
		Transport:     relayTransport{base: transport},
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, errRelayNotSent) {
				contract.WriteNotAccepted(w, "受付を交代しています。少し待って同じ要求を再送してください。")
				return
			}
			if errors.Is(context.Cause(r.Context()), errRelayStopped) && (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasSuffix(r.URL.Path, "/next") {
				w.Header().Set("X-Issue-Desk-Relay-Stop", "1")
			}
			// The request may have reached the writer: do not mark it as not accepted.
			contract.WriteProblem(w, contract.NewProblem(http.StatusBadGateway, "交代先の受付から応答を受け取れませんでした。結果を確かめてください。", ""))
		},
	}
	return x, nil
}

// Serve holds r until ready reports that this color serves again (local then
// handles it) or the peer is the writer. message is the rejection text used when
// no relay is configured, the wait ends, or the request already crossed once.
func (x *Relay) Serve(w http.ResponseWriter, r *http.Request, ready func() bool, local http.Handler, message string) {
	if x == nil || r.Header.Get(relayedHeader) != "" {
		contract.WriteNotAccepted(w, message)
		return
	}
	state, _ := r.Context().Value(relayStateKey{}).(*relayState)
	if state == nil {
		// Read the body first: the server notices a caller that gave up only after
		// the body was consumed, and an abandoned request must not be delivered later.
		control := http.NewResponseController(w)
		_ = control.SetReadDeadline(time.Now().Add(x.bodyWait))
		body, err := io.ReadAll(io.LimitReader(r.Body, relayBodyLimit+1))
		if err != nil || len(body) > relayBodyLimit {
			// Keep the deadline: the server drains the unread body before it
			// answers, and a stalled caller must not block that.
			contract.WriteNotAccepted(w, message)
			return
		}
		_ = control.SetReadDeadline(time.Time{})
		r.Body = io.NopCloser(bytes.NewReader(body))
		cli, err := x.takeKey(r)
		if err != nil {
			contract.WriteNotAccepted(w, "認証情報を読み書きできません。")
			return
		}
		state = &relayState{deadline: time.Now().Add(x.hold), cli: cli}
		r = r.WithContext(context.WithValue(r.Context(), relayStateKey{}, state))
	}
	tick := time.NewTicker(x.interval)
	defer tick.Stop()
	expired := time.NewTimer(time.Until(state.deadline))
	defer expired.Stop()
	stop := x.stop
	var stopTimer *time.Timer
	var stopDeadline <-chan time.Time
	defer func() {
		if stopTimer != nil {
			stopTimer.Stop()
		}
	}()
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	for {
		if r.Context().Err() != nil {
			return
		}
		if ready() {
			if x.grant(w, r, state) {
				local.ServeHTTP(w, r)
			}
			return
		}
		if x.peerActive() {
			if x.grant(w, r, state) {
				x.deliver(w, r)
			}
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-stop:
			if !read {
				contract.WriteNotAccepted(w, message)
				return
			}
			stop = nil
			stopTimer = time.NewTimer(x.stopGrace)
			stopDeadline = stopTimer.C
		case <-stopDeadline:
			contract.WriteNotAccepted(w, message)
			return
		case <-expired.C:
			contract.WriteNotAccepted(w, message)
			return
		case <-tick.C:
		}
	}
}

// deliver sends r to the peer. Stopping this color must not wait for a
// long-lived request it only passes on: a read (including an event stream) and
// a long poll (a path ending in /next) get relayStopGrace, then reconnect. Any other write is left to finish
// within the shutdown deadline.
func (x *Relay) deliver(w http.ResponseWriter, r *http.Request) {
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !read && !strings.HasSuffix(r.URL.Path, "/next") {
		x.proxy.ServeHTTP(w, r)
		return
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	grace := x.stopGrace
	go func() {
		select {
		case <-x.stop:
		case <-ctx.Done():
			return
		}
		wait := time.NewTimer(grace)
		defer wait.Stop()
		select {
		case <-wait.C:
			cancel(errRelayStopped)
		case <-ctx.Done():
		}
	}()
	x.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// takeKey consumes a local one-time key now: it expires after a minute, which a
// handoff can outlast. grant replaces it with a ticket when the request is delivered.
func (x *Relay) takeKey(r *http.Request) (bool, error) {
	r.Header.Del(auth.RelayKeyHeader)
	key := r.Header.Get("X-Desk-Key")
	if x.keys == nil || !relayKeyPattern.MatchString(key) || !auth.LoopbackRequest(r, x.port) {
		return false, nil
	}
	return x.keys.ConsumeCLIKey(r.Context(), auth.Hash(key), time.Now())
}

func (x *Relay) grant(w http.ResponseWriter, r *http.Request, state *relayState) bool {
	if !state.cli || state.granted {
		return true
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		contract.WriteNotAccepted(w, "認証情報を読み書きできません。")
		return false
	}
	ticket := base64.RawURLEncoding.EncodeToString(raw)
	if err := x.keys.AddCLIKey(r.Context(), auth.RelayKeyHash(ticket), time.Now().Add(time.Minute)); err != nil {
		contract.WriteNotAccepted(w, "認証情報を読み書きできません。")
		return false
	}
	r.Header.Set(auth.RelayKeyHeader, ticket)
	state.granted = true
	return true
}

// peerActive reads the peer's /healthz. Waiting requests share one result per interval.
func (x *Relay) peerActive() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.checked.IsZero() && time.Since(x.checked) < x.interval {
		return x.active
	}
	x.active = x.probe()
	x.checked = time.Now()
	return x.active
}

// probe does not use a waiting request's context: one caller giving up must not
// fail the shared result. The client bounds it to two seconds.
func (x *Relay) probe() bool {
	request, err := http.NewRequest(http.MethodGet, x.peer.String()+"/healthz", nil)
	if err != nil {
		return false
	}
	request.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(x.port))
	response, err := x.client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	var health struct {
		OK   bool   `json:"ok"`
		Role string `json:"role"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&health) != nil {
		return false
	}
	return health.OK && health.Role == "active"
}

// lazyRelayKeys opens the application store only when a local one-time key has
// to cross the colors, so a standby color keeps no writer resources open.
type lazyRelayKeys struct {
	config *Config
	mu     sync.Mutex
	store  *store.SQLite
}

func (k *lazyRelayKeys) open(ctx context.Context) (*store.SQLite, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.store == nil {
		s, err := OpenPostgresApplicationStore(ctx, k.config, k.config.ApplicationSchema)
		if err != nil {
			return nil, err
		}
		k.store = s
	}
	return k.store, nil
}

func (k *lazyRelayKeys) ConsumeCLIKey(ctx context.Context, hash string, now time.Time) (bool, error) {
	s, err := k.open(ctx)
	if err != nil {
		return false, err
	}
	return s.ConsumeCLIKey(ctx, hash, now)
}

func (k *lazyRelayKeys) AddCLIKey(ctx context.Context, hash string, expires time.Time) error {
	s, err := k.open(ctx)
	if err != nil {
		return err
	}
	return s.AddCLIKey(ctx, hash, expires)
}

func (k *lazyRelayKeys) Close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.store != nil {
		k.store.Close()
		k.store = nil
	}
}
