package consultationai

import (
	"context"
	"database/sql"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/usage"
)

func TestTurnCallTriggers(t *testing.T) {
	cases := []struct {
		name     string
		messages []any
		first    map[string]any
		want     []string
	}{
		{"initial", nil, nil, []string{"initial"}},
		{"reply", []any{Record{"role": "user", "text": "初回"}, Record{"role": "assistant", "text": "質問"}, Record{"role": "user", "text": "返信"}}, nil, []string{"user_reply"}},
		{"invalid_read", nil, map[string]any{"readFiles": []any{42}}, []string{"initial", "automatic_continuation"}},
		{"repeated_read", nil, map[string]any{"readFiles": []any{Record{"file": "README.md", "mode": "lines", "start": 1, "end": 1}}}, []string{"initial", "context_read", "format_repair"}},
		{"reviewed", nil, map[string]any{"reviewedFiles": []any{"missing.go"}}, []string{"initial", "format_repair"}},
		{"environment", nil, map[string]any{"ready": true, "body": "https://example.com/x にアクセスする"}, []string{"initial", "automatic_continuation"}},
		{"scope", nil, map[string]any{"ready": true, "changeScope": Record{"known": true, "paths": []any{"a.go", "b.go", "c.go", "d.go", "e.go"}}}, []string{"initial"}},
		{"split_limit", nil, map[string]any{"splitIssues": make([]any, 26)}, []string{"initial", "format_repair"}},
		{"reference", nil, map[string]any{"ready": true, "title": "題", "body": "実装", "questions": []any{}}, []string{"initial", "context_read"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				assertTriggerAccepted(t, r)
				got = append(got, r.CallTrigger)
				if r.CallTrigger == "format_repair" {
					want := map[string]string{"repeated_read": "repeated_read", "reviewed": "reviewed_unconfirmed", "split_limit": "split_over_limit"}[tc.name]
					if r.RepairKind != want || r.RepairField != "" {
						t.Fatalf("repair: %+v", r)
					}
				}
				v := reply()
				if len(got) == 1 || tc.name == "repeated_read" && len(got) == 2 {
					for k, x := range tc.first {
						v[k] = x
					}
				}
				return measuredAnswer(v), nil
			})
			if tc.name != "reference" {
				s.options.Reference = nil
			}
			if tc.messages != nil {
				d["messages"] = tc.messages
			}
			_, err := s.Turn(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("triggers=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestTurnResumesSavedReadTrigger(t *testing.T) {
	var got []string
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		assertTriggerAccepted(t, r)
		got = append(got, r.CallTrigger)
		return measuredAnswer(reply()), nil
	})
	config, _ := s.options.Config(context.Background())
	b, err := s.budget(d, config)
	if err != nil {
		t.Fatal(err)
	}
	reads, err := b.reads()
	if err != nil {
		t.Fatal(err)
	}
	if err = reads.begin(context.Background(), []any{"README.md"}); err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Turn(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"context_read"}) {
		t.Fatalf("triggers=%v", got)
	}
}

func TestCallRetryTriggersAndInput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		want    string
	}{
		{"rate", &ai.Failure{Kind: "rate_limit"}, "rate_limit_retry"},
		{"timeout", measuredTimeout{}, "failure_retry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []ai.Request
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				assertTriggerAccepted(t, r)
				got = append(got, r)
				if len(got) == 1 {
					return measuredAnswer(nil), tc.failure
				}
				return measuredAnswer(reply()), nil
			})
			s.options.Wait = func(context.Context, time.Duration) error { return nil }
			config, _ := s.options.Config(t.Context())
			b, _ := s.budget(d, config)
			ctx := context.WithValue(t.Context(), callTriggerKey{}, "initial")
			_, err := s.call(ctx, d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "original input", "full input", nil, b, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[0].CallTrigger != "initial" || got[1].CallTrigger != tc.want {
				t.Fatalf("requests=%+v", got)
			}
			expected := "original input"
			if tc.name == "timeout" {
				expected = "full input"
			}
			if got[0].Input != "original input" || got[1].Input != expected {
				t.Fatalf("input changed: %q %q", got[0].Input, got[1].Input)
			}
		})
	}
}

