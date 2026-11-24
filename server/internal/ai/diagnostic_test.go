package ai

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/usage"
)

func runShell(t *testing.T, parent context.Context, script string, timeout, idle time.Duration) (ProcessResult, error) {
	t.Helper()
	return Run(parent, ProcessRequest{Executable: "/bin/sh", Arguments: []string{"-c", script}, Directory: t.TempDir(), Timeout: timeout, IdleTimeout: idle})
}

func TestExecuteFailureDiagnosticsAndUsageEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, provider, step, stdout, stderr, cause, reason, evidence string
		truncated                                                     bool
	}{
		{"failed authentication", "codex", usage.DraftStep, `{"type":"turn.failed","error":{"message":"authentication SECRET https://example.com"}}`, "SECRET", "exit_nonzero", "authentication", "", false},
		{"stderr rate", "codex", usage.FilingCheckStep, `{"type":"turn.failed","error":{"message":"unknown SECRET"}}`, "rate limit SECRET", "exit_nonzero", "rate_limit", "", false},
		{"unknown", "codex", usage.DraftStep, `{"type":"turn.failed","error":{"message":"SECRET"}}`, "SECRET", "exit_nonzero", "unknown", "", false},
		{"spawn codex", "codex", usage.DraftStep, "", "", "spawn_failed", "", "process_not_started", false},
		{"spawn claude", "claude", usage.FilingCheckStep, "", "", "spawn_failed", "", "process_not_started", false},
		{"other step", "codex", "other", "", "", "spawn_failed", "", "", false},
		{"empty exit", "codex", usage.DraftStep, "", "", "exit_nonzero", "unknown", "", false},
		{"truncated", "codex", usage.DraftStep, `{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0,"cached_input_tokens":0}}`, "", "output_overflow", "", "", true},
		{"measured zero", "codex", usage.DraftStep, `{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0,"cached_input_tokens":0}}`, "", "exit_nonzero", "unknown", "measured_zero", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, _ := diagnosticFixture(t)
			request.Provider, request.Step = tc.provider, tc.step
			var observed Usage
			runner := Runner{Observe: func(_ Request, u Usage, _ time.Duration, _ bool) { observed = u }, Execute: func(_ context.Context, p ProcessRequest) (ProcessResult, error) {
				if p.Arguments[0] == "login" {
					return ProcessResult{Stdout: []byte("ChatGPT")}, nil
				}
				if p.Arguments[0] == "auth" {
					return ProcessResult{Stdout: []byte(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"oauth_token"}`)}, nil
				}
				return ProcessResult{Stdout: []byte(tc.stdout), Stderr: []byte(tc.stderr), Cause: tc.cause, Code: 7, Truncated: tc.truncated}, errors.New("fixture failure")
			}}
			answer, err := runner.Call(context.Background(), request)
			if err == nil || answer.Diagnostic.FailureReason != tc.reason || answer.Diagnostic.UsageEvidence != tc.evidence {
				t.Fatalf("err=%v diagnostic=%+v", err, answer.Diagnostic)
			}
			if tc.cause == "exit_nonzero" && (answer.Diagnostic.ExitCode == nil || *answer.Diagnostic.ExitCode != 7) {
				t.Fatalf("exit code: %+v", answer.Diagnostic)
			}
			if strings.Contains(strings.Join(mapStrings(answer.Diagnostic.Map()), "|"), "SECRET") {
				t.Fatal("secret in diagnostic")
			}
			if (observed.InputTokens == nil) != (answer.Usage.InputTokens == nil) || (observed.InputTokens != nil && *observed.InputTokens != *answer.Usage.InputTokens) {
				t.Fatal("observation differs from answer")
			}
			if tc.evidence == "process_not_started" {
				if answer.Usage.InputTokens == nil || *answer.Usage.InputTokens != 0 || answer.Usage.CostUSD != nil || (tc.provider == "codex" && answer.Usage.CacheWriteTokens != nil) || (tc.provider == "claude" && answer.Usage.CacheWriteTokens == nil) {
					t.Fatalf("usage: %+v", answer.Usage)
				}
			} else if tc.stdout == "" && answer.Usage.InputTokens != nil {
				t.Fatalf("missing usage became zero: %+v", answer.Usage)
			}
		})
	}
}

func TestMeasuredZeroEvidenceRequiresTargetStepAndFailure(t *testing.T) {
	for _, step := range []string{usage.DraftStep, usage.FilingCheckStep, usage.TierClassificationStep} {
		for _, fail := range []bool{false, true} {
			name := step + "/success"
			if fail {
				name = step + "/failure"
			}
			t.Run(name, func(t *testing.T) {
				request, _ := diagnosticFixture(t)
				request.Step = step
				runner := Runner{Execute: func(_ context.Context, p ProcessRequest) (ProcessResult, error) {
					if p.Arguments[0] == "login" {
						return ProcessResult{Stdout: []byte("ChatGPT")}, nil
					}
					for i, arg := range p.Arguments {
						if arg == "-o" {
							if err := os.WriteFile(p.Arguments[i+1], []byte(`{"ok":true}`), 0600); err != nil {
								t.Fatal(err)
							}
							break
						}
					}
					result := ProcessResult{Stdout: []byte(`{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0,"cached_input_tokens":0}}`)}
					if fail {
						result.Cause, result.Code = "exit_nonzero", 1
						return result, errors.New("execution failed")
					}
					return result, nil
				}}
				answer, err := runner.Call(context.Background(), request)
				if (err != nil) != fail {
					t.Fatalf("err=%v, fail=%v", err, fail)
				}
				want := ""
				if fail && step != usage.TierClassificationStep {
					want = "measured_zero"
				}
				if answer.Diagnostic.UsageEvidence != want {
					t.Fatalf("evidence=%q, want %q; usage=%+v", answer.Diagnostic.UsageEvidence, want, answer.Usage)
				}
			})
		}
	}
}

func TestRunDistinguishesIdleTimeoutFromTotalTimeout(t *testing.T) {
	// 何も出力せずに待つ → 無出力の時間切れ（IdleTimeout）
	idle, err := runShell(t, context.Background(), "sleep 5", 20*time.Second, 300*time.Millisecond)
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != "timeout" || idle.Cause != "timeout_idle" {
		t.Fatalf("idle: cause=%q err=%v", idle.Cause, err)
	}
	if idle.FirstOutput >= 0 || idle.StdoutBytes != 0 {
		t.Fatalf("idle: nothing was written, first=%v bytes=%d", idle.FirstOutput, idle.StdoutBytes)
	}
	// 出力は続くが、全体の制限時間を超える → 全体の時間切れ（Timeout）
	total, err := runShell(t, context.Background(), "while true; do echo tick; sleep 0.05; done", 400*time.Millisecond, 20*time.Second)
	if !errors.As(err, &failure) || failure.Kind != "timeout" || total.Cause != "timeout_total" {
		t.Fatalf("total: cause=%q err=%v", total.Cause, err)
	}
	if total.FirstOutput < 0 || total.LastOutput < total.FirstOutput || total.StdoutBytes == 0 {
		t.Fatalf("total: output offsets first=%v last=%v bytes=%d", total.FirstOutput, total.LastOutput, total.StdoutBytes)
	}
}

func TestRunReportsServerCancellationAndExitAndSpawnFailure(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	canceled, err := runShell(t, parent, "sleep 5", 20*time.Second, 20*time.Second)
	if !errors.Is(err, context.Canceled) || canceled.Cause != "canceled_by_server" {
		t.Fatalf("canceled: cause=%q err=%v", canceled.Cause, err)
	}
	exited, err := runShell(t, context.Background(), "echo out; echo err >&2; exit 3", 10*time.Second, 10*time.Second)
	if err == nil || exited.Cause != "exit_nonzero" || exited.Code != 3 || exited.StdoutBytes != 4 || exited.StderrBytes != 4 {
		t.Fatalf("exit: %+v err=%v", exited, err)
	}
	spawn, err := Run(context.Background(), ProcessRequest{Executable: "/nonexistent/ai-binary", Directory: t.TempDir()})
	if err == nil || spawn.Cause != "spawn_failed" {
		t.Fatalf("spawn: cause=%q err=%v", spawn.Cause, err)
	}
	ok, err := runShell(t, context.Background(), "echo done", 10*time.Second, 10*time.Second)
	if err != nil || ok.Cause != "" {
		t.Fatalf("ok: cause=%q err=%v", ok.Cause, err)
	}
}

func diagnosticFixture(t *testing.T) (Request, string) {
	t.Helper()
	dir := t.TempDir()
	schema := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schema, []byte(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return Request{Provider: "codex", Model: "gpt-test", Effort: "low", Workspace: dir, SchemaFile: schema, Step: "診断", Input: "SECRET-PROMPT-TEXT"}, dir
}

func TestCallDiagnosticForIdleTimeoutHoldsNoContent(t *testing.T) {
	request, _ := diagnosticFixture(t)
	runner := Runner{Execute: func(ctx context.Context, r ProcessRequest) (ProcessResult, error) {
		if len(r.Arguments) > 0 && r.Arguments[0] == "login" {
			return ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
		}
		return ProcessResult{Stdout: []byte(`{"type":"turn.started","secret":"SECRET-ANSWER"}` + "\n"), Stderr: []byte("SECRET-STDERR"), Cause: "timeout_idle", StdoutBytes: 49, StderrBytes: 13, FirstOutput: 500 * time.Millisecond, LastOutput: 600 * time.Millisecond}, failed("timeout", "AIの制限時間または応答待ち時間を超えました", true)
	}}
	answer, err := runner.Call(context.Background(), request)
	if err == nil {
		t.Fatal("want a timeout")
	}
	d := answer.Diagnostic
	if d.ID == "" || d.Kind != "timeout_idle" || d.Stage != "exit" || d.FinalEvent != "turn.started" || d.StdoutBytes != 49 || d.StderrBytes != 13 {
		t.Fatalf("diagnostic = %+v", d)
	}
	if d.FirstOutputMs == nil || *d.FirstOutputMs != 500 || d.LastOutputMs == nil || *d.LastOutputMs != 600 {
		t.Fatalf("output offsets = %v %v", d.FirstOutputMs, d.LastOutputMs)
	}
	if d.ExitCode != nil {
		t.Fatalf("a killed process has no meaningful exit code: %v", *d.ExitCode)
	}
	for _, secret := range []string{"SECRET-PROMPT-TEXT", "SECRET-ANSWER", "SECRET-STDERR"} {
		if strings.Contains(strings.Join(mapStrings(d.Map()), "|"), secret) {
			t.Fatalf("diagnostic leaks %q", secret)
		}
	}
}

func mapStrings(m map[string]any) []string {
	out := []string{}
	for k, v := range m {
		out = append(out, k)
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestCallDiagnosticForPreflightFailureAndSuccess(t *testing.T) {
	request, _ := diagnosticFixture(t)
	// 認証の確認で止まる → 実行前の失敗
	notLoggedIn := Runner{Execute: func(ctx context.Context, r ProcessRequest) (ProcessResult, error) {
		return ProcessResult{Stdout: []byte("Not logged in")}, nil
	}}
	answer, err := notLoggedIn.Call(context.Background(), request)
	if err == nil || answer.Diagnostic.Kind != "authentication" || answer.Diagnostic.Stage != "preflight" || answer.Diagnostic.StdoutBytes != 0 {
		t.Fatalf("preflight: err=%v diagnostic=%+v", err, answer.Diagnostic)
	}
	// 入力が大きすぎる → 実行前の失敗（送る内容が不正）
	big := request
	big.Input = strings.Repeat("x", 200)
	big.MaxInputBytes = 100
	answer, err = (&Runner{}).Call(context.Background(), big)
	if err == nil || answer.Diagnostic.Kind != "input_invalid" || answer.Diagnostic.Stage != "preflight" {
		t.Fatalf("input: err=%v diagnostic=%+v", err, answer.Diagnostic)
	}
	// 正常終了
	ok := Runner{Execute: func(ctx context.Context, r ProcessRequest) (ProcessResult, error) {
		if len(r.Arguments) > 0 && r.Arguments[0] == "login" {
			return ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
		}
		for i, a := range r.Arguments {
			if a == "-o" {
				if err := os.WriteFile(r.Arguments[i+1], []byte(`{"ok":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return ProcessResult{Stdout: []byte(`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}` + "\n"), FirstOutput: time.Millisecond, LastOutput: time.Millisecond}, nil
	}}
	answer, err = ok.Call(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if d := answer.Diagnostic; d.Kind != "completed" || d.Stage != "done" || d.FinalEvent != "turn.completed" || d.ID == "" {
		t.Fatalf("success diagnostic = %+v", d)
	}
}

func TestDiagnosticKindMapping(t *testing.T) {
	for _, tc := range []struct {
		name    string
		process ProcessResult
		ran     bool
		err     error
		want    string
	}{
		{"nil", ProcessResult{}, true, nil, "completed"},
		{"server cancel", ProcessResult{Cause: "canceled_by_server"}, true, context.Canceled, "canceled_by_server"},
		{"cancel before run", ProcessResult{}, false, context.Canceled, "canceled_by_server"},
		{"rate limit", ProcessResult{}, true, failed("rate_limit", "x", true), "rate_limit"},
		{"usage limit", ProcessResult{}, true, failed("usage_limit", "x", false), "usage_limit"},
		{"format", ProcessResult{}, true, failed("format", "x", false), "format"},
		{"schema", ProcessResult{}, false, failed("schema", "x", false), "input_invalid"},
		{"overflow", ProcessResult{Cause: "output_overflow"}, true, errors.New("x"), "output_overflow"},
		{"unknown", ProcessResult{}, true, errors.New("x"), "execution"},
	} {
		if got := diagnosticKind(tc.process, tc.ran, tc.err); got != tc.want {
			t.Errorf("%s: kind = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLastEventTypeOnlyAcceptsShortIdentifiers(t *testing.T) {
	for in, want := range map[string]string{
		"{\"type\":\"turn.completed\"}\n":                    "turn.completed",
		"{\"type\":\"turn.started\"}\n{\"type\":\"result\"}": "result",
		"":                                      "",
		"plain text without json":               "",
		"{\"type\":\"Has Spaces And Secrets\"}": "",
	} {
		if got := lastEventType([]byte(in)); got != want {
			t.Errorf("lastEventType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCallNonzeroExitRealProcess(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr, kind, reason string
		temporary                          bool
	}{
		{"rate", `{"type":"turn.failed","error":{"message":"rate limit"}}`, "", "rate_limit", "rate_limit", true},
		{"authentication", "", "authentication failed", "authentication", "authentication", false},
		{"usage", "", "usage limit exceeded", "usage_limit", "usage_limit", false},
		{"unknown", `{"type":"turn.failed","error":{"message":"unexpected"}}`, "", "execution", "unknown", false},
		{"unrelated bytes in failure", `{"type":"turn.failed","error":{"message":"unexpected 512 bytes"}}`, "", "execution", "unknown", false},
		{"unrelated bytes in stderr", "", "unexpected 512 bytes", "execution", "unknown", false},
		{"status code", `{"type":"turn.failed","error":{"message":"status code: 503"}}`, "", "execution", "provider_unavailable", false},
		{"http status", "", "HTTP/1.1 500", "execution", "provider_unavailable", false},
		{"error code", "", "error 599", "execution", "provider_unavailable", false},
		{"outside range", "", "status 5000", "execution", "unknown", false},
		{"overloaded", `{"type":"turn.failed","error":{"message":"overloaded SECRET https://example.test/private"}}`, "", "execution", "provider_unavailable", false},
		{"disconnected", "", "connection reset SECRET https://example.test/private", "execution", "provider_unavailable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, dir := diagnosticFixture(t)
			cli := filepath.Join(dir, "codex")
			script := "#!/bin/sh\nif [ \"$1\" = login ]; then echo ChatGPT; exit 0; fi\nprintf '%s\\n' '" + tc.stdout + "'\nprintf '%s\\n' '" + tc.stderr + "' >&2\nexit 1\n"
			if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			answer, err := (&Runner{Codex: cli}).Call(context.Background(), request)
			var failure *Failure
			if !errors.As(err, &failure) || failure.Kind != tc.kind || failure.Temporary != tc.temporary {
				t.Fatalf("failure=%+v err=%v", failure, err)
			}
			if answer.Diagnostic.Kind != "exit_nonzero" || answer.Diagnostic.FailureReason != tc.reason {
				t.Fatalf("diagnostic=%+v", answer.Diagnostic)
			}
			mapped := strings.Join(mapStrings(answer.Diagnostic.Map()), "|")
			if strings.Contains(mapped, "SECRET") || strings.Contains(mapped, "https://example.test/private") {
				t.Fatalf("content leaked: %s", mapped)
			}
		})
	}
}

func TestFailureReasonInputTooLong(t *testing.T) {
	for _, tc := range []struct{ provider, output string }{
		{"codex", `{"type":"turn.failed","error":{"message":"Your input exceeds the context window limit"}}`},
		{"claude", `{"type":"result","is_error":true,"result":"Prompt is too long"}`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			answer := callWithFailureOutput(t, tc.provider, tc.output)
			if answer.Diagnostic.FailureReason != "input_invalid" {
				t.Fatalf("diagnostic=%+v", answer.Diagnostic)
			}
		})
	}
}

func TestClaudeFailureHTTPStatus(t *testing.T) {
	for _, tc := range []struct{ name, message, want string }{
		{"unrelated bytes", "unexpected 512 bytes", "unknown"},
		{"status", "HTTP status: 503", "provider_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := callWithFailureOutput(t, "claude", `{"type":"result","is_error":true,"result":"`+tc.message+`"}`)
			if answer.Diagnostic.FailureReason != tc.want {
				t.Fatalf("diagnostic=%+v, want %s", answer.Diagnostic, tc.want)
			}
		})
	}
}

func TestFailureReasonKeepsContentOut(t *testing.T) {
	for _, tc := range []struct{ provider, output string }{
		{"codex", `{"type":"turn.failed","error":{"message":"overloaded SECRET-CONTENT https://example.test/private"}}`},
		{"claude", `{"type":"result","is_error":true,"result":"Prompt is too long SECRET-CONTENT https://example.test/private"}`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			answer := callWithFailureOutput(t, tc.provider, tc.output)
			mapped := strings.Join(mapStrings(answer.Diagnostic.Map()), "|")
			if strings.Contains(mapped, "SECRET-CONTENT") || strings.Contains(mapped, "https://example.test/private") {
				t.Fatalf("content leaked: %s", mapped)
			}
		})
	}
}

func callWithFailureOutput(t *testing.T, provider, output string) Answer {
	t.Helper()
	request, dir := diagnosticFixture(t)
	request.Provider = provider
	cli := filepath.Join(dir, "provider")
	script := "#!/bin/sh\nif [ \"$1\" = login ]; then echo ChatGPT; exit 0; fi\nif [ \"$1\" = auth ]; then echo '{\"loggedIn\":true,\"apiProvider\":\"firstParty\",\"authMethod\":\"oauth_token\"}'; exit 0; fi\nprintf '%s\\n' '" + output + "'\nexit 1\n"
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	answer, err := (&Runner{Codex: cli, Claude: cli}).Call(context.Background(), request)
	if err == nil {
		t.Fatal("expected failure")
	}
	return answer
}
