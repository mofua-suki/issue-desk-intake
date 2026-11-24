package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/completionplan"
)

func questionEvidence(kind, source, quote string) string {
	b, _ := json.Marshal(recoveryAnswerBasis{Kind: kind, Sources: []recoveryAnswerSource{{Source: source, Quote: quote}}})
	return string(b)
}

func TestRecoveryQuestionFitsExistingReplyLimit(t *testing.T) {
	detail := map[string]any{"status": "waiting", "body": "既存の型を維持する。"}
	action := map[string]any{"text": strings.Repeat("あ", 8000), "evidence": questionEvidence("fact", "body", "既存の型を維持する。")}
	if ok, reason := recoveryQuestionAnswer(detail, action); !ok {
		t.Fatal(reason)
	}
	action["text"] = strings.Repeat("あ", 8001)
	if ok, _ := recoveryQuestionAnswer(detail, action); ok {
		t.Fatal("既存の返信上限を超える回答を送信可能にしました")
	}
}

func TestRecoveryQuestionGroundedAnswer(t *testing.T) {
	detail := map[string]any{"status": "waiting", "body": "既存APIのJSON契約を維持する。", "comments": []any{map[string]any{"body": "型検査はpnpm checkで行う。"}}}
	for _, tc := range []struct {
		name, evidence, text string
		want                 bool
	}{
		{"fact from body", questionEvidence("fact", "body", "既存APIのJSON契約を維持する。"), "既存のJSON契約に従って実装してください。", true},
		{"convention from comment", `{"kind":"convention","sources":[{"source":"comment","index":0,"quote":"型検査はpnpm checkで行う。"}]}`, "型検査にはpnpm checkを実行してください。", true},
		{"free form evidence", "根拠があります", "実装してください。", false},
		{"invented quotation", questionEvidence("fact", "body", "Go 1.27で検証する。"), "実装してください。", false},
		{"comment without index", questionEvidence("fact", "comment", "型検査はpnpm checkで行う。"), "実装してください。", false},
		{"out of range", `{"kind":"fact","sources":[{"source":"comment","index":1,"quote":"型検査はpnpm checkで行う。"}]}`, "実装してください。", false},
		{"body with index", `{"kind":"fact","sources":[{"source":"body","index":0,"quote":"既存APIのJSON契約を維持する。"}]}`, "実装してください。", false},
		{"new approval category", questionEvidence("approval", "body", "既存APIのJSON契約を維持する。"), "進めてください。", false},
		{"new approval declaration", questionEvidence("fact", "body", "既存APIのJSON契約を維持する。"), "権限変更を承認します。", false},
		{"extra property", `{"kind":"fact","sources":[{"source":"body","quote":"既存APIのJSON契約を維持する。"}],"approved":true}`, "進めてください。", false},
		{"trailing JSON", questionEvidence("fact", "body", "既存APIのJSON契約を維持する。") + `{}`, "進めてください。", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := recoveryQuestionAnswer(detail, map[string]any{"text": tc.text, "evidence": tc.evidence})
			if got != tc.want || !got && reason == "" {
				t.Fatalf("answer=%v reason=%q", got, reason)
			}
		})
	}
}

func questionDecisionFixture() (map[string]any, map[string]any) {
	p := completionplan.Plan{Version: 1, Basis: "既存のAPIと権限を維持する", Approvals: []completionplan.Approval{{Category: "scope", ID: "scope", Phase: "before_implementation", Target: "API入力検証", Operation: "既存契約内で不正入力を拒否する", Condition: "既存のJSON契約を維持する"}}}
	for _, phase := range []string{"implementation", "verification", "merge", "deployment"} {
		p.Steps = append(p.Steps, completionplan.Step{Phase: phase, Owner: "担当AI", Prerequisite: "既存の承認規則", Evidence: "工程の結果を検証する"})
	}
	receipt, confirmed := completionplan.Confirmed(p, []any{
		map[string]any{"role": "assistant", "completionProposal": p.Fingerprint()},
		map[string]any{"role": "user", "id": "user-1", "text": "1-A", "at": "2026-11-07T04:00:00+09:00", "source": "screen-session"},
	})
	if !confirmed {
		panic("completion plan fixture did not receive its controller receipt")
	}
	receiptJSON, _ := json.Marshal(receipt)
	var receiptRecord map[string]any
	_ = json.Unmarshal(receiptJSON, &receiptRecord)
	body := "受入条件: 既存のJSON契約を維持する。対象はAPI入力検証。"
	h := sha256.Sum256([]byte(body))
	detail := map[string]any{
		"status": "waiting", "body": body,
		"completionPlan":         p.Record(),
		"completionApproval":     receiptRecord,
		"completionPlanEvidence": map[string]any{"bodySHA256": hex.EncodeToString(h[:])},
	}
	b, _ := json.Marshal(recoveryAnswerBasis{Kind: "pastDecision", ApprovalID: "scope", Sources: []recoveryAnswerSource{{Source: "body", Quote: "対象はAPI入力検証。"}}, Conditions: []recoveryAnswerSource{{Source: "body", Quote: "既存のJSON契約を維持する"}}})
	return detail, map[string]any{"text": "既存のJSON契約を維持する条件で、対象はAPI入力検証です。", "evidence": string(b)}
}

