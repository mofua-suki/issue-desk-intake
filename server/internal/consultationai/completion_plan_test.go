package consultationai

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/references"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTurnConfirmedPlanKeepsExplanationAndHoldNotices(t *testing.T) {
	p := completionFixture()
	p.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "設定", Operation: "変更", Condition: "確認"}}
	first := Record{"ready": true, "completionPlan": p.Record(), "questions": []any{}}
	prepareCompletionPlan(Record{}, first)
	q := fleet.Text(fleet.Elements(first["questions"])[0])
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		v := reply()
		v["reply"] = "接続方法は既存の設定を使います。確定申告の設定を確認します。条件は確定したわけではありません。起票案を確定しました。"
		v["questions"] = []any{q, "接続先はどこですか"}
		v["completionPlan"] = p.Record()
		v["newProject"] = Record{"needed": true}
		return measuredAnswer(v), nil
	})
	s.options.Reference = nil
	d["completionPlan"], d["completionProposal"] = p.Record(), p.Fingerprint()
	d["questions"], d["questionReasons"] = first["questions"], first["questionReasons"]
	d["messages"] = []any{Record{"role": "assistant", "completionProposal": p.Fingerprint(), "completionPlan": p.Record()}, Record{"id": "u", "role": "user", "text": "A", "at": "2026-11-07T10:00:00+09:00", "source": "screen-session", "references": []any{Record{"desk": float64(12), "historyUnread": true}}}}
	_, err := s.options.Records.Change(context.Background(), "drafts", draftID, func(*sql.Tx, Record) (Record, error) { return copyRecord(d), nil })
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	message := fleet.Text(out["reply"])
	for _, want := range []string{"接続方法は既存の設定を使います", "確定申告の設定を確認します。", "条件は確定したわけではありません。", "接続先はどこですか", "新しいリポジトリが必要な場合は、チャットのAIに相談してください。", references.HistoryUnreadNotice} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q: %s", want, message)
		}
	}
	if out["ready"] != false || out["completionApproval"] == nil || strings.Contains(message, "起票案を確定しました") || strings.Contains(message, q) {
		t.Fatal(out)
	}
}

func completionFixture() completionplan.Plan {
	p := completionplan.Plan{Version: 1, Basis: "owner/appの固定した受入条件", Approvals: []completionplan.Approval{}}
	for _, phase := range []string{"implementation", "verification", "merge", "deployment"} {
		p.Steps = append(p.Steps, completionplan.Step{Phase: phase, Owner: "コントローラー", Prerequisite: "対象の前提条件を確認済み", Evidence: "隔離検証の結果と版を照合"})
	}
	return p
}

func TestCompletionInvalidSavedPlanRequiresReconfirmation(t *testing.T) {
	candidate := completionFixture()
	for _, saved := range []any{Record{"version": 1}, "invalid"} {
		answer := Record{"ready": true, "completionPlan": candidate.Record(), "questions": []any{}}
		prepareCompletionPlan(Record{"completionPlan": saved}, answer)
		if answer["ready"] != false || answer["completionReconfirmation"] != candidate.Fingerprint() || len(fleet.Elements(answer["questions"])) != 1 || answer["completionApproval"] != nil {
			t.Fatalf("invalid saved plan bypassed reconfirmation: %#v", answer)
		}
	}
	rows := []any{Record{"role": "assistant", "completionProposal": candidate.Fingerprint(), "completionPlan": candidate.Record()}, Record{"id": "u", "role": "user", "text": "A", "at": "2026-11-07T10:00:00Z", "source": "cli-key"}}
	answer := Record{"ready": true, "completionPlan": candidate.Record(), "questions": []any{}}
	prepareCompletionPlan(Record{"completionPlan": "invalid", "messages": rows}, answer)
	if answer["ready"] != true || answer["completionApproval"] == nil || len(fleet.Elements(answer["questions"])) != 0 {
		t.Fatalf("valid saved presentation and reply were not preserved: %#v", answer)
	}
}

