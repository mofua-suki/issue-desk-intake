package consultationai

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"issue-desk-intake/server/internal/ai"
)

const completeResponseFixture = `{"executionPlan":null,"completionPlan":null,"reply":"質問です","basisConflicts":[],"title":"題名","body":"本文","ready":false,"readFiles":[],"reviewedFiles":[],"questions":["確認"],"imageReviews":[],"splitIssues":[],"crossProject":{"reason":"","scopes":[],"approvalMessageId":""},"changeScope":{"known":false,"paths":[],"reason":""},"dependsOnIssues":[],"kind":"implementation","escalate":false,"escalateReason":"","heavyReason":"","workType":"bugfix","estimate":{"lines":0,"files":0}}`

func TestTurnResponseKeepsCompleteValidation(t *testing.T) {
	source := filepath.Join("..", "..", "schemas", "intake.schema.json")
	schemaFile, _, cleanup, err := turnResponseSchema(t.TempDir(), source, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	schema, err := ai.Compile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	complete := Record{}
	err = json.Unmarshal([]byte(completeResponseFixture), &complete)
	if err != nil {
		t.Fatal(err)
	}
	reading := Record{"readFiles": []any{"README.md"}, "escalate": false, "escalateReason": ""}
	for _, answer := range []Record{reading, complete} {
		value, err := ai.Validate(schema, marshal(Record{"response": answer}))
		if err != nil {
			t.Fatal(err)
		}
		got, err := turnResponse(value, false)
		if err != nil || got["ready"] != false {
			t.Fatalf("%v %v", got, err)
		}
	}
	if _, err := turnResponse(Record{"response": reading}, true); err == nil {
		t.Fatal("最終回で途中回答を受理した")
	}
	for _, missing := range []string{"body", "completionPlan", "basisConflicts", "reviewedFiles", "executionPlan", "ready"} {
		copy := copyRecord(complete)
		delete(copy, missing)
		if _, err := ai.Validate(schema, marshal(Record{"response": copy})); err == nil {
			t.Fatalf("最終回答の %s が欠けても通過", missing)
		}
	}
	for _, invalid := range []Record{
		{"readFiles": []any{}, "escalate": false, "escalateReason": ""},
		{"readFiles": []any{"README.md"}, "escalate": false, "escalateReason": "", "ready": true},
		{"readFiles": []any{"README.md"}, "escalate": false, "escalateReason": "", "completionPlan": Record{}},
	} {
		if _, err := ai.Validate(schema, marshal(Record{"response": invalid})); err == nil {
			t.Fatalf("不正な途中回答を受理: %v", invalid)
		}
	}
	cleanup()
	if _, err := os.Stat(schemaFile); !os.IsNotExist(err) {
		t.Fatal("一時スキーマが残った")
	}
}

func TestTurnShortReadDoesNotAddModelCalls(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
		calls++
		if calls == 1 {
			return measuredAnswer(Record{"response": Record{"readFiles": []any{"open-requests"}, "escalate": false, "escalateReason": ""}}), nil
		}
		return measuredAnswer(Record{"response": reply()}), nil
	})
	result, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result["ready"] != false || result["reply"] == "追加資料を確認しています。" {
		t.Fatalf("calls=%d result=%v", calls, result)
	}
}

func TestFinalShortReadUsesFormatRepairWithoutBecomingReady(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
		calls++
		schema, err := ai.Compile(req.SchemaFile)
		if err != nil {
			t.Fatal(err)
		}
		short := Record{"readFiles": []any{"README.md"}, "escalate": false, "escalateReason": ""}
		complete := Record{}
		if err := json.Unmarshal([]byte(completeResponseFixture), &complete); err != nil {
			t.Fatal(err)
		}
		withRead := copyRecord(complete)
		withRead["readFiles"] = short["readFiles"]
		for _, answer := range []Record{short, withRead} {
			if _, err := ai.Validate(schema, marshal(Record{"response": answer})); err == nil {
				t.Fatalf("最終回の呼び出し%dが追加資料要求を許した: %v", calls, answer)
			}
		}
		value, err := ai.Validate(schema, marshal(Record{"response": complete}))
		if err != nil {
			t.Fatalf("最終回の呼び出し%dが完全な回答を拒否: %v", calls, err)
		}
		if calls == 1 {
			return measuredAnswer(Record{"response": short}), nil
		}
		if req.CallTrigger != "format_repair" {
			t.Fatalf("呼び出し理由=%s", req.CallTrigger)
		}
		return measuredAnswer(value), nil
	})
	d["consultationBudget"] = Record{"contextRead": Record{"rounds": float64(12), "ended": true, "inputs": []any{}}}
	result, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result["ready"] != false {
		t.Fatalf("calls=%d result=%v", calls, result)
	}
}
