package consultationai

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/filing"
)

func approvalFixture() completionplan.Plan {
	p := completionFixture()
	p.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "owner/app", Operation: "設定変更", Condition: "隔離環境"}}
	return p
}

func presentation(p completionplan.Plan) Record {
	return Record{"role": "assistant", "completionProposal": p.Fingerprint(), "completionPlan": p.Record()}
}

func explicitReply(id, text string) Record {
	return Record{"role": "user", "id": id, "text": text, "at": "2026-11-07T10:00:00+09:00", "source": "cli-key"}
}

func candidateAnswer(p completionplan.Plan) Record {
	return Record{"ready": true, "completionPlan": p.Record(), "questions": []any{}}
}

func saveCompletionTurn(d, answer Record) Record {
	next := copyRecord(d)
	for _, key := range []string{"completionPlan", "completionApproval", "completionProposal", "completionReconfirmation", "completionReconfirmationAfter", "completionComparison"} {
		next[key] = answer[key]
	}
	rows := append([]any{}, fleet.Elements(d["messages"])...)
	p, _ := completionplan.Parse(answer["completionPlan"])
	if answer["completionProposal"] != nil {
		rows = append(rows, presentation(p))
	} else {
		rows = append(rows, Record{"role": "assistant", "text": "説明"})
	}
	next["messages"] = rows
	return next
}

func TestSemanticChangesReconfirmAndRejectOldReceiptAtFiling(t *testing.T) {
	old := approvalFixture()
	rows := []any{presentation(old), explicitReply("first", "A")}
	receipt, _ := completionplan.Confirmed(old, rows)
	for _, change := range []func(*completionplan.Plan){
		func(p *completionplan.Plan) { p.Approvals[0].Target = "owner/other" },
		func(p *completionplan.Plan) { p.Approvals[0].Operation = "削除" },
		func(p *completionplan.Plan) { p.Approvals[0].Condition = "本番環境" },
		func(p *completionplan.Plan) { p.Approvals[0].Category = "destructive" },
		func(p *completionplan.Plan) { p.Approvals[0].Phase = "after_diff" },
		func(p *completionplan.Plan) { a := p.Approvals[0]; a.ID = "new"; p.Approvals = append(p.Approvals, a) },
	} {
		candidate, _ := completionplan.Parse(old.Record())
		change(&candidate)
		d := Record{"completionPlan": old.Record(), "messages": rows, "completionApproval": receipt}
		answer := candidateAnswer(candidate)
		prepareCompletionPlan(d, answer)
		if answer["ready"] != false || answer["completionReconfirmation"] != candidate.Fingerprint() || answer["completionApproval"] != nil || !strings.Contains(fleet.Text(fleet.Elements(answer["questions"])[0]), "変更箇所") {
			t.Fatal(answer)
		}
		pending := saveCompletionTurn(d, answer)
		pending["id"], pending["project"], pending["provider"], pending["title"], pending["body"] = draftID, "owner/app", "codex", "対象設定を変更", "固定した受入条件"
		pending["completionApproval"] = receipt // Attempt to replay the old receipt.
		if _, err := filing.Prepare(context.Background(), filing.BuildConfig{}, pending); err == nil {
			t.Fatal("old receipt filed a changed pending plan")
		}
	}
	// Dropping or relabelling the user-gated item is itself reconfirmed, and
	// the pending wait blocks filing of the now approval-free plan.
	for _, change := range []func(*completionplan.Plan){
		func(p *completionplan.Plan) { p.Approvals = []completionplan.Approval{} },
		func(p *completionplan.Plan) { p.Approvals[0].Category = "scope" },
	} {
		free, _ := completionplan.Parse(old.Record())
		change(&free)
		d := Record{"completionPlan": old.Record(), "messages": rows, "completionApproval": receipt}
		answer := candidateAnswer(free)
		prepareCompletionPlan(d, answer)
		if answer["ready"] != false || answer["completionReconfirmation"] != free.Fingerprint() {
			t.Fatal("candidate removed a user-gated item without the user", answer)
		}
		pending := saveCompletionTurn(d, answer)
		pending["id"], pending["project"], pending["provider"], pending["title"], pending["body"] = draftID, "owner/app", "codex", "対象設定を変更", "固定した受入条件"
		if _, err := filing.Prepare(context.Background(), filing.BuildConfig{}, pending); err == nil {
			t.Fatal("pending removal of a user-gated item was filed")
		}
		again := candidateAnswer(free)
		prepareCompletionPlan(pending, again)
		if again["ready"] != false || again["completionReconfirmation"] == nil {
			t.Fatal("repeating the approval-free candidate cleared the wait", again)
		}
	}
	// Adding an item that does not wait for the user keeps the approval.
	mixed, _ := completionplan.Parse(old.Record())
	mixed.Approvals = append(mixed.Approvals, completionplan.Approval{ID: "s", Category: "scope", Phase: "before_implementation", Target: "追加ファイル", Operation: "変更", Condition: "範囲拡大"})
	answer := candidateAnswer(mixed)
	prepareCompletionPlan(Record{"completionPlan": old.Record(), "messages": rows, "completionApproval": receipt}, answer)
	if answer["ready"] != true || answer["completionReconfirmation"] != nil {
		t.Fatal("scope item forced a reconfirmation", answer)
	}
}