func TestCompletionApprovalSplitChildren(t *testing.T) {
	p := completionFixture()
	p.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "owner/app", Operation: "設定変更", Condition: "隔離環境"}}
	presentation := Record{"role": "assistant", "completionProposal": p.Fingerprint(), "completionPlan": p.Record()}
	reply := Record{"role": "user", "id": "answer", "text": "A", "at": "2026-11-07T10:00:00Z", "source": "cli-key"}
	rows := []any{presentation, reply}
	receipt, ok := completionplan.Confirmed(p, rows)
	if !ok {
		t.Fatal("confirmation fixture")
	}
	for _, tc := range []struct {
		name          string
		messages      []any
		pending       bool
		wantQuestions int
	}{
		{"confirmed", rows, false, 0},
		{"pending", rows, true, 1},
		{"missing source", []any{presentation}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testService(t, nil)
			parent := Record{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/app", "owner": "owner", "_ephemeral": true, "messages": tc.messages, "completionPlanVersion": 1, "completionPlan": p.Record(), "completionApproval": receipt}
			if tc.pending {
				parent["completionReconfirmation"], parent["completionReconfirmationAfter"] = p.Fingerprint(), len(rows)
			}
			result, err := s.SplitChildren(context.Background(), parent, []string{"子A", "子B"})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range fleet.Elements(result["consultationChildren"]) {
				child, found, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(id))
				if err != nil || !found {
					t.Fatal(found, err)
				}
				answer := Record{"ready": true, "completionPlan": p.Record(), "questions": []any{}}
				prepareCompletionPlan(child, answer)
				if len(fleet.Elements(answer["questions"])) != tc.wantQuestions {
					t.Fatalf("questions: %v", answer)
				}
				if tc.pending && int(fleet.Number(child["completionReconfirmationAfter"])) != len(rows)+1 {
					t.Fatal("boundary lost", child)
				}
				child["title"], child["body"] = "修正", "## 目的\n修正する"
				child["changeScope"] = Record{"known": true, "paths": []any{"a.go"}}
				_, err = filing.Prepare(context.Background(), filing.BuildConfig{Config: map[string]any{"model": "test-model"}}, filing.Record(child))
				if (err == nil) != (tc.wantQuestions == 0) {
					t.Fatalf("filing validation: %v", err)
				}
			}
		})
	}
}
func TestCompletionPreflightPreservesLegacyAndRejectsMissingNewPlan(t *testing.T) {
	answer := Record{"ready": true}
	if got := completionPlanProblem(Record{}, answer); got != "" {
		t.Fatal(got)
	}
	if got := completionPlanProblem(Record{"completionPlanVersion": 1}, answer); got == "" {
		t.Fatal("new draft without completion route was accepted")
	}
}
func TestCompletionPreflightPresentsRequiredAndFinalApprovals(t *testing.T) {
	p := completionFixture()
	p.Approvals = []completionplan.Approval{{Category: "irreversible", ID: "operation", Phase: "before_implementation", Target: "保存データの移行", Operation: "復旧方式の変更", Condition: "利用者が保持期限を判断"}, {Category: "destructive", ID: "sql", Phase: "after_diff", Target: "削除SQL", Operation: "削除の承認", Condition: "SQLとheadを照合"}}
	answer := Record{"ready": true, "reply": "整理しました", "completionPlan": p.Record(), "questions": []any{}}
	prepareCompletionPlan(Record{}, answer)
	if answer["ready"] != false || answer["completionProposal"] != p.Fingerprint() || len(fleet.Elements(answer["questions"])) != 1 {
		t.Fatal(answer)
	}
	if !strings.Contains(fleet.Text(fleet.Elements(answer["questions"])[0]), "取り返しのつかない操作") || strings.Contains(fleet.Text(fleet.Elements(answer["questions"])[0]), "新しい権限") {
		t.Fatal(answer)
	}
	if !strings.Contains(fleet.Text(answer["reply"]), "対象SHA") {
		t.Fatal("remaining final approval was hidden", answer)
	}
	rows := []any{Record{"role": "assistant", "completionProposal": p.Fingerprint(), "completionPlan": p.Record()}, Record{"id": "u", "role": "user", "text": "A", "at": "2026-11-07T10:00:00+09:00", "source": "screen-session"}}
	accepted := Record{"ready": true, "completionPlan": p.Record(), "questions": []any{}}
	prepareCompletionPlan(Record{"messages": rows}, accepted)
	if accepted["ready"] != true || accepted["completionApproval"] == nil || len(fleet.Elements(accepted["questions"])) > 0 {
		t.Fatal(accepted)
	}
}

func TestTurnRepairsCompletionPlanBeforeReady(t *testing.T) {
	calls := 0
	p := completionFixture()
	s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
		calls++
		if calls == 2 && (req.CallTrigger != "format_repair" || req.RepairKind != "completion_plan") {
			t.Errorf("repair usage: trigger=%q kind=%q", req.CallTrigger, req.RepairKind)
		}
		value := reply()
		value["ready"], value["title"], value["body"], value["questions"] = true, "計画の題名", "実装の受入条件", []any{}
		if calls > 1 {
			value["completionPlan"] = p.Record()
		}
		return measuredAnswer(value), nil
	})
	s.options.Reference = nil
	d["completionPlanVersion"] = 1
	out, err := s.Turn(context.Background(), d)
	if err != nil || calls != 2 || out["ready"] != true || out["completionPlan"] == nil || !strings.Contains(fleet.Text(out["reply"]), "## 完了までの計画と承認") {
		t.Fatal(calls, out, err)
	}
}

