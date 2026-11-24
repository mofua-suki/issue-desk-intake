package consultationai

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

// Exercise the AI service's actual checkpoint writer and Records.SaveProxy in
// the window after takeover commits but before the successor saves its run.
func TestReceiptTakeoverFencesAICheckpointsBeforeNewRun(t *testing.T) {
	s, _ := testService(t, nil)
	ctx := context.Background()
	records := s.options.Records
	if _, err := records.Change(ctx, "drafts", draftID, func(*sql.Tx, Record) (Record, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := records.DB.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	other, err := store.OpenApplicationPostgres(ctx, os.Getenv(pgtest.DSNEnv), schema)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	second, err := consultations.OpenRecords(ctx, other.DB())
	if err != nil {
		t.Fatal(err)
	}
	var nanos atomic.Int64
	nanos.Store(time.Now().UnixNano())
	sender := auth.Principal{Kind: auth.PC, TokenID: "sender"}
	makeService := func(r *consultations.Records) *consultations.Service {
		v, err := consultations.New(consultations.Options{Records: r, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }, Now: func() time.Time { return time.Unix(0, nanos.Load()).UTC() }})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	firstService, nextService := makeService(records), makeService(second)
	entered, saveOld, savedOld := make(chan struct{}), make(chan struct{}), make(chan struct{})
	claimed, resume, successorEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOld, releaseNext, claimedOnce sync.Once
	var calls, events, submits atomic.Int32
	old, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: firstService, Principal: func(context.Context, string) (auth.Principal, error) { return sender, nil }, Publish: func(Record) { events.Add(1) }, Submit: func(context.Context, Record) (Record, error) { submits.Add(1); return nil, nil }, Turn: func(turnCtx context.Context, d Record) (Record, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-saveOld:
		case <-turnCtx.Done():
			return nil, turnCtx.Err()
		}
		budget, err := s.budget(d, map[string]any{})
		if err != nil {
			return nil, err
		}
		reads, err := budget.reads()
		if err != nil {
			return nil, err
		}
		// remember produces the shipped consultationBudget.contextRead.lastReply
		// checkpoint and reaches Service.save through callBudget.persist.
		if err := reads.remember(turnCtx, Record{"reply": "古い途中回答"}); err == nil {
			t.Error("old AI checkpoint accepted")
		}
		_, err = records.SaveProxy(turnCtx, draftID, fleet.Text(d["run"]), Record{"messages": []any{Record{"role": "proxy", "text": "古い代理回答"}}}, []Record{{"id": "stale-proxy", "project": "owner/app", "draft": draftID, "at": "fixture", "status": "active"}})
		if err == nil {
			t.Error("old proxy checkpoint accepted")
		}
		close(savedOld)
		return Record{"reply": "古い最終回答", "ready": true}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	defer releaseOld.Do(func() { close(saveOld) })
	firstService.BindEngine(old)
	if _, err := firstService.AcceptIssue(ctx, Record{"tier": "light", "id": draftID, "project": "owner/app", "text": "依頼", "hold": true}, sender); err != nil {
		t.Fatal(err)
	}
	<-entered
	var raw string
	if err := second.DB.QueryRowContext(ctx, "SELECT json FROM intake_receipts WHERE id=?", draftID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var receipt Record
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		t.Fatal(err)
	}
	instance := fleet.Text(fleet.Object(receipt["claim"])["instanceId"])
	next, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: nextService, Principal: func(context.Context, string) (auth.Principal, error) {
		claimedOnce.Do(func() { close(claimed) })
		<-resume
		return sender, nil
	}, Publish: func(Record) { events.Add(1) }, Turn: func(_ context.Context, d Record) (Record, error) {
		calls.Add(1)
		close(successorEntered)
		if fleet.Object(fleet.Object(d["consultationBudget"])["contextRead"])["lastReply"] != nil || len(fleet.Elements(d["messages"])) != 1 {
			t.Error("successor consumed stale checkpoint")
		}
		return Record{"reply": "新しい回答"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	defer releaseNext.Do(func() { close(resume) })
	nextService.BindEngine(next)
	// An initial scan can finish while the prior owner is still live. A later
	// scan may then acquire the claim independently of that completed call.
	if err := next.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var killed bool
	// Terminate only the liveness backend holding this exact instance's lock.
	if err := second.DB.QueryRowContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND granted AND classid::bigint=(hashtext(current_schema() || ':consultation-engine')::bigint & 4294967295) AND objid::bigint=(hashtext(?)::bigint & 4294967295)`, instance).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate=%v err=%v", killed, err)
	}
	nanos.Add(int64(91 * time.Second))
	recovered := make(chan error, 1)
	go func() { recovered <- next.Recover(ctx) }()
	<-claimed
	before, _, err := second.Get(ctx, "drafts", draftID)
	if err != nil || before["run"] != nil {
		t.Fatalf("old run survived claim: %v %v", before, err)
	}
	releaseOld.Do(func() { close(saveOld) })
	<-savedOld
	deadline := time.Now().Add(5 * time.Second)
	for old.Busy(draftID) {
		if time.Now().After(deadline) {
			t.Fatal("old execution not done")
		}
		time.Sleep(time.Millisecond)
	}
	old.Close()
	after, _, err := second.Get(ctx, "drafts", draftID)
	if err != nil || string(marshal(before)) != string(marshal(after)) {
		t.Fatalf("stale draft changed: %v", err)
	}
	var proxies int
	var exists bool
	if err := second.DB.QueryRowContext(ctx, "SELECT to_regclass(current_schema() || '.proxy_answers') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		if err := second.DB.QueryRowContext(ctx, "SELECT count(*) FROM proxy_answers WHERE draft=?", draftID).Scan(&proxies); err != nil {
			t.Fatal(err)
		}
	}
	if proxies != 0 || events.Load() != 1 || submits.Load() != 0 {
		t.Fatalf("stale effects: proxies=%d events=%d submits=%d", proxies, events.Load(), submits.Load())
	}
	releaseNext.Do(func() { close(resume) })
	if err := <-recovered; err != nil {
		t.Fatal(err)
	}
	// Recover may have returned before PostgreSQL releases the terminated
	// backend's lock. In that case the periodic scan owns the blocked claim;
	// wait for its actual turn before interpreting Busy=false as completion.
	select {
	case <-successorEntered:
	case <-time.After(25 * time.Second): // Two production recovery intervals plus scheduling allowance.
		t.Fatal("successor did not start")
	}
	deadline = time.Now().Add(5 * time.Second)
	for next.Busy(draftID) {
		if time.Now().After(deadline) {
			t.Fatal("successor not done")
		}
		time.Sleep(time.Millisecond)
	}
	final, _, err := second.Get(ctx, "drafts", draftID)
	messages := fleet.Elements(final["messages"])
	if err != nil || len(messages) != 2 || fleet.Object(messages[1])["text"] != "新しい回答" || calls.Load() != 2 || events.Load() != 3 {
		t.Fatalf("final=%v calls=%d events=%d err=%v", final, calls.Load(), events.Load(), err)
	}
}