func TestPendingChangeNeedsNewReplyEvenAfterRevertingOrRemovingApproval(t *testing.T) {
	old := approvalFixture()
	quote := "owner/appの設定変更を隔離環境で進めてください"
	old.Approvals[0].Evidence = &completionplan.Evidence{MessageID: "quoted", Quote: quote, At: "2026-11-07T10:00:00+09:00"}
	rows := []any{explicitReply("quoted", quote), presentation(old), explicitReply("old-A", "A")}
	d := Record{"completionPlan": old.Record(), "messages": rows}
	changed, _ := completionplan.Parse(old.Record())
	changed.Approvals[0].Condition = "本番環境"
	changed.Steps[0].Owner = "新しい担当"
	first := candidateAnswer(changed)
	prepareCompletionPlan(d, first)
	d = saveCompletionTurn(d, first)
	d["messages"] = append(fleet.Elements(d["messages"]), explicitReply("explain", "説明して"))
	second := candidateAnswer(old) // Restore both the old terms and old quotation.
	prepareCompletionPlan(d, second)
	if second["ready"] != false || second["completionReconfirmation"] != old.Fingerprint() {
		t.Fatal("rollback replayed the old A", second)
	}
	d = saveCompletionTurn(d, second)
	d["messages"] = append(fleet.Elements(d["messages"]), explicitReply("explain-again", "説明して"))
	third := candidateAnswer(old)
	prepareCompletionPlan(d, third)
	if third["ready"] != false || third["completionApproval"] != nil {
		t.Fatal("explanation cleared pending rollback", third)
	}
	d = saveCompletionTurn(d, third)
	d["messages"] = append(fleet.Elements(d["messages"]), explicitReply("new-A", "A"))
	cosmetic, _ := completionplan.Parse(old.Record())
	cosmetic.Steps[0].Evidence += "。"
	accepted := candidateAnswer(cosmetic)
	prepareCompletionPlan(d, accepted)
	if accepted["ready"] != true || accepted["completionReconfirmation"] != nil {
		t.Fatal("new answer followed by rewrite was lost", accepted)
	}
	if _, err := completionplan.Check(accepted["completionPlan"], d["messages"], accepted["completionApproval"]); err != nil {
		t.Fatal(err)
	}
	for _, reply := range []string{"取消します", "A、ただし本番には触れない", "B"} {
		revoked := saveCompletionTurn(d, accepted)
		revoked["messages"] = append(fleet.Elements(revoked["messages"]), explicitReply("revoked", reply))
		answer := candidateAnswer(cosmetic)
		prepareCompletionPlan(revoked, answer)
		if answer["completionApproval"] != nil || answer["ready"] != false {
			t.Fatal("revoked or conditional answer accepted", answer)
		}
	}
	// Removing the approval cannot clear an existing pending barrier.
	removed, _ := completionplan.Parse(changed.Record())
	removed.Approvals = []completionplan.Approval{}
	pending := saveCompletionTurn(Record{"messages": rows}, first)
	answer := candidateAnswer(removed)
	prepareCompletionPlan(pending, answer)
	if answer["ready"] != false || answer["completionReconfirmation"] == nil {
		t.Fatal(answer)
	}
}