func TestChangedCompletionBasisCannotReuseOriginalQuotation(t *testing.T) {
	p := completionFixture()
	quote := "owner/appの設定変更を隔離環境で進めてください"
	p.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "owner/app", Operation: "設定変更", Condition: "隔離環境", Evidence: &completionplan.Evidence{MessageID: "u", Quote: quote, At: "2026-11-07T10:00:00+09:00"}}}
	rows := []any{Record{"role": "user", "id": "u", "text": quote, "at": "2026-11-07T10:00:00+09:00", "source": "screen-session"}}
	old := p.Record()
	p.Approvals[0].Condition = "本番環境で実行する"
	answer := Record{"ready": true, "completionPlan": p.Record(), "questions": []any{}}
	prepareCompletionPlan(Record{"completionPlan": old, "messages": rows}, answer)
	if answer["ready"] != false || answer["completionApproval"] != nil || answer["completionProposal"] != p.Fingerprint() {
		t.Fatal("changed basis reinterpreted earlier quotation", answer)
	}
}

func TestChangedCompletionBasisStaysPendingAcrossExplanatoryTurnsPostgres(t *testing.T) {
	ctx := context.Background()
	p := completionFixture()
	quote := "owner/appの設定変更を隔離環境で進めてください"
	p.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "owner/app", Operation: "設定変更", Condition: "隔離環境", Evidence: &completionplan.Evidence{MessageID: "original", Quote: quote, At: "2026-11-07T10:00:00+09:00"}}}
	previous := p.Record()
	p.Approvals[0].Condition = "本番環境で実行する"
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		value := reply()
		value["ready"], value["title"], value["body"], value["questions"], value["completionPlan"] = true, "接続を変更する", "固定した受入条件", []any{}, p.Record()
		return measuredAnswer(value), nil
	})
	s.options.Reference = nil
	d["completionPlan"], d["completionPlanVersion"] = previous, 1
	d["messages"] = []any{Record{"role": "user", "id": "original", "text": quote, "at": "2026-11-07T10:00:00+09:00", "source": "screen-session"}}
	saveTurn := func(result Record, followup string) Record {
		t.Helper()
		saved, err := s.options.Records.Change(ctx, "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, current Record) (Record, error) {
			for _, key := range []string{"completionPlan", "completionApproval", "completionProposal", "completionReconfirmation", "completionReconfirmationAfter", "completionComparison", "context"} {
				current[key] = result[key]
			}
			rows := append([]any{}, fleet.Elements(d["messages"])...)
			rows = append(rows, Record{"role": "assistant", "text": result["reply"], "completionProposal": result["completionProposal"], "completionPlan": result["completionPlan"]})
			rows = append(rows, Record{"role": "user", "id": "followup-" + followup + "-" + string(marshal(len(rows))), "text": followup, "at": "2026-11-07T10:01:00+09:00", "source": "screen-session"})
			current["messages"] = rows
			return current, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	first, err := s.Turn(ctx, d)
	if err != nil || first["ready"] != false || first["completionReconfirmation"] != p.Fingerprint() {
		t.Fatal(first, err)
	}
	d = saveTurn(first, "説明して")
	second, err := s.Turn(ctx, d)
	if err != nil || second["ready"] != false || second["completionApproval"] != nil || second["completionReconfirmation"] != p.Fingerprint() {
		t.Fatal("old evidence restored approval on an explanatory turn", second, err)
	}
	d = saveTurn(second, "A")
	last := fleet.Object(fleet.Elements(d["messages"])[len(fleet.Elements(d["messages"]))-1])
	last["source"] = "pc-token"
	third, err := s.Turn(ctx, d)
	if err != nil || third["ready"] != true || third["completionApproval"] == nil || third["completionReconfirmation"] != nil {
		t.Fatal("explicit answer did not release the pending plan", third, err)
	}
	encoded, _ := json.Marshal(third["completionApproval"])
	var approved map[string]any
	_ = json.Unmarshal(encoded, &approved)
	if approved["source"] != "pc-token" {
		t.Fatal("old quote replaced new explicit answer", approved)
	}
	if _, err := completionplan.Check(third["completionPlan"], d["messages"], third["completionApproval"]); err != nil {
		t.Fatal("new explicit receipt did not pass filing", err)
	}
	d = saveTurn(third, "説明して")
	fourth, err := s.Turn(ctx, d)
	if err != nil || fourth["ready"] != true || fourth["completionReconfirmation"] != nil {
		t.Fatal("valid answer was forgotten by an explanatory turn", fourth, err)
	}
	encoded, _ = json.Marshal(fourth["completionApproval"])
	_ = json.Unmarshal(encoded, &approved)
	if approved["source"] != "pc-token" {
		t.Fatal("later explanation restored older human quote", approved)
	}

	if _, err := completionplan.Check(third["completionPlan"], d["messages"], third["completionApproval"]); err != nil {
		t.Fatal("new explicit receipt was replaced with the old quotation at filing", err)
	}
}

func TestBasisChangeWithoutUserGatedItemProceeds(t *testing.T) {
	old := completionFixture()
	old.Approvals = []completionplan.Approval{{ID: "s", Category: "scope", Phase: "before_implementation", Target: "対象設定", Operation: "変更", Condition: "指定経路で確認"}}
	p := completionFixture()
	p.Basis = "判断の前提を変更"
	answer := Record{"ready": true, "completionPlan": p.Record(), "questions": []any{}}
	prepareCompletionPlan(Record{"completionPlan": old.Record()}, answer)
	if answer["ready"] != true || answer["completionReconfirmation"] != nil || len(fleet.Elements(answer["questions"])) != 0 {
		t.Fatal("basis change without a user-gated item waited for the user", answer)
	}
}

func TestExistingGateCorrectedPlanSavedDisplayedAndFiled(t *testing.T) {
	ctx := context.Background()
	old := completionFixture()
	gate := "deletion"
	old.Approvals = []completionplan.Approval{{ID: "delete", Category: "destructive", Phase: "after_diff", Gate: &gate, Target: "削除対象", Operation: "削除", Condition: "差分を確認"}}
	candidate, err := completionplan.Parse(old.Record())
	if err != nil {
		t.Fatal(err)
	}
	candidate.Steps[2].Owner = "人の担当"
	candidate.Steps[2].Prerequisite = "検証結果を確認"
	candidate.Steps[2].Evidence = "マージ結果を確認"
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		v := reply()
		v["ready"], v["title"], v["body"], v["questions"], v["completionPlan"] = true, "担当の訂正", "## 目的\n担当を訂正する", []any{}, candidate.Record()
		return measuredAnswer(v), nil
	})
	s.options.Reference = nil
	d["completionPlan"], d["completionPlanVersion"] = old.Record(), 1
	out, err := s.Turn(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) {
		current["completionPlan"] = out["completionPlan"]
		current["completionPlanVersion"] = 1
		current["title"], current["body"] = "担当の訂正", "## 目的\n担当を訂正する"
		current["changeScope"] = Record{"known": true, "paths": []any{"a.go"}}
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	p, err := completionplan.Parse(saved["completionPlan"])
	if err != nil || !reflect.DeepEqual(p.Record(), candidate.Record()) || !strings.Contains(fleet.Text(out["reply"]), p.Section()) || out["completionApproval"] != nil || out["completionReconfirmation"] != nil {
		t.Fatal(p, out, err)
	}
	section, _, err := filing.SplitIssueSections(filing.Record(saved))
	if err != nil || section != p.Section() {
		t.Fatal(section, err)
	}
	prepared, err := filing.Prepare(ctx, filing.BuildConfig{Config: map[string]any{"model": "test-model"}, Project: func(string) error { return nil }, Now: s.options.Now}, filing.Record(saved))
	if err != nil || !strings.Contains(prepared.Body, section) {
		t.Fatal(prepared, err)
	}
}

