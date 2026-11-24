package consultations

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/completionplan"
)

func TestCompletionPlanProposalAndAuthenticatedReplyAreSavedTogether(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	principal := auth.Principal{Kind: auth.Screen}
	draft, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app", "hold": true}, principal, false)
	if err != nil || draft["completionPlanVersion"] == nil {
		t.Fatal(draft, err)
	}
	_, err = s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "起票内容"}, principal)
	if err != nil {
		t.Fatal(err)
	}
	p := consultationCompletionPlanFixture()
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		return Record{"ready": false, "reply": "計画を了承するならAと返信", "questions": []any{"計画の確認"}, "completionPlan": p.Record(), "completionProposal": p.Fingerprint(), "completionReconfirmation": p.Fingerprint()}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.attempt(ctx, todoID)
	saved, err := s.Draft(ctx, todoID, principal)
	if err != nil {
		t.Fatal(err)
	}
	rows := fleet.Items(saved["messages"])
	if len(rows) != 2 || record(rows[0])["source"] != "screen-session" || record(rows[1])["completionProposal"] != p.Fingerprint() || saved["completionPlan"] == nil || saved["completionReconfirmation"] != p.Fingerprint() {
		t.Fatal(saved)
	}
	presented, err := completionplan.Parse(record(rows[1])["completionPlan"])
	if err != nil || presented.Fingerprint() != p.Fingerprint() {
		t.Fatal("missing structured presentation", rows[1], err)
	}
	// Re-open the records reader as after a restart. The new JSON fields require
	// no schema conversion or guessed reconstruction of old presentations.
	reopened, err := OpenRecords(ctx, s.options.Records.DB)
	if err != nil {
		t.Fatal(err)
	}
	restored, found, err := reopened.Get(ctx, "drafts", todoID)
	if err != nil || !found {
		t.Fatal(restored, err)
	}
	snapshot, _ := completionplan.Parse(record(fleet.Items(restored["messages"])[1])["completionPlan"])
	if snapshot.Fingerprint() != p.Fingerprint() {
		t.Fatal("restart lost presentation", restored)
	}
	// A later plan edit cannot mutate the immutable historical record.
	p.Basis = "今回の候補で変更した前提"
	if snapshot.Fingerprint() != presented.Fingerprint() {
		t.Fatal("snapshot aliased a mutable plan")
	}
	p = presented
	_, err = s.Chat(ctx, todoID, Record{"revision": saved["revision"], "text": "A"}, principal)
	if err != nil {
		t.Fatal(err)
	}
	saved, err = s.Draft(ctx, todoID, principal)
	if err != nil {
		t.Fatal(err)
	}
	receipt, ok := completionplan.Confirmed(p, saved["messages"])
	if !ok || receipt.Quote != "A" || receipt.Source != "screen-session" {
		t.Fatal(receipt, ok, saved)
	}
}

func consultationCompletionPlanFixture() completionplan.Plan {
	p := completionplan.Plan{Version: 1, Basis: "記録した利用者の依頼と前提", Approvals: []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "対象設定", Operation: "変更", Condition: "指定した経路で確認"}}}
	for _, phase := range []string{"implementation", "verification", "merge", "deployment"} {
		p.Steps = append(p.Steps, completionplan.Step{Phase: phase, Owner: "コントローラー", Prerequisite: "固定した条件と隔離環境", Evidence: "成功した検証記録と対象版"})
	}
	return p
}

func TestPresentationWriterRejectsMismatchedAndMalformedCandidates(t *testing.T) {
	p := consultationCompletionPlanFixture()
	for _, result := range []Record{
		{"completionProposal": "wrong-fingerprint", "completionPlan": p.Record()},
		{"completionProposal": "invented", "completionPlan": Record{"version": 1}},
		{"completionProposal": "invented"},
	} {
		message := Record{"role": "assistant"}
		SaveCompletionPresentation(message, result)
		if message["completionPlan"] != nil || message["completionProposal"] != nil {
			t.Fatal(message)
		}
	}
}

func TestPresentationWriterDetachesHistoricalPlan(t *testing.T) {
	p := consultationCompletionPlanFixture()
	result := Record{"completionProposal": p.Fingerprint(), "completionPlan": p.Record()}
	message := Record{"role": "assistant"}
	SaveCompletionPresentation(message, result)
	record(result["completionPlan"])["basis"] = "AIの次の候補で変更した前提"
	snapshot, err := completionplan.Parse(message["completionPlan"])
	if err != nil || snapshot.Fingerprint() != p.Fingerprint() || message["completionProposal"] != p.Fingerprint() {
		t.Fatal(message, err)
	}
}
