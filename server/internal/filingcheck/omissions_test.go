package filingcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

func TestInputOmissionsRecordCandidateAndAncestor(t *testing.T) {
	m := Record{"target": Record{"decisionContext": Record{"references": []any{Record{"draftId": "parent", "messages": []any{Record{"text": "発言"}}}}}}, "consultations": []any{Record{"id": "c1", "body": "本文"}}}
	m = reduceReferences(reduceCandidates(m))
	got := inputOmissions(m)
	if len(got) != 2 || obj(got[0])["candidateId"] != "consultation:c1" || obj(got[0])["reason"] != "入力上限" || obj(got[1])["draftId"] != "parent" || obj(got[1])["reason"] != "入力上限" {
		t.Fatal(got)
	}
}

func TestOmittedUnfiledBodyDoesNotMaskOtherFailures(t *testing.T) {
	m := Record{"consultations": []any{Record{"id": "c1", "nonOverlapping": true}}, "evidenceFinal": true, "duplicateBodyRequired": "consultation:c1", "evidenceOmitted": []any{Record{"candidateId": "consultation:c1", "kind": "body", "bodyOmitted": true, "reason": "資料入力上限"}}}
	allowedDecision := func() Record {
		return Record{"proceed": true, "reason": "見えた範囲で判定", "evidenceRequests": []any{}}
	}
	if got := duplicateVerdict(allowedDecision(), m); got["proceed"] != true {
		t.Fatal(got)
	}
	m["evidenceMissing"] = []any{Record{"candidateId": "consultation:c1", "reason": "取得失敗"}}
	if got := duplicateVerdict(allowedDecision(), m); got["proceed"] != false || !strings.Contains(fleet.Text(got["reason"]), "資料不足") {
		t.Fatal(got)
	}
	filed := Record{"requests": []any{Record{"number": 3, "nonOverlapping": true}}, "evidenceFinal": true, "duplicateBodyRequired": "request:3", "evidenceOmitted": []any{Record{"candidateId": "request:3", "kind": "body", "reason": "資料入力上限"}}}
	if got := duplicateVerdict(allowedDecision(), filed); got["proceed"] != false || !strings.Contains(fleet.Text(got["reason"]), "資料不足") {
		t.Fatal(got)
	}
}

func TestExpandedConsultationBodyOmissionControlsDuplicateVerdict(t *testing.T) {
	for _, kind := range []string{"body", "reference"} {
		t.Run(kind, func(t *testing.T) {
			m := Record{"consultations": []any{Record{"id": "c1", "body": strings.Repeat("x", 61000), "nonOverlapping": true, "acceptsInstructions": false}}}
			out, err := (&Service{}).expand(context.Background(), Record{}, m, []any{Record{"candidateId": "consultation:c1", "kind": kind}})
			if err != nil {
				t.Fatal(err)
			}
			omitted := fleet.Elements(out["evidenceOmitted"])
			if len(omitted) != 1 {
				t.Fatalf("evidenceOmitted=%v", omitted)
			}
			row := obj(omitted[0])
			if row["candidateId"] != "consultation:c1" || row["bodyOmitted"] != true || row["reason"] != "資料入力上限" || row["kind"] != kind {
				t.Fatalf("unexpected external omission record: %v", row)
			}
			out["duplicateBodyRequired"] = "consultation:c1"
			if got := duplicateVerdict(Record{"proceed": true}, out); got["proceed"] != true {
				t.Fatal(got)
			}
			delete(out, "duplicateBodyRequired")
			if got := duplicateVerdict(Record{"proceed": false, "duplicate": Record{"candidateId": "consultation:c1"}}, out); got["proceed"] != true {
				t.Fatalf("nonOverlapping recheck: %v", got)
			}
		})
	}
	for _, row := range []Record{
		{"candidateId": "consultation:c1", "kind": "body", "reason": "資料入力上限"},
		{"candidateId": "consultation:c1", "kind": "reference/messages", "reason": "資料入力上限"},
		{"candidateId": "consultation:c1", "bodyOmitted": true, "reason": "件数上限"},
		{"candidateId": "consultation:other", "bodyOmitted": true, "reason": "資料入力上限"},
	} {
		m := Record{"consultations": []any{Record{"id": "c1"}}, "evidenceFinal": true, "duplicateBodyRequired": "consultation:c1", "evidenceOmitted": []any{row}}
		if got := duplicateVerdict(Record{"proceed": true}, m); got["proceed"] != false {
			t.Fatalf("row=%v verdict=%v", row, got)
		}
	}
}