func TestCorrectedExistingGateDoesNotReplaceEvidenceOrPendingPlan(t *testing.T) {
	gate := "deletion"
	old := completionFixture()
	old.Approvals = []completionplan.Approval{{ID: "delete", Category: "destructive", Phase: "after_diff", Gate: &gate, Target: "対象", Operation: "削除", Condition: "差分確認"}}
	for _, kind := range []string{"evidence", "pending", "receipt"} {
		t.Run(kind, func(t *testing.T) {
			p, _ := completionplan.Parse(old.Record())
			if kind == "evidence" {
				p.Approvals[0].Evidence = &completionplan.Evidence{MessageID: "u", Quote: "確認", At: "2026-11-07T10:00:00Z"}
			}
			candidate, _ := completionplan.Parse(p.Record())
			candidate.Steps[2].Owner = "人の担当"
			d := Record{"completionPlan": p.Record()}
			if kind == "pending" {
				d["completionReconfirmation"] = p.Fingerprint()
			}
			if kind == "receipt" {
				d["completionApproval"] = Record{"fingerprint": p.Fingerprint()}
			}
			answer := Record{"ready": true, "completionPlan": candidate.Record(), "questions": []any{}}
			prepareCompletionPlan(d, answer)
			got, err := completionplan.Parse(answer["completionPlan"])
			if err != nil || got.Steps[2].Owner != p.Steps[2].Owner || (kind == "evidence" && !reflect.DeepEqual(got.Approvals[0].Evidence, p.Approvals[0].Evidence)) || (kind != "evidence" && answer["completionReconfirmation"] == nil) {
				t.Fatal(answer, err)
			}
		})
	}
}