func TestRecoveryQuestionPastDecisionPreservesBasis(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any, map[string]any)
		want bool
	}{
		{"same decision", nil, true},
		{"body edited", func(d, _ map[string]any) { d["body"] = d["body"].(string) + "新しい受入条件" }, false},
		{"missing receipt", func(d, _ map[string]any) { delete(d, "completionApproval") }, false},
		{"AI or CLI sender", func(d, _ map[string]any) { d["completionApproval"].(map[string]any)["source"] = "cli-key" }, false},
		{"old fingerprint", func(d, _ map[string]any) { d["completionApproval"].(map[string]any)["fingerprint"] = "old" }, false},
		{"changed plan", func(d, _ map[string]any) { d["completionPlan"].(map[string]any)["basis"] = "新しい目的に変更" }, false},
		{"missing body evidence", func(d, _ map[string]any) { delete(d, "completionPlanEvidence") }, false},
		{"later approval", func(d, _ map[string]any) {
			p, _ := completionplan.Parse(d["completionPlan"])
			p.Approvals[0].Phase = "after_diff"
			d["completionPlan"] = p.Record()
			d["completionApproval"].(map[string]any)["fingerprint"] = p.Fingerprint()
		}, false},
		{"missing observed condition", func(_, a map[string]any) {
			var basis recoveryAnswerBasis
			_ = json.Unmarshal([]byte(a["evidence"].(string)), &basis)
			basis.Conditions = nil
			b, _ := json.Marshal(basis)
			a["evidence"] = string(b)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail, action := questionDecisionFixture()
			if tc.edit != nil {
				tc.edit(detail, action)
			}
			got, reason := recoveryQuestionAnswer(detail, action)
			if got != tc.want || !got && reason == "" {
				t.Fatalf("answer=%v reason=%q", got, reason)
			}
		})
	}
}

func TestRecoveryQuestionSnapshotFreezesAcceptanceAndApprovals(t *testing.T) {
	for _, key := range []string{"body", "comments", "result", "completionPlan", "completionApproval", "completionPlanEvidence", "generation"} {
		t.Run(key, func(t *testing.T) {
			detail, _ := questionDecisionFixture()
			snapshot, err := recoveryTargetSnapshot(detail)
			if err != nil || !recoveryTargetUnchanged(snapshot, detail) {
				t.Fatal(snapshot, err)
			}
			detail[key] = "changed"
			if recoveryTargetUnchanged(snapshot, detail) {
				t.Fatal("changed evidence was accepted")
			}
		})
	}
}