func TestCandidateCannotInventMissingPresentationOrApproval(t *testing.T) {
	p := approvalFixture()
	for _, d := range []Record{
		{"messages": []any{Record{"role": "assistant", "completionProposal": p.Fingerprint()}, explicitReply("u", "A")}},
		{"messages": []any{explicitReply("u", "A")}},
		{"completionPlan": p.Record(), "messages": []any{Record{"role": "assistant", "completionProposal": p.Fingerprint(), "completionPlan": nil}, explicitReply("u", "A")}},
	} {
		answer := candidateAnswer(p)
		answer["completionApproval"] = Record{"fingerprint": p.Fingerprint(), "source": "screen-session", "quote": "A"}
		answer["messages"] = []any{presentation(p), explicitReply("fake", "A")}
		prepareCompletionPlan(d, answer)
		if answer["ready"] != false || answer["completionApproval"] != nil {
			t.Fatal("AI declaration became a presentation", answer)
		}
	}
	// Verifiable fingerprint-only legacy history remains usable.
	d := Record{"completionPlan": p.Record(), "messages": []any{Record{"role": "assistant", "completionProposal": p.Fingerprint()}, explicitReply("legacy", "A")}}
	answer := candidateAnswer(p)
	prepareCompletionPlan(d, answer)
	if answer["ready"] != true || answer["completionApproval"] == nil {
		t.Fatal("verifiable legacy presentation rejected", answer)
	}
}

func TestMissingPlanCannotErasePendingReconfirmation(t *testing.T) {
	d := Record{"completionReconfirmation": "pending", "completionReconfirmationAfter": 2}
	answer := Record{"ready": true}
	prepareCompletionPlan(d, answer)
	if answer["ready"] != false || answer["completionReconfirmation"] != "pending" || answer["completionReconfirmationAfter"] != 2 {
		t.Fatal(answer)
	}
	d["id"] = draftID
	if _, err := filing.Prepare(context.Background(), filing.BuildConfig{}, d); err == nil {
		t.Fatal("missing plan bypassed pending filing barrier through legacy path")
	}
}

func TestRevokedReceiptCannotBecomeReady(t *testing.T) {
	p := approvalFixture()
	rows := []any{presentation(p), explicitReply("accepted", "A")}
	receipt, _ := completionplan.Confirmed(p, rows)
	for _, reply := range []string{"取消します", "A、ただし条件を変更する"} {
		d := Record{"completionPlan": p.Record(), "completionApproval": receipt, "messages": append(append([]any{}, rows...), explicitReply("cancelled", reply))}
		answer := candidateAnswer(p)
		prepareCompletionPlan(d, answer)
		if answer["ready"] != false || answer["completionApproval"] != nil || answer["completionReconfirmation"] != p.Fingerprint() {
			t.Fatal("revocation silently removed the need for confirmation", answer)
		}
		d = saveCompletionTurn(d, answer)
		d["messages"] = append(fleet.Elements(d["messages"]), explicitReply("explain", "説明して"))
		answer = candidateAnswer(p)
		prepareCompletionPlan(d, answer)
		if answer["ready"] != false || answer["completionApproval"] != nil {
			t.Fatal("explanation resurrected revoked receipt", answer)
		}
	}
}

