package consultationai

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

func TestRecoverOnlyDurableMatchingUsage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(Record)
		want   bool
	}{
		{"matched", func(Record) {}, true},
		{"other draft", func(r Record) { r["draft"] = "other" }, false},
		{"other call", func(r Record) { r["callId"] = "other" }, false},
		{"partial", func(r Record) { r["usagePartial"] = true }, false},
		{"unknown", func(r Record) { r["inputTokens"] = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d := testService(t, nil)
			cfg, _ := s.options.Config(t.Context())
			b, _ := s.budget(d, cfg)
			if err := b.reserveCall(t.Context(), "saved-call"); err != nil {
				t.Fatal(err)
			}
			row := Record{"callId": "saved-call", "draft": d["id"], "repo": d["project"], "source": "intake", "provider": "codex", "model": "fixture", "inputTokens": float64(10), "outputTokens": float64(3), "cacheReadTokens": float64(2), "cacheWriteTokens": float64(0)}
			tc.change(row)
			s.options.ReadUsage = func(context.Context, string) (map[string]any, error) { return row, nil }
			if err := s.reconcileMeasurement(t.Context(), d, b); err != nil {
				t.Fatal(err)
			}
			got := b.value["pendingCall"] == nil
			if got != tc.want {
				t.Fatal(b.value)
			}
			if tc.want {
				before := fleet.Number(b.value["weighted"])
				if before <= 0 || fleet.Number(b.value["calls"]) != 1 || fleet.Number(b.value["unknownCalls"]) != 0 {
					t.Fatal(b.value)
				}
				if err := s.reconcileMeasurement(t.Context(), d, b); err != nil || fleet.Number(b.value["weighted"]) != before {
					t.Fatal("二重計上", err, b.value)
				}
			} else if fleet.Number(b.value["unknownCalls"]) != 1 {
				t.Fatal("欠測を解消扱いした", b.value)
			}
		})
	}
}

func TestTurnReconcilesDurableUsageBeforeMeasurementGuard(t *testing.T) {
	reached := false
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		reached = true
		return measuredAnswer(nil), errors.New("fixture-after-recovery")
	})
	cfg, _ := s.options.Config(t.Context())
	b, _ := s.budget(d, cfg)
	if err := b.reserveCall(t.Context(), "saved-call"); err != nil {
		t.Fatal(err)
	}
	s.options.ReadUsage = func(context.Context, string) (map[string]any, error) {
		return Record{"callId": "saved-call", "draft": d["id"], "repo": d["project"], "source": "intake", "provider": "codex", "model": "fixture", "inputTokens": float64(10), "outputTokens": float64(3), "cacheReadTokens": float64(2), "cacheWriteTokens": float64(0)}, nil
	}
	_, err := s.Turn(t.Context(), d)
	if !reached || err == nil || !strings.Contains(err.Error(), "fixture-after-recovery") {
		t.Fatalf("回復後の呼出へ進めません: reached=%v error=%v", reached, err)
	}
}

func TestRecoveredUsageStillStopsAtExistingBudget(t *testing.T) {
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		t.Fatal("上限超えで呼出した")
		return ai.Answer{}, nil
	})
	cfg, _ := s.options.Config(t.Context())
	fleet.Object(cfg["intake"])["consultationBudget"] = Record{"calls": float64(40), "weighted": float64(10)}
	b, _ := s.budget(d, cfg)
	if err := b.reserveCall(t.Context(), "saved-call"); err != nil {
		t.Fatal(err)
	}
	s.options.ReadUsage = func(context.Context, string) (map[string]any, error) {
		return Record{"callId": "saved-call", "draft": d["id"], "repo": d["project"], "source": "intake", "provider": "codex", "inputTokens": float64(10), "outputTokens": float64(3), "cacheReadTokens": float64(2)}, nil
	}
	result, err := s.Turn(t.Context(), d)
	if err != nil || fleet.Object(fleet.Object(result["consultationBudget"])["stopped"]) == nil {
		t.Fatal(result, err)
	}
}

func TestReleasedCallStillRecoversKnownUsageOnce(t *testing.T) {
	s, d := testService(t, nil)
	cfg, _ := s.options.Config(t.Context())
	b, _ := s.budget(d, cfg)
	b.value["calls"], b.value["unknownCalls"] = float64(2), float64(0)
	b.value["measurementReleases"] = []any{Record{"callId": "not-measured"}, Record{"callId": "saved-call"}}
	if err := b.persist(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.options.ReadUsage = func(_ context.Context, id string) (map[string]any, error) {
		if id != "saved-call" {
			return nil, nil
		}
		return Record{"callId": id, "draft": d["id"], "repo": d["project"], "source": "intake", "provider": "codex", "inputTokens": float64(10), "cacheReadTokens": float64(2), "outputTokens": float64(3)}, nil
	}
	for i := 0; i < 2; i++ {
		if err := s.reconcileMeasurement(t.Context(), d, b); err != nil {
			t.Fatal(err)
		}
		if fleet.Number(b.value["weighted"]) != 23.2 || fleet.Number(b.value["calls"]) != 2 || fleet.Number(b.value["unknownCalls"]) != 0 || len(fleet.Elements(b.value["measurementReconciliations"])) != 1 {
			t.Fatal(b.value)
		}
	}
}
