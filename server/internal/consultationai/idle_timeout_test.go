package consultationai

import (
	"context"
	"database/sql"
	"errors"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/store"
	"issue-desk-intake/server/internal/usage"
	"os"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/operations"
)

func TestIdleTimeoutKnownStopsAndMissingRetriesOnce(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "known"}[known], func(t *testing.T) {
			s, d := testService(t, nil)
			config, _ := s.options.Config(t.Context())
			b, _ := s.budget(d, config)
			calls := 0
			s.options.RecordUsage = func(context.Context, ai.Request, ai.Usage, time.Duration, bool) error { return nil }
			s.options.Runner = &ai.Runner{Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
				if r.Arguments[0] == "login" {
					return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				calls++
				output := `{"type":"turn.started"}` + "\n"
				if known {
					output += `{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":2,"output_tokens":3}}` + "\n"
				}
				return ai.ProcessResult{Stdout: []byte(output), Cause: "timeout_idle"}, measuredTimeout{}
			}}
			_, err := s.call(t.Context(), d, config, models.Settings{Provider: "codex", Model: "fixture", Effort: "high"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil)
			wantCalls := 1
			if !known {
				wantCalls = 2
			}
			if !ai.IsIdleTimeout(err) || calls != wantCalls {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			if known {
				if isMeasurementFailure(err) || fleet.Number(b.value["unknownCalls"]) != 0 || d["usageMeasurementHold"] != nil {
					t.Fatal(err, b.value, d)
				}
			} else if !isMeasurementFailure(err) || fleet.Number(b.value["unknownCalls"]) != 2 || d["usageMeasurementHold"] == nil {
				t.Fatal(err, b.value, d)
			}
		})
	}
}

func TestProxyIdleTimeoutForwardsOnlyWhenUsageKnown(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "known", false: "unknown"}[known], func(t *testing.T) {
			calls := 0
			runner := &ai.Runner{Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
				if r.Arguments[0] == "login" {
					return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				calls++
				output := "{\"type\":\"turn.started\"}\n"
				if known {
					output += "{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"cached_input_tokens\":2,\"output_tokens\":3}}\n"
				}
				return ai.ProcessResult{Stdout: []byte(output), Cause: "timeout_idle"}, measuredTimeout{}
			}}
			s, d := testService(t, func(ctx context.Context, r ai.Request) (ai.Answer, error) {
				if r.Step == usage.ProxyConfirmationStep {
					return runner.Call(ctx, r)
				}
				return measuredAnswer(reply()), nil
			})
			s.options.RecordUsage = func(context.Context, ai.Request, ai.Usage, time.Duration, bool) error { return nil }
			config, _ := s.options.Config(t.Context())
			fleet.Object(fleet.Object(config["intake"])["proxy"])["enabled"] = true
			result, err := s.Turn(t.Context(), d)
			wantCalls := 1
			if !known {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("物理呼び出し回数=%d", calls)
			}
			saved, _, _ := s.options.Records.Get(t.Context(), "drafts", draftID)
			if known {
				if err != nil || fleet.Text(result["reply"]) != "質問です" || len(fleet.Elements(result["questions"])) != 1 || len(fleet.Elements(result["questionReasons"])) != 1 || saved["proxyPending"] != nil || fleet.Number(saved["attempts"]) != 0 {
					t.Fatalf("result=%v err=%v saved=%v", result, err, saved)
				}
			} else if !isMeasurementFailure(err) || saved["usageMeasurementHold"] == nil {
				t.Fatalf("err=%v saved=%v", err, saved)
			}
		})
	}
}