func TestRecoveryQuestionMixedApprovalSenders(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, selected string
		want                          bool
	}{
		{"AI quote after human quote", "screen-session", "pc-token", "second", false},
		{"human quote after AI quote", "pc-token", "screen-session", "second", true},
		{"human quote remains usable", "screen-session", "pc-token", "first", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail, action := questionDecisionFixture()
			p, err := completionplan.Parse(detail["completionPlan"])
			if err != nil {
				t.Fatal(err)
			}
			p.Approvals = []completionplan.Approval{}
			messages := []any{}
			for i, id := range []string{"first", "second"} {
				target, operation, condition := "対象"+id, "操作"+id, "前提"+id
				quote := target + "の" + operation + "を" + condition + "で実行してください。"
				at := "2026-11-07T04:00:00+09:00"
				source := tc.first
				if i == 1 {
					source = tc.second
				}
				p.Approvals = append(p.Approvals, completionplan.Approval{Category: "permission", ID: id, Phase: "before_implementation", Target: target, Operation: operation, Condition: condition, Evidence: &completionplan.Evidence{MessageID: id, Quote: quote, At: at}})
				messages = append(messages, map[string]any{"role": "user", "id": id, "text": quote, "at": at, "source": source})
			}
			receipt, ok := completionplan.Confirmed(p, messages)
			if !ok || len(receipt.Evidence) != 2 {
				t.Fatal(receipt, ok)
			}
			encoded, _ := json.Marshal(receipt)
			var receiptRecord map[string]any
			_ = json.Unmarshal(encoded, &receiptRecord)
			body := "対象firstと対象second。前提firstと前提secondを維持する。"
			hash := sha256.Sum256([]byte(body))
			detail["body"], detail["completionPlan"], detail["completionApproval"] = body, p.Record(), receiptRecord
			detail["completionPlanEvidence"] = map[string]any{"bodySHA256": hex.EncodeToString(hash[:])}
			basis := recoveryAnswerBasis{Kind: "pastDecision", ApprovalID: tc.selected, Sources: []recoveryAnswerSource{{Source: "body", Quote: "対象" + tc.selected}}, Conditions: []recoveryAnswerSource{{Source: "body", Quote: "前提" + tc.selected}}}
			encoded, _ = json.Marshal(basis)
			action["evidence"] = string(encoded)
			if got, reason := recoveryQuestionAnswer(detail, action); got != tc.want {
				t.Fatal(got, reason)
			}
		})
	}
}

func TestRecoveryQuestionAnswersWithoutUserMediationPostgres(t *testing.T) {
	r, _, a := recoveryActFixture(t)
	_, err := r.Service.PostgreSQL.Exec(`UPDATE jobs SET status='held',hold_reason='question',result='{"questions":["どの契約を使うか"]}'::jsonb`)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := r.Candidates(context.Background())
	if err != nil || len(candidates) != 1 {
		t.Fatal(candidates, err)
	}
	a.ReadTarget = func(context.Context, string, int64) (map[string]any, error) {
		return map[string]any{"project": "owner/repo", "number": 1, "status": "waiting", "body": "既存JSON契約を維持する。", "comments": []any{}}, nil
	}
	a.Judge = func(_ context.Context, items []any) (map[string]any, error) {
		result := recoveryDecision(items, "reply")
		action := result["actions"].([]any)[0].(map[string]any)
		action["evidence"] = questionEvidence("fact", "body", "既存JSON契約を維持する。")
		action["text"] = "既存JSON契約を維持してください。"
		return result, nil
	}
	sends := 0
	// Use the same public send contract as production, and count after receipt.
	a.Send = func(_ context.Context, _ string, _ int64, _, action, text string) error {
		if action != "reply" || text != "既存JSON契約を維持してください。" {
			t.Fatal(action, text)
		}
		sends++
		return nil
	}
	got, err := a.Act(context.Background(), candidates[0])
	if err != nil || got != Acted || sends != 1 {
		t.Fatal(got, sends, err)
	}
	_, err = a.Act(context.Background(), candidates[0])
	if err != nil || sends != 1 {
		t.Fatal(sends, err)
	}
}

