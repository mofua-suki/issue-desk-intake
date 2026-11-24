package startup

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/httpapi"
	"issue-desk-intake/server/internal/leadership"
	"issue-desk-intake/server/internal/operations"
	"issue-desk-intake/server/internal/processactivity"
	"issue-desk-intake/server/internal/store"
)

type RunOptions struct {
	Handoff          <-chan struct{}
	Listener         net.Listener
	Config           *Config
	DSN              string
	Assets, Revision string
	// PeerURL is the other color (ISSUE_DESK_PEER_URL). When set, a request this
	// color cannot serve waits for a writer and is delivered there (Relay).
	PeerURL        string
	Log            func(string)
	prepareHandoff func(*Handoff)
	wrapBoundary   func(*HandoffHTTP, http.Handler) http.Handler
	relayKeys      relayKeys
}

// handoffHandler wraps an interface so it can be stored in an atomic.Pointer.
type handoffHandler struct{ http.Handler }

func newHandoffHandler(wrap func(http.Handler) http.Handler, next http.Handler) *handoffHandler {
	return &handoffHandler{wrap(next)}
}

// Run exposes deployment health while waiting for the exclusive writer lease.
// The shared DB is open while waiting; the application store, AI tasks and
// business timers start only after the lease is held.
func Run(ctx context.Context, o RunOptions) error {
	if o.Config == nil {
		return errors.New("Serverの設定が必要です")
	}
	if o.Log == nil {
		o.Log = func(string) {}
	}
	db, err := store.OpenPostgres(ctx, o.DSN)
	if err != nil {
		return errors.New("共有DBへ接続できません")
	}
	defer db.Close()
	if _, err = store.Migrate(ctx, db); err != nil {
		return errors.New("共有DBの版を確認できません")
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	defer cancel()
	var active atomic.Pointer[App]
	var boundary atomic.Pointer[handoffHandler]
	var boundaryHTTP atomic.Pointer[HandoffHTTP]
	keys := o.relayKeys
	if keys == nil && o.PeerURL != "" {
		lazy := &lazyRelayKeys{config: o.Config}
		defer lazy.Close()
		keys = lazy
	}
	relay, err := NewRelay(o.PeerURL, o.Config.Port, keys, ctx.Done())
	if err != nil {
		return err
	}
	serving := func() bool {
		app, h := active.Load(), boundaryHTTP.Load()
		return app != nil && app.ctx.Err() == nil && h != nil && !h.Stopped()
	}
	mismatches := &operations.ContractMismatches{}
	healthChecker := store.NewHealthChecker(o.DSN)
	defer healthChecker.Close()
	health := serverHealth(healthChecker, &active)
	api := contractGuardedAPI(o.Config, mismatches, &active)
	assets, err := httpapi.New(httpapi.Options{Assets: o.Assets, Revision: o.Revision, API: api, Health: func(c context.Context) (int, map[string]any) { return health(c, false) }, DeploymentHealth: func(c context.Context) (int, map[string]any) { return health(c, true) }})
	if err != nil {
		return err
	}
	defer assets.Close()
	var handler http.Handler
	handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if active.Load() == nil && r.URL.Path != "/healthz" {
			relay.Serve(w, r, serving, handler, "Serverは待機中です。")
			return
		}
		if h := boundary.Load(); h != nil && r.URL.Path != "/healthz" {
			h.ServeHTTP(w, r)
			return
		}
		assets.ServeHTTP(w, r)
	})
	listener := o.Listener
	if listener == nil {
		listener, err = net.Listen("tcp", o.Config.Address())
	}
	if err != nil {
		return errors.New("Serverの待受ポートを開けません")
	}
	defer listener.Close()
	server := &http.Server{Handler: processactivity.Process.Wrap(handler), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	httpDone := make(chan error, 1)
	go func() { httpDone <- server.Serve(listener) }()
	started := openWhenLeader(lifecycle, o, db, mismatches)
	var app *App
	var lease *leadership.Lease
	defer func() {
		cancel()
		if app != nil {
			app.Close()
		}
		if lease != nil {
			lease.Release()
		}
	}()
	stopHTTP := func(shutdown context.Context) {
		if server.Shutdown(shutdown) != nil {
			server.Close()
		}
	}
	var coordinator *Handoff
	select {
	case result := <-started:
		if result.err != nil {
			server.Close()
			return result.err
		}
		app, lease = result.app, result.lease
		h, e := NewHandoff(app, lease)
		if e != nil {
			server.Close()
			return e
		}
		h.HTTP.Unavailable = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			relay.Serve(w, r, serving, handler, "受付を交代しています。少し待って同じ要求を再送してください。")
		})
		boundaryHTTP.Store(h.HTTP)
		wrap := h.HTTP.Handler
		if o.wrapBoundary != nil {
			wrap = func(next http.Handler) http.Handler { return o.wrapBoundary(h.HTTP, next) }
		}
		boundary.Store(newHandoffHandler(wrap, assets))
		if o.prepareHandoff != nil {
			o.prepareHandoff(h)
		}
		coordinator = h
		active.Store(app)
		o.Log("Serverの業務APIを開始しました。")
	case <-o.Handoff:
		cancel()
		result := <-started
		app, lease = result.app, result.lease
		if app != nil {
			app.Close()
			app = nil
		}
		if lease != nil {
			lease.Release()
			lease = nil
		}
		o.Log("待機中の交代通知を受け、書き手の取得を停止しました。")
		select {
		case <-ctx.Done():
		case <-httpDone:
		}
		server.Close()
		return nil
	case <-ctx.Done():
		cancel()
		server.Close()
		result := <-started
		app, lease = result.app, result.lease
		return nil
	case <-httpDone:
		cancel()
		server.Close()
		result := <-started
		app, lease = result.app, result.lease
		return errors.New("HTTPの待受が終了しました")
	}
	handoffCtx, cancelHandoff := context.WithCancel(context.Background())
	defer cancelHandoff()
	handoffDone := make(chan error, 1)
	handoffSignal := o.Handoff
	for {
		select {
		case <-handoffSignal:
			handoffSignal = nil
			done := handoffDone
			go func() { done <- coordinator.Run(handoffCtx) }()
		case e := <-handoffDone:
			if e != nil {
				o.Log("交代に失敗しました: " + e.Error())
			} else {
				active.Store(nil)
				o.Log("交代結果を反映し、待機を継続します。")
			}
			// Remain alive until deployment stops or restarts this container.
			handoffDone = nil
		case <-ctx.Done():
			cancelHandoff()
			if handoffSignal == nil && handoffDone != nil {
				<-handoffDone
			}
			active.Store(nil)
			shutdownApp(app, stopHTTP)
			return nil
		case <-lease.Lost():
			cancelHandoff()
			if handoffSignal == nil && handoffDone != nil {
				<-handoffDone
			}
			active.Store(nil)
			server.Close()
			app.Close()
			return errors.New("書き手のロックを失ったためServerを停止しました")
		case <-httpDone:
			cancelHandoff()
			if handoffSignal == nil && handoffDone != nil {
				<-handoffDone
			}
			active.Store(nil)
			server.Close()
			return errors.New("HTTPの待受が終了しました")
		}
	}
}

