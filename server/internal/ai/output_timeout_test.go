package ai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudePartialPlaintextFailures(t *testing.T) {
	for _, tc := range []struct {
		name, output, kind string
		temporary          bool
	}{
		{"rate-limit", "rate limit\\n", "rate_limit", true},
		{"authentication-incomplete-line", "Not logged in", "authentication", false},
		{"usage-limit", "usage limit\\n", "usage_limit", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := testRequest(t, "claude")
			request.OutputTimeout, request.IdleTimeout, request.Timeout = time.Second, time.Second, 3*time.Second
			exe := filepath.Join(request.Workspace, "claude")
			script := "#!/bin/sh\nif [ \"$1\" = auth ]; then echo '{\"loggedIn\":true,\"apiProvider\":\"firstParty\",\"authMethod\":\"oauth_token\"}'; exit; fi\nprintf '" + tc.output + "'\nexit 1\n"
			if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			_, err := (&Runner{Claude: exe}).Call(context.Background(), request)
			var failure *Failure
			if !errors.As(err, &failure) || failure.Kind != tc.kind || failure.Temporary != tc.temporary {
				t.Fatalf("failure=%+v err=%v", failure, err)
			}
		})
	}
}

func TestClaudePartialRunner(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		output     time.Duration
		cause      string
		flag       bool
	}{
		{"progress", `for i in 1 2 3 4; do printf '%s' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"x"}}}'; printf '\n'; sleep .12; done; echo '{"type":"result","subtype":"success","structured_output":{"ok":true},"usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}'`, 250 * time.Millisecond, "completed", true},
		{"empty", `while :; do echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}}'; echo '{"type":"ping"}'; sleep .05; done`, 250 * time.Millisecond, "timeout_idle", true},
		{"no-deadline", `echo '{"type":"result","subtype":"success","structured_output":{"ok":true},"usage":{"input_tokens":5,"output_tokens":2}}'`, 0, "completed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := testRequest(t, "claude")
			request.OutputTimeout, request.IdleTimeout, request.Timeout = tc.output, time.Second, 3*time.Second
			script := "#!/bin/sh\nif [ \"$1\" = auth ]; then echo '{\"loggedIn\":true,\"apiProvider\":\"firstParty\",\"authMethod\":\"oauth_token\"}'; exit; fi\n" +
				"found=no; for arg in \"$@\"; do [ \"$arg\" = --include-partial-messages ] && found=yes; done\n" +
				"[ \"$found\" = " + map[bool]string{true: "yes", false: "no"}[tc.flag] + " ] || exit 42\n" + tc.body + "\n"
			exe := filepath.Join(request.Workspace, "claude")
			if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			answer, err := (&Runner{Claude: exe}).Call(context.Background(), request)
			if answer.Diagnostic.Kind != tc.cause {
				t.Fatalf("answer=%+v err=%v", answer, err)
			}
			if tc.cause == "completed" && (err != nil || answer.Value["ok"] != true || answer.Usage.InputTokens == nil || *answer.Usage.InputTokens != 5) {
				t.Fatalf("answer=%+v err=%v", answer, err)
			}
		})
	}
}

func TestClaudePartialBoundedAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, body, cause string
		timeout           time.Duration
		cancel            bool
	}{
		{"many", `i=0; while [ "$i" -lt 90000 ]; do echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"secret partial content"}}}'; i=$((i+1)); done; echo '{"type":"result","subtype":"success","structured_output":{"ok":true},"usage":{"input_tokens":5,"output_tokens":2}}'`, "completed", 20 * time.Second, false},
		{"total", `while :; do echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"x"}}}'; sleep .05; done`, "timeout_total", 400 * time.Millisecond, false},
		{"cancel", `while :; do echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}}'; sleep .05; done`, "canceled_by_server", 3 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := testRequest(t, "claude")
			request.OutputTimeout, request.IdleTimeout, request.Timeout = time.Second, 2*time.Second, tc.timeout
			exe := filepath.Join(request.Workspace, "claude")
			script := "#!/bin/sh\nif [ \"$1\" = auth ]; then echo '{\"loggedIn\":true,\"apiProvider\":\"firstParty\",\"authMethod\":\"oauth_token\"}'; exit; fi\n" + tc.body
			if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				time.AfterFunc(400*time.Millisecond, cancel)
			}
			answer, err := (&Runner{Claude: exe}).Call(ctx, request)
			if answer.Diagnostic.Kind != tc.cause {
				t.Fatalf("answer=%+v err=%v", answer, err)
			}
			if tc.cause == "completed" && (err != nil || answer.Value["ok"] != true || answer.Usage.InputTokens == nil || *answer.Usage.InputTokens != 5) {
				t.Fatalf("answer=%+v err=%v", answer, err)
			}
		})
	}
}