func TestStructuredAndScopeIdleTimeoutDoNotRetry(t *testing.T) {
	for _, kind := range []string{"close-review", "ops-watch", "scope"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			s, _ := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
				calls++
				answer := measuredAnswer(nil)
				answer.Diagnostic = ai.Diagnostic{Kind: "timeout_idle", Stage: "exit"}
				return answer, measuredTimeout{}
			})
			var err error
			if kind == "scope" {
				_, err = s.EstimateScope(t.Context(), "owner/app", map[string]any{"body": "依頼"})
			} else {
				_, err = s.Answer(t.Context(), map[string]any{"kind": kind, "schema": `{"type":"object"}`})
			}
			if !ai.IsIdleTimeout(err) || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestIdleTimeoutEngineIntegration(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "known", false: "unknown"}[known], func(t *testing.T) {
			s, d := testService(t, nil)
			failures := operations.NewAIFailures(t.Context(), t.TempDir(), nil)
			defer failures.Close()
			now := time.Now()
			for i := 0; i < 3; i++ {
				failures.Observe(ai.Observation{StartedAt: now, FinishedAt: now, Outcome: "failed", Cause: "timeout", DiagnosticKind: "timeout_idle", Model: "baseline"})
			}
			checkCount := func() {
				t.Helper()
				rows, err := failures.Alerts(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				for _, raw := range rows {
					row := raw.(map[string]any)
					if row["kind"] == "intake_ai_idle_timeout" {
						if !strings.Contains(row["detail"].(string), "4件") {
							t.Fatal(row)
						}
						return
					}
				}
				t.Fatal("idle timeout alert missing", rows)
			}
			calls := 0
			s.options.Runner = &ai.Runner{ObserveResult: failures.Observe, Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
				if r.Arguments[0] == "login" {
					return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				calls++
				output := "{\"type\":\"turn.started\"}\n"
				if known {
					output += "{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"cached_input_tokens\":2,\"output_tokens\":3}}\n"
				}
				return ai.ProcessResult{Stdout: []byte(output), Cause: "timeout_idle"}, measuredTimeout{}
			}}
			ctx := t.Context()
			a, err := auth.New(auth.Config{}, &store.AuthStore{})
			if err != nil {
				t.Fatal(err)
			}
			var engine *consultations.Engine
			startEnabled := true
			cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: a, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(ctx context.Context, id string, d consultations.Record, p auth.Principal) (consultations.Record, error) {
				if !startEnabled {
					return d, nil
				}
				return engine.Start(ctx, id, d, p)
			}})
			if err != nil {
				t.Fatal(err)
			}
			engine, err = consultations.NewEngine(ctx, consultations.EngineOptions{Service: cs, Turn: s.Turn})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			_, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, r consultations.Record) (consultations.Record, error) {
				r["attempts"] = 2
				r["timeoutStreak"] = 2
				r["waitStartedAt"] = s.options.Now().Format(time.RFC3339Nano)
				return r, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = engine.Start(ctx, draftID, d, auth.Principal{Kind: auth.Screen}); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for engine.Busy(draftID) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if engine.Busy(draftID) {
				t.Fatal("engine did not finish")
			}
			saved, _, err := s.options.Records.Get(ctx, "drafts", draftID)
			if err != nil || calls != 1 || saved["status"] != "error" || fleet.Number(saved["attempts"]) != 2 || fleet.Number(saved["timeoutStreak"]) != 2 || saved["retryAt"] != nil {
				t.Fatalf("calls=%d draft=%v err=%v", calls, saved, err)
			}
			checkCount()
			if known && saved["usageMeasurementHold"] != nil {
				t.Fatal(saved)
			}
			if !known {
				if saved["usageMeasurementHold"] == nil {
					t.Fatal(saved)
				}
				// Keep the accepted reply available for inspection before another turn.
				startEnabled = false
				_, err = cs.Chat(ctx, draftID, consultations.Record{"messageId": "22222222-2222-4222-8222-222222222222", "revision": saved["revision"], "text": "範囲を絞ります"}, auth.Principal{Kind: auth.Screen})
				if err != nil {
					t.Fatal(err)
				}
				saved, _, _ = s.options.Records.Get(ctx, "drafts", draftID)
				checkCount()
				if saved["usageMeasurementHold"] != nil || fleet.Number(fleet.Object(saved["consultationBudget"])["unknownCalls"]) != 0 || len(fleet.Elements(fleet.Object(saved["consultationBudget"])["measurementReleases"])) != 1 {
					t.Fatalf("reply err=%v draft=%v", err, saved)
				}
			}
		})
	}
}

func TestWrapIdleTimeoutPreservesCauseAndAvoidsDoubleWrapping(t *testing.T) {
	cause := errors.New("measurement failed")
	idle := ai.Answer{Diagnostic: ai.Diagnostic{Kind: "timeout_idle"}}
	wrapped := wrapIdleTimeout(idle, cause)
	if !ai.IsIdleTimeout(wrapped) || !errors.Is(wrapped, cause) || wrapIdleTimeout(idle, wrapped) != wrapped {
		t.Fatal(wrapped)
	}
	if wrapIdleTimeout(ai.Answer{}, cause) != cause || wrapIdleTimeout(idle, nil) != nil {
		t.Fatal("unexpected wrapping")
	}
}

func TestIdleTimeoutWrapsBudgetMeasurementSaveFailure(t *testing.T) {
	s, d := testService(t, nil)
	config, _ := s.options.Config(t.Context())
	b, _ := s.budget(d, config)
	s.options.Runner = &ai.Runner{Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
		if r.Arguments[0] == "login" {
			return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
		}
		return ai.ProcessResult{Stdout: []byte("{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"cached_input_tokens\":2,\"output_tokens\":3}}\n"), Cause: "timeout_idle"}, measuredTimeout{}
	}}
	s.options.RecordUsage = func(ctx context.Context, _ ai.Request, _ ai.Usage, _ time.Duration, _ bool) error {
		_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, r Record) (Record, error) { r["run"] = "other"; return r, nil })
		return err
	}
	_, err := s.call(t.Context(), d, config, models.Settings{Provider: "codex", Model: "fixture", Effort: "high"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil)
	if !ai.IsIdleTimeout(err) || !strings.Contains(err.Error(), "実行権を失いました") {
		t.Fatal(err)
	}
}

