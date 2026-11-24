package consultationai

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/store"
	"issue-desk-intake/server/internal/usage"
)

type drainingRunner struct {
	call func(context.Context, ai.Request) (ai.Answer, error)
}

func (r drainingRunner) Call(ctx context.Context, req ai.Request) (ai.Answer, error) {
	return r.call(ctx, req)
}

func TestDrainingPathsRecoverThroughEngine(t *testing.T) {
	for _, step := range []string{usage.ProxyConfirmationStep} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrapped=%t", step, wrapped), func(t *testing.T) {
				var rejected atomic.Bool
				var targetCalls atomic.Int32
				s, d := testService(t, nil)
				config, _ := s.options.Config(t.Context())
				fleet.Object(fleet.Object(config["intake"])["proxy"])["enabled"] = true
				_, err := s.options.Records.Change(t.Context(), "drafts", draftID, func(_ *sql.Tx, _ Record) (Record, error) { return copyRecord(d), nil })
				if err != nil {
					t.Fatal(err)
				}
				s.options.Runner = drainingRunner{call: func(ctx context.Context, r ai.Request) (ai.Answer, error) {
					if r.Step == step {
						targetCalls.Add(1)
						if !rejected.Swap(true) {
							var err error = contract.NewProblem(503, "draining", "SERVER_DRAINING")
							if wrapped {
								err = fmt.Errorf("wrapped: %w", err)
							}
							return ai.Answer{}, err
						}
					}
					if r.BeforeCall != nil {
						if err := r.BeforeCall(); err != nil {
							return ai.Answer{}, err
						}
					}
					switch r.Step {
					case usage.ProxyConfirmationStep:
						return measuredAnswer(map[string]any{"answers": []any{map[string]any{"index": 1, "decision": "forward"}}}), nil
					default:
						return measuredAnswer(reply()), nil
					}
				}}
				ctx := t.Context()
				a, err := auth.New(auth.Config{}, &store.AuthStore{})
				if err != nil {
					t.Fatal(err)
				}
				cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: a, Now: s.options.Now, Project: func(string) error { return nil }})
				if err != nil {
					t.Fatal(err)
				}
				stalls, err := consultations.NewErrorStalls(ctx, cs, "revision", t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				old, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: cs, Turn: s.Turn, Stalls: stalls})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = old.Start(ctx, draftID, d, auth.Principal{Kind: auth.Screen}); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(10 * time.Second)
				var saved Record
				for {
					saved, _, err = s.options.Records.Get(ctx, "drafts", draftID)
					if err != nil {
						t.Fatal(err)
					}
					if saved["retryAt"] != nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("rejection not saved: %v", saved)
					}
					time.Sleep(time.Millisecond)
				}
				if saved["status"] != "thinking" || fleet.Number(saved["attempts"]) != 0 || saved["lastFailure"] != nil || saved["error"] != nil {
					t.Fatalf("saved: %v", saved)
				}
				if step == usage.ProxyConfirmationStep && (fleet.Object(saved["proxyPending"]) == nil || fleet.Object(saved["progress"]) == nil) {
					t.Fatalf("proxy progress: %v", saved)
				}
				var failures int
				if err := s.options.Records.DB.QueryRow("SELECT COUNT(*) FROM intake_error_failures").Scan(&failures); err != nil || failures != 0 {
					t.Fatalf("failures=%d err=%v", failures, err)
				}
				if err := old.Quiesce(ctx); err != nil {
					t.Fatal(err)
				}
				old.Close()
				_, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, r Record) (Record, error) {
					r["retryAt"] = s.options.Now().Add(-time.Second).Format(time.RFC3339Nano)
					return r, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				next, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: cs, Turn: s.Turn})
				if err != nil {
					t.Fatal(err)
				}
				defer next.Close()
				if err := next.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				deadline = time.Now().Add(10 * time.Second)
				for {
					saved, _, err = s.options.Records.Get(ctx, "drafts", draftID)
					if err != nil {
						t.Fatal(err)
					}
					if saved["status"] != "thinking" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("recovery: %v", saved)
					}
					time.Sleep(time.Millisecond)
				}
				if saved["status"] != "draft" || targetCalls.Load() != 2 || saved["proxyPending"] != nil || fleet.Number(saved["attempts"]) != 0 {
					t.Fatalf("recovered calls=%d saved=%v", targetCalls.Load(), saved)
				}
			})
		}
	}
}

func TestDrainingOnlyMatchesStatusAndCode(t *testing.T) {
	for _, err := range []error{nil, errors.New("other"), contract.NewProblem(502, "other", "SERVER_DRAINING"), contract.NewProblem(503, "other", "OTHER")} {
		if isServerDraining(err) {
			t.Fatalf("unexpected match: %v", err)
		}
	}
}

func TestOtherCallFailuresKeepFallbacks(t *testing.T) {
	for _, step := range []string{usage.ProxyConfirmationStep} {
		for _, failure := range []error{errors.New("ordinary failure"), contract.NewProblem(502, "wrong status", "SERVER_DRAINING"), contract.NewProblem(503, "wrong code", "OTHER")} {
			t.Run(fmt.Sprintf("%s/%v", step, failure), func(t *testing.T) {
				s, d := testService(t, nil)
				config, _ := s.options.Config(t.Context())
				fleet.Object(fleet.Object(config["intake"])["proxy"])["enabled"] = true
				s.options.Runner = drainingRunner{call: func(ctx context.Context, r ai.Request) (ai.Answer, error) {
					if r.BeforeCall != nil {
						if err := r.BeforeCall(); err != nil {
							return ai.Answer{}, err
						}
					}
					if r.Step == step {
						return measuredAnswer(nil), failure
					}
					if r.Step == usage.ProxyConfirmationStep {
						return measuredAnswer(map[string]any{"answers": []any{map[string]any{"index": 1, "decision": "forward"}}}), nil
					}
					return measuredAnswer(reply()), nil
				}}
				result, err := s.Turn(t.Context(), d)
				if err != nil {
					t.Fatal(err)
				}
				saved, _, err := s.options.Records.Get(t.Context(), "drafts", draftID)
				if err != nil {
					t.Fatal(err)
				}
				if step == usage.ProxyConfirmationStep && (len(fleet.Elements(result["questionReasons"])) != 1 || saved["proxyPending"] != nil) {
					t.Fatalf("result=%v saved=%v", result, saved)
				}
			})
		}
	}
}
