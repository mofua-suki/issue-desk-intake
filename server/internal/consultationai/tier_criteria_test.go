package consultationai

import (
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/tierpolicy"
)

func TestCommonPolicyReachesConsultationInputAndMeasurement(t *testing.T) {
	_, d := testService(t, nil)
	full := prompt(d, Record{"projects": []any{}}, nil, 1, nil, "")
	var usage ai.Usage
	measureInput(ai.Request{Input: full}, &usage, nil)
	if usage.PromptBytes["instructions"] == nil || *usage.PromptBytes["instructions"] != int64(len(turnInstructions+"\n以下は相談と資料のデータです:\n")) {
		t.Fatalf("共通基準を含む実入力の固定指示を計測できません: %+v", usage)
	}

	for _, rule := range []string{tierpolicy.ConsultationRules, tierpolicy.ReasonRules, tierpolicy.SelectionRules, tierpolicy.ImplementationRules(), tierpolicy.HandoffRules, tierpolicy.FinalizationRules} {
		if !strings.Contains(full, rule) {
			t.Fatal("相談のAI入力に共通基準がありません")
		}
	}
}

func TestPolicyUpdateStartsNewSessionWithCurrentInstructions(t *testing.T) {
	s, d, config, settings, state, _ := sessionFixture(t, "codex")
	state.Key = digest(string(marshal(Record{"settings": settings, "config": config})))
	d["aiSession"] = state
	next, err := s.session(t.Context(), d, config, settings, false)
	if err != nil || next.ID != "" {
		t.Fatal(err, next)
	}
	input := resumedPrompt(prompt(d, Record{}, nil, 1, nil, ""), d, &materials{}, next, nil, turnPromptState{})
	if !strings.Contains(input, tierpolicy.Guidance()) {
		t.Fatal("新しい共通基準で相談を開始していません")
	}
}
