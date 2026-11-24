package ai

import "testing"

func TestCodexUsageRequiresNoUnfinishedOrUnmeasuredTurn(t *testing.T) {
	const done = `{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0,"cached_input_tokens":0}}`
	for _, tc := range []struct {
		name, text     string
		partial, known bool
	}{
		{"全0実測", done, false, true},
		{"完了後に別turn開始", done + "\n" + `{"type":"turn.started"}`, true, true},
		{"完了後に別turn失敗", done + "\n" + `{"type":"turn.failed"}`, true, true},
		{"測定無しの完了turnを捨てない", done + "\n" + `{"type":"turn.completed"}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := Measure("codex", []byte(tc.text))
			if u.Partial == nil || *u.Partial != tc.partial || (u.InputTokens != nil) != tc.known {
				t.Fatal(u)
			}
			if u.CacheWriteTokens != nil {
				t.Fatal("Codex未提供のcacheWriteを0化しました")
			}
		})
	}
}
