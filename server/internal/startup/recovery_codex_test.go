package startup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var recoveryBuildOnce sync.Once
var recoveryBuildDir, recoveryBinary string
var recoveryBuildErr error
var recoveryBuildCount int

func recoveryCodex(t *testing.T, config *Config, id string) (string, string, string) {
	t.Helper()
	recoveryBuildOnce.Do(func() {
		recoveryBuildCount++
		recoveryBuildDir, recoveryBuildErr = os.MkdirTemp("", "recovery-codex-build-")
		if recoveryBuildErr != nil {
			return
		}
		source := filepath.Join(recoveryBuildDir, "main.go")
		recoveryBinary = filepath.Join(recoveryBuildDir, recoveryExecutableName())
		if recoveryBuildErr = os.WriteFile(source, []byte(recoveryCodexSource), 0600); recoveryBuildErr != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var out []byte
		out, recoveryBuildErr = exec.CommandContext(ctx, "go", "build", "-o", recoveryBinary, source).CombinedOutput()
		if recoveryBuildErr != nil {
			recoveryBuildErr = fmt.Errorf("build recovery CLI: %w: %s", recoveryBuildErr, out)
		}
	})
	if recoveryBuildErr != nil {
		t.Fatal(recoveryBuildErr)
	}
	dir := t.TempDir()
	binary, err := os.ReadFile(recoveryBinary)
	if err != nil {
		t.Fatal(err)
	}
	config.Codex = filepath.Join(dir, recoveryExecutableName())
	if err = os.WriteFile(config.Codex, binary, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "expected"), []byte(id), 0600); err != nil {
		t.Fatal(err)
	}
	started, release, launches := filepath.Join(dir, "started"), filepath.Join(dir, "release"), filepath.Join(dir, "launches")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	return started, release, launches
}

func recoveryExecutableName() string {
	if runtime.GOOS == "windows" {
		return "codex.exe"
	}
	return "codex"
}

const recoveryCodexSource = `package main
import ("encoding/json"; "fmt"; "io"; "os"; "path/filepath"; "strings"; "time")
func main() {
 args := os.Args[1:]
 if len(args) > 0 && args[0] == "login" { fmt.Println("Logged in using ChatGPT"); return }
 input, err := io.ReadAll(os.Stdin); if err != nil { panic(err) }
 exe, err := os.Executable(); if err != nil { panic(err) }; dir := filepath.Dir(exe)
 expected, err := os.ReadFile(filepath.Join(dir, "expected")); if err != nil { panic(err) }
 lines := strings.Split(string(input), "\n")
 var data struct { Skill string ` + "`json:\"skill\"`" + `; Messages []struct { Text string ` + "`json:\"text\"`" + ` } ` + "`json:\"messages\"`" + ` }
 if json.Unmarshal([]byte(lines[len(lines)-1]), &data) != nil || !strings.Contains(data.Skill, "新しいプロジェクトの相談") { return }
 matched := false; for _, m := range data.Messages { if m.Text == string(expected) { matched = true } }; if !matched { return }
 f, err := os.OpenFile(filepath.Join(dir, "launches"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); if err != nil { panic(err) }; if _, err = f.WriteString("started\n"); err != nil { panic(err) }; if err = f.Close(); err != nil { panic(err) }
 if err := os.WriteFile(filepath.Join(dir, "started"), nil, 0600); err != nil { panic(err) }
 deadline := time.Now().Add(90*time.Second)
 for { if _, err := os.Stat(filepath.Join(dir, "release")); err == nil { break }; if time.Now().After(deadline) { panic("release timeout") }; time.Sleep(10*time.Millisecond) }
 for i, arg := range args { if arg == "-o" && i+1 < len(args) { if err := os.WriteFile(args[i+1], []byte("{\"reply\":\"復旧した応答\",\"ready\":false,\"questions\":[\"確認\"],\"project\":{\"name\":\"test\",\"description\":\"test\",\"technology\":\"node\",\"placement\":\"nas\",\"testCommand\":[],\"setupCommand\":[],\"autoMerge\":false,\"autoDeploy\":false},\"splitIssues\":[]}"), 0600); err != nil { panic(err) }; break } }
 fmt.Println("{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":3,\"cached_input_tokens\":0}}")
}`

func assertRecoveryLaunches(t *testing.T, path string, want int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) && want == 0 {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "\n"); got != want || string(data) != strings.Repeat("started\n", want) {
		t.Fatalf("launches = %q, want %d lines", data, want)
	}
}

func TestRecoveryCodexIdentifiesProjectPrompt(t *testing.T) {
	const id = "22222222-2222-4222-8222-222222222222"
	a, b := &Config{}, &Config{}
	started, release, launches := recoveryCodex(t, a, id)
	otherStarted, otherRelease, otherLaunches := recoveryCodex(t, b, "other")
	if recoveryBuildCount != 1 {
		t.Fatalf("builds = %d", recoveryBuildCount)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run := func(args []string, input string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, a.Codex, args...)
		cmd.Stdin = strings.NewReader(input)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("call: %v: %s", err, out)
		}
	}
	run([]string{"login"}, "")
	run([]string{"exec"}, `unrelated`)
	run([]string{"exec"}, `{"skill":"新しいプロジェクトの相談","messages":[{"text":"`+id+`-other"}]}`)
	for _, path := range []string{started, otherStarted} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unexpected start %s: %v", path, err)
		}
	}
	assertRecoveryLaunches(t, launches, 0)
	assertRecoveryLaunches(t, otherLaunches, 0)
	cmd := exec.CommandContext(ctx, a.Codex, "exec", "-o", filepath.Join(t.TempDir(), "result"))
	cmd.Stdin = strings.NewReader(`{"skill":"新しいプロジェクトの相談","messages":[{"text":"` + id + `"}]}`)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600); cancel(); <-done })
	waitRecoveryProcess(t, ctx, started)
	if _, err := os.Stat(otherStarted); !os.IsNotExist(err) {
		t.Fatalf("other fixture started: %v", err)
	}
	assertRecoveryLaunches(t, otherLaunches, 0)
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assertRecoveryLaunches(t, launches, 1)
	if _, err := os.Stat(otherRelease); !os.IsNotExist(err) {
		t.Fatalf("other fixture released: %v", err)
	}
}
