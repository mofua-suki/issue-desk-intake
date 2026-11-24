package consultationai

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
)

// finishedReads は、最終回の回答を使い終えた（ended は付かない）読み足しの状態を作る。
func finishedReads(d Record, calls int) {
	d["consultationBudget"] = Record{"calls": float64(calls), "contextRead": Record{
		"rounds": float64(3), "final": Record{"draft": true}, "inputs": []any{digest("依頼")},
		"lastReply": Record{"ready": false, "reply": "古い返事", "title": "題名", "body": "本文", "questions": []any{}},
	}}
	d["messages"] = append(fleet.Elements(d["messages"]), Record{"role": "user", "id": "u2", "text": "新しい発言"})
}

func TestTurnAnswersNewInputAfterFinalDraftOnce(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		if !strings.Contains(r.Input, "新しい発言") {
			t.Error("新しい発言が入力に無い")
		}
		v := reply()
		v["reply"], v["questions"] = "新しい返事", []any{}
		return measuredAnswer(v), nil
	})
	finishedReads(d, 0)
	result, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(fleet.Text(result["reply"]), "新しい返事") || strings.Contains(fleet.Text(result["reply"]), "古い返事") {
		t.Fatalf("新しい発言に答えていない: calls=%d reply=%v", calls, result["reply"])
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Turn(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("同じ発言で再度呼んだ: calls=%d", calls)
	}
}

func TestTurnStopsInsteadOfOldReplyWhenNoCallLeft(t *testing.T) {
	calls := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		calls++
		return measuredAnswer(reply()), nil
	})
	config, _ := s.options.Config(context.Background())
	limits, err := s.budget(d, config)
	if err != nil {
		t.Fatal(err)
	}
	finishedReads(d, limits.calls-1)
	result, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || strings.Contains(fleet.Text(result["reply"]), "古い返事") || fleet.Object(result["consultationBudget"])["stopped"] == nil {
		t.Fatalf("枠が無いのに前の返事を返した: calls=%d result=%v", calls, result)
	}
}
