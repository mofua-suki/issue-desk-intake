package filingcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

func TestEvidenceExactBoundaries(t *testing.T) {
	for _, mode := range []string{"exact", "candidate-over", "document-over"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			s, d, material := fixture(t, func(_ context.Context, input map[string]any) (map[string]any, error) {
				calls++
				out := allowed()
				if calls > 1 {
					var sent Record
					if err := json.Unmarshal([]byte(strings.SplitN(fleet.Text(input["input"]), "\n確認資料:\n", 2)[1]), &sent); err != nil {
						t.Fatal(err)
					}
					evidence := fleet.Elements(sent["evidence"])
					expected := 12
					if calls == 3 {
						expected = 24
					}
					if len(evidence) != expected {
						t.Fatalf("call %d: %d evidence", calls, len(evidence))
					}
					seen := map[string]bool{}
					for _, item := range evidence {
						e := obj(item)
						key := fmt.Sprintf("%s:%s:%s", fleet.Text(e["candidateId"]), fleet.Text(e["kind"]), fleet.Text(e["body"]))
						seen[key] = true
					}
					for i := 3; i < 3+expected/2; i++ {
						for _, body := range []string{"first", "second"} {
							key := fmt.Sprintf("request:%d:body:%s", i, body)
							if !seen[key] {
								t.Fatalf("missing %s: %v", key, seen)
							}
						}
					}
				}
				if calls == 3 && mode == "exact" {
					return out, nil
				}
				out["proceed"] = false
				requests := []any{}
				start, end := 3, 9
				if calls == 2 {
					start, end = 9, 15
				}
				if calls == 3 {
					start, end = 15, 16
					if mode == "document-over" {
						start, end = 3, 4
					}
				}
				for i := start; i < end; i++ {
					lines := []int{1, 2}
					if calls == 3 {
						lines = []int{3}
					}
					for _, line := range lines {
						requests = append(requests, Record{"candidateId": fmt.Sprintf("request:%d", i), "kind": "body", "selection": Record{"body": Record{"startLine": line, "endLine": line}}})
					}
				}
				out["evidenceRequests"] = requests
				return out, nil
			})
			rows := []any{}
			for i := 3; i < 16; i++ {
				rows = append(rows, Record{"number": i, "body": "first\nsecond\nthird"})
			}
			(*material)["requests"] = rows
			err := s.Check(context.Background(), d)
			if (mode == "exact") != (err == nil) || calls != 3 {
				t.Fatal(err, calls)
			}
			saved, _, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
			if err != nil {
				t.Fatal(err)
			}
			state := obj(saved["filingCheckCalls"])
			checks := fleet.Elements(state["checks"])
			if len(checks) != 3 {
				t.Fatal(state)
			}
			for i, c := range checks {
				if fleet.WholeNumber(obj(c)["phase"]) != int64(i) {
					t.Fatal(checks)
				}
				if i < 2 && obj(c)["result"] != "evidence_requested" {
					t.Fatal(checks)
				}
			}
			lastResult := "evidence_requested"
			if mode == "exact" {
				lastResult = "allowed"
			}
			if obj(checks[2])["result"] != lastResult {
				t.Fatal(checks)
			}
			hold := obj(state["evidenceHold"])
			if mode == "exact" {
				if hold != nil || state["stopKind"] == "evidence_missing" {
					t.Fatal(state)
				}
			} else if state["stopKind"] != "evidence_missing" || !strings.Contains(string(raw(hold["evidenceMissing"])), "件数上限") || len(fleet.Elements(hold["requests"])) != 25 {
				t.Fatal(state)
			}
		})
	}
}