func TestConfirmedCompletionQuestionPreservesOtherQuestions(t *testing.T) {
	p := completionFixture()
	p.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "設定", Operation: "変更", Condition: "確認"}}
	first := Record{"ready": true, "completionPlan": p.Record(), "questions": []any{}}
	prepareCompletionPlan(Record{}, first)
	q := fleet.Text(fleet.Elements(first["questions"])[0])
	d := Record{"completionPlan": p.Record(), "completionProposal": p.Fingerprint(), "questions": first["questions"], "questionReasons": first["questionReasons"], "messages": []any{Record{"role": "assistant", "completionProposal": p.Fingerprint(), "completionPlan": p.Record()}, Record{"id": "u", "role": "user", "text": "1-A", "at": "2026-11-07T10:00:00+09:00", "source": "screen-session"}}}
	for _, ready := range []bool{false, true} {
		answer := Record{"ready": ready, "completionPlan": p.Record(), "questions": []any{q, "別の未確認事項"}}
		prepareCompletionPlan(d, answer)
		if !resolveConfirmedCompletionQuestions(d, answer) || len(fleet.Elements(answer["questions"])) != 1 || fleet.Text(fleet.Elements(answer["questions"])[0]) != "別の未確認事項" {
			t.Fatal(answer)
		}
		answer["ready"] = false
		reconcileCompletionReply(answer)
		if strings.Contains(fleet.Text(answer["reply"]), "確定") {
			t.Fatal(answer)
		}
	}
	changed, _ := completionplan.Parse(p.Record())
	changed.Basis += "変更"
	answer := Record{"ready": false, "completionPlan": changed.Record(), "questions": []any{q}}
	prepareCompletionPlan(d, answer)
	if !resolveConfirmedCompletionQuestions(d, answer) || answer["completionReconfirmation"] != nil || answer["ready"] != true {
		t.Fatal(answer)
	}
}

