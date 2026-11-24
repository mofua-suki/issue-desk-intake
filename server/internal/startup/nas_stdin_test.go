package startup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func requireProc(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("/proc によるプロセス検査は Linux 専用です")
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("/proc がないためプロセス検査を省略します")
	} else if err != nil {
		t.Fatal("/proc を確認できません:", err)
	}
}

// nasDeployFunction extracts a complete shell function from the shipped script.
func nasDeployFunction(t *testing.T, source, name string) string {
	t.Helper()
	start := strings.Index(source, name+"() {")
	if start < 0 {
		t.Fatalf("関数がありません: %s", name)
	}
	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("関数の終端がありません: %s", name)
	}
	return source[start : start+end+3]
}

// Exercise the real background deadline wrapper: dash otherwise replaces the
// asynchronous command's standard input with /dev/null before JSON validation.
func TestNASBoundedHealthVerifierPreservesPipeAndStrictMeaning(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "nas", "deploy-server.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// matches runs its verifier inside Docker. Use Node when available, and a
	// test-process verifier otherwise so the shell/pipe test is not skipped.
	bin := t.TempDir()
	docker := filepath.Join(bin, "docker")
	stub := `#!/bin/sh
printf '%s\n' "$@" > "$NAS_TEST_DOCKER_ARGS"
while [ "$1" != -e ]; do shift; done
shift
script=$1; shift
if [ -n "${NAS_TEST_NODE:-}" ]; then exec "$NAS_TEST_NODE" -e "$script" "$@"; fi
exec "$NAS_TEST_BINARY" -test.run '^TestNASHealthVerifierHelper$' -- "$@"
`
	if err := os.WriteFile(docker, []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	nodeName := os.Getenv("NAS_TEST_NODE_BINARY")
	if nodeName == "" {
		nodeName = "node"
	}
	node, _ := exec.LookPath(nodeName)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "NAS_TEST_NODE="+node, "NAS_TEST_BINARY="+binary, "NAS_TEST_HEALTH_HELPER=1", "ENTRY_DEADLINE=")
	functions := nasDeployFunction(t, string(raw), "bounded") + "\n" + nasDeployFunction(t, string(raw), "matches")
	target := strings.Repeat("b", 40)
	for _, shell := range []string{"sh", "dash"} {
		binary, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		t.Run(shell, func(t *testing.T) {
			for _, mode := range []string{"node", "helper"} {
				t.Run(mode, func(t *testing.T) {
					if mode == "node" && node == "" {
						t.Skip("Node がありません")
					}
					argsFile := filepath.Join(t.TempDir(), "docker-args")
					modeNode := node
					if mode == "helper" {
						modeNode = ""
					}
					prefix := `{"ok":true,"revision":"` + target + `","role":"active","padding":"`
					atLimit := prefix + strings.Repeat("x", 65536-len(prefix)-2) + `"}`
					for _, item := range []struct {
						body, role string
						ok         bool
					}{
						{`{"ok":true,"revision":"` + target + `","role":"active"}`, "active", true},
						{`{"ok":true,"revision":"` + target + `","role":"standby"}`, "active", false},
						{`{"db":{"ok":true},"ok":false,"revision":"` + target + `","role":"active"}`, "active", false},
						{`{"ok":true`, "any", false},
						{atLimit, "active", true},
						{prefix + strings.Repeat("x", 65537-len(prefix)-2) + `"}`, "active", false},
					} {
						if err := os.Remove(argsFile); err != nil && !os.IsNotExist(err) {
							t.Fatal(err)
						}
						ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
						command := exec.CommandContext(ctx, binary, "-c", "READ_LIMIT=3\n"+functions+"\nmatches \"$1\" \"$2\" \"$3\"", "sh", item.body, target, item.role)
						command.Env = append(env, "NAS_TEST_NODE="+modeNode, "NAS_TEST_DOCKER_ARGS="+argsFile, "ISSUE_DESK_INTAKE_IMAGE=health-test-image")
						output, err := command.CombinedOutput()
						contextErr := ctx.Err()
						cancel()
						if contextErr != nil {
							t.Fatal("健康照合が期限内に終わりません", contextErr, string(output))
						}
						if (err == nil) != item.ok {
							t.Fatal("JSON入力か厳密判定が変わりました", item.role, item.ok, err, string(output))
						}
						args, err := os.ReadFile(argsFile)
						if err != nil {
							t.Fatal(err)
						}
						lines := strings.Split(string(args), "\n")
						want := []string{"run", "--rm", "-i", "--pull", "never", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "1000:1000", "--entrypoint", "node", "health-test-image", "-e"}
						if len(lines) < len(want) || !reflect.DeepEqual(lines[:len(want)], want) {
							t.Fatalf("docker の隔離引数が不正: %q", lines)
						}
					}
				})
			}
		})
	}
}

