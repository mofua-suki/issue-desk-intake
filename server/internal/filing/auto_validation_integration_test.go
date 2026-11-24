package filing

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
)

func TestAutoValidationFailureResubmitsThroughFiling(t *testing.T) {
	f, api := fixture(t)
	ctx := context.Background()
	f.options.SplitPlan = func(d Record) ([]Record, error) { return nil, validateProposals(d) }
	draft, err := f.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d consultations.Record) (consultations.Record, error) {
		d["status"] = "auto_ready"
		d["splitIssues"] = []any{Record{"title": "一件目", "purpose": "目的", "acceptance": []any{"完了"}, "dependsOn": []any{}}, Record{"title": "二件目", "purpose": "目的", "acceptance": []any{"完了"}, "dependsOn": []any{0}}}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := consultations.New(consultations.Options{Records: f.options.Records, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }, Submit: f.Submit})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: c, Turn: func(context.Context, consultations.Record) (consultations.Record, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	p := auth.Principal{Kind: auth.Screen}
	_, failure := f.Submit(ctx, draftID, Record{"revision": draft["revision"]}, p)
	if failure == nil || !strings.Contains(failure.Error(), "依存先") {
		t.Fatalf("dependency validation was not reached: %v", failure)
	}
	engine.SaveAutoSubmitFailure(ctx, draftID, draft, failure)
	saved, err := c.Draft(ctx, draftID, p)
	if err != nil || saved["status"] != "submit_error" {
		t.Fatalf("failure not stored: %v %v", saved, err)
	}
	accepted, err := c.AcceptSubmission(ctx, draftID, consultations.Record{"revision": saved["revision"]}, p)
	if err != nil || accepted["accepted"] != true {
		t.Fatalf("accept: %v %v", accepted, err)
	}
	queued, err := c.Draft(ctx, draftID, p)
	if err != nil {
		t.Fatal(err)
	}
	_, failure = f.SubmitAccepted(ctx, draftID, Record{"revision": queued["revision"]}, p)
	if failure == nil || !strings.Contains(failure.Error(), "依存先") {
		t.Fatalf("dependency validation was not reached on resubmit: %v", failure)
	}
	engine.SaveAutoSubmitFailure(ctx, draftID, queued, failure)
	after, err := c.Draft(ctx, draftID, p)
	if err != nil || after["status"] != "submit_error" || api.posts != 0 {
		t.Fatalf("resubmit changed state: %v %v", after, err)
	}
}