func TestCompletionExactReplyRetainsPresentedPlanAndResolvesOnlySavedQuestion(t *testing.T) {
	for _, reply := range []string{"A", "1-A", "承認します", "この計画を承認します"} {
		t.Run(reply, func(t *testing.T) {
			p := approvalFixture()
			first := candidateAnswer(p)
			prepareCompletionPlan(Record{}, first)
			q := fleet.Text(fleet.Elements(first["questions"])[0])
			d := saveCompletionTurn(Record{}, first)
			d["questions"], d["questionReasons"] = first["questions"], first["questionReasons"]
			d["messages"] = append(fleet.Elements(d["messages"]), explicitReply("answer", reply))
			candidate, _ := completionplan.Parse(p.Record())
			candidate.Basis = "候補の異なる判断根拠"
			for i := range candidate.Steps {
				candidate.Steps[i].Prerequisite = "別の候補前提"
				candidate.Steps[i].Evidence = "別の候補確認方法"
			}
			for _, other := range []bool{false, true} {
				answer := candidateAnswer(candidate)
				answer["ready"], answer["questions"] = false, []any{q}
				if other {
					answer["questions"] = append(fleet.Elements(answer["questions"]), "AIが追加した別の質問")
				}
				prepareCompletionPlan(d, answer)
				if !resolveConfirmedCompletionQuestions(d, answer) || answer["ready"] != !other || len(fleet.Elements(answer["questions"])) != btoi(other) || answer["completionReconfirmation"] != nil {
					t.Fatal(answer)
				}
				effective, err := completionplan.Check(answer["completionPlan"], d["messages"], answer["completionApproval"])
				if err != nil || effective.Fingerprint() != p.Fingerprint() {
					t.Fatal(effective, err)
				}
				comparison := answer["completionComparison"].(completionplan.Comparison)
				if !comparison.Equivalent || !comparison.PresentedReplyCompatible || len(comparison.Changes) != 9 || comparison.Candidate != candidate.Fingerprint() {
					t.Fatal(comparison)
				}
			}
			// Without controller provenance only the controller's own verification
			// of the reply retires a plan approval question. A different question
			// claiming the controller reason is not a plan question and remains.
			other := "AIが追加した別の質問"
			d["questionReasons"] = []any{Record{"question": q, "reason": completionQuestionReason, "controller": false}, Record{"question": other, "reason": completionQuestionReason, "controller": false}}
			answer := candidateAnswer(candidate)
			answer["ready"], answer["questions"] = false, []any{q, other}
			prepareCompletionPlan(d, answer)
			if !resolveConfirmedCompletionQuestions(d, answer) || answer["ready"] != false || len(fleet.Elements(answer["questions"])) != 1 || fleet.Text(fleet.Elements(answer["questions"])[0]) != other {
				t.Fatal(answer)
			}
			// A model approval question is never evidence without a verified reply.
			unanswered := saveCompletionTurn(Record{}, first)
			answer = candidateAnswer(p)
			answer["ready"], answer["questions"] = false, []any{q}
			prepareCompletionPlan(unanswered, answer)
			if resolveConfirmedCompletionQuestions(unanswered, answer) || answer["ready"] != false || answer["completionApproval"] != nil {
				t.Fatal(answer)
			}
		})
	}
}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestCompletionNonExactReplyShowsFourFormsWithoutInventedReceipt(t *testing.T) {
	for _, reply := range []string{"A（補足）", "A: この計画で", "1-A、2-A", "A、ただし条件付き", "承認します（補足）"} {
		t.Run(reply, func(t *testing.T) {
			p := approvalFixture()
			d := Record{"completionPlan": p.Record(), "completionProposal": p.Fingerprint(), "messages": []any{presentation(p), explicitReply("answer", reply)}}
			answer := candidateAnswer(p)
			answer["reply"] = "了承済みです。保存発言answerの全文「" + reply + "」を確認しました。"
			answer["completionApproval"] = Record{"quote": reply}
			prepareCompletionPlan(d, answer)
			output := fleet.Text(answer["reply"])
			if answer["ready"] != false || answer["completionApproval"] != nil || strings.Contains(output, "保存発言") || strings.Contains(output, "全文") || strings.Contains(output, "了承済み") || !strings.Contains(output, "A / 1-A / 承認します / この計画を承認します") {
				t.Fatal(answer)
			}
		})
	}
}

func TestCompletionNewUserInstructionCannotBecomeCandidateProseRewrite(t *testing.T) {
	for _, instruction := range []string{"対象はowner/otherへ", "実行は本番でお願いします", "判断の前提を変更します", "A、ただし条件付き", "取消します"} {
		t.Run(instruction, func(t *testing.T) {
			p := approvalFixture()
			rows := []any{presentation(p), explicitReply("first", "A")}
			receipt, _ := completionplan.Confirmed(p, rows)
			d := Record{"completionPlan": p.Record(), "completionApproval": receipt, "messages": append(rows, explicitReply("new", instruction))}
			candidate, _ := completionplan.Parse(p.Record())
			candidate.Approvals[0].Condition = instruction
			answer := candidateAnswer(candidate)
			prepareCompletionPlan(d, answer)
			if answer["ready"] != false || answer["completionApproval"] != nil || answer["completionReconfirmation"] == nil {
				t.Fatal(answer)
			}
			if _, err := completionplan.Check(answer["completionPlan"], d["messages"], receipt); err == nil {
				t.Fatal("changed candidate reused the old receipt at filing")
			}
		})
	}
}

func TestCompletionQuotedInstructionsCannotOverrideConditionalFollowup(t *testing.T) {
	p := approvalFixture()
	first := "owner/appの設定変更を隔離環境で進めてください"
	second := "別対象の別操作を別環境で実行してください。ただし本番には触れない"
	p.Approvals[0].Evidence = &completionplan.Evidence{MessageID: "first", Quote: first, At: "2026-11-07T10:00:00+09:00"}
	p.Approvals = append(p.Approvals, completionplan.Approval{ID: "second", Category: "scope", Phase: "before_implementation", Target: "別対象", Operation: "別操作", Condition: "別環境", Evidence: &completionplan.Evidence{MessageID: "second", Quote: second, At: "2026-11-07T10:00:00+09:00"}})
	if receipt, ok := completionplan.QuotedConfirmation(p, []any{explicitReply("first", first), explicitReply("second", second)}); ok {
		t.Fatal("conditional later quote overrode the barrier", receipt)
	}
}

