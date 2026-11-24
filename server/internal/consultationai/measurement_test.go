package consultationai

import (
	"context"
	"database/sql"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/models"
)

type measuredTimeout struct{}

func (measuredTimeout) Error() string { return "fixture timeout" }
func (measuredTimeout) Timeout() bool { return true }
func measuredAnswer(value map[string]any) ai.Answer {
	input, output, read, write := int64(10), int64(3), int64(2), int64(0)
	complete := false
	return ai.Answer{Value: value, Usage: ai.Usage{InputTokens: &input, OutputTokens: &output, CacheReadTokens: &read, CacheWriteTokens: &write, Partial: &complete}}
}
func measurementSchema(t *testing.T, s *Service) string {
	t.Helper()
	p := filepath.Join(s.options.RuntimeDir, "measurement.schema.json")
	if err := os.WriteFile(p, []byte(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestPhysicalRunnerUnknownAndPartialRetryOnceAndKnownTimeoutRetries(t *testing.T) {
	const full = "{\"type\":\"turn.started\"}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"cached_input_tokens\":2,\"output_tokens\":3}}\n"
	for _, tc := range []struct {
		name, provider, output string
		truncated, known       bool
	}{
		{"欠測timeout", "codex", `{"type":"turn.started"}`, false, false},
		{"既知timeout", "codex", full, false, true},
		{"Codex実測0", "codex", `{"type":"turn.completed","usage":{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0}}`, false, true},
		{"後続turn未完了", "codex", full + `{"type":"turn.started"}`, false, false},
		{"末尾切詰め", "codex", full, true, false},
		{"Claude途中測定", "claude", `{"type":"assistant","message":{"id":"m","usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":0}}}`, false, false},
		{"Claude既知timeout", "claude", `{"type":"result","structured_output":{"ok":true},"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":0}}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d := testService(t, nil)
			config, _ := s.options.Config(t.Context())
			b, _ := s.budget(d, config)
			calls, records := 0, 0
			s.options.Wait = func(context.Context, time.Duration) error { return nil }
			s.options.RecordUsage = func(_ context.Context, _ ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
				records++
				if records == 1 && tc.output == `{"type":"turn.started"}` && u.InputTokens != nil {
					t.Fatal("欠測を0化")
				}
				return nil
			}
			s.options.Runner = &ai.Runner{Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
				if r.Arguments[0] == "login" {
					return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				if r.Arguments[0] == "auth" {
					return ai.ProcessResult{Stdout: []byte(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"claude.ai"}`)}, nil
				}
				calls++
				if calls == 1 {
					return ai.ProcessResult{Stdout: []byte(tc.output), Truncated: tc.truncated, Cause: "timeout_total"}, measuredTimeout{}
				}
				if !tc.known {
					return ai.ProcessResult{Stdout: []byte(tc.output), Truncated: tc.truncated, Cause: "timeout_total"}, measuredTimeout{}
				}
				for i, a := range r.Arguments {
					if a == "-o" {
						if err := os.WriteFile(r.Arguments[i+1], []byte(`{"ok":true}`), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				return ai.ProcessResult{Stdout: []byte(tc.output)}, nil
			}}
			_, err := s.call(t.Context(), d, config, models.Settings{Provider: tc.provider, Model: "fixture", Effort: "high"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil)
			want := 2
			if calls != want || records != want {
				t.Fatal(calls, records, err)
			}
			if tc.known {
				if err != nil || fleet.Number(b.value["unknownCalls"]) != 0 {
					t.Fatal(err, b.value)
				}
			} else {
				if !isMeasurementFailure(err) || fleet.Number(b.value["unknownCalls"]) != 2 || b.value["pendingCall"] != nil {
					t.Fatal(err, b.value)
				}
				if _, err = s.call(t.Context(), d, config, models.Settings{Provider: tc.provider, Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil); !isMeasurementFailure(err) || calls != 2 {
					t.Fatal("保存した未確定を無視", err, calls)
				}
			}
		})
	}
}

func TestUnknownBeforeCallAndBudgetExtensionCannotEraseEvidence(t *testing.T) {
	calls := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { calls++; return measuredAnswer(reply()), nil })
	d["consultationBudget"] = Record{"calls": 3, "weighted": 53838, "unknownCalls": 1, "added": Record{"calls": 40, "weighted": 6000000}}
	if _, err := s.options.Records.Change(t.Context(), "drafts", draftID, func(*sql.Tx, Record) (Record, error) { return copyRecord(d), nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Turn(t.Context(), d); !isMeasurementFailure(err) || calls != 0 {
		t.Fatal(err, calls)
	}
	saved, _, _ := s.options.Records.Get(t.Context(), "drafts", draftID)
	if string(marshal(saved["consultationBudget"])) != string(marshal(d["consultationBudget"])) {
		t.Fatal("既存未確定を消しました")
	}
}
func TestCapturedUsageAuditedEvenWhenStopFenceRejectsBudgetSave(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, d := testService(t, nil)
	config, _ := s.options.Config(ctx)
	b, _ := s.budget(d, config)
	records := 0
	var stopped string
	s.options.RecordUsage = func(_ context.Context, _ ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
		records++
		if u.InputTokens == nil || *u.InputTokens != 10 {
			t.Fatal(u)
		}
		return nil
	}
	s.options.Runner = callFunc(func(context.Context, ai.Request) (ai.Answer, error) {
		_, err := s.options.Records.Change(t.Context(), "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) {
			current["status"], current["run"], current["revision"] = "stalled", nil, 1
			stopped = string(marshal(current))
			return current, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		return measuredAnswer(nil), context.Canceled
	})
	_, err := s.call(ctx, d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil)
	if err == nil || records != 1 {
		t.Fatal(err, records)
	}
	saved, _, _ := s.options.Records.Get(t.Context(), "drafts", draftID)
	if string(marshal(saved)) != stopped {
		t.Fatal("停止後にdraftを更新しました")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal(ctx.Err())
	}
}

func TestProxyDoesNotConvertMeasurementFailureToNormalForward(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		if r.Step == usage.ProxyConfirmationStep {
			return ai.Answer{}, measuredTimeout{}
		}
		return measuredAnswer(reply()), nil
	})
	config, _ := s.options.Config(t.Context())
	fleet.Object(fleet.Object(config["intake"])["proxy"])["enabled"] = true
	_, err := s.Turn(t.Context(), d)
	if !isMeasurementFailure(err) || calls != 2 {
		t.Fatal(err, calls)
	}
	saved, _, _ := s.options.Records.Get(t.Context(), "drafts", draftID)
	if fleet.Object(saved["usageMeasurementHold"])["step"] != usage.ProxyConfirmationStep {
		t.Fatal("代理確認の未確定を保存しません")
	}
	if _, err = s.Turn(t.Context(), saved); !isMeasurementFailure(err) || calls != 2 {
		t.Fatal("未確定後に再呼出し", err, calls)
	}
}
func TestKnownMeasurementDoesNotEraseEarlierUnknownCall(t *testing.T) {
	s, d := testService(t, nil)
	config, _ := s.options.Config(t.Context())
	b, _ := s.budget(d, config)
	b.value["calls"], b.value["unknownCalls"], b.value["weighted"] = 3, 2, 53838
	b.value["pendingCall"] = Record{"id": "pending", "at": s.options.Now().Format(time.RFC3339Nano)}
	if err := b.persist(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := b.measured(t.Context(), ai.Request{Provider: "codex", Model: "fixture"}, measuredAnswer(nil).Usage); err != nil {
		t.Fatal(err)
	}
	if fleet.Number(b.value["unknownCalls"]) != 1 || fleet.Number(b.value["calls"]) != 3 || fleet.Number(b.value["weighted"]) <= 53838 {
		t.Fatal("既存未確定を消しました", b.value)
	}
}
func TestOnlyMeasuredRateLimitMayWaitAndRetry(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "欠測は待機前に停止", true: "既知測定は従来再試行"}[known], func(t *testing.T) {
			calls, waits := 0, 0
			s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
				calls++
				if calls == 1 {
					answer := ai.Answer{}
					if known {
						answer = measuredAnswer(nil)
					}
					return answer, &ai.Failure{Kind: "rate_limit", Message: "fixture"}
				}
				return measuredAnswer(map[string]any{"ok": true}), nil
			})
			now := s.options.Now()
			s.options.Now = func() time.Time { return now }
			s.options.Wait = func(_ context.Context, delay time.Duration) error { waits++; now = now.Add(delay); return nil }
			config, _ := s.options.Config(t.Context())
			b, _ := s.budget(d, config)
			_, err := s.call(t.Context(), d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil)
			if known {
				if err != nil || calls != 2 || waits != 1 {
					t.Fatal(err, calls, waits)
				}
			} else {
				if !isMeasurementFailure(err) || calls != 1 || waits != 0 {
					t.Fatal(err, calls, waits)
				}
				var failure *ai.Failure
				if !errors.As(err, &failure) || failure.Transient() {
					t.Fatal("未確定を自動再試行へ戻しました", err)
				}
			}
		})
	}
}