func TestTurnFormatRepairTrigger(t *testing.T) {
	var got []string
	var recorded []ai.Request
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		assertTriggerAccepted(t, r)
		got = append(got, r.CallTrigger)
		if len(got) == 2 && (r.RepairKind != "draft_format" || r.RepairField != "") {
			t.Fatalf("repair: %+v", r)
		}
		if len(got) == 1 {
			return measuredAnswer(nil), &ai.Failure{Kind: "format"}
		}
		return measuredAnswer(reply()), nil
	})
	s.options.RecordUsage = func(_ context.Context, r ai.Request, _ ai.Usage, _ time.Duration, _ bool) error {
		recorded = append(recorded, r)
		return nil
	}
	if _, err := s.Turn(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 2 || recorded[1].RepairKind != "draft_format" || recorded[0].RepairKind != "" {
		t.Fatalf("recorded: %+v", recorded)
	}
	if !reflect.DeepEqual(got, []string{"initial", "format_repair"}) {
		t.Fatal(got)
	}
}

func TestCallTriggerDoesNotChangeInput(t *testing.T) {
	var inputs []string
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		assertTriggerAccepted(t, r)
		inputs = append(inputs, r.Input)
		return measuredAnswer(reply()), nil
	})
	config, _ := s.options.Config(t.Context())
	settings := models.Settings{Provider: "codex", Model: "fixture"}
	for _, trigger := range []string{"initial", "user_reply"} {
		ctx := context.WithValue(t.Context(), callTriggerKey{}, trigger)
		if _, err := s.call(ctx, d, config, settings, usage.DraftStep, measurementSchema(t, s), "入力文", "入力文", nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(inputs, []string{"入力文", "入力文"}) {
		t.Fatal(inputs)
	}
}

func TestTurnEscalationTrigger(t *testing.T) {
	var got []string
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		assertTriggerAccepted(t, r)
		got = append(got, r.CallTrigger)
		v := reply()
		if len(got) == 1 {
			v["escalate"] = true
			v["escalateReason"] = "難しい"
		}
		return measuredAnswer(v), nil
	})
	config, _ := s.options.Config(t.Context())
	config["intake"].(map[string]any)["tiers"] = map[string]any{"standard": map[string]any{"model": "fixture"}}
	d["tier"] = Record{"level": "light"}
	d["consultationProvider"] = "codex"
	if _, err := s.Turn(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"initial", "automatic_continuation"}) {
		t.Fatal(got)
	}
}

func TestTurnFinalReadTrigger(t *testing.T) {
	var got []string
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		assertTriggerAccepted(t, r)
		got = append(got, r.CallTrigger)
		v := reply()
		if len(got) == 1 {
			v["readFiles"] = []any{Record{"file": "README.md", "mode": "lines", "start": 1, "end": 1}}
		}
		return measuredAnswer(v), nil
	})
	d["carriedRounds"] = 6
	if _, err := s.Turn(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"initial", "automatic_continuation"}) {
		t.Fatal(got)
	}
}

func TestCallResumeFailureTrigger(t *testing.T) {
	var got []ai.Request
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		assertTriggerAccepted(t, r)
		got = append(got, r)
		if len(got) == 1 {
			answer := measuredAnswer(nil)
			answer.Diagnostic = ai.Diagnostic{ID: "failed", Kind: "execution"}
			return answer, &ai.Failure{Kind: "execution"}
		}
		return measuredAnswer(reply()), nil
	})
	config, _ := s.options.Config(t.Context())
	b, _ := s.budget(d, config)
	state := &sessionState{ID: draftID, Draft: draftID, Workspace: filepath.Join(s.options.RuntimeDir, "intake-workspace")}
	ctx := context.WithValue(t.Context(), callTriggerKey{}, "user_reply")
	_, err := s.call(ctx, d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, measurementSchema(t, s), "resume", "full", state, b, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Resume || got[0].CallTrigger != "user_reply" || got[1].Resume || got[1].CallTrigger != "failure_retry" || got[1].Input != "full" {
		t.Fatalf("requests=%+v", got)
	}
}

func TestTurnUnknownHistoryLeavesTriggerEmpty(t *testing.T) {
	for _, messages := range [][]any{
		{Record{"role": "user", "text": "依頼"}, Record{"role": "proxy", "text": "代理"}},
		{Record{"role": "assistant", "text": "質問"}},
	} {
		t.Run(messages[len(messages)-1].(Record)["role"].(string), func(t *testing.T) {
			var got []string
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				got = append(got, r.CallTrigger)
				return measuredAnswer(reply()), nil
			})
			d["messages"] = messages
			if _, err := s.Turn(t.Context(), d); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, []string{""}) {
				t.Fatal(got)
			}
		})
	}
}

