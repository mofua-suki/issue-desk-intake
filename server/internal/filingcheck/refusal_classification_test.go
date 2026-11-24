package filingcheck

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"testing"
)

func TestInstructionsByteBudget(t *testing.T) {
	const maxBaseBytes = 16900
	const maxSplitDependencyBytes = 700
	if len(baseInstructions) > maxBaseBytes {
		t.Fatalf("既存の指示文が上限を超えました: %d > %d bytes", len(baseInstructions), maxBaseBytes)
	}
	if len(splitDependencyInstructions) > maxSplitDependencyBytes {
		t.Fatalf("分割依存の指示文が上限を超えました: %d > %d bytes", len(splitDependencyInstructions), maxSplitDependencyBytes)
	}
	if len(instructions) != len(baseInstructions)+len(splitDependencyInstructions) {
		t.Fatal("指示文の構成が一致しません")
	}
}

func TestRefusalCategoryExcludesExpandedFiledBodies(t *testing.T) {
	decision := Record{"proceed": false, "refusalCategory": "input_limit_only"}
	for _, id := range []string{"request:3", "job:3"} {
		for _, kind := range []string{"body", "reference", "future-kind"} {
			omissions := inputOmissions(Record{"evidenceOmitted": []any{Record{"candidateId": id, "kind": kind, "bodyOmitted": true, "reason": "資料入力上限"}}})
			if got := refusalCategory(decision, omissions); got != "unknown" {
				t.Fatalf("%s %s: %s", id, kind, got)
			}
		}
	}
	for _, row := range []Record{{"candidateId": "consultation:3", "kind": "body", "bodyOmitted": true, "reason": "資料入力上限"}, {"candidateId": "request:3", "kind": "body", "reason": "資料入力上限"}, {"candidateId": "request:3", "kind": "reference/messages", "reason": "資料入力上限"}} {
		if got := refusalCategory(decision, []any{row}); got != "input_limit_only" {
			t.Fatalf("%v: %s", row, got)
		}
	}
}

func TestRefusalCategoryUnreportedAndInvalid(t *testing.T) {
	omissions := []any{Record{"reason": "件数上限"}}
	for _, tc := range []struct{ category, want string }{
		{"", "unreported"}, {"unknown", "unreported"}, {"input_limit_only", "unknown"}, {"none", "unknown"},
	} {
		if got := refusalCategory(Record{"proceed": false, "refusalCategory": tc.category}, omissions); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.category, got, tc.want)
		}
	}
	for _, path := range []string{"../../schemas/filing-check.schema.json", "../../../api/schemas/filing-check.schema.json"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		values := fleet.Elements(obj(obj(schema["properties"])["refusalCategory"])["enum"])
		found := false
		for _, value := range values {
			if value == "unknown" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: unknown missing", path)
		}
	}
}

func TestRefusalCategoryExcludesFiledSummaries(t *testing.T) {
	material := Record{"omittedCandidateSummaries": []any{Record{"candidateId": "request:3"}, Record{"candidateId": "job:4"}}}
	omissions := inputOmissions(material)
	decision := Record{"proceed": false, "refusalCategory": "input_limit_only"}
	if got := refusalCategory(decision, omissions); got != "unknown" {
		t.Fatal(got, omissions)
	}
	omissions = append(omissions, Record{"reason": "件数上限"})
	if got := refusalCategory(decision, omissions); got != "unknown" {
		t.Fatal(got, omissions)
	}
	omissions = append(omissions, Record{"reason": "資料入力上限"})
	if got := refusalCategory(decision, omissions); got != "input_limit_only" {
		t.Fatal(got, omissions)
	}
}

func TestDuplicateFinalMissingBodyOverridesClassification(t *testing.T) {
	material := Record{"requests": []any{Record{"number": 3, "nonOverlapping": true}}, "evidenceFinal": true}
	decision := Record{"proceed": false, "refusalCategory": "input_limit_only", "duplicate": Record{"candidateId": "request:3"}}
	got := duplicateVerdict(decision, material)
	if got["proceed"] != false || got["duplicate"] != nil || refusalCategory(got, []any{Record{"reason": "入力上限"}}) != "other" {
		t.Fatal(got)
	}
}
