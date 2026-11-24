package consultations

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/auth"
	"testing"
)

func TestSubmitHTTPAcceptsPersistentIntentWithoutFiling(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.PC, TokenID: "one"}
	s.options.Submit = func(context.Context, string, Record, auth.Principal) (Record, error) {
		t.Fatal("HTTP受付で起票を実行した")
		return nil, nil
	}
	original := Record{"id": todoID, "project": "owner/app", "owner": "token:one", "hold": true, "status": "draft", "ready": true, "revision": 3, "questions": []any{}}
	if _, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return original, nil }); err != nil {
		t.Fatal(err)
	}
	value, code, err := s.postDraftAction(ctx, Record{"revision": 3}, p, "", todoID, "submit")
	d := record(value)
	if err != nil || code != 202 || d["accepted"] != true || d["id"] != todoID || d["status"] != "auto_ready" {
		t.Fatalf("受理: %v %d %v", value, code, err)
	}
	saved, err := s.Draft(ctx, todoID, p)
	rev, _ := integer(saved["revision"])
	if err != nil || rev != 4 || record(saved["submissionRequest"])["at"] == nil {
		t.Fatalf("保存: %v %v", saved, err)
	}
	if reason := s.refusal(saved, "finish"); reason == "" {
		t.Fatal("受理後に終了できる")
	}
	again, err := s.AcceptSubmission(ctx, todoID, Record{"revision": 3}, p)
	if err != nil || again["updatedAt"] != saved["updatedAt"] {
		t.Fatalf("同じ意図の再送: %v %v", again, err)
	}
	if _, err = s.AcceptSubmission(ctx, todoID, Record{"revision": 2}, p); err == nil || problem(err).Status != 409 {
		t.Fatalf("旧版を受け付けた: %v", err)
	}
	if _, err = s.AcceptSubmission(ctx, todoID, Record{"revision": 3}, auth.Principal{Kind: auth.PC, TokenID: "other"}); err == nil || problem(err).Status != 404 {
		t.Fatalf("他の送り手: %v", err)
	}
	next := clone(saved)
	queueChatTurn(next, saved, false, timestamp(s.options.Now()))
	if next["submissionRequest"] != nil {
		t.Fatal("新しい相談に過去の確定要求を引き継いだ")
	}
}

func TestSubmitAcceptanceKeepsHoldRevisionAndConflictGuards(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(Record)
		status int
	}{
		{"holdなし", func(d Record) { d["hold"] = false }, 403},
		{"回答待ち", func(d Record) { d["questions"] = []any{"確認"} }, 409},
		{"根拠の食い違い", func(d Record) {
			d["basisConflicts"] = []any{Record{"fact": "対象", "reason": "違う", "resolution": nil}}
		}, 409},
		{"終了済み", func(d Record) { d["status"] = "closed" }, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture(t)
			s.options.Submit = func(context.Context, string, Record, auth.Principal) (Record, error) {
				t.Fatal("起票した")
				return nil, nil
			}
			d := Record{"id": todoID, "project": "owner/app", "owner": "token:one", "hold": true, "status": "draft", "ready": true, "revision": 3, "questions": []any{}}
			tc.change(d)
			_, err := s.options.Records.Change(context.Background(), "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return d, nil })
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.AcceptSubmission(context.Background(), todoID, Record{"revision": 3}, auth.Principal{Kind: auth.PC, TokenID: "one"})
			if err == nil || problem(err).Status != tc.status {
				t.Fatalf("%v", err)
			}
			saved, _ := s.Draft(context.Background(), todoID, auth.Principal{Kind: auth.Screen})
			if saved["submissionRequest"] != nil {
				t.Fatal("拒否した意図を保存した")
			}
		})
	}
}

func TestSubmissionAcceptancePreservesUncertainPostReconciliation(t *testing.T) {
	for _, status := range []string{"submitting", "submit_unknown"} {
		t.Run(status, func(t *testing.T) {
			s, _, _ := fixture(t)
			s.options.Submit = func(context.Context, string, Record, auth.Principal) (Record, error) {
				t.Fatal("起票した")
				return nil, nil
			}
			original := Record{"id": todoID, "project": "owner/app", "status": status, "revision": 3, "ready": true, "questions": []any{}, "attempted": true}
			if _, err := s.options.Records.Change(context.Background(), "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return original, nil }); err != nil {
				t.Fatal(err)
			}
			d, err := s.AcceptSubmission(context.Background(), todoID, Record{"revision": 3}, auth.Principal{Kind: auth.Screen})
			if err != nil || d["status"] != status || d["attempted"] != true {
				t.Fatalf("照合状態を失った: %v %v", d, err)
			}
		})
	}
}
