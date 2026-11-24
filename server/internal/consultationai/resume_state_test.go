package consultationai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
)

func TestResumedTurnCarriesCurrentControlState(t *testing.T) {
	refused := Document{Project: "owner/app", Path: "missing.go", Reason: "missing", Text: "配信しない本文", Request: ReadRequest{File: "missing.go", Mode: "full"}}
	sent := Document{Project: "owner/app", Path: "README.md", Version: "revision", Text: "既送達の本文"}
	m := &materials{state: contextState{Projects: map[string]*projectState{"owner/app": {Reads: []Document{sent, refused}}}}}
	d := Record{"project": "owner/app", "messages": []any{}}
	shown := map[string]bool{documentKey(sent): true}
	state := &sessionState{ID: "session", Sent: shown}
	for _, current := range []turnPromptState{{Notes: "不足する実行分担を直してください", RemainingReadRounds: 2, Budget: Record{"calls": 3}}, {Notes: "追加読取の枠は終了しました", RemainingReadRounds: 0, Budget: Record{"calls": 4}}} {
		full := prompt(d, Record{}, current.Budget, current.RemainingReadRounds, nil, current.Notes)
		input := resumedPrompt(full, d, m, state, shown, current)
		_, raw, ok := strings.Cut(input, "\n")
		var got Record
		if !ok || json.Unmarshal([]byte(raw), &got) != nil {
			t.Fatal("再開入力が読めない")
		}
		if got["notes"] != current.Notes || fleet.Number(got["remainingReadRounds"]) != float64(current.RemainingReadRounds) || fleet.Number(fleet.Object(got["consultationBudget"])["calls"]) != fleet.Number(fleet.Object(current.Budget)["calls"]) {
			t.Fatal("現在の制御情報が渡っていない", got)
		}
		unread := fleet.Elements(got["unreadFiles"])
		if len(unread) != 1 || fleet.Object(unread[0])["path"] != "missing.go" || fleet.Object(unread[0])["reason"] != "missing" || fleet.Text(fleet.Object(unread[0])["text"]) != "" || len(fleet.Elements(got["documents"])) != 0 {
			t.Fatal("拒否資料や既送達本文の扱いが不正", got)
		}
		for _, start := range []*sessionState{nil, {}} {
			if resumedPrompt(full, d, m, start, shown, current) != full {
				t.Fatal("新規開始時の全文を変更した")
			}
		}
	}
}

func TestTurnResumedReadRefusalAndRemainingRounds(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		if r.OnSession != nil && r.Session == "" {
			if err := r.OnSession("11111111-1111-4111-8111-111111111112"); err != nil {
				t.Fatal(err)
			}
		}
		v := reply()
		if calls == 1 {
			v["readFiles"] = []any{"missing.go"}
		} else {
			if !r.Resume {
				t.Fatal("読取後に再開していない")
			}
			_, raw, _ := strings.Cut(r.Input, "\n")
			var got Record
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range fleet.Elements(got["unreadFiles"]) {
				doc := fleet.Object(item)
				if doc["path"] == "missing.go" && doc["reason"] == "missing" {
					found = true
				}
			}
			if !found || fleet.Number(got["remainingReadRounds"]) != 5 {
				t.Fatal("読取拒否や現在の残枠が再開先へ届かない", got)
			}
		}
		return measuredAnswer(v), nil
	})
	result, err := s.Turn(t.Context(), d)
	if err != nil || result == nil || calls != 2 {
		t.Fatalf("result=%v calls=%d err=%v", result, calls, err)
	}
}