func TestRecoveryFollowupCausePostgres(t *testing.T) {
	r := recoveryFixture(t)
	ctx := context.Background()
	recoveryInsert(t, r, 1, "held", "question", "followup", "owner/repo", `{}`, `{"summary":"元の指摘: API契約が不足","incomplete":{"reason":"契約資料が必要"}}`, 2, nil, "1 hour")
	recoveryInsert(t, r, 2, "held", "question", "followup", "owner/repo", `{}`, `{"questions":["何を使うか"],"summary":"旧い概要"}`, 1, nil, "1 hour")
	recoveryInsert(t, r, 3, "held", "question", "normal", "owner/repo", `{}`, `{"questions":[],"summary":"不足"}`, 0, nil, "1 hour")
	recoveryInsert(t, r, 4, "held", "question", "normal", "owner/repo", `{}`, `{"questions":["通常の質問"]}`, 0, nil, "1 hour")
	for _, n := range []int{1, 2} {
		recoveryExec(t, r, `INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES($1,now(),'nas','owner/repo',$2,$3,'test')`, fmt.Sprint(n), n, fmt.Sprintf(`{"repo":"owner/repo","issue":%d,"provider":"codex","inputTokens":1,"cacheReadTokens":0,"outputTokens":0,"step":"実装"}`, n))
	}
	cs, err := r.Candidates(ctx)
	if err != nil || len(cs) != 3 {
		t.Fatal(cs, err)
	}
	by := map[int64]RecoveryCandidate{}
	for _, c := range cs {
		by[c.Job.Issue] = c
	}
	if by[4].Cause == "" {
		t.Fatal("通常の質問が候補から外れました")
	}
	c := by[1]
	if c.Job.Generation != 2 || c.Job.Result["summary"] != "元の指摘: API契約が不足" || c.Job.Result["incomplete"].(map[string]any)["reason"] != "契約資料が必要" {
		t.Fatal(c)
	}
	recoveryExec(t, r, `UPDATE jobs SET result='{"questions":["何を使うか"],"summary":"新しい概要"}' WHERE issue=2`)
	ok, err := r.Recheck(ctx, by[2])
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	recoveryExec(t, r, `UPDATE jobs SET result='{"questions":[],"summary":"別の不足"}' WHERE issue=1`)
	ok, err = r.Recheck(ctx, c)
	if err != nil || ok {
		t.Fatal(ok, err)
	}
}

