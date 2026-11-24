package startup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunHandoffDeploySuccess(t *testing.T) {
	testRunAcceptedFiling(t, true, false, false, "native")
}

func TestRunHandoffDeployRollback(t *testing.T) {
	testRunAcceptedFiling(t, true, false, false, "rollback")
}

type runDeployResult struct {
	code   int
	output string
}
type runDeploy struct {
	root, mode string
	done       chan runDeployResult
}

func startRunDeploy(t *testing.T, ctx context.Context, mode string) *runDeploy {
	t.Helper()
	if mode == "" {
		return nil
	}
	root, env := nasScripts(t, mode)
	env = append(env, "ISSUE_DESK_UPDATE_TIMEOUT=3")
	stub := strings.Replace(nasDockerStub, `kill)echo green >"$NAS_TEST_STATE/owner";;`, `kill)[ "$2" = --signal=SIGUSR2 ] || exit 1; touch "$NAS_TEST_STATE/notified"; while [ ! -f "$NAS_TEST_STATE/handoff-complete" ]; do sleep 0.05; done; echo green >"$NAS_TEST_STATE/owner";;`, 1)
	if stub == nasDockerStub {
		t.Fatal("配置スタブに通知口がない")
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "docker"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	d := &runDeploy{root: root, mode: mode, done: make(chan runDeployResult, 1)}
	go func() {
		code, output := runNASScript(t, root, env, "deploy-server.sh", "--revision", strings.Repeat("b", 40))
		d.done <- runDeployResult{code, output}
	}()
	for {
		if _, err := os.Stat(filepath.Join(root, "state", "notified")); err == nil {
			return d
		}
		select {
		case result := <-d.done:
			t.Fatal("配置が通知前に終了", result)
		case <-ctx.Done():
			t.Fatal("配置が交代を通知しない")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (d *runDeploy) finish(t *testing.T, ctx context.Context) {
	t.Helper()
	if d == nil {
		return
	}
	if err := os.WriteFile(filepath.Join(d.root, "state", "handoff-complete"), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-d.done:
		if d.mode == "native" && (result.code != 0 || !strings.Contains(result.output, "[server:success]")) {
			t.Fatal("配置に失敗", result)
		}
		if d.mode == "rollback" && (result.code != 1 || !strings.Contains(result.output, "[server:switched-back:takeover]")) {
			t.Fatal("切り戻しに失敗", result)
		}
	case <-ctx.Done():
		t.Fatal("配置が完了しない")
	}
}
