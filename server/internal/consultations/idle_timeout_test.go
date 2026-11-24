package consultations

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
)

func TestIdleTimeoutDoesNotCountOrRetry(t *testing.T) {
	for _, tc := range []struct {
		name             string
		err              error
		attempts, streak int64
		status           string
		queued           bool
	}{
		{"idle known", &ai.IdleTimeoutError{Err: &ai.Failure{Kind: "timeout", Message: "idle", Temporary: true}}, 2, 2, "error", false},
		{"idle unknown", &ai.IdleTimeoutError{Err: &ai.Failure{Kind: "usage_unmeasured", Message: "hold"}}, 2, 2, "error", false},
		{"total", &ai.Failure{Kind: "timeout", Message: "total", Temporary: true}, 3, 3, "error", false},
		{"other", &ai.Failure{Kind: "execution", Message: "failure"}, 3, 0, "error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture(t)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
				_, e := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) { d["attempts"] = 2; d["timeoutStreak"] = 2; return d, nil })
				if e != nil {
					return nil, e
				}
				return nil, tc.err
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			if _, err = s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
				t.Fatal(err)
			}
			s.options.Start = engine.Start
			if _, err = s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "内容"}, p); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for engine.Busy(todoID) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			d, err := s.Draft(ctx, todoID, p)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := integer(d["attempts"])
			streak, _ := integer(d["timeoutStreak"])
			if a != tc.attempts || streak != tc.streak || d["status"] != tc.status || (d["queued"] == true) != tc.queued || d["retryAt"] != nil {
				t.Fatalf("draft=%+v", d)
			}
		})
	}
}

func TestIdleThenTotalTimeoutKeepsPreviousStreak(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	calls := 0
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		calls++
		if calls == 1 {
			_, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) { d["timeoutStreak"] = 2; return d, nil })
			if err != nil {
				return nil, err
			}
			return nil, &ai.IdleTimeoutError{Err: &ai.Failure{Kind: "timeout", Message: "idle", Temporary: true}}
		}
		return nil, &ai.Failure{Kind: "timeout", Message: "total", Temporary: true}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err = s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	s.options.Start = engine.Start
	if _, err = s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "内容"}, p); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for engine.Busy(todoID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_, err = s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
		d["status"] = "thinking"
		d["run"] = "second-run"
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Draft(ctx, todoID, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Start(ctx, todoID, d, p); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for engine.Busy(todoID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	d, err = s.Draft(ctx, todoID, p)
	if err != nil || calls != 2 || d["status"] != "error" || d["retryAt"] != nil {
		t.Fatalf("calls=%d draft=%v err=%v", calls, d, err)
	}
	if streak, _ := integer(d["timeoutStreak"]); streak != 3 {
		t.Fatal(d)
	}
}