func TestRunnerMissingTimeoutRetry(t *testing.T) {
	for _, kind := range []string{"timeout_idle", "timeout_total"} {
		for _, secondMissing := range []bool{false, true} {
			t.Run(kind+map[bool]string{true: "/missing", false: "/success"}[secondMissing], func(t *testing.T) {
				s, d := testService(t, nil)
				config, _ := s.options.Config(t.Context())
				b, _ := s.budget(d, config)
				calls := 0
				var rows []ai.Usage
				var ids []string
				s.options.RecordUsage = func(_ context.Context, r ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
					ids = append(ids, r.CallID)
					rows = append(rows, u)
					return nil
				}
				s.options.Runner = &ai.Runner{Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
					if r.Arguments[0] == "login" {
						return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
					}
					calls++
					if calls == 1 || secondMissing {
						return ai.ProcessResult{Stdout: []byte("{\"type\":\"turn.started\"}\n"), Cause: kind}, measuredTimeout{}
					}
					for i, arg := range r.Arguments {
						if arg == "-o" {
							if err := os.WriteFile(r.Arguments[i+1], []byte(`{"ok":true}`), 0600); err != nil {
								t.Fatal(err)
							}
						}
					}
					return ai.ProcessResult{Stdout: []byte("{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"cached_input_tokens\":2,\"output_tokens\":3}}\n")}, nil
				}}
				answer, err := s.call(t.Context(), d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil)
				if calls != 2 || len(rows) != 2 || rows[0].InputTokens != nil || ids[0] == "" || ids[0] == ids[1] {
					t.Fatalf("calls=%d rows=%v ids=%v err=%v", calls, rows, ids, err)
				}
				if secondMissing {
					if !isMeasurementFailure(err) || fleet.Number(b.value["unknownCalls"]) != 2 {
						t.Fatalf("err=%v budget=%v", err, b.value)
					}
				} else if err != nil || answer.Value["ok"] != true || rows[1].InputTokens == nil || fleet.Number(b.value["unknownCalls"]) != 1 {
					t.Fatalf("answer=%v err=%v budget=%v", answer, err, b.value)
				}
			})
		}
	}
}

func TestRunnerInterruptedRetryKeepsStoredUnknownAfterRestart(t *testing.T) {
	for _, kind := range []string{"timeout_idle", "timeout_total"} {
		t.Run(kind, func(t *testing.T) {
			s, d := testService(t, nil)
			config, _ := s.options.Config(t.Context())
			b, _ := s.budget(d, config)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			migrations, err := store.Migrations()
			if err != nil {
				t.Fatal(err)
			}
			for _, migration := range migrations {
				if migration.Name == "usage_rows" {
					if _, err := s.options.Records.DB.ExecContext(ctx, migration.SQL); err != nil {
						t.Fatal(err)
					}
				}
			}
			calls := 0
			s.options.Runner = &ai.Runner{Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
				if r.Arguments[0] == "login" {
					return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				calls++
				if calls == 1 {
					return ai.ProcessResult{Cause: kind, Stdout: []byte("{\"type\":\"turn.started\"}\n")}, measuredTimeout{}
				}
				cancel()
				return ai.ProcessResult{Cause: "canceled_by_server"}, context.Canceled
			}}
			store := &usage.Store{DB: s.options.Records.DB}
			s.options.RecordUsage = func(ctx context.Context, r ai.Request, u ai.Usage, _ time.Duration, _ bool) error {
				row := map[string]any{"at": "2026-11-01T00:00:00.000Z", "host": "nas", "source": "intake", "provider": r.Provider, "model": r.Model, "step": r.Step, "callId": r.CallID}
				if u.InputTokens != nil {
					row["inputTokens"] = float64(*u.InputTokens)
				}
				counts, err := store.AddRows(context.WithoutCancel(ctx), "intake", []map[string]any{row})
				if err == nil && counts.Added != 1 {
					return errors.New("usage not saved")
				}
				return err
			}
			_, err = s.call(ctx, d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, b, nil, nil)
			if !errors.Is(err, context.Canceled) || calls != 2 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			rows, _, err := store.Read(context.Background(), "", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
			if err != nil || len(rows) != 2 {
				t.Fatalf("rows=%v err=%v", rows, err)
			}
			for _, row := range rows {
				if _, ok := row["inputTokens"]; ok {
					t.Fatal("missing usage changed", row)
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
			if !isMeasurementFailure(ensureMeasurement(saved, fresh)) {
				t.Fatalf("restart allowed: %v", fresh.value)
			}
		})
	}
}