func TestCountLimitDoesNotExemptUnfiledBody(t *testing.T) {
	// 件数上限は要求の絞り込みで解消できるため、入力サイズ超過と区別する。
	requests, wanted := []any{}, []any{}
	for i := 1; i <= 12; i++ {
		requests = append(requests, Record{"number": i, "body": "本文"})
		wanted = append(wanted, Record{"candidateId": fmt.Sprintf("request:%d", i), "kind": "body"})
	}
	wanted = append(wanted, Record{"candidateId": "consultation:c1", "kind": "body"})
	m := Record{"requests": requests, "consultations": []any{Record{"id": "c1", "body": "本文"}}}
	out, err := (&Service{}).expand(context.Background(), Record{}, m, wanted)
	if err != nil || len(fleet.Elements(out["evidenceOmitted"])) != 1 || obj(fleet.Elements(out["evidenceOmitted"])[0])["reason"] != "件数上限" {
		t.Fatal(out, err)
	}
	out["duplicateBodyRequired"] = "consultation:c1"
	got := duplicateVerdict(Record{"proceed": true}, out)
	if got["proceed"] != false || !strings.Contains(fleet.Text(got["reason"]), "資料不足") {
		t.Fatal(got)
	}
}

func TestCheckPersistsInputLimitOmissions(t *testing.T) {
	s, d, material := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
		input := fleet.Text(in["input"])
		for _, text := range []string{"件数上限・取得失敗・取得できない候補外の要求は受付側で資料不足として停止", "資料入力上限・入力縮小による省略だけを理由に資料不足で停止しない例外は、件数上限には適用しません", "入力上限以外の必要部分の省略"} {
			if !strings.Contains(input, text) {
				t.Fatal(text)
			}
		}
		for _, obsolete := range []string{"件数超過だけでは停止せず最終判定へ進み", "候補外資料が判定に必須かを判定で判断"} {
			if strings.Contains(input, obsolete) {
				t.Fatal(obsolete)
			}
		}
		var sent Record
		if err := json.Unmarshal([]byte(strings.SplitN(input, "\n確認資料:\n", 2)[1]), &sent); err != nil {
			t.Fatal(err)
		}
		if len(fleet.Elements(sent["omittedCandidateSummaries"])) != 1 || fleet.WholeNumber(obj(obj(sent["target"])["decisionContext"])["omittedMessages"]) != 1 {
			t.Fatal(sent)
		}
		return allowed(), nil
	})
	(*material)["target"] = Record{"body": "目的", "decisionContext": Record{"references": []any{Record{"draftId": "parent", "messages": []any{Record{"text": strings.Repeat("a", 150000)}}}}}}
	(*material)["requests"] = []any{}
	(*material)["consultations"] = []any{Record{"id": "c1", "body": strings.Repeat("b", 50000)}}
	if err := s.Check(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
	if err != nil {
		t.Fatal(err)
	}
	checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
	omissions := fleet.Elements(obj(checks[len(checks)-1])["inputOmissions"])
	if len(omissions) != 2 || obj(omissions[0])["candidateId"] != "consultation:c1" || obj(omissions[1])["draftId"] != "parent" {
		t.Fatal(omissions)
	}
}

func TestExpandedReferenceBudgetOmissionIsRecorded(t *testing.T) {
	s := &Service{}
	m := Record{"requests": []any{Record{"number": 3, "body": "本文\n## 受付の経緯・根拠\n" + strings.Repeat("x", 61000)}}}
	out, err := s.expand(context.Background(), Record{}, m, []any{Record{"candidateId": "request:3", "kind": "reference"}})
	if err != nil {
		t.Fatal(err)
	}
	omissions := inputOmissions(out)
	if len(omissions) != 1 || obj(omissions[0])["candidateId"] != "request:3" || obj(omissions[0])["kind"] != "reference/messages" || obj(omissions[0])["bodyOmitted"] == true || obj(omissions[0])["reason"] != "資料入力上限" || len(fleet.Elements(out["evidence"])) != 1 {
		t.Fatal(out)
	}
	if got := refusalCategory(Record{"proceed": false, "refusalCategory": "input_limit_only"}, omissions); got != "input_limit_only" {
		t.Fatal(got)
	}
}

