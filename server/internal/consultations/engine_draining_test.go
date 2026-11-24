package consultations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

func TestDrainingTurnRecovery(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped=%t", wrapped), func(t *testing.T) {
			s, _, _ := fixture(t)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "依頼"}, p); err != nil {
				t.Fatal(err)
			}
			stalls, err := NewErrorStalls(ctx, s, "revision", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			old, err := NewEngine(ctx, EngineOptions{Service: s, Stalls: stalls, Turn: func(ctx context.Context, d Record) (Record, error) {
				calls.Add(1)
				_, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, current Record) (Record, error) {
					current["progress"] = Record{"marker": "saved"}
					return current, nil
				})
				if err != nil {
					return nil, err
				}
				var failure error = contract.NewProblem(503, "draining", "SERVER_DRAINING")
				if wrapped {
					failure = fmt.Errorf("turn: %w", failure)
				}
				return nil, failure
			}})
			if err != nil {
				t.Fatal(err)
			}
			d, _, err := s.options.Records.Get(ctx, "drafts", todoID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = old.Start(ctx, todoID, d, p); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for calls.Load() == 0 || func() bool { d, _, _ = s.options.Records.Get(ctx, "drafts", todoID); return text(d["retryAt"]) == "" }() {
				if time.Now().After(deadline) {
					t.Fatal("rejection was not saved")
				}
				time.Sleep(time.Millisecond)
			}
			if d["status"] != "thinking" || d["run"] != nil || d["queued"] != true || d["error"] != nil || d["lastFailure"] != nil || record(d["progress"])["marker"] != "saved" || len(fleet.Items(d["messages"])) != 1 {
				t.Fatalf("saved: %+v", d)
			}
			if n, _ := integer(d["attempts"]); n != 0 {
				t.Fatalf("attempts: %v", d["attempts"])
			}
			retry, err := time.Parse(time.RFC3339Nano, text(d["retryAt"]))
			if err != nil || retry.Sub(s.options.Now()) < 50*time.Second {
				t.Fatalf("retry: %v %v", retry, err)
			}
			var failures int
			if err := s.options.Records.DB.QueryRow("SELECT COUNT(*) FROM intake_error_failures").Scan(&failures); err != nil || failures != 0 {
				t.Fatalf("failures=%d err=%v", failures, err)
			}
			if err := old.Quiesce(ctx); err != nil {
				t.Fatal(err)
			}
			old.Close()
			if calls.Load() != 1 {
				t.Fatalf("old server retried: %d", calls.Load())
			}
			// Simulate the persisted retry deadline passing before the replacement starts.
			_, err = s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
				d["retryAt"] = timestamp(s.options.Now().Add(-time.Second))
				return d, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			next, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(_ context.Context, d Record) (Record, error) {
				if record(d["progress"])["marker"] != "saved" || len(fleet.Items(d["messages"])) != 1 {
					t.Errorf("recovered input: %+v", d)
				}
				return Record{"reply": "再開", "questions": []any{"確認事項"}}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if err = next.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			deadline = time.Now().Add(5 * time.Second)
			for next.Busy(todoID) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			d, _, err = s.options.Records.Get(ctx, "drafts", todoID)
			if err != nil || d["status"] != "draft" || len(fleet.Items(d["messages"])) != 2 {
				t.Fatalf("recovery: %+v %v", d, err)
			}
		})
	}
}

func TestOtherTurnErrorsRemainFailures(t *testing.T) {
	for _, failure := range []error{errors.New("other"), contract.NewProblem(502, "other status", "SERVER_DRAINING"), contract.NewProblem(503, "other code", "OTHER")} {
		t.Run(failure.Error(), func(t *testing.T) {
			s, _, _ := fixture(t)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "依頼"}, p); err != nil {
				t.Fatal(err)
			}
			stalls, err := NewErrorStalls(ctx, s, "revision", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			e, err := NewEngine(ctx, EngineOptions{Service: s, Stalls: stalls, Turn: func(context.Context, Record) (Record, error) { return nil, failure }})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			e.attempt(ctx, todoID)
			d, _, err := s.options.Records.Get(ctx, "drafts", todoID)
			n, _ := integer(d["attempts"])
			var count int
			queryErr := s.options.Records.DB.QueryRow("SELECT COUNT(*) FROM intake_error_failures").Scan(&count)
			if err != nil || queryErr != nil || d["status"] != "error" || n != 1 || count != 1 {
				t.Fatalf("failure: %+v count=%d err=%v query=%v", d, count, err, queryErr)
			}
		})
	}
}