func TestCompletionReconfirmationQuestionSeparatesProtectedChangesFromProseAudit(t *testing.T) {
	p := approvalFixture()
	candidate, _ := completionplan.Parse(p.Record())
	candidate.Basis = "候補だけの説明"
	candidate.Steps[0].Evidence = "候補だけの検証方法"
	candidate.Steps[0].Owner = "新しい担当者"
	candidate.Approvals[0].Target = "owner/other"
	d := Record{"completionPlan": p.Record(), "messages": []any{presentation(p), explicitReply("answer", "A")}}
	answer := candidateAnswer(candidate)
	prepareCompletionPlan(d, answer)
	question := fleet.Text(fleet.Elements(answer["questions"])[0])
	parts := strings.SplitN(question, "監査記録のみの自由文差（再確認の原因ではありません）: ", 2)
	if answer["completionApproval"] != nil || answer["completionReconfirmation"] == nil || len(parts) != 2 {
		t.Fatal(answer)
	}
	if !strings.Contains(parts[0], "再確認が必要な変更箇所: ") || !strings.Contains(parts[0], "approvals.p.target") || strings.Contains(parts[0], "basis") || strings.Contains(parts[0], ".evidence") {
		t.Fatal("reconfirmation cause mixed with prose audit", question)
	}
	if !strings.Contains(parts[1], "steps.implementation.owner") || !strings.Contains(parts[1], "basis") || !strings.Contains(parts[1], "steps.implementation.evidence") {
		t.Fatal("prose audit missing from question", question)
	}
}

const modelPlanQuestion = "【質問分類：判断の根拠の変化】上記の完了計画を了承しますか。了承する場合は A / 1-A / 承認します / この計画を承認します のいずれかだけで回答してください。"