func TestRecoveryFollowupHandoffPostgres(t *testing.T) {
	for _, scenario := range []string{"grounded", "missing", "unreadable", "repeat", "missing-repeat", "changed", "unreadable-second", "judge-reason"} {
		for _, question := range []bool{false, true} {
			name := fmt.Sprintf("%s/question=%t", scenario, question)
			t.Run(name, func(t *testing.T) {
				r := recoveryFixture(t)
				ctx := context.Background()
				result := `{"questions":[],"summary":"元の指摘と不足する契約資料"}`
				if question {
					result = `{"questions":["どの契約を使うか"],"summary":"元の指摘と不足する契約資料"}`
				}
				recoveryInsert(t, r, 1, "held", "question", "followup", "owner/repo", `{}`, result, 0, nil, "1 hour")
				recoveryExec(t, r, `INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES('u',now(),'nas','owner/repo',1,$1,'test')`, `{"repo":"owner/repo","issue":1,"provider":"codex","inputTokens":1,"cacheReadTokens":0,"outputTokens":0,"step":"実装"}`)
				cs, err := r.Candidates(ctx)
				if err != nil || len(cs) != 1 {
					t.Fatal(cs, err)
				}
				c := cs[0]
				sends, reads := 0, 0
				a := &RecoveryActions{Recovery: r, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
					reads++
					if scenario == "unreadable" || scenario == "unreadable-second" && reads == 2 {
						return nil, fmt.Errorf("unavailable")
					}
					body := "既存JSON契約を維持する。"
					if scenario == "changed" && reads == 2 {
						body = "変更された契約"
					}
					return map[string]any{"project": "owner/repo", "number": 1, "status": "waiting", "state": "open", "body": body, "comments": []any{}}, nil
				}, Judge: func(_ context.Context, items []any) (map[string]any, error) {
					d := recoveryDecision(items, "reply")
					action := d["actions"].([]any)[0].(map[string]any)
					action["text"] = "既存JSON契約を維持してください。"
					action["evidence"] = questionEvidence("fact", "body", "既存JSON契約を維持する。")
					if scenario == "missing" || scenario == "missing-repeat" {
						action["evidence"] = "missing"
					}
					if scenario == "judge-reason" {
						action["action"] = "none"
						action["reason"] = "資料不足 token=abc123 の確認が必要"
					}
					return d, nil
				}, Send: func(context.Context, string, int64, string, string, string) error { sends++; return nil }}
				got, err := a.Act(ctx, c)
				if err != nil {
					t.Fatal(got, err)
				}
				if scenario == "grounded" || scenario == "repeat" {
					if got != Acted || sends != 1 {
						t.Fatal(got, sends)
					}
				} else if got != NeedsUser || sends != 0 {
					t.Fatal(got, sends)
				}
				if scenario == "repeat" {
					recoveryExec(t, r, `UPDATE jobs SET generation=1 WHERE issue=1`)
					cs, err = r.Candidates(ctx)
					if err != nil || len(cs) != 1 {
						t.Fatal(cs, err)
					}
					c = cs[0]
					got, err = a.Act(ctx, c)
					if err != nil || got != Abandoned || sends != 1 {
						t.Fatal(got, sends, err)
					}
				}
				if scenario == "missing-repeat" {
					recoveryExec(t, r, `UPDATE jobs SET generation=1 WHERE issue=1`)
					cs, err = r.Candidates(ctx)
					if err != nil || len(cs) != 1 {
						t.Fatal(cs, err)
					}
					c = cs[0]
					got, err = a.Act(ctx, c)
					if err != nil || got != Abandoned || sends != 0 {
						t.Fatal(got, sends, err)
					}
				}
				if scenario != "grounded" {
					rows, err := r.Service.jobRecoveryAttention(ctx, map[string]bool{"owner/repo": true})
					if err != nil || len(rows) != 1 {
						t.Fatal(rows, err)
					}
					row := rows[0].(map[string]any)
					if row["handoff"] != true || !strings.Contains(row["reason"].(string), "受付担当") {
						t.Fatal(row)
					}
					if question && !strings.Contains(row["gist"].(string), "どの契約を使うか") {
						t.Fatal(row)
					}
					if scenario != "repeat" && !strings.Contains(row["gist"].(string), "元の指摘") {
						t.Fatal(row)
					}
					saved := recoveryStored(t, r, c)
					if !saved.Handoff || saved.RequiresUser == nil || *saved.RequiresUser || saved.SendCount > 1 {
						t.Fatal(saved)
					}
					if scenario == "judge-reason" {
						for _, reason := range []string{saved.Reason, row["reason"].(string)} {
							if strings.Contains(reason, "abc123") || !strings.Contains(reason, "[REDACTED]") || !strings.Contains(reason, "資料不足") {
								t.Fatalf("reason was not safely retained: %q", reason)
							}
						}
					}
					_, err = a.Act(ctx, c)
					if err != nil || sends > 1 {
						t.Fatal(sends, err)
					}
					r.Service.Gitea = urgentIssueReader{}
					rows, err = r.Service.jobRecoveryAttention(ctx, map[string]bool{"owner/repo": true})
					if err != nil || len(rows) != 0 {
						t.Fatal("クローズ後", rows, err)
					}
					r.Service.Gitea = nil
					recoveryExec(t, r, `UPDATE jobs SET status='closed',hold_reason=NULL WHERE issue=1`)
					rows, err = r.Service.jobRecoveryAttention(ctx, map[string]bool{"owner/repo": true})
					if err != nil || len(rows) != 0 {
						t.Fatal("解消後", rows, err)
					}
					recoveryExec(t, r, `UPDATE jobs SET status='held',hold_reason='question' WHERE issue=1`)
					recoveryExec(t, r, `UPDATE jobs SET generation=generation+1 WHERE issue=1`)
					rows, err = r.Service.jobRecoveryAttention(ctx, map[string]bool{"owner/repo": true})
					if err != nil || len(rows) != 0 {
						t.Fatal(rows, err)
					}
				}
			})
		}
	}
}