func TestCompletionApprovalQuotedEvidence(t *testing.T) {
	quote := "owner/repoの設定変更を隔離環境で進めてください"
	for _, tc := range []struct {
		name   string
		mutate func(*completionplan.Plan, []any) []any
		want   int
	}{
		{"一致", func(_ *completionplan.Plan, r []any) []any { return r }, 0},
		{"出典不一致", func(_ *completionplan.Plan, r []any) []any { r[0].(Record)["text"] = "別の発言"; return r }, 1},
		{"日時不一致", func(_ *completionplan.Plan, r []any) []any { r[0].(Record)["at"] = "2026-11-07T10:01:00Z"; return r }, 1},
		{"対象欠落", func(p *completionplan.Plan, r []any) []any { p.Approvals[0].Target = "別の対象"; return r }, 1},
		{"操作欠落", func(p *completionplan.Plan, r []any) []any { p.Approvals[0].Operation = "削除"; return r }, 1},
		{"条件欠落", func(p *completionplan.Plan, r []any) []any { p.Approvals[0].Condition = "本番環境"; return r }, 1},
		{"出典種別不正", func(_ *completionplan.Plan, r []any) []any { r[0].(Record)["source"] = "代理"; return r }, 1},
		{"取消", func(_ *completionplan.Plan, r []any) []any {
			return append(r, Record{"role": "user", "text": "取り消します"})
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := completionFixture()
			p.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "owner/repo", Operation: "設定変更", Condition: "隔離環境", Evidence: &completionplan.Evidence{MessageID: "u", Quote: quote, At: "2026-11-07T10:00:00Z"}}}
			rows := tc.mutate(&p, []any{Record{"id": "u", "role": "user", "text": quote, "at": "2026-11-07T10:00:00Z", "source": "pc-token"}})
			answer := Record{"ready": true, "completionPlan": p.Record(), "questions": []any{}}
			prepareCompletionPlan(Record{"messages": rows}, answer)
			if len(fleet.Elements(answer["questions"])) != tc.want || (answer["completionApproval"] != nil) != (tc.want == 0) {
				t.Fatal(answer)
			}
			if tc.want == 0 {
				if _, err := completionplan.Check(answer["completionPlan"], rows, answer["completionApproval"]); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCompletionApprovalFollowupAndTarget(t *testing.T) {
	p := completionFixture()
	p.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "owner/repo", Operation: "設定変更", Condition: "隔離環境"}}
	rows := []any{Record{"role": "assistant", "completionProposal": p.Fingerprint(), "completionPlan": p.Record()}, Record{"role": "user", "id": "u", "text": "A", "at": "2026-11-07T10:00:00Z", "source": "cli-key"}, Record{"role": "user", "id": "v", "text": "重複はありません", "at": "2026-11-07T10:01:00Z", "source": "cli-key"}}
	for _, tc := range []struct {
		name   string
		change func(*completionplan.Plan)
		want   int
	}{
		{"無関係な返信", func(*completionplan.Plan) {}, 0},
		{"対象の外側の空白", func(p *completionplan.Plan) { p.Approvals[0].Target = " owner/repo " }, 0},
		{"対象の変更", func(p *completionplan.Plan) { p.Approvals[0].Target = "owner/other" }, 1},
		{"対象の判定不能", func(p *completionplan.Plan) { p.Approvals[0].Target = "owner/repoの設定" }, 1},
		{"操作の拡大", func(p *completionplan.Plan) { p.Approvals[0].Operation = "設定変更と削除" }, 1},
		{"条件の拡大", func(p *completionplan.Plan) { p.Approvals[0].Condition = "全環境" }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, _ := completionplan.Parse(p.Record())
			tc.change(&candidate)
			answer := Record{"ready": true, "completionPlan": candidate.Record(), "questions": []any{}}
			prepareCompletionPlan(Record{"completionPlan": p.Record(), "messages": rows}, answer)
			if len(fleet.Elements(answer["questions"])) != tc.want {
				t.Fatal(answer)
			}
			if tc.want == 0 {
				if _, err := completionplan.Check(answer["completionPlan"], rows, answer["completionApproval"]); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCompletionReconfirmInstructionDeviation(t *testing.T) {
	old := completionFixture()
	old.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "owner/app", Operation: "設定変更", Condition: "隔離環境"}}
	rows := []any{Record{"role": "assistant", "completionProposal": old.Fingerprint(), "completionPlan": old.Record()}, Record{"id": "u", "role": "user", "text": "A", "at": "2026-11-07T10:00:00Z", "source": "cli-key"}}
	for _, tc := range []struct {
		name      string
		change    func(*completionplan.Plan)
		questions int
	}{
		{"basis and steps", func(p *completionplan.Plan) {
			p.Basis = "新しい前提"
			p.Steps[0].Owner = "別担当"
			p.Steps[0].Prerequisite = "新しい準備"
			p.Steps[0].Evidence = "新しい確認"
		}, 0},
		{"split", func(p *completionplan.Plan) { p.Approvals[0].Target += "（案2）" }, 0},
		{"condition", func(p *completionplan.Plan) { p.Approvals[0].Condition = "本番環境" }, 1},
		{"operation", func(p *completionplan.Plan) { p.Approvals[0].Operation = "削除" }, 1},
		{"target", func(p *completionplan.Plan) { p.Approvals[0].Target = "owner/other" }, 1},
		{"unknown", func(p *completionplan.Plan) { p.Approvals[0].Target += "（案2と本番）" }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, _ := completionplan.Parse(old.Record())
			tc.change(&candidate)
			answer := Record{"ready": true, "completionPlan": candidate.Record(), "questions": []any{}}
			prepareCompletionPlan(Record{"completionPlan": old.Record(), "messages": rows}, answer)
			if len(fleet.Elements(answer["questions"])) != tc.questions {
				t.Fatalf("unexpected questions: %#v", answer)
			}
			if tc.questions == 0 {
				p, err := completionplan.Parse(answer["completionPlan"])
				if err != nil || p.Fingerprint() != old.Fingerprint() || answer["completionApproval"] == nil {
					t.Fatalf("reply was retargeted: %#v", answer)
				}
			}
		})
	}
	empty := completionFixture()
	rewritten, _ := completionplan.Parse(empty.Record())
	rewritten.Basis = "新しい前提"
	rewritten.Steps[0].Evidence = "別の確認"
	answer := Record{"ready": true, "completionPlan": rewritten.Record(), "questions": []any{}}
	prepareCompletionPlan(Record{"completionPlan": empty.Record()}, answer)
	if len(fleet.Elements(answer["questions"])) != 0 {
		t.Fatal(answer)
	}
}

func TestSplitChildrenCompareRewrittenApproval(t *testing.T) {
	p := completionFixture()
	p.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "owner/app", Operation: "設定変更", Condition: "隔離環境"}}
	rows := []any{Record{"role": "assistant", "completionProposal": p.Fingerprint(), "completionPlan": p.Record()}, Record{"role": "user", "id": "answer", "text": "A", "at": "2026-11-07T10:00:00Z", "source": "cli-key"}}
	receipt, ok := completionplan.Confirmed(p, rows)
	if !ok {
		t.Fatal("confirmation failed")
	}
	s, _ := testService(t, nil)
	parent := Record{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/app", "owner": "owner", "_ephemeral": true, "messages": rows, "completionPlanVersion": 1, "completionPlan": p.Record(), "completionApproval": receipt}
	result, err := s.SplitChildren(context.Background(), parent, []string{"子A", "子B"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range fleet.Elements(result["consultationChildren"]) {
		child, found, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(id))
		if err != nil || !found {
			t.Fatal(found, err)
		}
		for _, tc := range []struct {
			name   string
			target string
			ready  bool
		}{
			{"split annotation", "owner/app（案2）", true},
			{"changed target", "owner/other", false},
		} {
			t.Run(fleet.Text(id)+"/"+tc.name, func(t *testing.T) {
				candidate, err := completionplan.Parse(p.Record())
				if err != nil {
					t.Fatal(err)
				}
				candidate.Basis = "書き直した判断根拠"
				candidate.Approvals[0].Target = tc.target
				answer := Record{"ready": true, "completionPlan": candidate.Record(), "questions": []any{}}
				prepareCompletionPlan(child, answer)
				if answer["ready"] != tc.ready || (answer["completionApproval"] != nil) != tc.ready || (len(fleet.Elements(answer["questions"])) == 0) != tc.ready {
					t.Fatalf("unexpected child decision: %#v", answer)
				}
				if tc.ready {
					approved, err := completionplan.Check(answer["completionPlan"], child["messages"], answer["completionApproval"])
					if err != nil || approved.Fingerprint() != p.Fingerprint() {
						t.Fatal(approved, err)
					}
				} else if answer["completionReconfirmation"] == nil {
					t.Fatal("changed target did not require reconfirmation", answer)
				}
			})
		}
	}
}

// gateReply builds the complete structured response sent by the intake model.
func gateReply(gates ...any) map[string]any {
	p := completionFixture()
	for i := range gates {
		category := "permission"
		if gates[i] == "deletion" {
			category = "destructive"
		}
		p.Approvals = append(p.Approvals, completionplan.Approval{ID: fmt.Sprintf("approval-%d", i), Phase: "after_diff", Category: category, Target: "対象", Operation: "承認", Condition: "差分を確認"})
	}
	raw, _ := json.Marshal(p.Record())
	var plan map[string]any
	_ = json.Unmarshal(raw, &plan)
	for i, a := range plan["approvals"].([]any) {
		item := a.(map[string]any)
		item["gate"] = gates[i]
		item["evidence"] = nil
		item["children"] = nil
	}
	return map[string]any{"reply": "整理しました", "basisConflicts": []any{}, "title": "題名", "body": "## 受入条件\n- 内容", "ready": true, "readFiles": []any{}, "reviewedFiles": []any{"README.md"}, "questions": []any{}, "imageReviews": []any{}, "splitIssues": []any{}, "crossProject": map[string]any{"reason": "", "scopes": []any{}, "approvalMessageId": ""}, "changeScope": map[string]any{"known": true, "paths": []any{"a.go"}, "reason": ""}, "dependsOnIssues": []any{}, "kind": "implementation", "escalate": false, "escalateReason": "", "heavyReason": "", "workType": "bugfix", "estimate": map[string]any{"lines": 10, "files": 1}, "completionPlan": plan, "executionPlan": map[string]any{"version": 1, "requirements": []any{map[string]any{"id": "fixture", "criterion": "内容", "owner": "implementer", "phase": "verification", "method": "go test ./...", "expected": "終了コード0", "evidenceKind": "fixed_test", "capabilities": []any{}, "inputs": []any{}, "children": []any{}}}}}
}

func gateTurn(t *testing.T, candidates ...map[string]any) (*Service, Record, int) {
	t.Helper()
	calls := 0
	var s *Service
	s, d := testService(t, func(_ context.Context, request ai.Request) (ai.Answer, error) {
		if calls >= len(candidates) {
			t.Fatalf("unexpected repair call %d", calls+1)
		}
		if calls > 0 {
			saved, found, err := s.options.Records.Get(context.Background(), "drafts", draftID)
			if err != nil || !found || saved["completionPlan"] != nil {
				t.Fatalf("invalid plan saved before repair: %v %v", saved, err)
			}
		}
		candidate := candidates[calls]
		calls++
		schema, err := ai.Compile(request.SchemaFile)
		if err != nil {
			t.Fatal(err)
		}
		// Turn uses the transport envelope; the candidate and its approval
		// fields remain unchanged so invalid plans still fail real validation.
		raw, err := json.Marshal(Record{"response": candidate})
		if err != nil {
			t.Fatal(err)
		}
		value, err := ai.Validate(schema, raw)
		if err != nil {
			return measuredAnswer(nil), &ai.Failure{Kind: "format", Message: err.Error()}
		}
		return measuredAnswer(value), nil
	})
	s.options.Reference = nil
	d["completionPlanVersion"] = 1
	out, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.options.Records.Change(context.Background(), "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) {
		current["completionPlan"] = out["completionPlan"]
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, found, err := s.options.Records.Get(context.Background(), "drafts", draftID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	return s, saved, calls
}

func TestTurnExistingGateSchemaRoundTrip(t *testing.T) {
	_, saved, calls := gateTurn(t, gateReply("permission", "deletion", nil))
	if calls != 1 {
		t.Fatal(calls)
	}
	p, err := completionplan.Parse(saved["completionPlan"])
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Approvals) != 3 || p.Approvals[0].Gate == nil || *p.Approvals[0].Gate != "permission" || p.Approvals[1].Gate == nil || *p.Approvals[1].Gate != "deletion" || p.Approvals[2].Gate != nil {
		t.Fatal(p.Approvals)
	}
	pending, err := jobs.PendingFinalApprovals(&jobs.Job{View: map[string]any{"completionPlan": saved["completionPlan"]}})
	if err != nil || len(pending) != 1 || pending[0].ID != "approval-2" {
		t.Fatal(pending, err)
	}
}

func TestTurnExistingGateInvalidOutputRepair(t *testing.T) {
	for _, tc := range []struct {
		name   string
		gate   any
		before bool
	}{{"unknown", "other", false}, {"empty", "", false}, {"type", 42, false}, {"before", "permission", true}} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := gateReply(tc.gate)
			if tc.before {
				invalid["completionPlan"].(map[string]any)["approvals"].([]any)[0].(map[string]any)["phase"] = "before_implementation"
			}
			_, saved, calls := gateTurn(t, invalid, gateReply(nil))
			if calls != 2 {
				t.Fatal(calls)
			}
			p, err := completionplan.Parse(saved["completionPlan"])
			if err != nil || len(p.Approvals) != 1 || p.Approvals[0].Gate != nil || p.Approvals[0].Phase != "after_diff" {
				t.Fatal(p, err)
			}
		})
	}
}

