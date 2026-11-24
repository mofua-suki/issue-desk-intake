package filing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"strings"
	"testing"
)

func planFixture() completionplan.Plan {
	p := completionplan.Plan{Version: 1, Basis: "既存の受入条件と認証を維持する", Approvals: []completionplan.Approval{}}
	for _, phase := range []string{"implementation", "verification", "merge", "deployment"} {
		p.Steps = append(p.Steps, completionplan.Step{Phase: phase, Owner: "コントローラー", Prerequisite: "依頼対象の必要条件を確認", Evidence: "固定検証と配置版を照合"})
	}
	return p
}

func TestPreparePendingApprovalFreePlanRequiresFreshPresentationReply(t *testing.T) {
	d := draftFixture()
	p := planFixture()
	presentation := func() Record {
		return Record{"role": "assistant", "completionProposal": p.Fingerprint(), "completionPlan": p.Record()}
	}
	reply := func(id string) Record {
		return Record{"role": "user", "id": id, "text": "A", "at": "2026-11-07T10:00:00+09:00", "source": "cli-key"}
	}
	rows := []any{presentation(), reply("old")}
	old, _ := completionplan.Confirmed(p, rows)
	d["completionPlan"], d["completionReconfirmation"], d["completionReconfirmationAfter"] = p.Record(), p.Fingerprint(), 2
	d["messages"], d["completionApproval"] = rows, old
	if _, err := Prepare(context.Background(), configFixture(), d); err == nil {
		t.Fatal("old A cleared a new pending barrier for the same fingerprint")
	}
	rows = append(rows, presentation(), reply("fresh"))
	receipt, _ := completionplan.Confirmed(p, rows)
	d["messages"], d["completionApproval"] = rows, receipt
	// Simulate backup/restore of the additive JSON format before filing.
	raw, _ := json.Marshal(d)
	var restored Record
	_ = json.Unmarshal(raw, &restored)
	prepared, err := Prepare(context.Background(), configFixture(), restored)
	if err != nil || prepared.Job.View["completionApproval"] == nil {
		t.Fatal(prepared, err)
	}
	restored["completionReconfirmation"] = "different-target"
	if _, err := Prepare(context.Background(), configFixture(), restored); err == nil {
		t.Fatal("pending target mismatch accepted")
	}
}
func TestPrepareCarriesCompletionPlanAndFinalBodyEvidence(t *testing.T) {
	d := draftFixture()
	p := planFixture()
	d["completionPlanVersion"] = 1
	d["completionPlan"] = p.Record()
	prepared, err := Prepare(context.Background(), configFixture(), d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prepared.Body, "## 完了までの計画と承認") || prepared.Job.View["completionPlan"] == nil {
		t.Fatal(prepared)
	}
	sum := sha256.Sum256([]byte(prepared.Body))
	if row(prepared.Job.View["completionPlanEvidence"])["bodySHA256"] != hex.EncodeToString(sum[:]) {
		t.Fatal("hash did not cover final body", prepared.Job.View)
	}
}
func TestPrepareRejectsInventedAndChangedPlanApproval(t *testing.T) {
	d := draftFixture()
	p := planFixture()
	p.Approvals = []completionplan.Approval{{Category: "permission", ID: "configure", Phase: "before_implementation", Target: "入口の設定", Operation: "設定変更", Condition: "確認した接続経路だけ"}}
	d["completionPlan"] = p.Record()
	d["completionApproval"] = Record{"fingerprint": p.Fingerprint(), "quote": "承認します"}
	if _, err := Prepare(context.Background(), configFixture(), d); err == nil {
		t.Fatal("AI assertion became approval")
	}
	rows := []any{Record{"role": "assistant", "completionProposal": p.Fingerprint()}, Record{"id": "u", "role": "user", "text": "A", "at": "2026-11-07T10:00:00+09:00", "source": "screen-session"}}
	receipt, ok := completionplan.Confirmed(p, rows)
	if !ok {
		t.Fatal("fixture")
	}
	d["messages"], d["completionApproval"] = rows, receipt
	prepared, err := Prepare(context.Background(), configFixture(), d)
	if err != nil || prepared.Job.View["completionApproval"] == nil {
		t.Fatal(prepared, err)
	}
	p.Basis = "接続経路を変更する"
	d["completionPlan"] = p.Record()
	if _, err := Prepare(context.Background(), configFixture(), d); err == nil {
		t.Fatal("old approval survived basis change")
	}
}

