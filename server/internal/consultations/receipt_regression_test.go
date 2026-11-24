package consultations

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
)

func TestReceiptLivenessSessionReconnectsPeriodicRecovery(t *testing.T) {
	services, _ := receiptServices(t)
	ctx := context.Background()
	var calls, events atomic.Int32
	e := receiptEngineFor(t, services[0], func(context.Context, Record) (Record, error) {
		calls.Add(1)
		return Record{"reply": "接続復旧"}, nil
	}, func(Record) { events.Add(1) })
	services[0].BindEngine(e)
	if err := e.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := services[1].AcceptIssue(ctx, receiptInput(todoID), auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	if err := e.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	waitReceipt(t, "first execution", func() bool { return !e.Busy(todoID) })
	assertReceiptResult(t, services[0], todoID, "接続復旧")
	e.receipts.mu.Lock()
	var pid int
	err := e.receipts.live.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid)
	e.receipts.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var killed bool
	if err := services[1].options.Records.DB.QueryRowContext(ctx, "SELECT pg_terminate_backend(?)", pid).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate=%v err=%v", killed, err)
	}
	if _, err := services[1].AcceptIssue(ctx, receiptInput(sendID), auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	// No explicit Recover or HTTP resend after the failure: use the shipping timer.
	deadline := time.NewTimer(2*receiptRecoveryInterval + 5*time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for events.Load() != 4 {
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatalf("session did not recover: calls=%d events=%d", calls.Load(), events.Load())
		}
	}
	waitReceipt(t, "reconnected execution", func() bool { return !e.Busy(sendID) })
	assertReceiptResult(t, services[1], sendID, "接続復旧")
	if calls.Load() != 2 {
		t.Fatalf("duplicate executions: %d", calls.Load())
	}
}

func TestReceiptStopDiscardCannotResurrect(t *testing.T) {
	for _, race := range []bool{false, true} {
		name := "before-discovery"
		if race {
			name = "after-claim"
		}
		t.Run(name, func(t *testing.T) {
			services, clock := receiptServices(t)
			ctx := context.Background()
			screen := auth.Principal{Kind: auth.Screen}
			sender := auth.Principal{Kind: auth.PC, TokenID: "sender"}
			var calls, events atomic.Int32
			entered := make(chan struct{})
			old := receiptEngineFor(t, services[0], func(ctx context.Context, _ Record) (Record, error) {
				calls.Add(1)
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			}, func(Record) { events.Add(1) })
			old.options.Principal = func(context.Context, string) (auth.Principal, error) { return sender, nil }
			services[0].BindEngine(old)
			if _, err := services[0].AcceptIssue(ctx, receiptInput(todoID), sender); err != nil {
				t.Fatal(err)
			}
			<-entered
			d, _, err := services[0].options.Records.Get(ctx, "drafts", todoID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = services[0].Stop(ctx, todoID, Record{"operationId": sendID, "revision": d["revision"], "reason": "停止"}, screen); err != nil {
				t.Fatal(err)
			}
			waitReceipt(t, "stop not complete", func() bool { return !old.Busy(todoID) })
			if readReceipt(t, services[0], todoID)["phase"] != "processing" {
				t.Fatal("test must exercise unfinished receipt")
			}
			before := events.Load()
			claimed, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var next *Engine
			var recovery chan error
			if race {
				// Freeze the successor just after its real claim commits, before it reads
				// the draft. Deletion must win even against this already accepted recovery.
				old.Close()
				clock.advance(receiptTakeoverAfter + time.Second)
				next = receiptEngineFor(t, services[1], func(context.Context, Record) (Record, error) { calls.Add(1); return Record{"reply": "復活"}, nil }, func(Record) { events.Add(1) })
				next.options.Principal = func(context.Context, string) (auth.Principal, error) { close(claimed); <-release; return sender, nil }
				services[1].BindEngine(next)
				recovery = make(chan error, 1)
				go func() { recovery <- next.Recover(ctx) }()
				<-claimed
			}
			if _, err := services[0].Discard(ctx, todoID, Record{}, screen); err != nil {
				t.Fatal(err)
			}
			if race {
				once.Do(func() { close(release) })
				if err := <-recovery; err != nil {
					t.Fatal(err)
				}
			} else {
				next = old
			}
			for i := 0; i < 2; i++ {
				if err := next.Recover(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if d, found, err := services[1].options.Records.Get(ctx, "drafts", todoID); err != nil || found {
				t.Fatalf("resurrected: %v %v", d, err)
			}
			if calls.Load() != 1 || events.Load() != before {
				t.Fatalf("calls=%d events=%d before=%d", calls.Load(), events.Load(), before)
			}
			if readReceipt(t, services[1], todoID)["phase"] != "rejected" {
				t.Fatal("withdrawn receipt remains pending")
			}
		})
	}
}