// openedApp は書き手の取得とアプリの起動の結果。
type openedApp struct {
	app   *App
	lease *leadership.Lease
	err   error
}

// openWhenLeader は書き手のロックを待って取得し、アプリを起動して結果を1回だけ返す。起動に失敗したらロックを手放す。
func openWhenLeader(lifecycle context.Context, o RunOptions, db *sql.DB, mismatches *operations.ContractMismatches) <-chan openedApp {
	started := make(chan openedApp, 1)
	go func() {
		lease, e := leadership.Wait(lifecycle, o.DSN, leadership.Options{Log: o.Log})
		if e != nil {
			started <- openedApp{err: e}
			return
		}
		app, e := OpenApp(lease.Context(), o.Config, db, o.Revision, o.Log, mismatches)
		if e != nil {
			lease.Release()
			started <- openedApp{err: e}
			return
		}
		recordServerRevision(lease.Context(), db, o.Revision, o.Log)
		started <- openedApp{app: app, lease: lease}
	}()
	return started
}

// shutdownApp は新しいHTTPの受付を止め、受け付け済みのHTTPとAIの処理を同じ期限で待ってからアプリを閉じる。
func shutdownApp(app *App, stopHTTP func(context.Context)) {
	// Stop new HTTP work, then wait for accepted HTTP and AI using one deadline.
	// The application store remains open until both have completed or the grace expires.
	shutdown, stop := context.WithTimeout(context.Background(), 45*time.Second)
	defer stop()
	if app.Live != nil {
		app.Live.Close()
	}
	httpClosed := make(chan struct{})
	go func() { stopHTTP(shutdown); close(httpClosed) }()
	if app.Engine != nil {
		app.Engine.Shutdown(shutdown)
	}
	<-httpClosed
	app.Close()
}

// serverHealth は共有DBへの接続と書き手の状態から、稼働確認の応答を作る関数を返す。deployment は書き手の取得前でも配置の確認として成功にする。
func serverHealth(healthChecker *store.HealthChecker, active *atomic.Pointer[App]) func(context.Context, bool) (int, map[string]any) {
	return func(checkctx context.Context, deployment bool) (int, map[string]any) {
		ok := healthChecker.Check(checkctx) == nil
		role := "standby"
		ready := false
		if app := active.Load(); app != nil && app.ctx.Err() == nil {
			role = "active"
			ready = true
		}
		status := 503
		if ok && (ready || deployment) {
			status = 200
		}
		return status, map[string]any{"ok": ok, "ready": ok && ready, "role": role, "db": map[string]any{"ok": ok, "configured": true}, "web": map[string]any{"ok": true}}
	}
}

// contractGuardedAPI は取り決めの版が違う要求を断って記録し、書き手を取得した後だけ業務APIへ渡す入口を返す。
func contractGuardedAPI(config *Config, mismatches *operations.ContractMismatches, active *atomic.Pointer[App]) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if supplied := r.Header.Get(contract.Header); supplied != "" && supplied != contract.Version {
			updating := operations.ContractUpdating(config.RuntimeDir, time.Now())
			mismatches.Record(supplied, updating, time.Now())
			if updating {
				w.Header().Set("x-issue-desk-updating", "1")
			}
			contract.WriteProblem(w, contract.NewProblem(409, "APIの取り決めの版が一致しません。", "CONTRACT_MISMATCH"))
			return
		}
		app := active.Load()
		if app == nil || app.ctx.Err() != nil {
			contract.WriteNotAccepted(w, "Serverは書き手の取得を待っています。")
			return
		}
		app.Mux.ServeHTTP(w, r)
	})
}
