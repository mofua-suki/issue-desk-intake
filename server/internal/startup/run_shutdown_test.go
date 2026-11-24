package startup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/leadership"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This submission recovers a marker already present on Gitea. It exercises the
// real authenticated filing HTTP route and application PostgreSQL completion
// and outbox, without making an AI call or posting a duplicate issue.
func TestRunShutdownKeepsApplicationStoreOpenUntilAcceptedFilingHTTPIsSaved(t *testing.T) {
	testRunAcceptedFiling(t, false, false, false, "")
}

func TestRunHandoffKeepsAcceptedFilingAndStaysStandby(t *testing.T) {
	testRunAcceptedFiling(t, true, false, false, "")
}

func TestRunHandoffThenShutdownKeepsAcceptedFiling(t *testing.T) {
	testRunAcceptedFiling(t, true, true, false, "")
}

func TestRunHandoffStopsOnLostLease(t *testing.T) {
	testRunAcceptedFiling(t, true, false, true, "")
}

func TestRunHandoffFailureKeepsLeaseAndStops(t *testing.T) {
	for _, mode := range []string{"failure", "deadline"} {
		t.Run(mode, func(t *testing.T) { testRunAcceptedFiling(t, true, false, false, mode) })
	}
}

func testRunAcceptedFiling(t *testing.T, handoff, stopDuring, loseLease bool, deployMode string) {
	pg := appPostgres(t)
	ctx, deadline := context.WithTimeout(t.Context(), 40*time.Second)
	defer deadline()
	serial, e := pg.Conn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer serial.Close()
	if _, e = serial.ExecContext(ctx, "SELECT pg_advisory_lock(2026110145)"); e != nil {
		t.Fatal(e)
	}
	defer serial.ExecContext(context.Background(), "SELECT pg_advisory_unlock(2026110145)")
	var schema string
	if e = pg.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); e != nil {
		t.Fatal(e)
	}
	dsn, e := url.Parse(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if e != nil {
		t.Fatal(e)
	}
	query := dsn.Query()
	query.Set("search_path", schema)
	dsn.RawQuery = query.Encode()
	id := "55555555-5555-5555-5555-555555555555"
	issue := map[string]any{"number": 9, "title": "起票の結果", "state": "open", "body": "確認済みの本文\n<!-- issue-desk:" + id + " -->", "user": map[string]any{"id": 1, "login": "intake"}, "created_at": "2026-11-01T00:00:00.000Z"}
	var armed atomic.Bool
	entered, released := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseBlocked := func() { releaseOnce.Do(func() { close(released) }) }
	initialList := make(chan struct{}, 1)
	var once sync.Once
	var writes atomic.Int32
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" {
			writes.Add(1)
			w.WriteHeader(500)
			return
		}
		switch r.URL.Path {
		case "/api/v1/user":
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "login": "intake"})
		case "/api/v1/users/owner":
			json.NewEncoder(w).Encode(map[string]any{"id": 2, "login": "owner"})
		case "/api/v1/repos/owner/repo/issues":
			if armed.Load() {
				once.Do(func() { close(entered) })
				select {
				case <-released:
				case <-r.Context().Done():
					return
				}
				json.NewEncoder(w).Encode([]any{issue})
			} else {
				select {
				case initialList <- struct{}{}:
				default:
				}
				json.NewEncoder(w).Encode([]any{})
			}
		case "/api/v1/repos/owner/repo/issues/9":
			json.NewEncoder(w).Encode(issue)
		default:
			w.WriteHeader(404)
		}
	}))
	defer func() { releaseBlocked(); git.Close() }()
	dir := t.TempDir()
	private := filepath.Join(dir, "private")
	if e = os.MkdirAll(private, 0700); e != nil {
		t.Fatal(e)
	}
	token, admin := filepath.Join(private, "intake-token"), filepath.Join(private, "admin-token")
	for _, path := range []string{token, admin} {
		if e = os.WriteFile(path, []byte("fixture-token"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	base := "http://" + listener.Addr().String()
	file := filepath.Join(dir, "config.local.json")
	writeConfig(t, file, map[string]any{"workerId": "nas", "giteaUrl": git.URL, "runtimeDir": "runtime", "tokenFile": admin, "intakeTokenFile": token, "intakeAccount": "intake", "gitUser": "owner", "codex": "/missing-fixture-codex", "repositories": []any{map[string]any{"name": "owner/repo"}}, "intake": map[string]any{"publicUrl": base, "port": port, "aiConcurrency": 1}})
	writeConfig(t, filepath.Join(dir, "fleet.json"), map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"nas": map[string]any{"repositories": []any{map[string]any{"name": "owner/repo"}}}}})
	writeConfig(t, filepath.Join(dir, "assignments.json"), map[string]any{"pollSeconds": 300, "hosts": map[string]any{"nas": map[string]any{"maxImplementations": 1, "repositories": map[string]any{"owner/repo": map[string]any{"rank": 1}}}}, "repositories": map[string]any{}})
	stageApplicationAssets(t, dir)
	config, e := Load(file, dir, "")
	if e != nil {
		t.Fatal(e)
	}
	env := useApplicationSchema(t, config)
	local := openAppStore(t, env)
	records, e := consultations.OpenRecords(ctx, local.DB())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = records.Change(ctx, "drafts", id, func(*sql.Tx, consultations.Record) (consultations.Record, error) {
		return consultations.Record{"id": id, "project": "owner/repo", "hold": true, "status": "draft", "ready": true, "revision": 0, "questions": []any{}, "messages": []any{}, "preparedJob": map[string]any{"Scope": []any{"a.go"}}}, nil
	}); e != nil {
		t.Fatal(e)
	}
	if e = local.CreateSession(ctx, auth.Hash("review-session"), auth.Session{CSRF: "review-csrf", ExpiresAt: time.Now().Add(time.Hour)}, time.Now()); e != nil {
		t.Fatal(e)
	}
	local.Close()
	assets := filepath.Join(dir, "assets")
	ui := strings.Repeat("a", 64)
	if e = os.MkdirAll(filepath.Join(assets, "v", ui), 0700); e != nil {
		t.Fatal(e)
	}
	for name, body := range map[string]string{"ui-version.json": `{"ui":"` + ui + `"}`, "index.html": "<!doctype html>", "sw.js": "// service worker", "v/" + ui + "/app.js": "// client"} {
		if e = os.WriteFile(filepath.Join(assets, name), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	running, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	notifications := make(chan struct{}, 4)
	logs := make(chan string, 100)
	allowHandoff := make(chan struct{})
	var unblockOnce sync.Once
	unblockHandoff := func() { unblockOnce.Do(func() { close(allowHandoff) }) }
	defer unblockHandoff()
	var handoffHTTP atomic.Pointer[HandoffHTTP]
	var wraps atomic.Int32
	go func() {
		done <- Run(running, RunOptions{wrapBoundary: func(h *HandoffHTTP, next http.Handler) http.Handler { wraps.Add(1); return h.Handler(next) }, prepareHandoff: func(h *Handoff) {
			handoffHTTP.Store(h.HTTP)
			if deployMode == "failure" || deployMode == "deadline" {
				h.stage2 = append(h.stage2, handoffStep{"injected", func(context.Context) error {
					if deployMode == "deadline" {
						return context.DeadlineExceeded
					}
					return errors.New("fixture")
				}})
			}
		}, Handoff: notifications, Log: func(s string) {
			select {
			case logs <- s:
			default:
			}
			if loseLease && strings.Contains(s, "handoff stage=stage1 result=start") {
				<-allowHandoff
			}
		}, Listener: listener, Config: config, DSN: dsn.String(), Assets: assets, Revision: strings.Repeat("b", 40)})
	}()
	client := &http.Client{Timeout: 5 * time.Second}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		response, err := client.Get(base + "/healthz")
		active := false
		if err == nil {
			var value map[string]any
			json.NewDecoder(response.Body).Decode(&value)
			response.Body.Close()
			active = value["role"] == "active"
		}
		if active {
			break
		}
		select {
		case e := <-done:
			t.Fatal("起動できない", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
	select {
	case <-initialList:
	case <-ctx.Done():
		t.Fatal("起動時の一覧照合なし")
	}
	armed.Store(true)
	request, e := http.NewRequestWithContext(ctx, "POST", base+"/api/drafts/"+id+"/submit", strings.NewReader(`{"revision":0}`))
	if e != nil {
		t.Fatal(e)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", base)
	request.Header.Set("X-CSRF-Token", "review-csrf")
	request.AddCookie(&http.Cookie{Name: "desk_session", Value: "review-session"})
	type responseResult struct {
		code int
		body string
		err  error
	}
	responseDone := make(chan responseResult, 1)
	go func() {
		r, e := client.Do(request)
		if e != nil {
			responseDone <- responseResult{err: e}
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		responseDone <- responseResult{code: r.StatusCode, body: string(body)}
	}()
	var accepted responseResult
	select {
	case accepted = <-responseDone:
		if accepted.err != nil || accepted.code != 202 {
			t.Fatal("起票依頼の受理", accepted)
		}
		var receipt map[string]any
		if json.Unmarshal([]byte(accepted.body), &receipt) != nil || receipt["accepted"] != true || receipt["id"] != id {
			t.Fatal("受理の識別子", accepted)
		}
	case <-ctx.Done():
		t.Fatal("受理応答がない", ctx.Err())
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("非同期の起票照合に到達しない", ctx.Err())
	}
	// The blocked Gitea read may have been initiated by startup numbering and
	// shared with filing. Do not mistake that background read for an accepted
	// HTTP submission: wait until the filing handler persists its intent.
	observer := openAppStore(t, env)
	observed, e := consultations.OpenRecords(ctx, observer.DB())
	if e != nil {
		t.Fatal(e)
	}
	for {
		draft, found, err := observed.Get(ctx, "drafts", id)
		if err != nil {
			t.Fatal(err)
		}
		if found && draft["status"] == "submitting" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("起票の受理を確認できない", ctx.Err())
		case <-tick.C:
		}
	}
	if e = observer.Close(); e != nil {
		t.Fatal(e)
	}
	var partial *io.PipeWriter
	var partialDone chan int
	if handoff && !stopDuring && !loseLease {
		reader, writer := io.Pipe()
		partial = writer
		defer writer.Close()
		partialDone = make(chan int, 1)
		req, err := http.NewRequestWithContext(ctx, "POST", base+"/api/drafts/"+id+"/submit", reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", base)
		req.Header.Set("X-CSRF-Token", "review-csrf")
		req.AddCookie(&http.Cookie{Name: "desk_session", Value: "review-session"})
		go func() {
			response, err := client.Do(req)
			if err != nil {
				partialDone <- 0
				return
			}
			response.Body.Close()
			partialDone <- response.StatusCode
		}()
		if _, err = partial.Write([]byte(`{"revision":`)); err != nil {
			t.Fatal(err)
		}
		// Observe registration at the actual HTTP boundary before proceeding.
		for {
			h := handoffHTTP.Load()
			h.mu.Lock()
			count := h.active
			h.mu.Unlock()
			if count >= 1 {
				break
			}
			select {
			case <-tick.C:
			case <-ctx.Done():
				t.Fatal("要求が境界に登録されない", ctx.Err())
			}
		}
		select {
		case code := <-partialDone:
			t.Fatal("受理済み要求が早く終了", code)
		default:
		}
	}
	if wraps.Load() != 1 {
		t.Fatal("境界の生成回数", wraps.Load())
	}
	deploymentMode := deployMode
	if deployMode == "failure" || deployMode == "deadline" {
		deploymentMode = ""
	}
	deployment := startRunDeploy(t, ctx, deploymentMode)
	if handoff {
		notifications <- struct{}{}
		select {
		case line := <-logs:
			for !strings.Contains(line, "handoff stage=stage1 result=start") {
				select {
				case line = <-logs:
				case <-ctx.Done():
					t.Fatal("段1に入らない")
				}
			}
		case <-ctx.Done():
			t.Fatal("段1に入らない")
		}
		notifications <- struct{}{}
		response, err := client.Get(base + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		json.NewDecoder(response.Body).Decode(&value)
		response.Body.Close()
		if response.StatusCode != 200 || value["role"] != "active" {
			t.Fatal("段1のhealth", response.StatusCode, value)
		}
		response, err = client.Get(base + "/")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("段1の画面応答", response.StatusCode)
		}
		sessionRequest, err := http.NewRequestWithContext(ctx, "GET", base+"/api/session", nil)
		if err != nil {
			t.Fatal(err)
		}
		sessionRequest.AddCookie(&http.Cookie{Name: "desk_session", Value: "review-session"})
		response, err = client.Do(sessionRequest)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("段1の業務API応答", response.StatusCode)
		}
		if loseLease {
			var killed bool
			if err := pg.QueryRowContext(ctx, "SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND granted AND classid=0 AND objid=$1 AND objsubid=1", leadership.Key).Scan(&killed); err != nil || !killed {
				t.Fatal("書き手接続を切断できない", killed, err)
			}
			// The watcher runs every ten seconds. The lock loss may close the
			// listener immediately, while accepted work is still unwinding.
			for {
				response, err := client.Get(base + "/healthz")
				if err != nil {
					break
				}
				var health map[string]any
				json.NewDecoder(response.Body).Decode(&health)
				response.Body.Close()
				if health["role"] != "active" {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("ロック喪失を検知しない")
				case <-time.After(50 * time.Millisecond):
				}
			}
			unblockHandoff()
			releaseBlocked()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "ロックを失った") {
					t.Fatal("ロック喪失を報告しない", err)
				}
			case <-ctx.Done():
				t.Fatal("ロック喪失で終了しない", ctx.Err())
			}
			return
		}
		if stopDuring {
			stop()
			select {
			case err := <-done:
				t.Fatal("受理済み処理より先に終了", err)
			case <-time.After(100 * time.Millisecond):
			}
		}
	} else {
		stop()
		// Shutdown closes the listener before waiting for accepted requests. Wait for
		// that observable boundary, then allow the already accepted filing to finish.
		for {
			connection, e := net.DialTimeout("tcp", listener.Addr().String(), 10*time.Millisecond)
			if e != nil {
				break
			}
			connection.Close()
			select {
			case <-tick.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}

	releaseBlocked()
	if deployMode == "failure" || deployMode == "deadline" {
		for {
			select {
			case line := <-logs:
				if strings.Contains(line, "交代に失敗しました:") {
					class := "operation_failed"
					if deployMode == "deadline" {
						class = "timeout"
					}
					if !strings.Contains(line, "class="+class) {
						t.Fatal(line)
					}
					goto failed
				}
			case err := <-done:
				t.Fatal("失敗後に終了", err)
			case <-ctx.Done():
				t.Fatal("失敗ログなし", ctx.Err())
			}
		}
	failed:
		// The injected failure and HTTP boundary run concurrently. Observe the
		// boundary itself before checking the response to new requests.
		for {
			h := handoffHTTP.Load()
			h.mu.Lock()
			stopped := h.stopped
			h.mu.Unlock()
			if stopped {
				break
			}
			select {
			case <-tick.C:
			case <-ctx.Done():
				t.Fatal("HTTP境界が停止しない", ctx.Err())
			}
		}
		if wraps.Load() != 1 {
			t.Fatal("要求ごとに境界を生成", wraps.Load())
		}
		response, err := client.Get(base + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		var health map[string]any
		json.NewDecoder(response.Body).Decode(&health)
		response.Body.Close()
		if health["role"] != "active" {
			t.Fatal(health)
		}
		response, err = client.Get(base + "/api/meta")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 503 {
			t.Fatal(response.StatusCode)
		}
		probe, acquired, err := leadership.Try(ctx, dsn.String(), leadership.Options{})
		if probe != nil {
			probe.Release()
		}
		if err != nil || acquired {
			t.Fatal("失敗後のリース", acquired, err)
		}
		select {
		case err := <-done:
			t.Fatal("失敗後に終了", err)
		default:
		}
		partial.Close()
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return
	}
	if partial != nil {
		for {
			response, err := client.Get(base + "/api/meta")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode == 503 {
				if response.Header.Get("Retry-After") != "2" {
					t.Fatal("段2のRetry-After", response.Header)
				}
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("段2へ進まない")
			case <-time.After(10 * time.Millisecond):
			}
		}
		select {
		case code := <-partialDone:
			t.Fatal("段2で受理済み要求を待たない", code)
		default:
		}
		probe, acquired, err := leadership.Try(ctx, dsn.String(), leadership.Options{})
		if err != nil || acquired {
			if probe != nil {
				probe.Release()
			}
			t.Fatal("受理済みHTTPの終了前にロックを返却", acquired, err)
		}
		if _, err := partial.Write([]byte(`999}`)); err != nil {
			t.Fatal(err)
		}
		partial.Close()
		select {
		case code := <-partialDone:
			if code == 0 {
				t.Fatal("受理済み要求が失敗")
			}
		case <-ctx.Done():
			t.Fatal("受理済み要求が終わらない")
		}
	}
	if handoff && !stopDuring {
		for {
			select {
			case line := <-logs:
				if strings.Contains(line, "handoff stage=stage2 result=complete") {
					goto completed
				}
			case err := <-done:
				t.Fatal("交代後にプロセスが終了", err)
			case <-ctx.Done():
				t.Fatal("交代が完了しない", ctx.Err())
			}
		}
	completed:
		deployment.finish(t, ctx)
		response, err := client.Get(base + "/api/meta")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 503 || response.Header.Get("Retry-After") != "2" {
			t.Fatal("段2の応答", response.StatusCode, response.Header)
		}
		// Once released, another writer can acquire the real lease. Repeated
		// signals must not let the old process compete for it afterwards.
		other, ok, err := leadership.Try(ctx, dsn.String(), leadership.Options{})
		if err != nil || !ok {
			t.Fatal("ロックを返却しない", ok, err)
		}
		other.Release()
		notifications <- struct{}{}
		// Stage completion is logged before Run consumes the result. Wait for
		// Run to acknowledge that result before probing lease reacquisition.
		for {
			select {
			case line := <-logs:
				if strings.Contains(line, "交代結果を反映し、待機を継続します。") {
					goto acknowledged
				}
			case err := <-done:
				t.Fatal("交代後に終了", err)
			case <-ctx.Done():
				t.Fatal("交代結果が反映されない", ctx.Err())
			}
		}
	acknowledged:
		select {
		case err := <-done:
			t.Fatal("交代後に終了", err)
		default:
		}
		other, ok, err = leadership.Try(ctx, dsn.String(), leadership.Options{})
		if err != nil || !ok {
			t.Fatal("旧色がロックを再取得", ok, err)
		}
		other.Release()
		stop()
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if wraps.Load() != 1 {
		t.Fatal("要求ごとに境界を生成", wraps.Load())
	}
	saved := openAppStore(t, env)
	restored, e := consultations.OpenRecords(context.Background(), saved.DB())
	if e != nil {
		t.Fatal(e)
	}
	draft, ok, e := restored.Get(context.Background(), "drafts", id)
	if e != nil || !ok || draft["status"] != "submitted" {
		t.Fatal(draft, e)
	}
	var count int
	if e = pg.QueryRowContext(context.Background(), "SELECT count(*)FROM jobs WHERE repo='owner/repo'AND issue=9").Scan(&count); e != nil || count != 1 {
		t.Fatal("起票後の配布保存なし", count, e)
	}
	if writes.Load() != 0 {
		t.Fatal("同じ起票を再投稿", writes.Load())
	}
}
