package consultations

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
)

func testReceiptOwnerPeriodicRecovery(t *testing.T) {
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	services, _ := receiptServices(t)
	ctx := context.Background()
	var ownerCalls, standbyCalls, events atomic.Int32
	done := make(chan struct{}, 1)
	owner := receiptEngineFor(t, services[0], func(context.Context, Record) (Record, error) {
		ownerCalls.Add(1)
		return Record{"reply": "別色を発見"}, nil
	}, func(r Record) {
		events.Add(1)
		if revision, _ := integer(r["revision"]); revision > 1 {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	})
	standby := receiptEngineFor(t, services[1], func(context.Context, Record) (Record, error) { standbyCalls.Add(1); return nil, nil }, nil)
	services[0].BindEngine(owner)
	// Starting an owner with no receipts must still install exactly one scanner.
	for i := 0; i < 4; i++ {
		if err := owner.Recover(ownerCtx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := services[1].AcceptIssue(ctx, receiptInput(todoID), auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	// Use the shipped ten-second timer, not a test-only interval or manual Recover.
	select {
	case <-done:
	case <-time.After(2*receiptRecoveryInterval + 5*time.Second):
		t.Fatal("periodic discovery did not run")
	}
	waitReceipt(t, "owner still active", func() bool { return !owner.Busy(todoID) })
	assertReceiptResult(t, services[1], todoID, "別色を発見")
	standby.receipts.mu.Lock()
	standbyLive := standby.receipts.live
	standby.receipts.mu.Unlock()
	if ownerCalls.Load() != 1 || standbyCalls.Load() != 0 || events.Load() != 2 || standbyLive != nil {
		t.Fatalf("owner=%d standby=%d events=%d live=%v", ownerCalls.Load(), standbyCalls.Load(), events.Load(), standbyLive != nil)
	}
	cancelOwner()
	<-owner.quiesced
	if err := owner.Quiesce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := services[1].AcceptIssue(ctx, receiptInput(sendID), auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	// An explicit scan and the periodic deadline both respect the stop fence.
	if err := owner.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(receiptRecoveryInterval + 50*time.Millisecond)
	if r := readReceipt(t, services[1], sendID); r["phase"] != "accepted" || r["claim"] != nil {
		t.Fatalf("stopped scanner acquired: %v", r)
	}
	next := receiptEngineFor(t, services[1], func(context.Context, Record) (Record, error) { return Record{"reply": "新担当"}, nil }, nil)
	services[1].BindEngine(next)
	if err := next.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	waitReceiptCompleted(t, services[1], sendID, next)
	assertReceiptResult(t, services[1], sendID, "新担当")
}

func TestReceiptStopDuringPreparationAndFirstChat(t *testing.T) {
	for _, stage := range []string{"saved", "claim-read", "claimed", "message"} {
		t.Run(stage, func(t *testing.T) {
			services, clock := receiptServices(t)
			ctx := context.Background()
			var calls atomic.Int32
			old := receiptEngineFor(t, services[0], func(context.Context, Record) (Record, error) {
				calls.Add(1)
				return Record{"reply": "停止前の誤実行"}, nil
			}, nil)
			services[0].BindEngine(old)
			screen := auth.Principal{Kind: auth.Screen}
			// The receiving colour has no Engine, as in production standby startup.
			if _, err := services[1].AcceptIssue(ctx, receiptInput(todoID), screen); err != nil {
				t.Fatal(err)
			}
			r := readReceipt(t, services[1], todoID)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var locked *sql.Tx
			if stage == "claim-read" {
				var err error
				locked, err = services[1].options.Records.DB.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer locked.Rollback()
				if _, err = locked.ExecContext(ctx, "UPDATE intake_receipts SET json=json WHERE id=?", todoID); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "claimed" {
				// Token revalidation is after the durable claim and before draft creation.
				input := receiptInput(todoID)
				if _, err := services[1].options.Records.DB.Exec("DELETE FROM intake_receipts"); err != nil {
					t.Fatal(err)
				}
				var err error
				r, err = services[1].acceptReceipt(ctx, input, "issue", todoID, todoID, auth.Principal{Kind: auth.PC, TokenID: "sender"})
				if err != nil {
					t.Fatal(err)
				}
				old.options.Principal = func(context.Context, string) (auth.Principal, error) {
					close(entered)
					<-release
					return auth.Principal{Kind: auth.PC, TokenID: "sender"}, nil
				}
			}
			if stage == "message" {
				// Chat really commits its user message before invoking this start snapshot.
				services[0].options.Start = func(ctx context.Context, id string, d Record, p auth.Principal) (Record, error) {
					close(entered)
					<-release
					return old.Start(ctx, id, d, p)
				}
			}
			accepted := make(chan error, 1)
			if stage != "saved" {
				go func() { _, err := old.Start(ctx, todoID, Record{"receipt": r}, screen); accepted <- err }()
				if stage == "claim-read" {
					waitReceipt(t, "preparation not registered", func() bool { old.mu.Lock(); defer old.mu.Unlock(); return old.turns > 0 })
				} else {
					<-entered
				}
			}
			quiesced := make(chan error, 1)
			go func() { quiesced <- old.Quiesce(ctx) }()
			<-old.quiesced
			if stage != "saved" {
				select {
				case err := <-quiesced:
					t.Fatalf("preparation escaped wait: %v", err)
				default:
				}
				if locked != nil {
					if err := locked.Commit(); err != nil {
						t.Fatal(err)
					}
				}
				once.Do(func() { close(release) })
				err := <-accepted
				if err == nil || problem(err).Status != 503 {
					t.Fatalf("start fence: %v", err)
				}
			}
			if err := <-quiesced; err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatal("AI began after fence")
			}
			saved := readReceipt(t, services[1], todoID)
			if saved["phase"] == "rejected" || saved["phase"] == "handed" {
				t.Fatalf("receipt lost: %v", saved)
			}
			d, _, err := services[1].options.Records.Get(ctx, "drafts", todoID)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "message" && (d["status"] != "thinking" || len(fleet.Items(d["messages"])) != 1) {
				t.Fatalf("Chat did not commit: %v", d)
			}
			old.Close()
			clock.advance(receiptTakeoverAfter + time.Second)
			next := receiptEngineFor(t, services[1], func(context.Context, Record) (Record, error) { calls.Add(1); return Record{"reply": "再開"}, nil }, nil)
			next.options.Principal = func(context.Context, string) (auth.Principal, error) {
				return auth.Principal{Kind: auth.PC, TokenID: "sender"}, nil
			}
			services[1].BindEngine(next)
			if err := next.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			waitReceiptCompleted(t, services[1], todoID, next)
			assertReceiptResult(t, services[1], todoID, "再開")
			if calls.Load() != 1 {
				t.Fatalf("calls=%d", calls.Load())
			}
		})
	}
}

func TestReceiptQueuedTurnStopsBeforeAIAndTransfers(t *testing.T) {
	services, clock := receiptServices(t)
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	defer once.Do(func() { close(release) })
	old := receiptEngineFor(t, services[0], func(ctx context.Context, d Record) (Record, error) {
		calls.Add(1)
		if d["id"] == todoID {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return Record{"reply": "保存"}, nil
	}, nil)
	services[0].BindEngine(old)
	screen := auth.Principal{Kind: auth.Screen}
	if _, err := services[0].AcceptIssue(ctx, receiptInput(todoID), screen); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := services[0].AcceptIssue(ctx, receiptInput(sendID), screen); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- old.Quiesce(ctx) }()
	<-old.quiesced
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("queued AI ran: %d", calls.Load())
	}
	assertReceiptResult(t, services[0], todoID, "保存")
	old.Close()
	clock.advance(receiptTakeoverAfter + time.Second)
	next := receiptEngineFor(t, services[1], func(context.Context, Record) (Record, error) { calls.Add(1); return Record{"reply": "引継ぎ"}, nil }, nil)
	services[1].BindEngine(next)
	if err := next.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	waitReceiptCompleted(t, services[1], sendID, next)
	assertReceiptResult(t, services[1], sendID, "引継ぎ")
	if calls.Load() != 2 {
		t.Fatal("wrong AI count")
	}
}

func TestReceiptOwnerCancellationPreservesAcceptedAI(t *testing.T) {
	services, _ := receiptServices(t)
	ctx := context.Background()
	ownerCtx, cancelOwner := context.WithCancel(ctx)
	defer cancelOwner()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var events atomic.Int32
	old := receiptEngineFor(t, services[0], func(aiCtx context.Context, _ Record) (Record, error) {
		close(entered)
		select {
		case <-release:
			return Record{"reply": "完了を保存"}, nil
		case <-aiCtx.Done():
			return nil, aiCtx.Err()
		}
	}, func(Record) { events.Add(1) })
	services[0].BindEngine(old)
	if err := old.Recover(ownerCtx); err != nil {
		t.Fatal(err)
	}
	if _, err := services[0].AcceptIssue(ctx, receiptInput(todoID), auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	<-entered
	cancelOwner()
	if _, err := services[1].AcceptIssue(ctx, receiptInput(sendID), auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	r := readReceipt(t, services[1], sendID)
	if _, err := old.Start(ctx, sendID, Record{"receipt": r}, auth.Principal{}); err == nil || problem(err).Status != 503 {
		t.Fatalf("owner loss did not fence acquisition: %v", err)
	}
	<-old.quiesced
	wait := make(chan error, 1)
	go func() { wait <- old.Quiesce(ctx) }()
	select {
	case err := <-wait:
		t.Fatalf("accepted AI cancelled or dropped: %v", err)
	default:
	}
	once.Do(func() { close(release) })
	if err := <-wait; err != nil {
		t.Fatal(err)
	}
	assertReceiptResult(t, services[1], todoID, "完了を保存")
	if events.Load() != 2 {
		t.Fatalf("events=%d", events.Load())
	}
	if r := readReceipt(t, services[1], sendID); r["claim"] != nil || r["phase"] != "accepted" {
		t.Fatalf("new receipt taken after loss: %v", r)
	}
	next := receiptEngineFor(t, services[1], func(context.Context, Record) (Record, error) { return Record{"reply": "次の担当"}, nil }, nil)
	services[1].BindEngine(next)
	if err := next.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	waitReceiptCompleted(t, services[1], sendID, next)
	assertReceiptResult(t, services[1], sendID, "次の担当")
}
