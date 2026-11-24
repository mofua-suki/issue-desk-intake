package consultations

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

type receiptClock struct{ nanos atomic.Int64 }

func (c *receiptClock) now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *receiptClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

// Both stores use the production PostgreSQL connector and schema, with separate
// pools. The second colour has HTTP/storage services but no bound Engine.
func receiptServices(t *testing.T) ([2]*Service, *receiptClock) {
	t.Helper()
	first, _, _ := fixture(t)
	var schema string
	if err := first.options.Records.DB.QueryRow("SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	other, err := store.OpenApplicationPostgres(context.Background(), os.Getenv(pgtest.DSNEnv), schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	records, err := OpenRecords(context.Background(), other.DB())
	if err != nil {
		t.Fatal(err)
	}
	options := first.options
	options.Records, options.Start = records, nil
	second, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	clock := &receiptClock{}
	clock.nanos.Store(time.Now().UTC().UnixNano())
	first.options.Now, second.options.Now = clock.now, clock.now
	first.options.Start = nil
	return [2]*Service{first, second}, clock
}
func receiptEngineFor(t *testing.T, s *Service, turn func(context.Context, Record) (Record, error), publish func(Record)) *Engine {
	t.Helper()
	e, err := NewEngine(context.Background(), EngineOptions{Service: s, Turn: turn, Publish: publish})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}
func receiptInput(id string) Record {
	return Record{"id": id, "project": "owner/app", "text": "本文", "hold": true, "tier": "light"}
}
func readReceipt(t *testing.T, s *Service, id string) Record {
	t.Helper()
	var raw string
	if err := s.options.Records.DB.QueryRow("SELECT json FROM intake_receipts WHERE id=?", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	r, err := decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func waitReceipt(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(time.Millisecond)
	}
}

// An earlier recovery scan may have returned before the previous owner's lock
// is released. Busy=false alone then describes an idle scanner, not completion.
func waitReceiptCompleted(t *testing.T, s *Service, id string, engines ...*Engine) {
	t.Helper()
	deadline := time.NewTimer(2*receiptRecoveryInterval + 5*time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		d, found, err := s.options.Records.Get(context.Background(), "drafts", id)
		if err != nil {
			t.Fatal(err)
		}
		idle := true
		for _, engine := range engines {
			idle = idle && !engine.Busy(id)
		}
		if found && text(d["status"]) != "thinking" && idle {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("receipt did not finish: %v", d)
		}
	}
}
func assertReceiptResult(t *testing.T, s *Service, id, reply string) {
	t.Helper()
	d, found, err := s.options.Records.Get(context.Background(), "drafts", id)
	if err != nil || !found || d["status"] != "draft" {
		t.Fatalf("draft=%v err=%v", d, err)
	}
	users, assistants := 0, 0
	for _, value := range fleet.Items(d["messages"]) {
		m := record(value)
		if m["role"] == "user" {
			users++
		}
		if m["role"] == "assistant" {
			assistants++
			if m["text"] != reply {
				t.Fatalf("reply=%v", m)
			}
		}
	}
	r := readReceipt(t, s, id)
	if users != 1 || assistants != 1 || r["phase"] != "handed" || record(r["claim"])["instanceId"] == nil {
		t.Fatalf("users=%d assistants=%d receipt=%v", users, assistants, r)
	}
}

func testTwoEngineReceiptDiscovery(t *testing.T) {
	services, _ := receiptServices(t)
	ctx := context.Background()
	var calls, events atomic.Int32
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	turn := func(ctx context.Context, d Record) (Record, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			return Record{"reply": "一度だけ"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	engines := [2]*Engine{}
	for i := range engines {
		engines[i] = receiptEngineFor(t, services[i], turn, func(Record) { events.Add(1) })
		services[i].BindEngine(engines[i])
	}
	// Save via the same receiving entry, with execution intentionally unbound.
	services[1].options.Start = nil
	if _, err := services[1].AcceptIssue(ctx, receiptInput(todoID), auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	services[1].BindEngine(engines[1])
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, e := range engines {
		wg.Add(1)
		go func(e *Engine) {
			defer wg.Done()
			<-start
			if err := e.Recover(ctx); err != nil {
				t.Error(err)
			}
		}(e)
	}
	close(start)
	wg.Wait()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no AI")
	}
	// Repeat discovery through both the receipt replay and thinking-draft Start.
	for i, e := range engines {
		if err := e.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		d, _, _ := services[i].options.Records.Get(ctx, "drafts", todoID)
		if _, err := e.Start(ctx, todoID, d, auth.Principal{}); err != nil {
			t.Fatal(err)
		}
		if _, err := services[i].AcceptIssue(ctx, receiptInput(todoID), auth.Principal{Kind: auth.Screen}); err != nil {
			t.Fatal(err)
		}
	}
	once.Do(func() { close(release) })
	waitReceiptCompleted(t, services[0], todoID, engines[:]...)
	assertReceiptResult(t, services[0], todoID, "一度だけ")
	if calls.Load() != 1 || events.Load() != 2 {
		t.Fatalf("calls=%d events=%d", calls.Load(), events.Load())
	}
}

func TestReceiptChangedClaimDiscardsResult(t *testing.T) {
	services, clock := receiptServices(t)
	ctx := context.Background()
	var events, submits, oldCalls, newCalls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	e := receiptEngineFor(t, services[0], func(ctx context.Context, d Record) (Record, error) {
		oldCalls.Add(1)
		close(entered)
		select {
		case <-release:
			return Record{"reply": "古い回答", "ready": true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, func(Record) { events.Add(1) })
	e.options.Submit = func(context.Context, Record) (Record, error) { submits.Add(1); return nil, nil }
	services[0].BindEngine(e)
	input := receiptInput(todoID)
	input["hold"] = false
	if _, err := services[0].AcceptIssue(ctx, input, auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	<-entered
	// Simulate loss of the old database session while its external AI is still
	// returning. The successor must acquire a real new stamp through Recover,
	// not through a fabricated receipt JSON or a replacement-only test helper.
	e.receipts.mu.Lock()
	err := e.receipts.live.Rollback()
	e.receipts.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(receiptTakeoverAfter + time.Second)
	next := receiptEngineFor(t, services[1], func(context.Context, Record) (Record, error) {
		newCalls.Add(1)
		return Record{"reply": "新しい回答", "ready": false, "questions": []any{"確認事項"}}, nil
	}, func(Record) { events.Add(1) })
	services[1].BindEngine(next)
	if err := next.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	waitReceiptCompleted(t, services[1], todoID, next)
	assertReceiptResult(t, services[1], todoID, "新しい回答")
	beforeReceipt, _ := canonicalJSON(readReceipt(t, services[1], todoID))
	beforeDraft, _, err := services[1].options.Records.Get(ctx, "drafts", todoID)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := canonicalJSON(beforeDraft)
	once.Do(func() { close(release) })
	waitReceipt(t, "old result still active", func() bool { return !e.Busy(todoID) })
	afterDraft, _, err := services[1].options.Records.Get(ctx, "drafts", todoID)
	after, _ := canonicalJSON(afterDraft)
	afterReceipt, _ := canonicalJSON(readReceipt(t, services[1], todoID))
	if err != nil || before != after || beforeReceipt != afterReceipt || events.Load() != 3 || submits.Load() != 0 || oldCalls.Load() != 1 || newCalls.Load() != 1 {
		t.Fatalf("stale result escaped: events=%d submits=%d old=%d new=%d err=%v", events.Load(), submits.Load(), oldCalls.Load(), newCalls.Load(), err)
	}
}

func TestReceiptExpiryRequiresDeadInstance(t *testing.T) {
	services, clock := receiptServices(t)
	ctx := context.Background()
	entered := make(chan struct{})
	var oldCalls, newCalls atomic.Int32
	old := receiptEngineFor(t, services[0], func(ctx context.Context, _ Record) (Record, error) {
		oldCalls.Add(1)
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil)
	services[0].BindEngine(old)
	if _, err := services[0].AcceptIssue(ctx, receiptInput(todoID), auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	<-entered
	original := readReceipt(t, services[0], todoID)
	acquired, _ := time.Parse(time.RFC3339Nano, text(record(original["claim"])["acquiredAt"]))
	next := receiptEngineFor(t, services[1], func(context.Context, Record) (Record, error) {
		newCalls.Add(1)
		return Record{"reply": "交代後"}, nil
	}, nil)
	services[1].BindEngine(next)
	// Even far beyond HANDOFF_MS and the takeover age, a live AI keeps its lock.
	clock.advance(2 * receiptTakeoverAfter)
	if err := next.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if newCalls.Load() != 0 {
		t.Fatal("live instance displaced")
	}
	old.Close()
	// A dead instance is still protected at and just before the strict boundary.
	for _, offset := range []time.Duration{-time.Nanosecond, 0} {
		clock.nanos.Store(acquired.Add(receiptTakeoverAfter + offset).UnixNano())
		if err := next.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		if newCalls.Load() != 0 {
			t.Fatal("claim taken at/before expiry")
		}
	}

	contender := receiptEngineFor(t, services[0], func(context.Context, Record) (Record, error) {
		newCalls.Add(1)
		return Record{"reply": "交代後"}, nil
	}, nil)
	services[0].BindEngine(contender)
	clock.nanos.Store(acquired.Add(receiptTakeoverAfter + time.Nanosecond).UnixNano())
	var wg sync.WaitGroup
	for _, e := range []*Engine{next, contender} {
		wg.Add(1)
		go func(e *Engine) {
			defer wg.Done()
			if err := e.Recover(ctx); err != nil {
				t.Error(err)
			}
		}(e)
	}
	wg.Wait()
	waitReceiptCompleted(t, services[1], todoID, next, contender)
	assertReceiptResult(t, services[1], todoID, "交代後")
	if oldCalls.Load() != 1 || newCalls.Load() != 1 {
		t.Fatal("wrong execution count")
	}
	instance := record(readReceipt(t, services[1], todoID)["claim"])["instanceId"]
	if instance != next.receipts.instance && instance != contender.receipts.instance {
		t.Fatal("claim not replaced")
	}

}

// PostgreSQL row locks make the test wait for the real result-save transaction.
func TestReceiptQuiesceWaitsForResultCommit(t *testing.T) {
	services, _ := receiptServices(t)
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	var events atomic.Int32
	var once sync.Once
	defer once.Do(func() { close(release) })
	e := receiptEngineFor(t, services[0], func(ctx context.Context, _ Record) (Record, error) {
		close(entered)
		select {
		case <-release:
			return Record{"reply": "保存完了"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, func(Record) { events.Add(1) })
	services[0].BindEngine(e)
	if _, err := services[0].AcceptIssue(ctx, receiptInput(todoID), auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	<-entered
	tx, err := services[1].options.Records.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE intake_receipts SET json=json WHERE id=?", todoID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Quiesce(ctx) }()
	<-e.quiesced
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		t.Fatalf("commit not waited for: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := e.Start(ctx, todoID, Record{"receipt": readReceipt(t, services[0], todoID)}, auth.Principal{}); err == nil {
		t.Fatal("new claim during quiesce")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	assertReceiptResult(t, services[1], todoID, "保存完了")
	if events.Load() != 2 {
		t.Fatalf("events=%d", events.Load())
	}
}

func TestLegacyHandedThinkingReceiptUsesClaim(t *testing.T) {
	services, _ := receiptServices(t)
	ctx := context.Background()
	screen := auth.Principal{Kind: auth.Screen}
	if _, err := services[1].AcceptIssue(ctx, receiptInput(todoID), screen); err != nil {
		t.Fatal(err)
	}
	if _, err := services[0].CreateDraft(ctx, receiptInput(todoID), screen, true); err != nil {
		t.Fatal(err)
	}
	// Reproduce the shipped legacy ordering: Chat committed, then handed, while
	// thinking. It has no claim and must still be discovered through draft Start.
	services[0].options.Start = func(_ context.Context, _ string, d Record, _ auth.Principal) (Record, error) { return d, nil }
	if _, err := services[0].Chat(ctx, todoID, Record{"id": todoID, "messageId": todoID, "text": "本文"}, screen); err != nil {
		t.Fatal(err)
	}
	r := readReceipt(t, services[0], todoID)
	r["phase"] = "handed"
	raw, _ := canonicalJSON(r)
	if _, err := services[0].options.Records.DB.Exec("UPDATE intake_receipts SET json=? WHERE id=?", raw, todoID); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	engines := [2]*Engine{}
	for i := range engines {
		engines[i] = receiptEngineFor(t, services[i], func(context.Context, Record) (Record, error) {
			calls.Add(1)
			return Record{"reply": "旧下書きを再開"}, nil
		}, nil)
		services[i].BindEngine(engines[i])
	}
	var wg sync.WaitGroup
	for _, e := range engines {
		wg.Add(1)
		go func(e *Engine) {
			defer wg.Done()
			if err := e.Recover(ctx); err != nil {
				t.Error(err)
			}
		}(e)
	}
	wg.Wait()
	waitReceiptCompleted(t, services[0], todoID, engines[:]...)
	assertReceiptResult(t, services[0], todoID, "旧下書きを再開")
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

// Recover the original send after a temporary failure creating only its draft.
func TestReceiptRecoversAfterDraftStoreFailure(t *testing.T) {
	services, _ := receiptServices(t)
	s, ctx := services[0], t.Context()
	const id = "6568f50f-8908-41db-b459-e4bbc11d0971"
	p := auth.Principal{Kind: auth.Screen}
	receipt, err := s.acceptReceipt(ctx, receiptInput(id), "issue", id, id, p)
	if err != nil {
		t.Fatal(err)
	}
	db := s.options.Records.DB
	if _, err := db.ExecContext(ctx, `CREATE TABLE receipt_failure_fixture (active BOOLEAN NOT NULL);
		INSERT INTO receipt_failure_fixture VALUES (true);
		CREATE FUNCTION fail_receipt_draft() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF EXISTS (SELECT FROM receipt_failure_fixture WHERE active) THEN
				RAISE EXCEPTION 'temporary draft write failure' USING ERRCODE = '40001';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER fail_receipt_draft BEFORE INSERT ON drafts FOR EACH ROW EXECUTE FUNCTION fail_receipt_draft()`); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	engine := receiptEngineFor(t, s, func(_ context.Context, _ Record) (Record, error) {
		calls.Add(1)
		return Record{"reply": "復旧"}, nil
	}, nil)
	s.BindEngine(engine)
	if _, err := engine.accept(ctx, receipt, p); err == nil || problem(err).Status != 503 {
		t.Fatalf("expected a temporary failure: %v", err)
	}
	if phase := readReceipt(t, s, id)["phase"]; phase != "processing" {
		t.Fatalf("receipt phase after failure: %v", phase)
	}
	if _, err := db.ExecContext(ctx, `UPDATE receipt_failure_fixture SET active=false`); err != nil {
		t.Fatal(err)
	}
	// A restored receipt can contain an old escaped Unicode value PostgreSQL
	// cannot extract. It still belongs in the recovery scan.
	if _, err := db.ExecContext(ctx, `UPDATE intake_receipts SET json = $1 || substr(json,2) WHERE id=$2`, `{"legacyNote":"\ud800",`, id); err != nil {
		t.Fatal(err)
	}
	if err := engine.recoverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitReceiptCompleted(t, s, id, engine)
	assertReceiptResult(t, s, id, "復旧")
	if err := engine.recoverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("turn calls = %d, want 1", got)
	}
}