func TestUnmeasuredHoldKeepsOriginalFailureAndDiagnosticWithoutLeakingContent(t *testing.T) {
	s, d := testService(t, nil)
	config, _ := s.options.Config(t.Context())
	b, _ := s.budget(d, config)
	s.options.RecordUsage = func(context.Context, ai.Request, ai.Usage, time.Duration, bool) error { return nil }
	s.options.Runner = &ai.Runner{Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
		if r.Arguments[0] == "login" {
			return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
		}
		return ai.ProcessResult{Stdout: []byte(`{"type":"turn.started"}`), Stderr: []byte("SECRET-STDERR"), Cause: "timeout_idle", StdoutBytes: 23, StderrBytes: 13, Duration: 240 * time.Second, FirstOutput: time.Second, LastOutput: 2 * time.Second}, measuredTimeout{}
	}}
	settings := models.Settings{Provider: "codex", Model: "fixture", Effort: "high"}
	_, err := s.call(t.Context(), d, config, settings, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil)
	if !isMeasurementFailure(err) {
		t.Fatal(err)
	}
	for _, want := range []string{"元の呼び出し: 無出力が続いて時間切れ", "段階 exit", "診断ID "} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("停止の理由に %q がありません: %s", want, err.Error())
		}
	}
	saved, _, _ := s.options.Records.Get(t.Context(), "drafts", draftID)
	hold := fleet.Object(saved["usageMeasurementHold"])
	diagnostic := fleet.Object(hold["diagnostic"])
	if hold["reason"] != "missing" || hold["originalFailure"] != "error" || diagnostic["kind"] != "timeout_idle" || diagnostic["stage"] != "exit" || diagnostic["finalEvent"] != "turn.started" {
		t.Fatalf("保留 = %v", hold)
	}
	if fleet.Number(diagnostic["stdoutBytes"]) != 23 || fleet.Number(diagnostic["stderrBytes"]) != 13 || diagnostic["exitCode"] != nil {
		t.Fatalf("診断 = %v", diagnostic)
	}
	if strings.Contains(string(marshal(saved["usageMeasurementHold"])), "SECRET") {
		t.Fatal("保留に本文が入っています")
	}
	// 保存した保留があるかぎり、次の呼び出しは実行されず、同じ理由を示す。
	again := ensureMeasurement(saved, nil)
	if !isMeasurementFailure(again) || !strings.Contains(again.Error(), "元の呼び出し: 無出力が続いて時間切れ") || !strings.Contains(again.Error(), fleet.Text(diagnostic["id"])) {
		t.Fatal(again)
	}
}