func TestClaudePartialSplitAndIsolation(t *testing.T) {
	script := `printf '%s' '{"type":"stream_event","event":{"type":"content_block_delta",'; sleep .05; printf '%s\n' '"delta":{"type":"text_delta","text":"SECRET","usage":{"input_tokens":999},"session_id":"bad"}}}'; printf '%s\n' '{"type":"result","structured_output":{"ok":true},"usage":{"input_tokens":5}}'`
	var lines []string
	result, err := Run(context.Background(), ProcessRequest{Executable: "/bin/sh", Arguments: []string{"-c", script}, Directory: t.TempDir(), Timeout: time.Second, IdleTimeout: time.Second, OutputTimeout: time.Second, ClaudePartial: true, OnLine: func(line string) { lines = append(lines, line) }})
	if err != nil || strings.Contains(string(result.Stdout), "SECRET") || len(lines) != 1 || strings.Contains(lines[0], "SECRET") {
		t.Fatalf("result=%+v lines=%q err=%v", result, lines, err)
	}
	usage := Measure("claude", result.Stdout)
	if usage.InputTokens == nil || *usage.InputTokens != 5 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestOutputDeadlineBoundsBusySilentProvider(t *testing.T) {
	start := time.Now()
	sample := func(int) groupSample {
		n := int64(time.Since(start) / time.Millisecond)
		return groupSample{at: time.Now(), cpuTicks: &n}
	}
	result, err := runWithSampler(context.Background(), ProcessRequest{Executable: "/bin/sh", Arguments: []string{"-c", "sleep 5"}, Directory: t.TempDir(), Timeout: 2 * time.Second, IdleTimeout: 50 * time.Millisecond, OutputTimeout: 200 * time.Millisecond}, sample)
	if err == nil || result.Cause != "timeout_idle" || result.Idle == nil || result.Duration >= time.Second {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
func TestOutputDeadlineResetsOnActualOutput(t *testing.T) {
	result, err := Run(context.Background(), ProcessRequest{Executable: "/bin/sh", Arguments: []string{"-c", "printf first; sleep 0.8; printf second; sleep 0.8; printf third; sleep 0.8; printf done"}, Directory: t.TempDir(), Timeout: 10 * time.Second, IdleTimeout: 5 * time.Second, OutputTimeout: 2 * time.Second})
	if err != nil || string(result.Stdout) != "firstsecondthirddone" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// TestIdleDeadlineDiagnostic exercises both timer paths through the persisted JSON form.
func TestIdleDeadlineDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		busy   bool
		output time.Duration
		want   string
	}{
		{"output", true, 300 * time.Millisecond, "output_deadline"},
		{"inactivity", false, 2 * time.Second, "inactivity_deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			sample := func(int) groupSample {
				n := int64(0)
				if tc.busy {
					n = int64(time.Since(start) / time.Millisecond)
				}
				return groupSample{at: time.Now(), cpuTicks: &n}
			}
			result, err := runWithSampler(context.Background(), ProcessRequest{Executable: "/bin/sh", Arguments: []string{"-c", "printf 'SECRET-STDERR' >&2; sleep 5"}, Input: "SECRET-PROMPT", Directory: t.TempDir(), Timeout: 5 * time.Second, IdleTimeout: 150 * time.Millisecond, OutputTimeout: tc.output}, sample)
			if err == nil || result.Cause != "timeout_idle" {
				t.Fatalf("cause=%s err=%v", result.Cause, err)
			}
			d := diagnose("exit", result, true, err, start)
			raw, e := json.Marshal(d.Map())
			if e != nil {
				t.Fatal(e)
			}
			var stored map[string]any
			if e = json.Unmarshal(raw, &stored); e != nil {
				t.Fatal(e)
			}
			if stored["kind"] != "timeout_idle" || stored["idleCondition"] != tc.want {
				t.Fatalf("diagnostic=%s", raw)
			}
			if strings.Contains(string(raw), "SECRET-") {
				t.Fatalf("secret in diagnostic: %s", raw)
			}
		})
	}
	for _, tc := range []struct {
		name, script, cause string
		timeout             time.Duration
		cancel              bool
	}{
		{"normal", "echo ok", "", 2 * time.Second, false},
		{"total", "sleep 5", "timeout_total", 100 * time.Millisecond, false},
		{"canceled", "sleep 5", "canceled_by_server", 2 * time.Second, true},
		{"nonzero", "exit 1", "exit_nonzero", 2 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				time.AfterFunc(50*time.Millisecond, cancel)
			}
			start := time.Now()
			result, err := Run(ctx, ProcessRequest{Executable: "/bin/sh", Arguments: []string{"-c", tc.script}, Directory: t.TempDir(), Timeout: tc.timeout, IdleTimeout: time.Second, OutputTimeout: time.Second})
			if result.Cause != tc.cause {
				t.Fatalf("cause=%q err=%v", result.Cause, err)
			}
			raw, e := json.Marshal(diagnose("exit", result, true, err, start).Map())
			if e != nil {
				t.Fatal(e)
			}
			var stored map[string]any
			if e = json.Unmarshal(raw, &stored); e != nil {
				t.Fatal(e)
			}
			if _, ok := stored["idleCondition"]; ok {
				t.Fatalf("unexpected condition: %s", raw)
			}
		})
	}
	// A timeout observation from an older executor has no condition to infer.
	old := diagnose("exit", ProcessResult{Cause: "timeout_idle"}, true, failed("timeout", "timeout", true), time.Now()).Map()
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored["idleCondition"]; ok {
		t.Fatalf("old observation gained a condition: %s", raw)
	}
}