func TestExpandedFiledReferenceWithOmittedBody(t *testing.T) {
	for _, id := range []string{"request:3", "job:3"} {
		for _, selected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/selected=%t", id, selected), func(t *testing.T) {
				body := strings.Repeat("x", 61000) + "\n## 受付の経緯・根拠\n" + strings.Repeat("r", 61000)
				m := Record{"requests": []any{Record{"number": 3, "body": body}}}
				if id == "job:3" {
					m = Record{"jobs": []any{Record{"issue": 3, "body": body}}}
				}
				wanted := Record{"candidateId": id, "kind": "reference"}
				if selected {
					wanted["selection"] = Record{"reference": Record{"startLine": 1, "endLine": 2}}
				}
				out, err := (&Service{}).expand(context.Background(), Record{}, m, []any{wanted})
				if err != nil {
					t.Fatal(err)
				}
				omissions := inputOmissions(out)
				if len(omissions) != 1 || obj(omissions[0])["candidateId"] != id || obj(omissions[0])["kind"] != "reference" || obj(omissions[0])["bodyOmitted"] != true || obj(omissions[0])["reason"] != "資料入力上限" || len(fleet.Elements(out["evidence"])) != 0 {
					t.Fatalf("omissions=%v evidence count=%d", omissions, len(fleet.Elements(out["evidence"])))
				}
				out["duplicateBodyRequired"] = id
				if got := duplicateVerdict(Record{"proceed": true}, out); got["proceed"] != false || got["refusalCategory"] != "other" {
					t.Fatalf("filed body omission: %v", got)
				}
				if got := refusalCategory(Record{"proceed": false, "refusalCategory": "input_limit_only"}, omissions); got != "unknown" {
					t.Fatal(got)
				}
			})
		}
	}
}

func TestRefusalClassificationAndRecordedOmissions(t *testing.T) {
	for _, tc := range []struct {
		name, category string
		filed          bool
	}{
		{"ancestor and consultation input limit", "input_limit_only", false},
		{"filed body required", "other", true},
		{"unclassified refusal", "unreported", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d, material := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
				var sent Record
				if err := json.Unmarshal([]byte(strings.SplitN(fleet.Text(in["input"]), "\n確認資料:\n", 2)[1]), &sent); err != nil {
					t.Fatal(err)
				}
				if len(inputOmissions(sent)) != 2 {
					t.Fatal(sent)
				}
				out := allowed()
				out["proceed"] = false
				out["reason"] = "資料不足"
				if tc.category != "unreported" {
					out["refusalCategory"] = tc.category
				}
				return out, nil
			})
			(*material)["target"] = Record{"body": "目的", "decisionContext": Record{"references": []any{Record{"draftId": "parent", "messages": []any{Record{"text": strings.Repeat("a", 150000)}}}}}}
			(*material)["requests"] = []any{}
			if tc.filed {
				(*material)["requests"] = []any{Record{"number": 3, "body": strings.Repeat("b", 50000)}}
			} else {
				(*material)["consultations"] = []any{Record{"id": "c1", "body": strings.Repeat("b", 50000)}}
			}
			if err := s.Check(context.Background(), d); err == nil {
				t.Fatal("拒否が失われた")
			}
			saved, _, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
			if err != nil {
				t.Fatal(err)
			}
			checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
			check := obj(checks[len(checks)-1])
			omissions := fleet.Elements(check["inputOmissions"])
			if check["refusalCategory"] != tc.category || obj(omissions[0])["bodyRequiredForDuplicateCheck"] == true != tc.filed || obj(omissions[1])["draftId"] != "parent" {
				t.Fatal(check)
			}
		})
	}
}

func TestReportedRefusalCategoryPersistsSeparately(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reported any
		want     any
		category string
	}{
		{"explicit unknown", "unknown", "unknown", "unreported"},
		{"legacy missing", nil, nil, "unreported"},
		{"invalid value", "private text", nil, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d, _ := fixture(t, func(_ context.Context, _ map[string]any) (map[string]any, error) {
				out := allowed()
				out["proceed"], out["reason"] = false, "資料不足"
				if tc.reported != nil {
					out["refusalCategory"] = tc.reported
				}
				return out, nil
			})
			if err := s.Check(context.Background(), d); err == nil {
				t.Fatal("拒否が失われた")
			}
			saved, _, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
			if err != nil {
				t.Fatal(err)
			}
			checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
			check := obj(checks[len(checks)-1])
			if check["reportedRefusalCategory"] != tc.want || check["refusalCategory"] != tc.category {
				t.Fatal(check)
			}
		})
	}
}

func TestRefusalCategoryRequiresRecordedInputLimit(t *testing.T) {
	if got := refusalCategory(Record{"proceed": false, "refusalCategory": "input_limit_only"}, []any{Record{"reason": "件数上限"}}); got != "unknown" {
		t.Fatal(got)
	}
	if got := refusalCategory(Record{"proceed": false, "refusalCategory": "other"}, []any{Record{"reason": "件数上限"}}); got != "other" {
		t.Fatal(got)
	}
}