func TestPrepareCannotClearPendingReconfirmationWithOldQuote(t *testing.T) {
	d := draftFixture()
	p := planFixture()
	quote := "owner/repoの設定変更を隔離環境で進めてください"
	p.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "owner/repo", Operation: "設定変更", Condition: "隔離環境", Evidence: &completionplan.Evidence{MessageID: "u", Quote: quote, At: "2026-11-07T10:00:00+09:00"}}}
	rows := []any{Record{"id": "u", "role": "user", "text": quote, "at": "2026-11-07T10:00:00+09:00", "source": "screen-session"}}
	receipt, ok := completionplan.Confirmed(p, rows)
	if !ok {
		t.Fatal("fixture")
	}
	d["completionPlan"], d["completionApproval"], d["completionReconfirmation"], d["messages"] = p.Record(), receipt, p.Fingerprint(), rows
	if _, err := Prepare(context.Background(), configFixture(), d); err == nil {
		t.Fatal("pending basis change reused old quote at filing")
	}
}

func TestSplitCompletionBarrierSurvivesParentAndChildPreparation(t *testing.T) {
	d := draftFixture()
	d["splitIssues"] = []any{
		Record{"title": "先行", "purpose": "部品", "acceptance": []any{"動く"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"src/a.go"}, "reason": ""}},
		Record{"title": "後続", "purpose": "接続", "acceptance": []any{"繋がる"}, "dependsOn": []any{1}, "changeScope": Record{"known": true, "paths": []any{"src/b.go"}, "reason": ""}},
	}
	p := planFixture() // The changed plan no longer has approval items.
	presentation := Record{"role": "assistant", "completionProposal": p.Fingerprint(), "completionPlan": p.Record()}
	oldRows := []any{presentation, Record{"role": "user", "id": "old", "text": "A", "at": "2026-11-07T10:00:00Z", "source": "screen-session"}}
	oldReceipt, _ := completionplan.Confirmed(p, oldRows)
	d["completionPlan"], d["completionReconfirmation"], d["completionReconfirmationAfter"] = p.Record(), p.Fingerprint(), 2
	d["messages"], d["completionApproval"] = oldRows, oldReceipt
	if _, err := PlanSplit(d); err == nil {
		t.Fatal("split planner bypassed pending parent")
	}
	// Service.split must validate before even invoking an injected planner.
	s := &Service{options: Options{SplitPlan: func(Record) ([]Record, error) {
		t.Fatal("pending parent reached split planner")
		return nil, nil
	}}}
	if _, err := s.split(context.Background(), d); err == nil {
		t.Fatal("split service bypassed pending parent")
	}
	rows := append(append([]any{}, oldRows...), presentation, Record{"role": "user", "id": "fresh", "text": "A", "at": "2026-11-07T10:01:00Z", "source": "screen-session"})
	receipt, _ := completionplan.Confirmed(p, rows)
	d["messages"], d["completionApproval"] = rows, receipt
	children, err := PlanSplit(d)
	if err != nil || len(children) != 2 {
		t.Fatal(children, err)
	}
	for _, child := range children {
		child["id"] = draftUUID
		if child["completionReconfirmation"] != p.Fingerprint() || child["completionReconfirmationAfter"] != 2 {
			t.Fatal("split lost pending state", child)
		}
		if _, err := Prepare(context.Background(), configFixture(), child); err != nil {
			t.Fatal("fresh answer was lost by split", err)
		}
		fingerprint := splitFingerprint(child)
		child["completionReconfirmationAfter"] = 4
		if splitFingerprint(child) == fingerprint {
			t.Fatal("split persistence ignores changed confirmation boundary")
		}
		child["messages"], child["completionApproval"] = oldRows, oldReceipt
		if _, err := Prepare(context.Background(), configFixture(), child); err == nil {
			t.Fatal("split child accepted old response")
		}
	}
}

func TestPlanSplitApprovalChildren(t *testing.T) {
	d := draftFixture()
	d["splitIssues"] = []any{
		Record{"title": "先行", "purpose": "部品", "acceptance": []any{"動く"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"src/a.go"}}},
		Record{"title": "後続", "purpose": "接続", "acceptance": []any{"繋がる"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"src/b.go"}}},
	}
	p := planFixture()
	p.Approvals = []completionplan.Approval{{Category: "scope", ID: "scope", Phase: "after_diff", Target: "変更範囲", Operation: "変更する", Condition: "差分を確認", Children: []int{3}}}
	d["completionPlan"] = p.Record()
	children, err := PlanSplit(d)
	if problem, ok := err.(*contract.Problem); !ok || problem.Status != 409 || len(children) != 0 {
		t.Fatalf("out of range: children=%v err=%v", children, err)
	}
	p.Approvals[0].Children = []int{2}
	d["completionPlan"] = p.Record()
	children, err = PlanSplit(d)
	if err != nil || len(children) != 2 {
		t.Fatalf("split: %v %v", children, err)
	}
	for i, child := range children {
		child["id"], child["splitIndex"] = draftUUID, i+1
		prepared, err := Prepare(context.Background(), configFixture(), child)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Job.View["splitIndex"] != int64(i+1) {
			t.Fatalf("index: %v", prepared.Job.View)
		}
		got, err := completionplan.Parse(prepared.Job.View["completionPlan"])
		if err != nil || !reflect.DeepEqual(got, p) {
			t.Fatalf("plan: %v %v", got, err)
		}
	}
}

