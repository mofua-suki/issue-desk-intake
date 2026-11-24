package startup

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/leadership"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunStandbyServesActualHealthWithoutOpeningWriterResources(t *testing.T) {
	testRunStandby(t, false, false)
}

// With the other color configured, a standby color delivers business requests
// to the writer instead of failing them, still without opening writer resources.
func TestRunStandbyDeliversBusinessRequestsToWriterColor(t *testing.T) {
	testRunStandby(t, false, true)
}

func TestRunStandbyHandoffDoesNotAcquireWriter(t *testing.T) {
	testRunStandby(t, true, false)
}

func testRunStandby(t *testing.T, handoff, relayed bool) {
	db := appPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	serial, e := db.Conn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer serial.Close()
	if _, e = serial.ExecContext(ctx, "SELECT pg_advisory_lock(2026110145)"); e != nil {
		t.Fatal(e)
	}
	defer serial.ExecContext(context.Background(), "SELECT pg_advisory_unlock(2026110145)")
	held, e := db.Conn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer held.Close()
	if _, e = held.ExecContext(ctx, "SELECT pg_advisory_lock($1)", leadership.Key); e != nil {
		t.Fatal(e)
	}
	defer held.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", leadership.Key)
	var schema string
	if e = db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if e != nil {
		t.Fatal(e)
	}
	params := u.Query()
	params.Set("search_path", schema)
	params.Set("application_name", schema)
	u.RawQuery = params.Encode()
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, "runtime")
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
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	running, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	notifications := make(chan struct{}, 2)
	stoppedWaiting := make(chan struct{}, 1)
	peerURL := ""
	var peer *relayPeer
	if relayed {
		peer = newRelayPeer(t)
		peer.active.Store(true)
		peerURL = peer.server.URL
	}
	go func() {
		done <- Run(running, RunOptions{PeerURL: peerURL, relayKeys: &relayTestKeys{keys: map[string]time.Time{}}, Handoff: notifications, Log: func(line string) {
			if strings.Contains(line, "待機中の交代通知") {
				stoppedWaiting <- struct{}{}
			}
		}, Listener: listener, Config: &Config{RuntimeDir: runtimeDir, SQLiteFile: filepath.Join(runtimeDir, "intake.sqlite"), LedgerFile: filepath.Join(runtimeDir, "intake-desk-numbers.jsonl")}, DSN: u.String(), Assets: assets, Revision: strings.Repeat("b", 40)})
	}()
	client := &http.Client{Timeout: time.Second}
	base := "http://" + listener.Addr().String()
	var health map[string]any
	retry := time.NewTicker(10 * time.Millisecond)
	defer retry.Stop()
	for {
		response, err := client.Get(base + "/healthz")
		if err == nil {
			e = json.NewDecoder(response.Body).Decode(&health)
			response.Body.Close()
			if e != nil || response.StatusCode != 200 {
				t.Fatal(response.StatusCode, health, e)
			}
			break
		}
		select {
		case e := <-done:
			t.Fatal("待機前にServer終了", e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-retry.C:
		}
	}
	if health["db"].(map[string]any)["ok"] != true || health["db"].(map[string]any)["configured"] != true || health["web"].(map[string]any)["ok"] != true || health["ui"] != ui {
		t.Fatal(health)
	}
	if health["role"] != "standby" || health["ready"] != false || health["ok"] != true || health["revision"] != strings.Repeat("b", 40) {
		t.Fatal(health)
	}
	if !handoff {
		if _, err := db.ExecContext(ctx, "ALTER TABLE schema_migrations RENAME TO schema_migrations_offline"); err != nil {
			t.Fatal(err)
		}
		waitHealth := func(want int, ok bool) {
			t.Helper()
			for {
				response, err := client.Get(base + "/healthz")
				if err != nil {
					t.Fatal(err)
				}
				var body map[string]any
				err = json.NewDecoder(response.Body).Decode(&body)
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode == want {
					if body["ok"] != ok || body["ready"] != false || body["role"] != "standby" || body["db"].(map[string]any)["ok"] != ok || body["web"].(map[string]any)["ok"] != true || body["ui"] != ui || body["revision"] != strings.Repeat("b", 40) {
						t.Fatal(body)
					}
					return
				}
				select {
				case <-ctx.Done():
					t.Fatal("health did not change", ctx.Err())
				case <-retry.C:
				}
			}
		}
		waitHealth(http.StatusServiceUnavailable, false)
		if _, err := db.ExecContext(ctx, "ALTER TABLE schema_migrations_offline RENAME TO schema_migrations"); err != nil {
			t.Fatal(err)
		}
		waitHealth(http.StatusOK, true)
	}
	for _, path := range []string{"/", "/api/meta", "/api/health"} {
		response, e := client.Get(base + path)
		if e != nil {
			t.Fatal(e)
		}
		response.Body.Close()
		if relayed {
			if response.StatusCode != 201 || response.Header.Get("X-Writer") != "peer" {
				t.Fatal("待機色が書き手の色へ届けていない", path, response.StatusCode)
			}
			continue
		}
		if response.StatusCode != 503 || response.Header.Get("X-Issue-Desk-Not-Accepted") != "1" {
			t.Fatal("待機中に業務を公開", path, response.StatusCode)
		}
	}
	if relayed {
		peer.mu.Lock()
		delivered := len(peer.seen)
		peer.mu.Unlock()
		if delivered != 3 {
			t.Fatal("届けた回数が要求の数と一致しない", delivered)
		}
	}
	if _, e = os.Stat(runtimeDir); !os.IsNotExist(e) {
		t.Fatal("待機中にSQLite/ゲートの保存先を作成", e)
	}
	if handoff {
		notifications <- struct{}{}
		notifications <- struct{}{}
		// Wait for the handoff branch to cancel the waiter before unlocking.
		// The Run remains alive and must not acquire the lease after it is freed.
		select {
		case <-stoppedWaiting:
		case <-ctx.Done():
			t.Fatal("待機取消が完了しない")
		}
		if _, err := held.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", leadership.Key); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2200 * time.Millisecond)
		var got bool
		if err := held.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", leadership.Key).Scan(&got); err != nil || !got {
			t.Fatal("待機中の通知後に書き手を取得した", got, err)
		}
		if _, err := held.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", leadership.Key); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatal("通知だけで終了", err)
		default:
		}
	}
	stop()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("待機の取消が終了しない")
	}
	if _, e = os.Stat(runtimeDir); !os.IsNotExist(e) {
		t.Fatal("取消時に書き手資源を作成", e)
	}
	// Closing the client socket does not wait for PostgreSQL to remove the
	// backend from pg_stat_activity. Keep checking for actual removal.
	var connections int
	for {
		if e = db.QueryRowContext(ctx, "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1", schema).Scan(&connections); e != nil {
			t.Fatalf("停止後の接続: %d %v", connections, e)
		}
		if connections == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("停止後の接続: %d %v", connections, ctx.Err())
		case <-retry.C:
		}
	}
}
