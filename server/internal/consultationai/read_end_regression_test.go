package consultationai

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/references"
)

var readEndCursor = regexp.MustCompile(`続き: ([a-f0-9]{64}:[0-9]+:[0-9]+)`)

func readEndRows() ([]references.Record, references.Record) {
	rows := []references.Record{}
	for i := 0; i < 150; i++ {
		rows = append(rows, references.Record{"number": i + 1, "title": "SQLite", "body": "本文", "created_at": fmt.Sprintf("%03d", 150-i)})
	}
	return rows, references.Record{"title": "SQLite"}
}

func readEndAnswer(ready bool) ai.Answer {
	v := reply()
	v["ready"] = ready
	v["title"], v["body"], v["questions"] = "題名", "実装内容", []any{}
	return measuredAnswer(v)
}

func readEndReferences(s *Service, continuation func(string) (Reference, error)) {
	rows, draft := readEndRows()
	s.options.Reference = func(context.Context, Record, string) (Reference, error) {
		return Reference{Text: references.Open("owner/app", rows, nil, draft)}, nil
	}
	s.options.ReferenceContinuation = func(_ context.Context, _ Record, cursor string) (Reference, error) {
		return continuation(cursor)
	}
}

func readEndPage(cursor string) (Reference, error) {
	rows, draft := readEndRows()
	page, err := references.OpenContinuation("owner/app", rows, nil, draft, cursor)
	return Reference{Text: page.Text}, err
}

// The allowance ends while the AI is still asking for the next page, so the
// controller has to follow the rest of the list for the final answer.
func TestReadEndFollowsOverlapContinuations(t *testing.T) {
	inputs := []string{}
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		inputs = append(inputs, r.Input)
		switch len(inputs) {
		case 1:
			v := reply()
			v["questions"], v["readFiles"] = []any{}, []any{map[string]any{"file": "open-requests", "mode": "full"}}
			return measuredAnswer(v), nil
		case 2:
			cursor := readEndCursor.FindStringSubmatch(r.Input)
			if cursor == nil {
				t.Fatal("一覧の続きが示されていない")
			}
			v := reply()
			v["questions"], v["readFiles"] = []any{}, []any{map[string]any{"file": "open-requests", "mode": "keyword", "keyword": cursor[1]}}
			return measuredAnswer(v), nil
		}
		return readEndAnswer(true), nil
	})
	d["consultationBudget"] = Record{"contextRead": Record{"rounds": float64(11), "inputs": []any{}}}
	continuations := 0
	readEndReferences(s, func(cursor string) (Reference, error) {
		continuations++
		return readEndPage(cursor)
	})
	result, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if continuations == 0 || len(inputs) != 3 || !strings.Contains(inputs[2], "owner/app#150") {
		t.Fatalf("続きが読まれていない: continuations=%d calls=%d", continuations, len(inputs))
	}
	if result["ready"] != true || len(fleet.Elements(result["questions"])) != 0 {
		t.Fatalf("読めたのに確定できない: ready=%v questions=%v", result["ready"], result["questions"])
	}
}

func TestReadEndUnreadContinuationAsksOnce(t *testing.T) {
	calls := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		calls++
		return readEndAnswer(calls == 1), nil
	})
	d["consultationBudget"] = Record{"contextRead": Record{"rounds": float64(12), "ended": true, "inputs": []any{}}}
	readEndReferences(s, func(string) (Reference, error) { return Reference{}, errors.New("取得失敗") })
	result, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if got := fleet.Elements(result["questions"]); calls != 2 || len(got) != 1 || fleet.Text(got[0]) != overlapUnreadQuestion || result["ready"] == true {
		t.Fatalf("未取得の質問: calls=%d ready=%v questions=%v", calls, result["ready"], got)
	}
}
