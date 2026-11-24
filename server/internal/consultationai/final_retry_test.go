package consultationai

import (
	"context"
	"testing"

	"issue-desk-intake/server/internal/ai"
)

// 最終回の呼び出しが時間切れになっても、呼び出し枠が残っていれば再試行し、
// 読取だけの仮応答で相談を終えない。
func TestFinalRoundRetriesAfterTimeout(t *testing.T) {
	calls := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		calls++
		if calls == 1 {
			return measuredAnswer(nil), measuredTimeout{}
		}
		value := reply()
		value["ready"] = true
		value["title"], value["body"], value["questions"] = "題名", "実装内容", []any{}
		return measuredAnswer(value), nil
	})
	s.options.Reference = nil
	d["consultationBudget"] = Record{"contextRead": Record{"rounds": float64(12), "ended": true, "inputs": []any{}}}
	result, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result["ready"] != true {
		t.Fatalf("calls=%d result=%v", calls, result)
	}
}
