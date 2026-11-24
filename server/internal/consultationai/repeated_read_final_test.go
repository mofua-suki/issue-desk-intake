package consultationai

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
)

// 受付AIが直したあとも同じ資料を要求し続けても、読取だけの仮応答で相談を終えず、
// 最終回の呼び出しで既読の根拠から回答させる。
func TestRepeatedReadAfterRepairAsksForFinalAnswer(t *testing.T) {
	var got []string
	finalAsked := false
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		assertTriggerAccepted(t, r)
		got = append(got, r.CallTrigger)
		v := reply()
		if strings.Contains(r.Input, "これ以上の資料は読まず") {
			finalAsked = true
			return measuredAnswer(v), nil
		}
		v["readFiles"] = []any{Record{"file": "README.md", "mode": "lines", "start": 1, "end": 1}}
		return measuredAnswer(v), nil
	})
	out, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"initial", "context_read", "format_repair", "automatic_continuation"}
	if !finalAsked || !reflect.DeepEqual(got, want) {
		t.Fatalf("triggers=%v final=%v", got, finalAsked)
	}
	if out["reply"] != "質問です" || len(fleet.Elements(out["questions"])) != 1 {
		t.Fatalf("turn ended without the final answer: %v", out)
	}
}

// 最終回でもAIが読取だけ（完全な回答の無い形）を返したときは、形式の失敗で相談を
// エラーにせず、従来どおり仮応答で終える。
func TestRepeatedReadFinalReadOnlyReplyEndsQuietly(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		return measuredAnswer(map[string]any{"response": map[string]any{"readFiles": []any{Record{"file": "README.md", "mode": "lines", "start": 1, "end": 1}}}}), nil
	})
	out, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if calls != 4 || out["ready"] == true || len(fleet.Elements(out["readFiles"])) != 0 {
		t.Fatalf("calls=%d out=%v", calls, out)
	}
}