// Check nonempty triggers against the production usage schema. Paths where the
// trigger can be empty must instead compare the actual value with their expected
// value explicitly, as TestTurnUnknownHistoryLeavesTriggerEmpty does.
func assertTriggerAccepted(t *testing.T, r ai.Request) {
	t.Helper()
	if r.CallTrigger == "" {
		t.Fatal("相談の下書きの CallTrigger が空です")
	}
	row := map[string]any{"at": "2026-11-01T00:00:00.000Z", "source": "intake", "host": "nas", "step": r.Step, "callTrigger": r.CallTrigger}
	if !usage.ValidUsage(row) {
		t.Fatalf("実際の呼び出しの callTrigger が保存時に拒否されます: step=%q trigger=%q", r.Step, r.CallTrigger)
	}
}

func TestTurnRetryAfterFailedCall(t *testing.T) {
	var got []string
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		assertTriggerAccepted(t, r)
		got = append(got, r.CallTrigger)
		if len(got) == 1 {
			answer := measuredAnswer(nil)
			answer.Diagnostic = ai.Diagnostic{ID: "failed", Kind: "execution"}
			return answer, &ai.Failure{Kind: "execution"}
		}
		return measuredAnswer(reply()), nil
	})
	if _, err := s.Turn(t.Context(), d); err == nil {
		t.Fatal("expected failure")
	}
	saved, ok, err := s.options.Records.Get(t.Context(), "drafts", draftID)
	if err != nil || !ok {
		t.Fatalf("saved: %v %v", ok, err)
	}
	if _, err = s.Turn(t.Context(), saved); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"initial", "failure_retry"}) {
		t.Fatal(got)
	}
}

func TestInitialTriggerSavedInputsAndRecovery(t *testing.T) {
	d := Record{"messages": []any{Record{"role": "user", "text": "依頼"}}, "consultationBudget": Record{"calls": float64(0)}}
	b := &callBudget{draft: d}
	reads := &readBudget{budget: b, value: Record{"inputs": []any{digest("依頼")}}}
	if got := initialCallTrigger(d, reads); got != "" {
		t.Fatalf("inconsistent: %q", got)
	}
	fleet.Object(d["consultationBudget"])["calls"] = float64(1)
	for _, tc := range []struct{ kind, want string }{{"execution", "failure_retry"}, {"canceled_by_server", "recovery_continuation"}, {"", "recovery_continuation"}} {
		d["aiDiagnostics"] = []any{Record{"kind": tc.kind, "calls": float64(1), "step": usage.DraftStep}}
		if got := initialCallTrigger(d, reads); got != tc.want {
			t.Fatalf("%s: %s", tc.kind, got)
		}
	}
}

func TestTurnFormatFailureCarriesFieldToUsage(t *testing.T) {
	var requests []ai.Request
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		requests = append(requests, r)
		if len(requests) == 1 {
			return measuredAnswer(nil), &ai.Failure{Kind: "format", Field: "title"}
		}
		return measuredAnswer(reply()), nil
	})
	var recorded []ai.Request
	s.options.RecordUsage = func(_ context.Context, r ai.Request, _ ai.Usage, _ time.Duration, _ bool) error {
		recorded = append(recorded, r)
		return nil
	}
	if _, err := s.Turn(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 2 || recorded[1].RepairKind != "draft_format" || recorded[1].RepairField != "title" {
		t.Fatalf("recorded: %+v", recorded)
	}
}