// The deadline must terminate the actual command, including when output is redirected.
func TestNASBoundedTerminatesCommandAndEnforcesDeadline(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "nas", "deploy-server.sh"))
	if err != nil {
		t.Fatal(err)
	}
	function := nasDeployFunction(t, string(raw), "bounded")
	for _, shell := range []string{"sh", "dash"} {
		binary, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		t.Run(shell, func(t *testing.T) {
			timerDuration := "15." + strconv.Itoa(os.Getpid())
			pidFile := filepath.Join(t.TempDir(), "sleep.pid")
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// The redirected call matches the update-record and probe paths.
			script := function + `
bounded "$1" sh -c 'sleep 0.2' >/dev/null 2>&1 || exit 1
echo finished
bounded 3 sh -c 'exit 7' >/dev/null 2>&1
[ "$?" -eq 7 ] || exit 3
bounded 1 sh -c 'echo "$$" > "$1"; exec sleep "$2"' sh "$2" "$1" >/dev/null 2>&1
code=$?
[ "$code" -ne 0 ] || exit 2
echo timed-out
`
			command := exec.CommandContext(ctx, binary, "-c", script, "sh", timerDuration, pidFile)
			command.Env = append(os.Environ(), "ENTRY_DEADLINE=")
			output, err := command.CombinedOutput()
			if ctx.Err() != nil || err != nil || string(output) != "finished\ntimed-out\n" {
				t.Fatalf("コマンドまたは期限切れが不正: %v %v %q", ctx.Err(), err, output)
			}
			pidBytes, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal("sleep の PID を取得できません", err)
			}
			pid := strings.TrimSpace(string(pidBytes))
			if n, err := strconv.Atoi(pid); err != nil || n <= 0 {
				t.Fatalf("不正な PID: %q", pid)
			}
			t.Run("process-exit", func(t *testing.T) {
				requireProc(t, "/proc")
				// timeout は SIGKILL を送ると自分のプロセスグループごと終わり、sleep の後始末を待たない。
				// SIGKILL を受けた sleep が /proc から消えるのは少し後なので、負荷が高くても判定がぶれないよう期限まで読み直す。
				deadline := time.Now().Add(3 * time.Second)
				for {
					cmdline, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
					if nasProcessGone(err) {
						return
					}
					if err != nil {
						t.Fatalf("sleep の PID を確認できません: %v", err)
					}
					if !strings.Contains(string(cmdline), "sleep\x00"+timerDuration+"\x00") {
						return
					}
					if time.Now().After(deadline) {
						t.Fatalf("期限切れの sleep が残っています: PID %s", pid)
					}
					time.Sleep(20 * time.Millisecond)
				}
			})
		})
	}
}

// A proc entry can disappear between lookup and read, including as ESRCH.
func nasProcessGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// Invoked only by the Docker stub when Node is absent. The production JS is
// also exercised by the Node-backed path and nas_scripts_test.go. This Go
// approximation differs from JavaScript in JSON type handling and counts bytes,
// while JavaScript counts UTF-16 code units; only the shared ASCII cases and
// byte limit are covered here.
func TestNASHealthVerifierHelper(t *testing.T) {
	if os.Getenv("NAS_TEST_HEALTH_HELPER") != "1" {
		return
	}
	args := os.Args
	if len(args) < 2 {
		os.Exit(1)
	}
	var value struct {
		OK       bool   `json:"ok"`
		Revision string `json:"revision"`
		Role     string `json:"role"`
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 65537))
	if err != nil || len(input) > 65536 {
		os.Exit(1)
	}
	decoder := json.NewDecoder(strings.NewReader(string(input)))
	if decoder.Decode(&value) != nil {
		os.Exit(1)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		os.Exit(1)
	}
	revision, role := args[len(args)-2], args[len(args)-1]
	if !value.OK || (revision != "any" && value.Revision != revision) || (role != "any" && value.Role != role) {
		os.Exit(1)
	}
	os.Exit(0)
}