func TestCompletionPlanChildrenWithoutSplit(t *testing.T) {
	d := draftFixture()
	p := planFixture()
	p.Approvals = []completionplan.Approval{{Category: "scope", ID: "scope", Phase: "after_diff", Target: "変更範囲", Operation: "変更する", Condition: "差分を確認", Children: []int{1}}}
	d["completionPlan"] = p.Record()
	_, err := Prepare(context.Background(), configFixture(), d)
	if problem, ok := err.(*contract.Problem); !ok || problem.Status != 409 {
		t.Fatalf("expected 409: %v", err)
	}
	p.Approvals[0].Children = nil
	d["completionPlan"] = p.Record()
	if _, err := Prepare(context.Background(), configFixture(), d); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresSplitApprovalChildrenFiling(t *testing.T) {
	ctx := context.Background()
	s, api := fixture(t)
	s.options.SplitPlan = PlanSplit
	s.options.Build = func(ctx context.Context, d Record) (Prepared, error) { return Prepare(ctx, configFixture(), d) }
	s.options.Check = func(_ context.Context, d Record) error { _, err := prepareCompletionPlan(d); return err }
	p := planFixture()
	p.Approvals = []completionplan.Approval{{ID: "scope", Category: "scope", Phase: "after_diff", Target: "対象", Operation: "変更", Condition: "差分確認", Children: []int{3}}}
	parent, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) {
		d := draftFixture()
		d["status"], d["revision"] = current["status"], current["revision"]
		d["completionPlan"] = p.Record()
		d["splitIssues"] = []any{
			Record{"title": "先行", "purpose": "部品", "acceptance": []any{"動く"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"src/a.go"}}},
			Record{"title": "後続", "purpose": "接続", "acceptance": []any{"繋がる"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"src/b.go"}}},
		}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, auth.Principal{Kind: auth.Screen})
	if problem, ok := err.(*contract.Problem); !ok || problem.Status != 409 || api.posts != 0 {
		t.Fatal(err, api.posts)
	}
	stored, _, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || len(fleet.Items(stored["splitChildren"])) != 0 {
		t.Fatal(stored, err)
	}
	p.Approvals[0].Children = []int{2}
	parent, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) { d["completionPlan"] = p.Record(); return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, auth.Principal{Kind: auth.Screen})
	if err != nil || api.posts != 2 {
		t.Fatal(result, err, api.posts)
	}
	for i := 1; i <= 2; i++ {
		job, err := s.options.Jobs.ByIssue(ctx, "owner/repo", int64(i))
		if err != nil || job == nil {
			t.Fatal(job, err)
		}
		index, ok := integer(job.View["splitIndex"])
		got, parseErr := completionplan.Parse(job.View["completionPlan"])
		if !ok || index != int64(i) || parseErr != nil || !reflect.DeepEqual(got, p) {
			t.Fatal(job.View, parseErr)
		}
	}
}

func TestPostgresCompletionPlanChildrenWithoutSplit(t *testing.T) {
	ctx := context.Background()
	s, api := fixture(t)
	s.options.Build = func(ctx context.Context, d Record) (Prepared, error) { return Prepare(ctx, configFixture(), d) }
	s.options.Check = func(_ context.Context, d Record) error { _, err := prepareCompletionPlan(d); return err }
	p := planFixture()
	p.Approvals = []completionplan.Approval{{ID: "scope", Category: "scope", Phase: "after_diff", Target: "対象", Operation: "変更", Condition: "差分確認", Children: []int{1}}}
	parent, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) {
		d := draftFixture()
		d["status"], d["revision"], d["completionPlan"] = current["status"], current["revision"], p.Record()
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, auth.Principal{Kind: auth.Screen})
	if problem, ok := err.(*contract.Problem); !ok || problem.Status != 409 || api.posts != 0 {
		t.Fatal(err, api.posts)
	}
	p.Approvals[0].Children = nil
	parent, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) { d["completionPlan"] = p.Record(); return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, auth.Principal{Kind: auth.Screen})
	if err != nil || api.posts != 1 || result["status"] != "submitted" {
		t.Fatal(result, err, api.posts)
	}
}