func TestRecoveryFollowupBoundariesPostgres(t *testing.T) {
	r := recoveryFixture(t)
	ctx := context.Background()
	for i, tc := range []struct{ kind, view, result string }{
		{"followup", `{}`, `{"questions":[],"summary":"契約不足"}`},
		{"normal", `{}`, `{"questions":[],"summary":"契約不足"}`},
		{"followup", `{"permissionHold":{}}`, `{"questions":[],"summary":"契約不足"}`},
		{"followup", `{"deletionApproval":{}}`, `{"questions":[],"summary":"契約不足"}`},
		{"followup", `{"finalApprovalHold":{}}`, `{"questions":[],"summary":"契約不足"}`},
		{"followup", `{"workerExecution":{"implementationRuns":4}}`, `{"questions":[],"summary":"契約不足"}`},
		{"followup", `{"stopRequested":true}`, `{"questions":[],"summary":"契約不足"}`},
		{"followup", `{"prerequisiteMissingGenerations":[1]}`, `{"status":"failed","questions":[],"summary":"契約不足","prerequisiteMissing":{"reason":"missing","artifacts":[{"artifact":"API","repo":"owner/repo","issue":1}]}}`},
		{"followup", `{}`, `{"questions":[],"summary":"削除の承認"}`},
		{"followup", `{}`, `{"questions":[],"summary":"契約不足"}`},
		{"followup", `{}`, `{"questions":[],"summary":"権限の範囲を定めるファイルの変更"}`},
	} {
		recoveryInsert(t, r, i+1, "held", "question", tc.kind, "owner/repo", tc.view, tc.result, 1, nil, "1 hour")
	}
	// The same existing gates also apply when the followup has a concrete question.
	for i, view := range []string{`{"permissionHold":{}}`, `{"deletionApproval":{}}`, `{"finalApprovalHold":{}}`, `{"workerExecution":{"implementationRuns":4}}`, `{"stopRequested":true}`, `{"prerequisiteMissingGenerations":[1]}`} {
		result := `{"questions":["どの契約を使うか"]}`
		if i == 5 {
			result = `{"status":"failed","questions":["どの契約を使うか"],"prerequisiteMissing":{"reason":"missing","artifacts":[{"artifact":"API","repo":"owner/repo","issue":1}]}}`
		}
		recoveryInsert(t, r, i+12, "held", "question", "followup", "owner/repo", view, result, 1, nil, "1 hour")
	}
	for n := 1; n <= 17; n++ {
		tokens := 1
		if n == 10 {
			tokens = 101
		}
		recoveryExec(t, r, `INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES($1,now(),'nas','owner/repo',$2,$3,'test')`, fmt.Sprint(n), n, fmt.Sprintf(`{"repo":"owner/repo","issue":%d,"provider":"codex","inputTokens":%d,"cacheReadTokens":0,"outputTokens":0,"step":"実装"}`, n, tokens))
	}
	cs, err := r.Candidates(ctx)
	if err != nil || len(cs) != 1 || cs[0].Job.Issue != 1 {
		t.Fatal(cs, err)
	}
	c := cs[0]
	sends := 0
	reads := 0
	a := &RecoveryActions{Recovery: r, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
		reads++
		if reads == 2 {
			recoveryExec(t, r, `UPDATE jobs SET generation=2 WHERE issue=1`)
		}
		return map[string]any{"project": "owner/repo", "number": 1, "status": "waiting", "body": "契約を維持する", "comments": []any{}}, nil
	}, Judge: func(_ context.Context, items []any) (map[string]any, error) {
		d := recoveryDecision(items, "reply")
		action := d["actions"].([]any)[0].(map[string]any)
		action["evidence"] = questionEvidence("fact", "body", "契約を維持する")
		return d, nil
	}, Send: func(context.Context, string, int64, string, string, string) error { sends++; return nil }}
	got, err := a.Act(ctx, c)
	if err != nil || got != Skipped || sends != 0 || reads != 2 {
		t.Fatal(got, sends, reads, err)
	}
}

func TestRecoveryFollowupUserApprovalPostgres(t *testing.T) {
	r := recoveryFixture(t)
	ctx := context.Background()
	recoveryInsert(t, r, 1, "held", "question", "followup", "owner/repo", `{}`, `{"questions":["権限変更を承認しますか"]}`, 0, nil, "1 hour")
	recoveryExec(t, r, `INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES('u',now(),'nas','owner/repo',1,$1,'test')`, `{"repo":"owner/repo","issue":1,"provider":"codex","inputTokens":1,"cacheReadTokens":0,"outputTokens":0,"step":"実装"}`)
	cs, err := r.Candidates(ctx)
	if err != nil || len(cs) != 1 {
		t.Fatal(cs, err)
	}
	sends := 0
	a := &RecoveryActions{Recovery: r, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
		return map[string]any{"project": "owner/repo", "number": 1, "status": "waiting", "body": "権限変更が必要", "comments": []any{}}, nil
	}, Judge: func(_ context.Context, items []any) (map[string]any, error) {
		d := recoveryDecision(items, "notify")
		action := d["actions"].([]any)[0].(map[string]any)
		action["requiresUser"] = true
		return d, nil
	}, Send: func(context.Context, string, int64, string, string, string) error { sends++; return nil }}
	got, err := a.Act(ctx, cs[0])
	saved := recoveryStored(t, r, cs[0])
	if err != nil || got != NeedsUser || sends != 0 || saved.RequiresUser == nil || !*saved.RequiresUser || saved.Handoff {
		t.Fatal(got, sends, saved, err)
	}
}