func TestTurnExistingGateNullCompatibility(t *testing.T) {
	_, saved, _ := gateTurn(t, gateReply(nil))
	current, err := completionplan.Parse(saved["completionPlan"])
	if err != nil {
		t.Fatal(err)
	}
	legacy := completionFixture()
	legacy.Approvals = []completionplan.Approval{{ID: "approval-0", Phase: "after_diff", Category: "permission", Target: "対象", Operation: "承認", Condition: "差分を確認"}}
	oldJSON, _ := json.Marshal(legacy.Record())
	newJSON, _ := json.Marshal(saved["completionPlan"])
	if !bytes.Equal(oldJSON, newJSON) || current.Fingerprint() != legacy.Fingerprint() || !reflect.DeepEqual(completionplan.Compare(legacy, legacy), completionplan.Compare(legacy, current)) {
		t.Fatal(string(oldJSON), string(newJSON))
	}
	old, err := completionplan.Parse(legacy.Record())
	if err != nil {
		t.Fatal(err)
	}
	pending := func(plan any) []jobs.PendingFinalApproval {
		v, e := jobs.PendingFinalApprovals(&jobs.Job{View: map[string]any{"completionPlan": plan, "executionPlan": map[string]any{"version": 1, "requirements": []any{map[string]any{"id": "fixture", "criterion": "内容", "owner": "implementer", "phase": "verification", "method": "go test ./...", "expected": "終了コード0", "evidenceKind": "fixed_test", "capabilities": []any{}, "inputs": []any{}, "children": []any{}}}}}})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	if !reflect.DeepEqual(pending(old.Record()), pending(saved["completionPlan"])) {
		t.Fatal("pending approvals differ")
	}
}

func TestCompletionPlanChildrenRoundTrip(t *testing.T) {
	for _, children := range []any{[]any{2}, nil} {
		t.Run(fmt.Sprint(children), func(t *testing.T) {
			value := gateReply(nil)
			item := value["completionPlan"].(map[string]any)["approvals"].([]any)[0].(map[string]any)
			item["children"] = children
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"../../schemas/intake.schema.json", "../../../api/schemas/intake.schema.json"} {
				schema, err := ai.Compile(filepath.Clean(path))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ai.Validate(schema, raw); err != nil {
					t.Fatal(path, err)
				}
			}
			_, saved, calls := gateTurn(t, value)
			p, err := completionplan.Parse(saved["completionPlan"])
			if err != nil || calls != 1 {
				t.Fatal(p, calls, err)
			}
			legacy := completionFixture()
			legacy.Approvals = []completionplan.Approval{{ID: "approval-0", Phase: "after_diff", Category: "permission", Target: "対象", Operation: "承認", Condition: "差分を確認"}}
			if children == nil {
				if p.Fingerprint() != legacy.Fingerprint() {
					t.Fatal("null changed legacy fingerprint")
				}
			} else {
				legacy.Approvals[0].Children = []int{2}
				if !reflect.DeepEqual(p, legacy) {
					t.Fatal(p, legacy)
				}
			}
		})
	}
}
