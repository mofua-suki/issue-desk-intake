package consultationai

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

func TestFinishedDraftRejectsLateWritesAndRecovery(t *testing.T) {
	for _, status := range []string{"thinking", "auto_ready"} {
		t.Run(status, func(t *testing.T) {
			aiService, _ := testService(t, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			records := aiService.options.Records
			if _, err := records.Change(ctx, "drafts", draftID, func(*sql.Tx, Record) (Record, error) { return nil, nil }); err != nil {
				t.Fatal(err)
			}
			makeService := func(r *consultations.Records) *consultations.Service {
				s, err := consultations.New(consultations.Options{Records: r, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }, Now: time.Now})
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			service := makeService(records)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			var calls, submits, fenced atomic.Int32
			turn := func(_ context.Context, d Record) (Record, error) {
				calls.Add(1)
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				// Use a live context: rejection must come from the durable finish fence,
				// not merely cancellation of the worker's context.
				budget, err := aiService.budget(d, map[string]any{})
				if err != nil {
					return nil, err
				}
				reads, err := budget.reads()
				if err != nil {
					return nil, err
				}
				if err := reads.remember(ctx, Record{"reply": "遅延した途中回答"}); err == nil || !strings.Contains(err.Error(), "実行権") {
					t.Errorf("終了後の途中保存: %v", err)
				}
				fenced.Add(1)
				if _, err := records.SaveProxy(ctx, draftID, fleet.Text(d["run"]), Record{"messages": []any{Record{"role": "proxy", "text": "遅延した代理回答"}}}, []Record{{"id": "late-finish-proxy", "project": "owner/app", "draft": draftID, "at": "fixture", "status": "active"}}); err == nil || !strings.Contains(err.Error(), "実行権") {
					t.Errorf("終了後の代理回答: %v", err)
				}
				fenced.Add(1)
				return Record{"reply": "遅延した最終回答", "ready": true}, nil
			}
			sender := auth.Principal{Kind: auth.PC, TokenID: "sender"}
			engine, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: service, Turn: turn, Principal: func(context.Context, string) (auth.Principal, error) { return sender, nil }, Submit: func(context.Context, Record) (Record, error) { submits.Add(1); return nil, nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			defer releaseOnce.Do(func() { close(release) })
			service.BindEngine(engine)
			if _, err := service.AcceptIssue(ctx, Record{"id": draftID, "project": "owner/app", "text": "保存する原文", "hold": true, "tier": "light"}, sender); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			before, err := records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, d Record) (Record, error) {
				d["status"], d["ready"], d["queued"], d["retryAt"] = status, true, true, "later"
				d["consultationBudget"] = Record{"calls": 2, "weighted": 1200, "unknownCalls": 0}
				d["approval"] = Record{"statement": "この内容で進める", "instructionAt": "2026-11-01T00:00:00Z"}
				fleet.Object(fleet.Elements(d["messages"])[0])["images"] = []any{Record{"id": "saved-image", "name": "添付.png"}}
				return d, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			attachment := []byte("保存する添付のバイト列")
			if _, err := records.DB.ExecContext(ctx, "INSERT INTO images(id,scope,message,json,bytes,bound) VALUES(?,?,?,?,?,1)", "saved-image", draftID, "user-message", `{"name":"添付.png"}`, attachment); err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Format(time.RFC3339Nano)
			finished, err := service.Finish(ctx, draftID, Record{"instructionAt": at}, auth.Principal{Kind: auth.Screen})
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"messages", "consultationBudget", "approval"} {
				if string(marshal(before[key])) != string(marshal(finished[key])) {
					t.Fatalf("終了で%sが変化", key)
				}
			}
			if finished["status"] != "submitted" || finished["finishedWithoutIssue"] != true || finished["finishInstructionAt"] != at || finished["run"] != nil || finished["queued"] != nil || finished["retryAt"] != nil || finished["ready"] != false {
				t.Fatalf("終了記録=%v", finished)
			}
			releaseOnce.Do(func() { close(release) })
			for engine.Busy(draftID) {
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			engine.SaveAutoSubmitFailure(ctx, draftID, before, contract.NewProblem(409, "古い自動起票の失敗", ""))
			engine.Close()
			if fenced.Load() != 2 {
				t.Fatalf("途中保存と代理回答の両経路を通っていない: %d", fenced.Load())
			}
			after, _, err := records.Get(ctx, "drafts", draftID)
			if err != nil || string(marshal(after)) != string(marshal(finished)) {
				t.Fatalf("遅延保存で終了記録が変化: %v %v", after, err)
			}
			var proxies int
			if err := records.DB.QueryRowContext(ctx, "SELECT count(*) FROM proxy_answers WHERE draft=?", draftID).Scan(&proxies); err != nil || proxies != 0 {
				t.Fatalf("代理回答=%d err=%v", proxies, err)
			}
			var schema string
			if err := records.DB.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
				t.Fatal(err)
			}
			if err := records.DB.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.OpenApplicationPostgres(ctx, os.Getenv(pgtest.DSNEnv), schema)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			nextRecords, err := consultations.OpenRecords(ctx, reopened.DB())
			if err != nil {
				t.Fatal(err)
			}
			nextService := makeService(nextRecords)
			next, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: nextService, Turn: func(context.Context, Record) (Record, error) { calls.Add(1); return Record{"ready": true}, nil }, Submit: func(context.Context, Record) (Record, error) { submits.Add(1); return nil, nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			nextService.BindEngine(next)
			if err := next.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if next.Busy(draftID) || calls.Load() != 1 || submits.Load() != 0 {
				t.Fatalf("終了後に再実行: calls=%d submits=%d", calls.Load(), submits.Load())
			}
			var savedAttachment []byte
			if err := nextRecords.DB.QueryRowContext(ctx, "SELECT bytes FROM images WHERE id=? AND scope=?", "saved-image", draftID).Scan(&savedAttachment); err != nil || string(savedAttachment) != string(attachment) {
				t.Fatalf("添付が変化: %q %v", savedAttachment, err)
			}
			final, _, err := nextRecords.Get(ctx, "drafts", draftID)
			if err != nil || string(marshal(final)) != string(marshal(finished)) {
				t.Fatalf("復旧で終了記録が変化: %v %v", final, err)
			}
		})
	}
}
