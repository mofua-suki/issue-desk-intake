package consultationai

import (
	"context"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/models"
)

func TestSlowIncidentAnswerUsesReviewProviderAndMasksCredentials(t *testing.T) {
	calls := 0
	s, _ := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		if r.Provider != "codex" || r.Step != "review" {
			t.Errorf("provider=%s step=%s", r.Provider, r.Step)
		}
		if strings.Contains(r.Input, "secret-fixture-value") {
			t.Error("資格情報がAI入力に残った")
		}
		return measuredAnswer(map[string]any{"judgement": "no_problem", "cause": "", "directPR": false}), nil
	})
	request := map[string]any{"kind": "slow-incident", "schema": `{"type":"object"}`, "data": map[string]any{"authorization": "Bearer secret-fixture-value"}}
	if _, err := s.Answer(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	s.resolve = func(context.Context, map[string]any, map[string]any, string) (models.Settings, error) {
		return models.Settings{Provider: "claude", Model: "fixture"}, nil
	}
	if _, err := s.Answer(t.Context(), request); err == nil {
		t.Fatal("Codex以外への代替実行を許した")
	}
	if calls != 1 {
		t.Fatalf("無効化後の実行=%d", calls)
	}
}
