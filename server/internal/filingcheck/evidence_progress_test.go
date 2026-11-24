package filingcheck

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

func TestEvidenceMissingDistinctSelectionsPersist(t *testing.T) {
	calls := 0
	one := Record{"body": Record{"section": "不存在1"}}
	two := Record{"body": Record{"section": "不存在2"}}
	s, d, material := fixture(t, func(_ context.Context, input map[string]any) (map[string]any, error) {
		calls++
		out := allowed()
		if calls == 1 {
			out["proceed"] = false
			out["evidenceRequests"] = []any{Record{"candidateId": "request:3", "kind": "body", "selection": one}, Record{"candidateId": "request:3", "kind": "body", "selection": one}, Record{"candidateId": "request:3", "kind": "body", "selection": two}}
		} else {
			var sent Record
			if err := json.Unmarshal([]byte(strings.SplitN(fleet.Text(input["input"]), "\n確認資料:\n", 2)[1]), &sent); err != nil {
				t.Fatal(err)
			}
			if got := len(fleet.Elements(sent["evidenceMissing"])); got != 2 {
				t.Fatalf("missing sent: %v", sent["evidenceMissing"])
			}
		}
		return out, nil
	})
	(*material)["requests"] = []any{Record{"number": 3, "body": "## 目的\n本文"}}
	if err := s.Check(context.Background(), d); err == nil {
		t.Fatal("未解消の選択失敗を許可した")
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
	if err != nil {
		t.Fatal(err)
	}
	missing := fleet.Elements(obj(obj(saved["filingCheckCalls"])["evidenceHold"])["evidenceMissing"])
	if calls != 2 || len(missing) != 2 || string(raw(obj(missing[0])["selection"])) == string(raw(obj(missing[1])["selection"])) {
		t.Fatalf("calls=%d missing=%v", calls, missing)
	}
}

func TestUnfittableEvidenceGetsFinalDecisionWithoutAcquisition(t *testing.T) {
	for _, tc := range []struct {
		name    string
		again   []string
		allowed bool
		want    []string
		absent  []string
	}{{"見えた範囲で許可", nil, true, nil, nil}, {"不可欠として再要求", []string{"request:3"}, false, []string{"容量上限で読めない資料が判定に不可欠です: 候補ID=request:3", "consultation:<相談ID>"}, []string{"別の資料が求められました"}}, {"別の候補を要求", []string{"request:9"}, false, []string{"別の資料が求められました: 候補ID=request:9", "相談の本文へ書き足して"}, []string{"判定に不可欠です", "consultation:<相談ID>"}}, {"両方を要求", []string{"request:3", "request:9"}, false, []string{"容量上限で読めない資料が判定に不可欠です: 候補ID=request:3", "別の資料が求められました: 候補ID=request:9"}, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s, d, material := fixture(t, func(_ context.Context, input map[string]any) (map[string]any, error) {
				calls++
				out := allowed()
				ids := tc.again
				if calls == 1 {
					ids = []string{"request:3"}
				}
				if len(ids) > 0 {
					out["proceed"] = false
					requests := []any{}
					for _, id := range ids {
						requests = append(requests, Record{"candidateId": id, "kind": "body"})
					}
					out["evidenceRequests"] = requests
				}
				if calls == 2 {
					text := fleet.Text(input["input"])
					var sent Record
					if err := json.Unmarshal([]byte(strings.SplitN(text, "\n確認資料:\n", 2)[1]), &sent); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(text, finalBudgetInstructions) || sent["evidenceBudgetFinal"] != true || !strings.Contains(string(raw(sent["evidenceOmitted"])), "資料入力上限") {
						t.Fatalf("最終判定の入力ではない: %v", sent["evidenceOmitted"])
					}
				}
				return out, nil
			})
			(*material)["requests"] = []any{Record{"number": 3, "body": strings.Repeat("x", 61000)}}
			err := s.Check(context.Background(), d)
			saved, _, e := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
			if e != nil {
				t.Fatal(e)
			}
			savedCalls := obj(saved["filingCheckCalls"])
			hold := obj(savedCalls["evidenceHold"])
			budget := obj(savedCalls["budget"])
			if fleet.WholeNumber(savedCalls["count"]) != 2 || fleet.WholeNumber(budget["evidence"]) != 1 || fleet.WholeNumber(budget["decisions"]) != 1 {
				t.Fatalf("budget=%v count=%v", budget, savedCalls["count"])
			}
			if calls != 2 || (err == nil) != tc.allowed {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if tc.allowed && (hold != nil || savedCalls["outcome"] != "allowed") {
				t.Fatalf("saved=%v", savedCalls)
			}
			if !tc.allowed && (!strings.Contains(fleet.Text(hold["reason"]), "60000") || savedCalls["stopKind"] != "evidence_missing") {
				t.Fatalf("saved=%v", savedCalls)
			}
			for _, want := range tc.want {
				if !strings.Contains(fleet.Text(hold["reason"]), want) {
					t.Fatalf("理由に %q が無い: %v", want, hold["reason"])
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(fleet.Text(hold["reason"]), absent) {
					t.Fatalf("理由に %q がある: %v", absent, hold["reason"])
				}
			}
		})
	}
}

func TestInputLimitOnlyMissing(t *testing.T) {
	limit := Record{"candidateId": "request:3", "kind": "body", "reason": omissionInputLimitReason}
	for name, tc := range map[string]struct {
		items []any
		want  bool
	}{
		"容量上限だけ":   {[]any{limit}, true},
		"空":        {nil, false},
		"件数上限を含む":  {[]any{limit, Record{"candidateId": "request:4", "reason": "件数上限"}}, false},
		"取得不能を含む":  {[]any{limit, Record{"candidateId": "request:4", "reason": "候補を取得できません"}}, false},
		"選択失敗":     {[]any{Record{"candidateId": "request:3", "reason": "選択失敗: 節がありません"}}, false},
		"入力全体の超過":  {[]any{Record{"reason": "入力上限", "detail": "x"}}, false},
		"同一選択の繰返し": {[]any{Record{"reason": "同一の資料選択が繰り返されました"}}, false},
	} {
		if got := inputLimitOnlyMissing(tc.items); got != tc.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

func TestRecentEvidenceGetsPriorityAcrossRounds(t *testing.T) {
	calls := 0
	s, d, material := fixture(t, func(_ context.Context, input map[string]any) (map[string]any, error) {
		calls++
		var sent Record
		if err := json.Unmarshal([]byte(strings.SplitN(fleet.Text(input["input"]), "\n確認資料:\n", 2)[1]), &sent); err != nil {
			t.Fatal(err)
		}
		if calls == 3 {
			if !strings.Contains(string(raw(sent["evidence"])), strings.Repeat("b", 1000)) {
				t.Fatalf("new body absent: %v", sent["evidenceOmitted"])
			}
			return allowed(), nil
		}
		out := allowed()
		out["proceed"] = false
		out["evidenceRequests"] = []any{Record{"candidateId": []string{"request:3", "request:4"}[calls-1], "kind": "body"}}
		return out, nil
	})
	(*material)["requests"] = []any{Record{"number": 3, "body": strings.Repeat("a", 35000)}, Record{"number": 4, "body": strings.Repeat("b", 35000)}}
	if err := s.Check(context.Background(), d); err != nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestPreviousHoldProjectionPreservesStoredOriginal(t *testing.T) {
	for _, mode := range []string{"needed", "unneeded", "essential-too-large"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			alternatives := []any{}
			for i := 0; i < 115; i++ {
				alternatives = append(alternatives, Record{"number": i + 1, "title": strings.Repeat("旧", 100), "acceptsInstructions": true})
			}
			prior := Record{"fingerprint": "previous", "revision": 1, "codeRevision": "old-code", "at": "old-time", "reason": "必要な拒否根拠", "alternatives": alternatives}
			if mode == "essential-too-large" {
				prior["reason"] = strings.Repeat("根", 43000)
			}
			s, d, material := fixture(t, func(_ context.Context, input map[string]any) (map[string]any, error) {
				calls++
				var sent Record
				if err := json.Unmarshal([]byte(strings.SplitN(fleet.Text(input["input"]), "\n確認資料:\n", 2)[1]), &sent); err != nil {
					t.Fatal(err)
				}
				previous := obj(sent["previousRefusal"])
				if previous["fingerprint"] != "previous" || previous["codeRevision"] != "old-code" || previous["at"] != "old-time" || previous["reason"] != prior["reason"] || len(fleet.Elements(sent["alternativeIssues"])) != 1 {
					t.Fatalf("hold fields lost: %v", previous)
				}
				if calls == 2 && mode != "essential-too-large" {
					projection := obj(previous["alternatives"])
					if mode == "needed" && (projection == nil || fleet.WholeNumber(projection["count"]) != 115 || len(fleet.Elements(projection["items"])) != 115 || projection["source"] != "filingCheckHold.alternatives" || !strings.Contains(fleet.Text(projection["note"]), "未確認")) {
						t.Fatalf("projection=%v", previous["alternatives"])
					}
					if mode == "unneeded" && len(fleet.Elements(previous["alternatives"])) != 115 {
						t.Fatal("不要な縮小")
					}
					if !strings.Contains(string(raw(sent["evidence"])), "必要本文") {
						t.Fatal("本文なし")
					}
					return allowed(), nil
				}
				out := allowed()
				out["proceed"] = false
				out["evidenceRequests"] = []any{Record{"candidateId": "request:3", "kind": "body"}}
				return out, nil
			})
			d["filingCheckHold"] = prior
			_, e := s.options.Records.Change(context.Background(), "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, row Record) (Record, error) { row["filingCheckHold"] = prior; return row, nil })
			if e != nil {
				t.Fatal(e)
			}
			targetLength := 85000
			if mode == "unneeded" {
				targetLength = 1000
			}
			if mode == "essential-too-large" {
				targetLength = 1000
			}
			(*material)["target"] = Record{"body": strings.Repeat("t", targetLength), "dependsOnIssues": []any{Record{"source": "gitea", "number": 7}}}
			(*material)["requests"] = []any{Record{"number": 3, "title": "最新候補", "body": "必要本文" + strings.Repeat("b", 35000)}}
			err := s.Check(context.Background(), d, "explicit")
			saved, _, e := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
			if e != nil {
				t.Fatal(e)
			}
			if mode == "essential-too-large" {
				// The mandatory reason and requested body cannot both fit, so the final decision runs without it.
				if err == nil || calls != 2 || string(raw(saved["filingCheckHold"])) != string(raw(prior)) || !strings.Contains(fleet.Text(obj(obj(saved["filingCheckCalls"])["evidenceHold"])["reason"]), "160000") {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				return
			}
			if err != nil || calls != 2 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			stored := obj(obj(saved["filingRefusalOverturned"])["previous"])
			if string(raw(stored)) != string(raw(prior)) {
				t.Fatal("保存原本が変更された")
			}
		})
	}
}

func TestOmissionDiagnosticsDoNotAdvancePhase(t *testing.T) {
	calls := 0
	s, d, material := fixture(t, func(_ context.Context, _ map[string]any) (map[string]any, error) {
		calls++
		out := allowed()
		out["proceed"] = false
		id := "request:3"
		if calls > 1 {
			id = "request:4"
		}
		out["evidenceRequests"] = []any{Record{"candidateId": id, "kind": "body"}}
		return out, nil
	})
	(*material)["requests"] = []any{Record{"number": 3, "body": "取得済み本文"}, Record{"number": 4, "body": strings.Repeat("z", 61000)}}
	if err := s.Check(context.Background(), d); err == nil {
		t.Fatal("不可欠な資料が読めないのに進行した")
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
	if err != nil {
		t.Fatal(err)
	}
	savedCalls := obj(saved["filingCheckCalls"])
	hold := obj(savedCalls["evidenceHold"])
	budget := obj(savedCalls["budget"])
	// 容量上限で読めない request:4 は、追加取得なしの最終判定を1回行ってから止める。
	if calls != 3 || fleet.WholeNumber(savedCalls["count"]) != 3 || fleet.WholeNumber(budget["evidence"]) != 2 || fleet.WholeNumber(budget["decisions"]) != 1 || savedCalls["stopKind"] != "evidence_missing" || !strings.Contains(fleet.Text(hold["reason"]), "request:4") || !strings.Contains(fleet.Text(hold["reason"]), "60000") || len(fleet.Elements(hold["evidenceMissing"])) != 1 {
		t.Fatalf("calls=%d hold=%v", calls, hold)
	}
}

func TestOneReplacementKeepsOtherMissingSelection(t *testing.T) {
	calls := 0
	s, d, material := fixture(t, func(_ context.Context, input map[string]any) (map[string]any, error) {
		calls++
		out := allowed()
		if calls == 1 {
			out["proceed"] = false
			out["evidenceRequests"] = []any{Record{"candidateId": "request:3", "kind": "body", "selection": Record{"body": Record{"section": "不在1"}}}, Record{"candidateId": "request:3", "kind": "body", "selection": Record{"body": Record{"section": "不在2"}}}}
		} else if calls == 2 {
			out["proceed"] = false
			out["evidenceRequests"] = []any{Record{"candidateId": "request:3", "kind": "body", "selection": Record{"body": Record{"section": "目的"}}}}
		} else {
			var sent Record
			if err := json.Unmarshal([]byte(strings.SplitN(fleet.Text(input["input"]), "\n確認資料:\n", 2)[1]), &sent); err != nil {
				t.Fatal(err)
			}
			if len(fleet.Elements(sent["evidenceMissing"])) != 1 || !strings.Contains(string(raw(sent["evidence"])), "本文") {
				t.Fatalf("sent=%v", sent)
			}
		}
		return out, nil
	})
	(*material)["requests"] = []any{Record{"number": 3, "body": "## 目的\n本文"}}
	if err := s.Check(context.Background(), d); err == nil {
		t.Fatal("未解消の選択がある")
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
	if err != nil {
		t.Fatal(err)
	}
	missing := fleet.Elements(obj(obj(saved["filingCheckCalls"])["evidenceHold"])["evidenceMissing"])
	if calls != 3 || len(missing) != 1 || !strings.Contains(string(raw(missing)), "不在2") {
		t.Fatalf("calls=%d missing=%v", calls, missing)
	}
}
