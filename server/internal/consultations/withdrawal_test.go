package consultations

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
)

func TestQueuedConsultationReturnsToTodoAndCannotRestartLate(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	secondRuns := make(chan struct{}, 1)
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(ctx context.Context, d Record) (Record, error) {
		if d["id"] == todoID {
			close(firstStarted)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		} else {
			secondRuns <- struct{}{}
		}
		return Record{"reply": "回答", "ready": false}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	s.BindEngine(engine)
	for _, id := range []string{todoID, photoID} {
		if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": id, "project": "owner/app"}, p, false); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Chat(ctx, id, Record{"messageId": sendID, "text": "最初の依頼"}, p); err != nil {
			t.Fatal(err)
		}
		if id == todoID {
			<-firstStarted
		}
	}
	todo, err := s.ReturnToTodo(ctx, photoID, Record{}, p)
	if err != nil || todo["body"] != "最初の依頼" || todo["returnedAt"] == nil {
		t.Fatalf("%+v %v", todo, err)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for (engine.Busy(todoID) || engine.Busy(photoID)) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-secondRuns:
		t.Fatal("withdrawn queued work executed")
	default:
	}
	if _, found, _ := s.options.Records.Get(ctx, "drafts", photoID); found {
		t.Fatal("late result recreated draft")
	}
	if _, err := s.Draft(ctx, photoID, p); problem(err).Code != "draft_returned_to_todo" {
		t.Fatalf("lost redirection: %v", err)
	}
	var count int
	s.options.Records.DB.QueryRow("SELECT count(*) FROM draft_withdrawals WHERE draft=? AND operation='todo'", photoID).Scan(&count)
	if count != 1 {
		t.Fatal("withdrawal not recorded")
	}
	again, err := s.ReturnToTodo(ctx, photoID, Record{}, p)
	if err != nil || again["body"] != todo["body"] {
		t.Fatal("replay failed", err)
	}
}
func TestWithdrawalRejectsOldPCInstructionWithoutChangingDraft(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.PC, TokenID: "one", Delegated: true}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
		d["messages"] = []any{Record{"role": "user", "text": "内容"}}
		return d, nil
	})
	if _, err := s.ReturnToTodo(ctx, todoID, Record{"instructionAt": "2020-01-01T00:00:00Z"}, p); err == nil {
		t.Fatal("old instruction accepted")
	}
	if _, found, _ := s.options.Records.Get(ctx, "drafts", todoID); !found {
		t.Fatal("draft removed on rejected instruction")
	}
}
