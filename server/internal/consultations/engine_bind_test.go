package consultations

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"sync"
	"testing"

	"issue-desk-intake/server/internal/auth"
)

func TestEngineSnapshotCompatibility(t *testing.T) {
	s, _, _ := fixture(t)
	calls := 0
	opts := s.options
	opts.Start = func(context.Context, string, Record, auth.Principal) (Record, error) { calls++; return nil, nil }
	opts.Active = func(string) bool { calls++; return true }
	opts.Cancel = func(string) { calls++ }
	opts.StopExecution = func(string, stopChange) (Record, error) { calls++; return nil, nil }
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		f := s.engineSnapshot()
		if f.Start == nil || f.Active == nil || f.Cancel == nil || f.StopExecution == nil {
			t.Fatal("incomplete functions")
		}
		f.Start(context.Background(), "", nil, auth.Principal{})
		if !f.Active("") {
			t.Fatal("wrong active")
		}
		f.Cancel("")
		f.StopExecution("", nil)
	}
	check()
	s.options.Start = func(context.Context, string, Record, auth.Principal) (Record, error) { calls += 2; return nil, nil }
	s.options.Active = func(string) bool { calls += 2; return true }
	s.options.Cancel = func(string) { calls += 2 }
	s.options.StopExecution = func(string, stopChange) (Record, error) { calls += 2; return nil, nil }
	check()
	if calls != 12 {
		t.Fatalf("calls: %d", calls)
	}
}

func TestEngineSnapshotAtomicBinding(t *testing.T) {
	s := &Service{}
	e := &Engine{}
	initial := s.engineSnapshot()
	if initial.Start != nil || initial.Active != nil || initial.Cancel != nil || initial.StopExecution != nil {
		t.Fatal("initial functions not empty")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.BindEngine(e) }()
	for i := 0; i < 1000; i++ {
		f := s.engineSnapshot()
		n := 0
		if f.Start != nil {
			n++
		}
		if f.Active != nil {
			n++
		}
		if f.Cancel != nil {
			n++
		}
		if f.StopExecution != nil {
			n++
		}
		if n != 0 && n != 4 {
			wg.Wait()
			t.Fatalf("partial binding: %d", n)
		}
	}
	wg.Wait()
	f := s.engineSnapshot()
	if f.Start == nil || f.Active == nil || f.Cancel == nil || f.StopExecution == nil {
		t.Fatal("binding missing")
	}
}

func TestChatWithoutEngine(t *testing.T) {
	s, _, _ := fixture(t)
	s.options.Start = nil
	_, err := s.Chat(context.Background(), todoID, Record{}, auth.Principal{Kind: auth.Screen})
	if err == nil || problem(err).Status != 503 || problem(err).Message != "相談の実行先を利用できません。" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReceiptWithoutEngine(t *testing.T) {
	s, _, _ := fixture(t)
	s.options.Start = nil
	receipt, err := s.acceptReceipt(context.Background(), Record{"project": "owner/app"}, "issue", todoID, todoID, auth.Principal{Kind: auth.Screen})
	if err != nil || receipt["phase"] != "accepted" {
		t.Fatalf("unexpected receipt: %v, %v", receipt, err)
	}
}

func TestBindEngineConcurrentReceipt(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	e, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		return Record{"reply": "確認", "ready": false, "questions": []any{"確認事項"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s.BindEngine(e)
	started, done := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		for {
			select {
			case <-done:
				return
			default:
				s.BindEngine(e)
			}
		}
	}()
	<-started
	input := Record{"id": todoID, "project": "owner/app", "tier": "light", "text": "依頼の本文", "hold": true}
	_, err = s.AcceptIssue(ctx, input, auth.Principal{Kind: auth.Screen})
	close(done)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
}

func TestBindEngineConcurrentChat(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		return Record{"reply": "確認", "ready": false, "questions": []any{"確認事項"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s.BindEngine(e)
	start := make(chan struct{})
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(start)
		for {
			select {
			case <-done:
				return
			default:
				s.BindEngine(e)
			}
		}
	}()
	<-start
	for i := 0; i < 100; i++ {
		_ = s.busy(Record{"id": todoID})
		f := s.engineSnapshot()
		if f.Start == nil || f.Active == nil || f.Cancel == nil || f.StopExecution == nil {
			close(done)
			wg.Wait()
			t.Fatal("partial binding")
		}
	}
	_, err = s.Chat(ctx, todoID, Record{"messageId": sendID, "text": "相談"}, p)
	close(done)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	// Close cancels the context, so wait for the accepted turn to finish first.
	e.wg.Wait()
	e.Close()
	draft, err := s.Draft(ctx, todoID, p)
	if err != nil {
		t.Fatal(err)
	}
	messages := fleet.Items(draft["messages"])
	if draft["status"] != "draft" || len(messages) != 2 || text(record(messages[1])["role"]) != "assistant" || text(record(messages[1])["text"]) != "確認" {
		t.Fatalf("turn was not saved: %+v", draft)
	}
	// Legacy readers see the same binding until their migration.
	if s.options.Start == nil || s.options.Active == nil || s.options.Cancel == nil || s.options.StopExecution == nil {
		t.Fatal("legacy binding missing")
	}
}