func TestMeasurementStopExplainsInterruptedAndUnknownCallsAndKeepsOldHoldMessage(t *testing.T) {
	pending := &callBudget{value: Record{"unknownCalls": 1.0, "pendingCall": Record{"id": "x", "at": "2026-11-02T03:23:27Z"}}}
	err := ensureMeasurement(Record{}, pending)
	if !isMeasurementFailure(err) || !strings.Contains(err.Error(), "完了の記録が無い呼び出し") || !strings.Contains(err.Error(), "2026-11-02T03:23:27Z") || !strings.Contains(err.Error(), "再起動") {
		t.Fatal(err)
	}
	unknown := &callBudget{value: Record{"unknownCalls": 2.0}}
	if err := ensureMeasurement(Record{}, unknown); !isMeasurementFailure(err) || !strings.Contains(err.Error(), "2件") {
		t.Fatal(err)
	}
	// 診断を持たない古い保留は、従来と同じ文のまま止まる。
	old := ensureMeasurement(Record{"usageMeasurementHold": Record{"reason": "missing"}}, nil)
	if !isMeasurementFailure(old) || old.Error() != measurementMessage {
		t.Fatal(old)
	}
	// 何も無ければ止めない。
	if err := ensureMeasurement(Record{}, &callBudget{value: Record{}}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedCallsLeaveCorrelatedDiagnosticsAndHistoryIsBounded(t *testing.T) {
	const full = "{\"type\":\"turn.started\"}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"cached_input_tokens\":2,\"output_tokens\":3}}\n"
	s, d := testService(t, nil)
	config, _ := s.options.Config(t.Context())
	b, _ := s.budget(d, config)
	calls := 0
	s.options.Wait = func(context.Context, time.Duration) error { return nil }
	s.options.RecordUsage = func(context.Context, ai.Request, ai.Usage, time.Duration, bool) error { return nil }
	s.options.Runner = &ai.Runner{Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
		if r.Arguments[0] == "login" {
			return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
		}
		calls++
		if calls == 1 {
			// 使用量は取れたが時間切れ → 保留にはならず、1回やり直す。この失敗の診断が残る。
			return ai.ProcessResult{Stdout: []byte(full), Cause: "timeout_total", StdoutBytes: 99}, measuredTimeout{}
		}
		for i, a := range r.Arguments {
			if a == "-o" {
				if err := os.WriteFile(r.Arguments[i+1], []byte(`{"ok":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return ai.ProcessResult{Stdout: []byte(full)}, nil
	}}
	settings := models.Settings{Provider: "codex", Model: "fixture", Effort: "high"}
	if _, err := s.call(t.Context(), d, config, settings, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil); err != nil {
		t.Fatal(err)
	}
	saved, _, _ := s.options.Records.Get(t.Context(), "drafts", draftID)
	history := fleet.Elements(saved["aiDiagnostics"])
	if len(history) != 1 {
		t.Fatalf("失敗した1回だけが残るはず: %v", history)
	}
	entry := fleet.Object(history[0])
	if entry["kind"] != "timeout_total" || entry["provider"] != "codex" || entry["model"] != "fixture" || entry["effort"] != "high" || entry["step"] != usage.DraftStep || fleet.Number(entry["attempt"]) != 1 || entry["at"] == nil || entry["id"] == "" {
		t.Fatalf("診断 = %v", entry)
	}
	if entry["finalEvent"] != "turn.completed" || strings.Contains(string(marshal(entry)), "input_tokens") {
		t.Fatalf("最後のイベント種別だけが入り、出力の中身は入らないはず: %v", entry)
	}

	// 件数の上限: 古いものから捨て、新しいものを残す。成功の診断は残さない。
	for i := 0; i < diagnosticHistoryLimit+5; i++ {
		answer := ai.Answer{Diagnostic: ai.Diagnostic{ID: "id-" + string(rune('a'+i)), Kind: "timeout_idle", Stage: "exit"}}
		s.rememberDiagnostic(t.Context(), d, ai.Request{Provider: "codex", Step: "x", Attempt: 1, Turn: 1}, answer)
	}
	s.rememberDiagnostic(t.Context(), d, ai.Request{}, ai.Answer{Diagnostic: ai.Diagnostic{ID: "ok", Kind: "completed"}})
	saved, _, _ = s.options.Records.Get(t.Context(), "drafts", draftID)
	history = fleet.Elements(saved["aiDiagnostics"])
	if len(history) != diagnosticHistoryLimit {
		t.Fatalf("件数 = %d, 上限 = %d", len(history), diagnosticHistoryLimit)
	}
	if fleet.Object(history[len(history)-1])["id"] != "id-"+string(rune('a'+diagnosticHistoryLimit+4)) {
		t.Fatalf("最新が残っていません: %v", history[len(history)-1])
	}
	for _, h := range history {
		if fleet.Object(h)["id"] == "ok" {
			t.Fatal("成功の診断を残しました")
		}
	}
}

func TestMissingTimeoutRetryBudgetAndRestart(t *testing.T) {
	for _, kind := range []string{"timeout_idle", "timeout_total"} {
		for _, scenario := range []string{"success", "missing_again", "record_failure", "interrupted", "limit", "existing", "pending", "other_missing"} {
			for _, nilBudget := range []bool{false, true} {
				t.Run(kind+"/"+scenario+map[bool]string{true: "/nil", false: "/shared"}[nilBudget], func(t *testing.T) {
					calls := 0
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					s, d := testService(t, func(_ context.Context, _ ai.Request) (ai.Answer, error) {
						calls++
						if calls == 1 || scenario == "missing_again" {
							k := kind
							if scenario == "other_missing" {
								k = "execution"
							}
							return ai.Answer{Diagnostic: ai.Diagnostic{ID: "diagnostic", Kind: k, Stage: "exit"}}, measuredTimeout{}
						}
						if scenario == "interrupted" {
							cancel()
							return ai.Answer{Diagnostic: ai.Diagnostic{Kind: "canceled_by_server"}}, context.Canceled
						}
						return measuredAnswer(map[string]any{"ok": true}), nil
					})
					config, _ := s.options.Config(ctx)
					if scenario == "limit" {
						fleet.Object(config["intake"])["consultationBudget"] = Record{"calls": 1}
					}
					if scenario == "existing" {
						d["consultationBudget"] = Record{"unknownCalls": 1}
					}
					if scenario == "pending" {
						d["consultationBudget"] = Record{"pendingCall": Record{"id": "old"}}
					}
					b, err := s.budget(d, config)
					if err != nil {
						t.Fatal(err)
					}
					var ids []string
					var measurements []ai.Usage
					s.options.RecordUsage = func(_ context.Context, req ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
						ids = append(ids, req.CallID)
						measurements = append(measurements, u)
						if scenario == "record_failure" {
							return errors.New("save failed")
						}
						return nil
					}
					passed := b
					if nilBudget {
						passed = nil
					}
					answer, err := s.call(ctx, d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, passed, nil, nil)
					switch scenario {
					case "success":
						if err != nil || answer.Value["ok"] != true || calls != 2 || len(ids) != 2 || ids[0] == ids[1] || ids[0] == "" || measurements[0].InputTokens != nil {
							t.Fatalf("err=%v calls=%d ids=%v", err, calls, ids)
						}
						if !nilBudget {
							_, err = s.call(ctx, d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil)
							if err != nil || calls != 3 || fleet.Number(b.value["unknownCalls"]) != 1 || fleet.Number(b.value["weighted"]) <= 0 {
								t.Fatalf("continuation err=%v calls=%d budget=%v", err, calls, b.value)
							}
						}
					case "missing_again":
						if !isMeasurementFailure(err) || calls != 2 || len(ids) != 2 {
							t.Fatalf("err=%v calls=%d", err, calls)
						}
					case "existing", "pending":
						if !isMeasurementFailure(err) || calls != 0 {
							t.Fatalf("err=%v calls=%d", err, calls)
						}
						return
					case "interrupted":
						if !errors.Is(err, context.Canceled) || calls != 2 {
							t.Fatalf("err=%v calls=%d", err, calls)
						}
					case "limit":
						if err == nil || calls != 1 {
							t.Fatalf("err=%v calls=%d", err, calls)
						}
					default:
						if !isMeasurementFailure(err) || calls != 1 {
							t.Fatalf("err=%v calls=%d", err, calls)
						}
					}
					saved, _, err := s.options.Records.Get(context.Background(), "drafts", draftID)
					if err != nil {
						t.Fatal(err)
					}
					fresh, err := s.budget(saved, config)
					if err != nil {
						t.Fatal(err)
					}
					if fleet.Number(fresh.value["unknownCalls"]) < 1 {
						t.Fatalf("lost unknown: %v", fresh.value)
					}
					if !isMeasurementFailure(ensureMeasurement(saved, fresh)) {
						t.Fatalf("restart admitted unknown: %v", fresh.value)
					}
				})
			}
		}
	}
}

func TestTimeoutRetryContinuesProxyTurn(t *testing.T) {
	for _, stage := range []string{usage.ProxyConfirmationStep} {
		t.Run(stage, func(t *testing.T) {
			stageCalls := 0
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				if r.Step == stage {
					stageCalls++
					if stageCalls == 1 {
						return ai.Answer{Diagnostic: ai.Diagnostic{ID: "timeout", Kind: "timeout_total", Stage: "exit"}}, measuredTimeout{}
					}
					return measuredAnswer(map[string]any{"answers": []any{map[string]any{"index": float64(1), "decision": "forward"}}, "readFiles": []any{}}), nil
				}
				return measuredAnswer(reply()), nil
			})
			config, _ := s.options.Config(t.Context())
			fleet.Object(fleet.Object(config["intake"])["proxy"])["enabled"] = true
			var ids []string
			s.options.RecordUsage = func(_ context.Context, r ai.Request, _ ai.Usage, _ time.Duration, _ bool) error {
				ids = append(ids, r.CallID)
				return nil
			}
			result, err := s.Turn(t.Context(), d)
			if err != nil || result == nil || stageCalls != 2 {
				t.Fatalf("result=%v err=%v stageCalls=%d", result, err, stageCalls)
			}
			saved, _, err := s.options.Records.Get(t.Context(), "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.budget(saved, config)
			if err != nil {
				t.Fatal(err)
			}
			if fleet.Number(b.value["unknownCalls"]) != 1 || fleet.Number(b.value["calls"]) < 3 || !isMeasurementFailure(ensureMeasurement(saved, b)) {
				t.Fatalf("budget=%v ids=%v", b.value, ids)
			}
		})
	}
}

func TestMeasurementGuardRechecksAfterWait(t *testing.T) {
	calls := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { calls++; return measuredAnswer(nil), nil })
	config, _ := s.options.Config(t.Context())
	b, _ := s.budget(d, config)
	d["rateLimitWait"] = Record{"provider": "codex", "until": s.options.Now().Add(time.Second).Format(time.RFC3339Nano)}
	s.options.Wait = func(ctx context.Context, _ time.Duration) error {
		_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, r Record) (Record, error) {
			r["consultationBudget"] = Record{"calls": 1, "unknownCalls": 1}
			return r, nil
		})
		return err
	}
	_, err := s.call(t.Context(), d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil)
	if !isMeasurementFailure(err) || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestSuccessfulTimeoutExceptionCannotBeRepeated(t *testing.T) {
	for _, kind := range []string{"timeout_idle", "timeout_total"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
				calls++
				if calls == 1 || calls == 3 {
					return ai.Answer{Diagnostic: ai.Diagnostic{ID: "timeout", Kind: kind, Stage: "exit"}}, measuredTimeout{}
				}
				return measuredAnswer(map[string]any{"ok": true}), nil
			})
			config, _ := s.options.Config(t.Context())
			b, _ := s.budget(d, config)
			s.options.RecordUsage = func(context.Context, ai.Request, ai.Usage, time.Duration, bool) error { return nil }
			call := func() error {
				_, err := s.call(t.Context(), d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil)
				return err
			}
			if err := call(); err != nil || calls != 2 {
				t.Fatalf("first err=%v calls=%d", err, calls)
			}
			if err := call(); !isMeasurementFailure(err) || calls != 3 {
				t.Fatalf("second err=%v calls=%d", err, calls)
			}
			if err := call(); !isMeasurementFailure(err) || calls != 3 {
				t.Fatalf("third err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestMeasurementSaveDoesNotOverwriteConcurrentUnknown(t *testing.T) {
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		return measuredAnswer(map[string]any{"ok": true}), nil
	})
	config, _ := s.options.Config(t.Context())
	b, _ := s.budget(d, config)
	s.options.RecordUsage = func(ctx context.Context, _ ai.Request, _ ai.Usage, _ time.Duration, _ bool) error {
		_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, r Record) (Record, error) {
			v := fleet.Object(r["consultationBudget"])
			v["unknownCalls"] = fleet.Number(v["unknownCalls"]) + 1
			return r, nil
		})
		return err
	}
	_, err := s.call(t.Context(), d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil)
	if !isMeasurementFailure(err) {
		t.Fatal(err)
	}
	saved, _, err := s.options.Records.Get(t.Context(), "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	v := fleet.Object(saved["consultationBudget"])
	if fleet.Number(v["unknownCalls"]) != 2 || v["pendingCall"] == nil {
		t.Fatalf("overwrote unknown: %v", v)
	}
}

func TestMeasurementHoldGuidanceMatchesReleaseEligibility(t *testing.T) {
	for _, tc := range []struct {
		reason string
		resume bool
	}{
		{"input_invalid", false}, {"authentication", false}, {"usage_limit", false}, {"rate_limit", false}, {"unknown", true}, {"", true},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			hold := Record{"reason": "missing", "originalFailure": "execution", "diagnostic": Record{"kind": "exit_nonzero", "failureReason": tc.reason}}
			err := measurementHoldFailure(hold, nil)
			if !isMeasurementFailure(err) {
				t.Fatal(err)
			}
			if got := strings.Contains(err.Error(), "相談へ返信すると再開します"); got != tc.resume {
				t.Fatalf("guidance=%s", err)
			}
			if !tc.resume && !strings.Contains(err.Error(), "返信では解除できません") {
				t.Fatal(err)
			}
		})
	}
}

func TestMeasurementHoldGuidanceAfterReleaseLimit(t *testing.T) {
	hold := Record{"reason": "missing", "originalFailure": "execution", "diagnostic": Record{"kind": "exit_nonzero", "failureReason": "unknown"}}
	budget := Record{"measurementReleases": []any{Record{}, Record{}, Record{}}}
	err := measurementHoldFailure(hold, budget)
	if !strings.Contains(err.Error(), "返信では解除できません") || strings.Contains(err.Error(), "相談へ返信すると再開します") {
		t.Fatal(err)
	}
}

// 未確定の件数だけが残った停止の案内は、返信で解除できるときだけ再開を約束する。
func TestUnknownCallGuidanceMatchesReleaseEligibility(t *testing.T) {
	for _, tc := range []struct {
		name        string
		diagnostics []any
		releases    []any
		resume      bool
	}{
		{"timeout", []any{Record{"kind": "timeout_idle", "id": "d1"}}, nil, true},
		{"no-diagnostic", nil, nil, false},
		{"other", []any{Record{"kind": "authentication", "id": "d1"}}, nil, false},
		{"limit", []any{Record{"kind": "timeout_total", "id": "d1"}}, []any{Record{}, Record{}, Record{}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Record{"aiDiagnostics": tc.diagnostics}
			budget := &callBudget{value: Record{"unknownCalls": float64(1), "measurementReleases": tc.releases}}
			err := ensureMeasurement(d, budget)
			if !isMeasurementFailure(err) {
				t.Fatal(err)
			}
			if got := strings.Contains(err.Error(), "相談へ返信すると再開します"); got != tc.resume {
				t.Fatalf("guidance=%s", err)
			}
			if !tc.resume && !strings.Contains(err.Error(), "返信では解除できません") {
				t.Fatal(err)
			}
		})
	}
}