func TestEffectiveTierFollowsResolvedPhysicalCalls(t *testing.T) {
	for _, tc := range []struct {
		name, level, step, mode string
		fixed                   bool
		want                    []string
	}{
		{"light", "light", "intake", "", false, []string{"light"}},
		{"standard", "standard", "intake", "", false, []string{"standard"}},
		{"heavy", "heavy", "intake", "", false, []string{"heavy"}},
		{"review_notes", "heavy", models.ReviewNotesStep, "", false, []string{""}},
		{"fixed", "heavy", "intake", "", true, []string{""}},
		{"switch", "heavy", "intake", "switch", false, []string{"heavy", "standard"}},
		{"resolve_failure", "heavy", "intake", "failure", false, []string{"heavy", "heavy"}},
		{"provider_mismatch", "heavy", "intake", "mismatch", false, []string{"heavy", "heavy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, records []ai.Request
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				calls = append(calls, r)
				if tc.mode != "" && len(calls) == 1 {
					return measuredAnswer(nil), measuredTimeout{}
				}
				return measuredAnswer(reply()), nil
			})
			env := pgtest.NewIsolated(t)
			env.DB.SetMaxOpenConns(1)
			if _, err := env.DB.ExecContext(t.Context(), "CREATE TEMP TABLE settings (key text, value jsonb, expires_at timestamptz)"); err != nil {
				t.Fatal(err)
			}
			resolver := &models.Service{DB: env.DB}
			config := map[string]any{"model": "shared", "codexEquivalents": map[string]any{"haiku": "shared"}, "intake": map[string]any{"reasoningEffort": "high", "tiers": map[string]any{
				"light":    map[string]any{"model": "shared", "reasoningEffort": "low"},
				"standard": map[string]any{"model": "shared", "reasoningEffort": "medium"},
				"heavy":    map[string]any{"model": "shared", "reasoningEffort": "high"},
			}}}
			if tc.fixed {
				delete(config["intake"].(map[string]any), "tiers")
			}
			d["tier"] = Record{"level": tc.level}
			settings, err := resolver.Resolve(t.Context(), config, map[string]any(d), tc.step)
			if err != nil {
				t.Fatal(err)
			}
			s.resolve = func(ctx context.Context, c, draft map[string]any, step string) (models.Settings, error) {
				if tc.mode == "failure" {
					return models.Settings{}, errors.New("resolution failed")
				}
				candidate, err := resolver.Resolve(ctx, c, draft, step)
				if tc.mode == "mismatch" {
					candidate.Provider = "claude"
				}
				return candidate, err
			}
			s.options.RecordUsage = func(_ context.Context, r ai.Request, _ ai.Usage, _ time.Duration, _ bool) error {
				records = append(records, r)
				return nil
			}
			_, err = s.call(t.Context(), d, config, settings, usage.DraftStep, measurementSchema(t, s), "input", "full", nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != len(tc.want) || len(records) != len(tc.want) {
				t.Fatalf("calls=%d records=%d", len(calls), len(records))
			}
			for i, tier := range tc.want {
				effort := settings.Effort
				if i == 1 && tc.mode == "switch" {
					effort = "medium"
				}
				for _, r := range []ai.Request{calls[i], records[i]} {
					if r.Tier != tier || r.Provider != "codex" || r.Model != "shared" || r.Effort != effort {
						t.Fatalf("call %d: tier=%q provider=%q model=%q effort=%q", i, r.Tier, r.Provider, r.Model, r.Effort)
					}
				}
			}
		})
	}
}

func TestProxyRestoresEffectiveTierFromSavedSettings(t *testing.T) {
	for _, tier := range []string{"heavy", ""} {
		t.Run(map[bool]string{true: "saved", false: "legacy"}[tier != ""], func(t *testing.T) {
			var got ai.Request
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				got = r
				return measuredAnswer(nil), errors.New("stop")
			})
			config, _ := s.options.Config(t.Context())
			fleet.Object(fleet.Object(config["intake"])["proxy"])["enabled"] = true
			savedSettings := models.Settings{Provider: "codex", Model: "saved-model", Effort: "high", Tier: tier}
			d["proxyPending"] = Record{"questions": []any{"質問"}, "settings": savedSettings}
			if _, err := s.options.Records.Change(t.Context(), "drafts", draftID, func(_ *sql.Tx, _ Record) (Record, error) { return copyRecord(d), nil }); err != nil {
				t.Fatal(err)
			}
			saved, _, err := s.options.Records.Get(t.Context(), "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			m := &materials{state: contextState{Projects: map[string]*projectState{}}}
			_, err = s.proxy(t.Context(), saved, Record{"questions": []any{"質問"}}, config, m, models.Settings{Provider: "codex", Model: "current", Effort: "low", Tier: "light"}, false)
			if err != nil {
				t.Fatal(err)
			}
			if got.Tier != tier || got.Model != "saved-model" || got.Effort != "high" || got.Provider != "codex" {
				t.Fatalf("restored request: %+v", got)
			}
		})
	}
}
