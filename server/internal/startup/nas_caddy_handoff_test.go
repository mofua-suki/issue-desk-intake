package startup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNASCaddyHandoff(t *testing.T) {
	caddy := os.Getenv("ISSUE_DESK_TEST_CADDY")
	if caddy == "" {
		if _, err := os.Stat("/opt/agent-tools/bin/caddy"); err == nil {
			caddy = "/opt/agent-tools/bin/caddy"
		} else {
			caddy = "caddy"
		}
	}
	version, err := exec.Command(caddy, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("Caddy version: %v: %s", err, version)
	}
	t.Logf("Caddy version: %s", strings.TrimSpace(string(version)))
	var owner atomic.Int32
	var posts atomic.Int32
	var forwarded atomic.Int32
	var healthChecks [2]atomic.Int32
	var disableBlueHealth atomic.Bool
	entered := make(chan struct{}, 10)
	relayEntered := make(chan struct{}, 2)
	relayRelease := make(chan struct{})
	relayStop := make(chan struct{})
	var oldRelay *Relay
	owner.Store(1)
	servers := make([]*httptest.Server, 2)
	for i := range servers {
		color := int32(i + 1)
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				healthChecks[color-1].Add(1)
				if color == 1 && disableBlueHealth.Load() {
					http.Error(w, "unhealthy", http.StatusServiceUnavailable)
					return
				}
				role := "standby"
				if owner.Load() == color {
					role = "active"
				}
				revision := strings.Repeat("a", 40)
				if color == 2 {
					revision = strings.Repeat("b", 40)
				}
				fmt.Fprintf(w, `{"ok":true,"role":"%s","revision":"%s"}`, role, revision)
				return
			}
			if r.Method == http.MethodPost {
				posts.Add(1)
				conn, _, e := w.(http.Hijacker).Hijack()
				if e == nil {
					conn.Close()
				}
				return
			}
			if color == 1 && owner.Load() == 0 {
				entered <- struct{}{}
				oldRelay.Serve(w, r, never, http.NotFoundHandler(), "交代中")
				return
			}
			if color == 2 && r.Header.Get(relayedHeader) != "" {
				forwarded.Add(1)
				relayEntered <- struct{}{}
				select {
				case <-relayRelease:
				case <-r.Context().Done():
					return
				}
			}
			// Model the deployed server's hold/relay behavior while no writer exists.
			if owner.Load() == 0 {
				entered <- struct{}{}
			}
			for owner.Load() == 0 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
			writer := owner.Load()
			if writer != color {
				forwarded.Add(1)
				req, e := http.NewRequestWithContext(r.Context(), r.Method, servers[writer-1].URL+r.URL.RequestURI(), nil)
				if e != nil {
					http.Error(w, e.Error(), http.StatusBadGateway)
					return
				}
				resp, e := http.DefaultClient.Do(req)
				if e != nil {
					http.Error(w, e.Error(), http.StatusBadGateway)
					return
				}
				defer resp.Body.Close()
				w.WriteHeader(resp.StatusCode)
				io.Copy(w, resp.Body)
				return
			}
			fmt.Fprintf(w, "%s writer=%d", r.URL.Path, writer)
		}))
		defer servers[i].Close()
	}
	oldRelay = testRelay(t, servers[1].URL, nil, relayStop)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	admin := adminListener.Addr().String()
	adminListener.Close()
	root := t.TempDir()
	active := filepath.Join(root, "active.caddy")
	config := filepath.Join(root, "Caddyfile")
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "nas", "entry", "Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "admin localhost:2019", "admin "+admin)
	text = strings.ReplaceAll(text, ":8787 {", "http://"+address+" {")
	text = strings.ReplaceAll(text, "/entry/active.caddy", active)
	oldText := strings.Replace(text, "health_uri /healthz", "health_body active\n\thealth_uri /healthz", 1)
	if err = os.WriteFile(config, []byte(oldText), 0600); err != nil {
		t.Fatal(err)
	}
	block := fmt.Sprintf("reverse_proxy %s {\n to %s\n import intake_retry\n}\n", strings.TrimPrefix(servers[0].URL, "http://"), strings.TrimPrefix(servers[1].URL, "http://"))
	if err = os.WriteFile(active, []byte(block), 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	cmd := exec.Command(caddy, "run", "--config", config, "--adapter", "caddyfile")
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(path, want string) error {
		resp, e := client.Get("http://" + address + path)
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		b, e := io.ReadAll(resp.Body)
		if e != nil || resp.StatusCode != 200 || string(b) != want {
			return fmt.Errorf("%s: status=%d body=%q err=%v", path, resp.StatusCode, b, e)
		}
		return nil
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err = get("/", "/ writer=1"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Caddy startup: %v: %s", err, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The actual deployment script operates on the two synthetic colors. Its
	// Docker adapter applies its generated entry file to this Caddy instance.
	release, state, deployDone := startCaddyDeploy(t, caddy, config, active, admin, address, servers, "native")
	if raw, e := os.ReadFile(filepath.Join(state, "blue")); e != nil || strings.TrimSpace(string(raw)) != "true" {
		t.Fatalf("old writer stopped before handoff: %q %v", raw, e)
	}
	// Before the writer yields, make the old upstream unhealthy to prove the
	// standby is eligible and can serve the public path through its relay.
	disableBlueHealth.Store(true)
	deadline = time.Now().Add(10 * time.Second)
	for {
		before := forwarded.Load()
		err = get("/candidate", "/candidate writer=1")
		if err == nil && forwarded.Load() > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("standby was not eligible: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	disableBlueHealth.Store(false)
	forwarded.Store(0)
	owner.Store(0)
	checks := [2]int32{healthChecks[0].Load(), healthChecks[1].Load()}
	healthDeadline := time.NewTimer(10 * time.Second)
	defer healthDeadline.Stop()
	for healthChecks[0].Load() <= checks[0] || healthChecks[1].Load() <= checks[1] {
		select {
		case result := <-deployDone:
			t.Fatalf("deployment stopped while waiting for Caddy health checks: %v: %s", result.err, result.output)
		case <-healthDeadline.C:
			t.Fatal("Caddy did not health-check both upstreams after reload")
		case <-time.After(20 * time.Millisecond):
		}
	}
	results := make(chan error, 2)
	for _, path := range []string{"/", "/api/projects"} {
		go func(p string) { results <- get(p, p+" writer=2") }(path)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Error("request did not reach either standby server during handoff")
		}
	}
	owner.Store(2)
	for range 2 {
		select {
		case <-relayEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("requests did not reach new writer through old relay")
		}
	}
	close(relayStop)
	close(relayRelease)
	if err := os.WriteFile(release, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = <-results; err != nil {
			t.Error(err)
		}
	}
	if forwarded.Load() != 2 {
		t.Errorf("handoff forwarded %d requests, want 2", forwarded.Load())
	}
	switchTo := func(first, second int) {
		block := fmt.Sprintf("reverse_proxy %s {\n to %s\n import intake_retry\n}\n", strings.TrimPrefix(servers[first].URL, "http://"), strings.TrimPrefix(servers[second].URL, "http://"))
		if e := os.WriteFile(active, []byte(block), 0600); e != nil {
			t.Fatal(e)
		}
		output, e := exec.Command(caddy, "reload", "--config", config, "--adapter", "caddyfile", "--address", admin).CombinedOutput()
		if e != nil {
			t.Fatalf("reload: %v: %s", e, output)
		}
	}
	select {
	case result := <-deployDone:
		if result.err != nil || !strings.Contains(result.output, "[server:success]") {
			t.Fatalf("deployment: %v: %s", result.err, result.output)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("deployment did not finish")
	}
	if err = get("/healthz", `{"ok":true,"role":"active","revision":"`+strings.Repeat("b", 40)+`"}`); err != nil {
		t.Error("new writer confirmation:", err)
	}
	owner.Store(1)
	switchTo(0, 1)
	// A failed takeover runs the production rollback, including the same entry
	// write/reload and an actual health check through Caddy.
	release, state, rollbackDone := startCaddyDeploy(t, caddy, config, active, admin, address, servers, "rollback")
	owner.Store(0)
	if err := os.WriteFile(release, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	owner.Store(2)
	seenGreen := false
	rollbackDeadline := time.NewTimer(45 * time.Second)
	defer rollbackDeadline.Stop()
	for {
		raw, _ := os.ReadFile(filepath.Join(state, "owner"))
		value := strings.TrimSpace(string(raw))
		if value == "green" {
			seenGreen = true
		}
		if seenGreen && value == "blue" {
			owner.Store(1)
			break
		}
		select {
		case result := <-rollbackDone:
			t.Fatalf("rollback ended early: %v: %s", result.err, result.output)
		case <-rollbackDeadline.C:
			t.Fatal("old writer was not restored")
		case <-time.After(20 * time.Millisecond):
		}
	}
	select {
	case result := <-rollbackDone:
		if result.err == nil || !strings.Contains(result.output, "[server:switched-back:takeover]") {
			t.Fatalf("rollback: %v: %s", result.err, result.output)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("rollback did not finish")
	}
	if err = get("/api/projects", "/api/projects writer=1"); err != nil {
		t.Error("rollback:", err)
	}
	_, failedState, failedDone := startCaddyDeploy(t, caddy, config, active, admin, address, servers, "reload-failure")
	result := <-failedDone
	if result.err == nil || !strings.Contains(result.output, "[server:aborted:entry]") {
		t.Fatalf("reload failure: %v: %s", result.err, result.output)
	}
	log, e := os.ReadFile(filepath.Join(filepath.Dir(failedState), "docker.log"))
	if e != nil {
		t.Fatal(e)
	}
	for _, line := range strings.Split(string(log), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "kill" || (fields[0] == "stop" && fields[len(fields)-1] == "blue") || (fields[0] == "compose" && strings.Contains(" "+line+" ", " up ") && strings.Contains(line, "issue-desk-intake-entry")) {
			t.Fatalf("unexpected handoff or entry recreation: %s", line)
		}
	}
	if err = get("/failure", "/failure writer=1"); err != nil {
		t.Fatal("old writer after reload failure:", err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+address+"/api/write", strings.NewReader("payload"))
	resp, e := client.Do(req)
	if e != nil {
		t.Errorf("lost response POST: %v", e)
	} else {
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "再試行してください") {
			t.Errorf("lost response POST: status=%d body=%q error=%v", resp.StatusCode, body, readErr)
		}
	}
	if posts.Load() != 1 {
		t.Errorf("lost response POST delivered %d times", posts.Load())
	}
}

type caddyDeployResult struct {
	output string
	err    error
}

// Docker is replaced only at the boundary: the deploy script still writes its
// own Caddy block and drives reload, health checks and writer handoff.
func startCaddyDeploy(t *testing.T, caddy, config, active, admin, address string, servers []*httptest.Server, mode string) (string, string, <-chan caddyDeployResult) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node is required:", err)
	}
	root, env := nasScripts(t, mode)
	state := filepath.Join(root, "state")
	stub := strings.Replace(nasDockerStub, `sh) echo "$7" >"$NAS_TEST_STATE/entry";;`, `sh)
   script=$(printf '%s' "$5" | sed "s|/entry|$NAS_TEST_ENTRY_DIR|g")
   sh -c "$script" sh "$7" "${8:-0}"
   echo "$7" >"$NAS_TEST_STATE/entry"
   sed -i "s|issue-desk-intake-blue:8787|$NAS_TEST_BLUE|g;s|issue-desk-intake-green:8787|$NAS_TEST_GREEN|g" "$NAS_TEST_ACTIVE";;`, 1)
	stub = strings.Replace(stub, "inspect) if", `cp)
  sed "s|admin localhost:2019|admin $NAS_TEST_ADMIN|g;s|:8787 {|http://$NAS_TEST_ADDRESS {|g;s|/entry/active.caddy|$NAS_TEST_ACTIVE|g" "$2" >"$NAS_TEST_CONFIG";;
inspect) if`, 1)
	stub = strings.Replace(stub, `caddy)exit 0;;`, `caddy)
  if [ "$4" = validate ]; then exec "$NAS_TEST_CADDY" validate --config "$NAS_TEST_CONFIG" --adapter caddyfile; fi
  if [ "$NAS_TEST_CASE" = reload-failure ]; then exit 1; fi
  exec "$NAS_TEST_CADDY" reload --config "$NAS_TEST_CONFIG" --adapter caddyfile --address "$NAS_TEST_ADMIN";;`, 1)
	stub = strings.Replace(stub, `wget)case " $* " in *' -S '*)echo '  HTTP/1.1 200 OK' >&2;;*)health entry;;esac;;`, `wget)case " $* " in *' -S '*)echo '  HTTP/1.1 200 OK' >&2;;*)exec "$NAS_TEST_NODE" -e 'fetch(process.env.NAS_TEST_URL+"/healthz").then(async r=>{const body=await r.text();process.stdout.write(body);if(!r.ok){process.exitCode=1;return}const b=JSON.parse(body);if(b.ok===true&&b.role==="active"&&b.revision===process.env.NAS_TEST_TARGET)require("fs").writeFileSync(process.env.NAS_TEST_STATE+"/entry-confirmed", "yes")}).catch(()=>process.exitCode=1)';;esac;;`, 1)
	stub = strings.Replace(stub, `stop)echo false >"$NAS_TEST_STATE/$last";;`, `stop)
  if [ "$last" = blue ] && [ "$NAS_TEST_CASE" = native ]; then
   [ "$(cat "$NAS_TEST_STATE/owner")" = green ] && [ "$(cat "$NAS_TEST_STATE/entry")" = green ] && [ -f "$NAS_TEST_STATE/entry-confirmed" ] || exit 1
  fi
  echo false >"$NAS_TEST_STATE/$last";;`, 1)
	stub = strings.Replace(stub, `kill)echo green >"$NAS_TEST_STATE/owner";;`, `kill)
  echo none >"$NAS_TEST_STATE/owner"
  touch "$NAS_TEST_STATE/notified"
  while [ ! -f "$NAS_TEST_STATE/release" ]; do sleep 0.05; done
  echo green >"$NAS_TEST_STATE/owner";;`, 1)
	if err := os.WriteFile(filepath.Join(root, "bin", "docker"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	env = append(env, "ISSUE_DESK_UPDATE_TIMEOUT=3", "NAS_TEST_CADDY="+caddy, "NAS_TEST_CONFIG="+config, "NAS_TEST_ACTIVE="+active, "NAS_TEST_ENTRY_DIR="+filepath.Dir(active), "NAS_TEST_ADMIN="+admin, "NAS_TEST_ADDRESS="+address, "NAS_TEST_URL=http://"+address, "NAS_TEST_BLUE="+strings.TrimPrefix(servers[0].URL, "http://"), "NAS_TEST_GREEN="+strings.TrimPrefix(servers[1].URL, "http://"))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(root, "deploy", "nas", "deploy-server.sh"), "--revision", strings.Repeat("b", 40))
	cmd.Env = env
	done := make(chan caddyDeployResult, 1)
	go func() { output, err := cmd.CombinedOutput(); done <- caddyDeployResult{string(output), err} }()
	if mode == "reload-failure" {
		return "", state, done
	}
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(filepath.Join(state, "notified")); err == nil {
			return filepath.Join(state, "release"), state, done
		}
		select {
		case result := <-done:
			t.Fatalf("deployment stopped before handoff: %v: %s", result.err, result.output)
		case <-deadline.C:
			t.Fatal("deployment did not request handoff")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
