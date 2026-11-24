package filing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/consultations"
)

func TestPostgresSplitRetryRefreshesCompletionProvenance(t *testing.T) {
	for _, approvals := range []string{"required", "empty"} {
		for _, stage := range []string{"check", "build"} {
			t.Run(approvals+"/"+stage, func(t *testing.T) {
				ctx := context.Background()
				s, api := fixture(t)
				principal := auth.Principal{Kind: auth.Screen}
				p := planFixture()
				if approvals == "required" {
					p.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "対象設定", Operation: "変更", Condition: "隔離環境"}}
				}
				presentation := Record{"id": "presentation-old", "role": "assistant", "text": p.Section()}
				consultations.SaveCompletionPresentation(presentation, Record{"completionProposal": p.Fingerprint(), "completionPlan": p.Record()})
				oldRows := []any{presentation, Record{"role": "user", "id": "old", "text": "A", "at": "2026-11-07T10:00:00Z", "source": "screen-session"}}
				oldReceipt, ok := completionplan.Confirmed(p, oldRows)
				if !ok {
					t.Fatal("invalid initial receipt")
				}
				parent, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) {
					d := draftFixture()
					d["messages"], d["completionPlan"], d["completionApproval"], d["completionPlanVersion"] = oldRows, p.Record(), oldReceipt, 1
					d["status"], d["revision"] = current["status"], current["revision"]
					d["splitIssues"] = []any{
						Record{"title": "先行", "purpose": "部品", "acceptance": []any{"動く"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"src/a.go"}}},
						Record{"title": "後続", "purpose": "接続", "acceptance": []any{"繋がる"}, "dependsOn": []any{1}, "changeScope": Record{"known": true, "paths": []any{"src/b.go"}}},
					}
					return d, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				s.options.SplitPlan = PlanSplit
				interrupted := false
				interrupt := func(at string) error {
					if stage == at && !interrupted {
						interrupted = true
						return errors.New("fixture: interrupted before POST")
					}
					return nil
				}
				s.options.Check = func(_ context.Context, d Record) error {
					if _, err := prepareCompletionPlan(d); err != nil {
						return err
					}
					return interrupt("check")
				}
				builds := 0
				s.options.Build = func(ctx context.Context, d Record) (Prepared, error) {
					prepared, err := Prepare(ctx, configFixture(), d)
					if err != nil {
						return prepared, err
					}
					if err := interrupt("build"); err != nil {
						return Prepared{}, err
					}
					builds++
					return prepared, nil
				}
				if _, err = s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, principal); err == nil || !interrupted || api.posts != 0 {
					t.Fatal("pre-POST failure not observed", err, api.posts)
				}
				parent, _, err = s.options.Records.Get(ctx, "drafts", draftID)
				ids := fleet.Items(parent["splitChildren"])
				if err != nil || len(ids) != 2 {
					t.Fatal(parent, err)
				}
				for _, id := range ids {
					child, found, err := s.options.Records.Get(ctx, "drafts", fleet.Text(id))
					if err != nil || !found || child["attempted"] == true || child["preparedJob"] != nil || child["issue"] != nil {
						t.Fatal(child, err)
					}
					if _, err = Prepare(ctx, configFixture(), child); err != nil {
						t.Fatal("initial saved child has invalid provenance", err)
					}
				}

				// Present a semantic change on the parent; an old A must not clear it.
				p.Basis = "認証条件を変更し、新しい接続経路で確認する"
				parent, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
					message := Record{"id": "presentation-new", "role": "assistant", "text": p.Section()}
					consultations.SaveCompletionPresentation(message, Record{"completionProposal": p.Fingerprint(), "completionPlan": p.Record()})
					d["completionPlan"], d["completionProposal"] = p.Record(), p.Fingerprint()
					d["completionReconfirmation"], d["completionReconfirmationAfter"] = p.Fingerprint(), len(fleet.Items(d["messages"]))
					d["messages"] = append(fleet.Items(d["messages"]), message)
					d["status"], d["ready"], d["questions"] = "draft", false, []any{"変更した計画を了承しますか"}
					return d, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = Prepare(ctx, configFixture(), parent); err == nil {
					t.Fatal("semantic change reused old A")
				}
				chat, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: s.options.Project, Start: func(_ context.Context, _ string, d Record, _ auth.Principal) (Record, error) { return d, nil }})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = chat.Chat(ctx, draftID, Record{"revision": parent["revision"], "text": "A"}, principal); err != nil {
					t.Fatal(err)
				}
				parent, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
					_, receipt, ok := completionplan.PresentedAfter(p, d["messages"], completionplan.PendingAfter(d["messages"], d["completionReconfirmationAfter"]))
					if !ok {
						t.Fatal("new authenticated A was not saved")
					}
					d["completionApproval"] = receipt
					d["status"], d["ready"], d["questions"] = "draft", true, []any{}
					return d, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				result, err := s.Submit(ctx, draftID, Record{"revision": parent["revision"]}, principal)
				if err != nil || result["status"] != "submitted" || api.posts != 2 || builds != 2 {
					t.Fatal("split retry lost the new reply", result, err, api.posts, builds)
				}
				// Read through a newly opened Records instance, as after a restart.
				reopened, err := consultations.OpenRecords(ctx, s.options.Records.DB)
				if err != nil {
					t.Fatal(err)
				}
				parent, found, err := reopened.Get(ctx, "drafts", draftID)
				if err != nil || !found {
					t.Fatal(parent, err)
				}
				for i, id := range ids {
					if fleet.Text(fleet.Items(result["splitChildren"])[i]) != fleet.Text(id) {
						t.Fatal("retry replaced a saved child ID")
					}
					child, found, err := reopened.Get(ctx, "drafts", fleet.Text(id))
					if err != nil || !found {
						t.Fatal(child, err)
					}
					for _, field := range []string{"messages", "completionPlan", "completionApproval", "completionReconfirmation", "completionReconfirmationAfter"} {
						want, _ := json.Marshal(parent[field])
						got, _ := json.Marshal(child[field])
						if string(want) != string(got) {
							t.Fatalf("child has stale %s: %s != %s", field, got, want)
						}
					}
					if _, err = completionplan.Check(child["completionPlan"], child["messages"], child["completionApproval"]); err != nil {
						t.Fatal(err)
					}
					if _, err = Prepare(ctx, configFixture(), child); err != nil {
						t.Fatal("saved retry child cannot be prepared", err)
					}
					// Neither the old reply nor a new receipt with only old history suffices.
					stale := clone(child)
					stale["messages"] = oldRows
					if _, err = Prepare(ctx, configFixture(), stale); err == nil {
						t.Fatal("new receipt accepted without new presentation/reply")
					}
					stale["completionApproval"] = oldReceipt
					if _, err = Prepare(ctx, configFixture(), stale); err == nil {
						t.Fatal("old reply cleared new confirmation")
					}
				}
			})
		}
	}
}
