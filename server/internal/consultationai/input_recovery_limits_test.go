package consultationai

import (
	"context"
	"errors"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/models"
)

func TestInputRecoveryUsesRecordedSplitDecision(t *testing.T) {
	const statement = "この依頼は分けないでください"
	const evidence = "## 利用者の判断と根拠\n原文「" + statement + "」日時: 2026-01-01"
	message := func(text string) Record {
		return Record{"role": "user", "text": text, "at": "2026-01-01T03:00:00Z"}
	}
	for _, tc := range []struct {
		name, body string
		messages   []any
		depth      float64
		stops      bool
	}{
		{"saved decision", evidence, []any{message(statement)}, 0, true},
		{"unrelated question in the original statement", evidence, []any{message(statement + "。いつ終わりますか？")}, 0, true},
		{"later instruction about test data", evidence, []any{message(statement), message("テストデータは用途ごとに分けてください")}, 0, true},
		{"later instruction to split the request", evidence, []any{message(statement), message("この依頼は分けてください")}, 0, false},
		{"later permission to split the request", evidence, []any{message(statement), message("この依頼は分割しても構いません。")}, 0, false},
		{"later question about splitting the request", evidence, []any{message(statement), message("この依頼は分割しても構いませんか？")}, 0, true},
		{"decision in a reply before the body exists", "", []any{message(statement), message(evidence)}, 0, true},
		{"unquoted", "", []any{message("分割しない")}, 0, false},
		{"negated", evidence, []any{message(statement + "とは言っていない")}, 0, false},
		{"withdrawn", evidence, []any{message(statement), message("その判断を撤回する")}, 0, false},
		{"initial CLI request carries the user's decision", "", []any{message(evidence)}, 0, true},
		{"one issue in the initial request", "", []any{message(strings.ReplaceAll(evidence, statement, "この依頼は1件で進めてください"))}, 0, true},
		{"the number of issues is optional", "", []any{message(strings.ReplaceAll(evidence, statement, "この依頼は1件でも2件でも構いません"))}, 0, false},
		{"initial quotation is a question", "", []any{message(strings.ReplaceAll(evidence, statement, "この依頼は分割しない方がいいですか？"))}, 0, false},
		{"withdrawn CLI decision", "", []any{message(evidence), message("この判断を撤回する")}, 0, false},
		{"invalid date in CLI request", "", []any{message(strings.ReplaceAll(evidence, "2026-01-01", "2026-02-30"))}, 0, false},
		{"later quotation cannot verify itself", "", []any{message("元の依頼"), message(evidence)}, 0, false},
		{"two later copied quotations cannot verify each other", evidence, []any{message("元の依頼"), message(evidence), message(evidence)}, 0, false},
		{"generated parent message", evidence, []any{message("子"), message(statement)}, 2, false},
		{"generated request has a quoted decision", "", []any{message(evidence), message(evidence), message("元の依頼")}, 2, false},
		{"actual parent statement", evidence, []any{message("子"), message("親"), message(statement)}, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
				calls++
				return measuredAnswer(map[string]any{"parts": []any{"src/a.go", "別の要件"}}), nil
			})
			messages := append([]any(nil), tc.messages...)
			first := copyRecord(messages[0].(Record))
			first["text"] = first["text"].(string) + "\n\n## 目的\nsrc/a.go " + strings.Repeat("目的", 30)
			messages[0] = first
			d["body"], d["messages"], d["consultationDepth"] = tc.body, messages, tc.depth
			if tc.depth > 0 {
				d["parentConsultation"] = "fixture-parent"
			}
			split := false
			s.options.Split = func(context.Context, Record, []string) (Record, error) {
				split = true
				return Record{"recovered": true}, nil
			}
			cause := errors.New("input too large")
			result, err := s.splitInput(context.Background(), d, nil, models.Settings{Provider: "codex"}, cause)
			if tc.stops {
				if !errors.Is(err, cause) || split || calls != 0 {
					t.Fatalf("split=%v calls=%d err=%v", split, calls, err)
				}
			} else if err != nil || !split || calls != 1 || result["recovered"] != true {
				t.Fatalf("split=%v calls=%d result=%v err=%v", split, calls, result, err)
			}
		})
	}
}

func TestInputRecoveryCurrentNamedFileLimits(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		parts        []string
		valid        bool
	}{
		{"path missing", "server/a.go plus context", []string{"other", "requirements"}, false},
		{"readme missing", "README.md plus context", []string{"other", "requirements"}, false},
		{"directory missing", "server/ plus context", []string{"other", "requirements"}, true},
		{"extensionless missing", "server/Dockerfile plus context", []string{"other", "requirements"}, true},
		{"extensionless added", "original context long enough", []string{"server/Dockerfile", "other"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateParts(tc.parts, tc.source); (err == nil) != tc.valid {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestInputRecoverySplitUsesConversationUTF8Bytes(t *testing.T) {
	// The source is "[user]\nあ\n\n": 10 runes but 12 UTF-8 bytes.
	for _, tc := range []struct {
		name, part string
		wantSplit  bool
	}{
		{"more characters but fewer bytes", "abcdefghijk", true},
		{"equal bytes", "abcdefghijkl", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
				return measuredAnswer(map[string]any{"parts": []any{tc.part, "x"}}), nil
			})
			d["messages"] = []any{Record{"role": "user", "text": "あ"}}
			d["body"] = ""
			split := false
			s.options.Split = func(_ context.Context, _ Record, parts []string) (Record, error) {
				split = true
				return Record{"parts": parts}, nil
			}
			cause := errors.New("input too large")
			_, err := s.splitInput(context.Background(), d, nil, models.Settings{Provider: "codex"}, cause)
			if split != tc.wantSplit || (tc.wantSplit && err != nil) || (!tc.wantSplit && !errors.Is(err, cause)) {
				t.Fatalf("split=%v err=%v", split, err)
			}
		})
	}
}