func TestCompletionDoubleNumberedModelQuestionAtFiling(t *testing.T) {
	// The option descriptions were not supplied in the report and are synthetic.
	question := "2. 1. 更新計画を了承しますか？\n1-A: 計画を了承する（選択肢の説明は仮）\n1-B: 計画を修正する（選択肢の説明は仮）\n回答は A / 1-A / 承認します / この計画を承認します のいずれかだけにしてください。"
	p := approvalFixture()
	d := Record{"completionPlan": p.Record(), "messages": []any{presentation(p), explicitReply("answer", "1-A")}}
	for _, extra := range []string{"", "\n2. 削除も行いますか？ 2-A: 行う / 2-B: 行わない", "\n対象環境はどこですか？"} {
		answer := candidateAnswer(p)
		answer["ready"], answer["questions"] = false, []any{question + extra}
		answer["questionReasons"] = []any{Record{"question": question + extra, "reason": "確認"}}
		prepareCompletionPlan(d, answer)
		removed := resolveConfirmedCompletionQuestions(d, answer)
		if removed != (extra == "") || (answer["ready"] == true) != (extra == "") || len(fleet.Elements(answer["questions"])) != btoi(extra != "") {
			t.Fatal(answer)
		}
		if extra != "" {
			if fleet.Text(fleet.Elements(answer["questions"])[0]) != question+extra {
				t.Fatal("別の未確認事項が消えました", answer)
			}
			continue
		}
		if len(fleet.Elements(answer["questionReasons"])) != 0 {
			t.Fatal(answer)
		}
		for _, key := range []string{"id", "project", "provider", "title", "body"} {
			answer[key] = map[string]string{"id": draftID, "project": "owner/app", "provider": "codex", "title": "設定を変更", "body": "固定した受入条件で検証する"}[key]
		}
		answer["messages"] = d["messages"]
		answer["changeScope"] = Record{"known": true, "paths": []any{"server/example.go"}}
		if _, err := filing.Prepare(context.Background(), filing.BuildConfig{Config: map[string]any{"model": "gpt-6.1"}, Project: func(string) error { return nil }}, answer); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompletionModelApprovalQuestionRetiredAfterVerifiedReply(t *testing.T) {
	for _, other := range []bool{false, true} {
		p := approvalFixture()
		first := candidateAnswer(p)
		prepareCompletionPlan(Record{}, first)
		d := saveCompletionTurn(Record{}, first)
		d["questions"], d["questionReasons"] = first["questions"], first["questionReasons"]
		d["messages"] = append(fleet.Elements(d["messages"]), explicitReply("answer", "A"))
		answer := candidateAnswer(p)
		answer["ready"], answer["questions"] = false, []any{modelPlanQuestion}
		answer["questionReasons"] = []any{Record{"question": modelPlanQuestion, "reason": "計画の了承が必要です"}}
		if other {
			answer["questions"] = append(fleet.Elements(answer["questions"]), "実装資料はどこですか")
		}
		prepareCompletionPlan(d, answer)
		if !resolveConfirmedCompletionQuestions(d, answer) || answer["ready"] != !other || len(fleet.Elements(answer["questions"])) != btoi(other) || len(fleet.Elements(answer["questionReasons"])) != 0 || answer["completionReconfirmation"] != nil || answer["completionApproval"] == nil {
			t.Fatal(answer)
		}
		if other && fleet.Text(fleet.Elements(answer["questions"])[0]) != "実装資料はどこですか" {
			t.Fatal("unrelated question removed", answer)
		}
		if _, err := completionplan.Check(answer["completionPlan"], d["messages"], answer["completionApproval"]); err != nil {
			t.Fatal(err)
		}
		// A later turn after approval also retires the model's repeated question.
		next := saveCompletionTurn(d, answer)
		next["messages"] = append(fleet.Elements(next["messages"]), explicitReply("later", "1-A"))
		again := candidateAnswer(p)
		again["ready"], again["questions"] = false, []any{modelPlanQuestion}
		prepareCompletionPlan(next, again)
		if !resolveConfirmedCompletionQuestions(next, again) || again["ready"] != true || again["completionReconfirmation"] != nil || again["completionApproval"].(completionplan.Receipt).MessageID != "answer" {
			t.Fatal(again)
		}
	}
}

func TestCompletionUnconfirmedPlanHasOnlyControllerQuestion(t *testing.T) {
	mixed := "1. 計画を了承しますか。A / 1-A / 承認します / この計画を承認します\n2. 削除も行いますか。2-A: 行う / 2-B: 行わない"
	for _, d := range []Record{
		{},
		{"completionPlan": approvalFixture().Record(), "messages": []any{presentation(approvalFixture()), explicitReply("answer", "A（補足）")}},
		{"completionPlan": approvalFixture().Record(), "messages": []any{Record{"role": "assistant", "text": "AIだけの了承質問"}, explicitReply("answer", "A")}},
	} {
		p := approvalFixture()
		answer := candidateAnswer(p)
		answer["ready"], answer["questions"] = false, []any{modelPlanQuestion, "実装資料はどこですか", mixed}
		answer["questionReasons"] = []any{Record{"question": modelPlanQuestion, "reason": "了承済みです"}}
		answer["reply"] = "了承を確認しました"
		prepareCompletionPlan(d, answer)
		if resolveConfirmedCompletionQuestions(d, answer) || answer["ready"] != false || answer["completionApproval"] != nil {
			t.Fatal("model question or self-claim became approval evidence", answer)
		}
		plan := 0
		for _, raw := range fleet.Elements(answer["questions"]) {
			if q := fleet.Text(raw); q == modelPlanQuestion {
				t.Fatal("model plan question remained", answer)
			} else if strings.Contains(q, "起票前に上記の対象・操作・条件を確認してください") {
				plan++
			}
		}
		questions := fleet.Elements(answer["questions"])
		if plan != 1 || len(questions) != 3 || fleet.Text(questions[0]) != "実装資料はどこですか" || fleet.Text(questions[1]) != mixed {
			t.Fatal("plan confirmation must be one controller question beside unrelated ones", answer)
		}
		for _, raw := range fleet.Elements(answer["questionReasons"]) {
			if fleet.Text(fleet.Object(raw)["question"]) == modelPlanQuestion {
				t.Fatal("reason of removed question remained", answer)
			}
		}
	}
}

func TestCompletionInstructionsForbidModelApprovalQuestions(t *testing.T) {
	if !strings.Contains(instructions, "計画の了承を求める質問はコントローラーが出します。questionsに計画の了承・承認を求める質問") || !strings.Contains(instructions, "了承後に同じ計画の了承を再び求めません") {
		t.Fatal("fixed instructions must forbid model-written plan approval questions")
	}
}
