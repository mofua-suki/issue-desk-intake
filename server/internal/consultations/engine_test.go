package consultations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

func TestDurableReceiptRecoveryAndSingleExecution(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	screen := auth.Principal{Kind: auth.Screen}
	input := Record{"id": todoID, "project": "owner/app", "tier": "light", "text": "依頼の本文", "hold": true}
	receipt, err := s.acceptReceipt(ctx, input, "issue", todoID, todoID, screen)
	if err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(ctx context.Context, d Record) (Record, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return Record{"reply": "確認しました", "title": "件名", "body": "本文", "ready": true, "questions": []any{}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	s.options.Start = engine.Start
	if err := engine.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("saved send not recovered")
	}
	if _, err := s.AcceptIssue(ctx, input, screen); err != nil {
		t.Fatal(err)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for engine.Busy(todoID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	d, err := s.Draft(ctx, todoID, screen)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || text(d["status"]) != "draft" || d["ready"] != true || len(fleet.Items(d["messages"])) != 2 {
		t.Fatalf("duplicate/lost execution: %d %+v", calls.Load(), d)
	}
	var raw string
	s.options.Records.DB.QueryRow("SELECT json FROM intake_receipts WHERE id=?", receipt["id"]).Scan(&raw)
	r, err := decode(raw)
	if err != nil || r["phase"] != "handed" {
		t.Fatalf("receipt: %s %v", raw, err)
	}
	input["text"] = "別の本文"
	if _, err := s.AcceptIssue(ctx, input, screen); err == nil {
		t.Fatal("conflicting send accepted")
	}
}

func TestProxyContinuationCarriesProgress(t *testing.T) {
	for _, sources := range []Record{{"doc": true}, {}} {
		t.Run(fmt.Sprintf("sources=%d", len(sources)), func(t *testing.T) {
			s, _, _ := fixture(t)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "依頼"}, p); err != nil {
				t.Fatal(err)
			}
			engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(ctx context.Context, d Record) (Record, error) {
				_, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, current Record) (Record, error) {
					current["progress"] = Record{"initialDocuments": sources, "projects": Record{"extra": true}}
					return current, nil
				})
				return Record{"_continue": true, "context": Record{"projects": Record{}, "revision": "new"}, "carriedContext": Record{"revision": "old"}}, err
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			engine.attempt(ctx, todoID)
			d, err := s.Draft(ctx, todoID, p)
			if err != nil {
				t.Fatal(err)
			}
			carried := record(d["carriedContext"])
			got := record(carried["initialDocuments"])
			if d["progress"] != nil || got == nil || len(got) != len(sources) || d["waitSource"] != "proxy" || carried["revision"] != "new" || record(carried["projects"])["extra"] != true {
				t.Fatalf("continuation: %+v", d)
			}
		})
	}
}

// Collection completion belongs to the AI turn; the engine carries its context unchanged.
func TestProxyContinuationPreservesCollectionStateFromTurn(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "依頼"}, p); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		return Record{"_continue": true, "context": Record{"initialDocumentsCollecting": true}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.attempt(ctx, todoID)
	d, err := s.Draft(ctx, todoID, p)
	if err != nil {
		t.Fatal(err)
	}
	if d["waitSource"] != "proxy" || record(d["carriedContext"])["initialDocumentsCollecting"] != true {
		t.Fatalf("continuation context: %+v", d)
	}
}

func TestStoppedLeaderKeepsThinkingForNextLeader(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	started := make(chan struct{})
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(ctx context.Context, d Record) (Record, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.options.Start = engine.Start
	if _, err = s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "内容"}, p); err != nil {
		t.Fatal(err)
	}
	<-started
	engine.Close()
	d, err := s.Draft(ctx, todoID, p)
	if err != nil || d["status"] != "thinking" {
		t.Fatalf("interrupted result lost: %+v %v", d, err)
	}
	next, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		return Record{"reply": "再開", "ready": false, "questions": []any{"確認"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	s.options.Start = next.Start
	if err = next.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for next.Busy(todoID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	d, err = s.Draft(ctx, todoID, p)
	if err != nil || d["status"] != "draft" || len(fleet.Items(d["messages"])) != 2 {
		t.Fatalf("not recovered: %+v %v", d, err)
	}
}
func TestRevokedSenderCannotRecoverReceipt(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.PC, TokenID: "gone"}
	input := Record{"id": todoID, "project": "owner/app", "tier": "light", "text": "本文"}
	if _, err := s.acceptReceipt(ctx, input, "issue", todoID, todoID, p); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Principal: func(context.Context, string) (auth.Principal, error) { return auth.Principal{}, refuse(401, "失効") }, Turn: func(context.Context, Record) (Record, error) { t.Fatal("revoked sender ran"); return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	s.options.Start = engine.Start
	if err = engine.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.options.Records.Get(ctx, "drafts", todoID); found {
		t.Fatal("revoked sender created draft")
	}
}
func TestMessageRevisionOwnershipAndAtomicImages(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.PC, TokenID: "one"}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "内容"}, auth.Principal{Kind: auth.PC, TokenID: "other"}); err == nil {
		t.Fatal("other owner chatted")
	}
	if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "内容", "images": []any{photoID}}, p); err == nil {
		t.Fatal("token attached image")
	}
	d, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "内容"}, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "内容"}, p); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Items(d["messages"])) != 1 {
		t.Fatal("replay appended message")
	}
}
func TestReceiptLinkChecksProviderAndImages(t *testing.T) {
	d := Record{"owner": "token:a", "consultationProvider": "claude", "messages": []any{Record{"id": sendID, "role": "user", "text": "内容", "images": []any{Record{"id": photoID}}}}}
	r := Record{"owner": "token:a", "sendId": sendID, "kind": "issue"}
	input := map[string]any{"text": "内容", "consultationProvider": "codex", "images": []any{photoID}}
	if sent(d, r, input) {
		t.Fatal("wrong provider linked")
	}
	input["consultationProvider"] = "claude"
	if !sent(d, r, input) {
		t.Fatal("matching send not linked")
	}
	input["images"] = []any{}
	if sent(d, r, input) {
		t.Fatal("wrong images linked")
	}
}

func TestAutoSubmitValidationFailureRecovery(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "依頼"}, p); err != nil {
		t.Fatal(err)
	}
	var events []Record
	calls := 0
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		return Record{"reply": "提案", "title": "件名", "body": "本文", "ready": true, "questions": []any{}, "splitIssues": []any{Record{"dependsOn": []any{}}, Record{"dependsOn": []any{0}}}}, nil
	}, Submit: func(_ context.Context, d Record) (Record, error) {
		calls++
		if len(fleet.Items(d["splitIssues"])) != 2 {
			t.Fatal("plan missing")
		}
		return nil, contract.NewProblem(400, "依存先は前の番号だけ指定してください", "")
	}, Publish: func(d Record) { events = append(events, d) }})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.attempt(ctx, todoID)
	d, err := s.Draft(ctx, todoID, p)
	if err != nil {
		t.Fatal(err)
	}
	if d["status"] != "submit_error" || text(d["error"]) == "" || text(record(d["lastFailure"])["cause"]) == "" || d["body"] != "本文" || len(fleet.Items(d["messages"])) != 2 || len(fleet.Items(d["splitIssues"])) != 2 {
		t.Fatalf("failure not preserved: %+v", d)
	}
	if len(events) == 0 {
		t.Fatal("保存後の通知がありません")
	}
	eventRevision, _ := integer(events[len(events)-1]["revision"])
	savedRevision, _ := integer(d["revision"])
	if calls != 1 || eventRevision != savedRevision {
		t.Fatalf("notifications: %+v", events)
	}
	view := s.DraftView(d, p, "")
	actions := fmt.Sprint(view["actions"])
	if !strings.Contains(actions, "chat") || !strings.Contains(actions, "resubmit") {
		t.Fatalf("actions: %v", actions)
	}
	if draftRefusal(d, "resubmit", false) != "" {
		t.Fatal("resubmit refused")
	}
	if _, err = s.Chat(ctx, todoID, Record{"revision": d["revision"], "text": "依存番号を修正してください"}, p); err != nil {
		t.Fatalf("chat: %v", err)
	}
}

func TestAutoSubmitProblemCodeRecovery(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "依頼"}, p); err != nil {
		t.Fatal(err)
	}
	var events []Record
	calls := 0
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		return Record{"reply": "提案", "title": "件名", "body": "本文", "ready": true, "questions": []any{}, "splitIssues": []any{Record{"dependsOn": []any{}}, Record{"dependsOn": []any{0}}}}, nil
	}, Submit: func(_ context.Context, d Record) (Record, error) {
		calls++
		if len(fleet.Items(d["splitIssues"])) != 2 {
			t.Fatal("plan missing")
		}
		return nil, contract.NewProblem(409, "自動起票は無効です", "automatic_filing_disabled")
	}, Publish: func(d Record) { events = append(events, d) }})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.attempt(ctx, todoID)
	d, err := s.Draft(ctx, todoID, p)
	if err != nil {
		t.Fatal(err)
	}
	if d["status"] != "submit_error" || d["error"] != "自動起票は無効です" || d["errorCode"] != "automatic_filing_disabled" || text(record(d["lastFailure"])["cause"]) == "" || d["body"] != "本文" || len(fleet.Items(d["messages"])) != 2 || len(fleet.Items(d["splitIssues"])) != 2 {
		t.Fatalf("failure not preserved: %+v", d)
	}
	if len(events) == 0 {
		t.Fatal("保存後の通知がありません")
	}
	eventRevision, _ := integer(events[len(events)-1]["revision"])
	savedRevision, _ := integer(d["revision"])
	if calls != 1 || eventRevision != savedRevision {
		t.Fatalf("notifications: %+v", events)
	}
	view := s.DraftView(d, p, "")
	actions := fmt.Sprint(view["actions"])
	if !strings.Contains(actions, "chat") || !strings.Contains(actions, "resubmit") {
		t.Fatalf("actions: %v", actions)
	}
	if draftRefusal(d, "resubmit", false) != "" {
		t.Fatal("resubmit refused")
	}
	if _, err = s.Chat(ctx, todoID, Record{"revision": d["revision"], "text": "依存番号を修正してください"}, p); err != nil {
		t.Fatalf("chat: %v", err)
	}
	d, err = s.Draft(ctx, todoID, p)
	if err != nil || d["error"] != nil || d["errorCode"] != nil {
		t.Fatalf("chat後のエラーが残っています: %+v %v", d, err)
	}
}

func TestAutoSubmitTemporaryFailurePreservesReady(t *testing.T) {
	for _, failure := range []error{contract.NewProblem(503, "入れ替え中", "SERVER_DRAINING"), errors.New("Serverを停止中です"), context.Canceled, contract.NewProblem(429, "一時的な制限", ""), contract.NewProblem(408, "タイムアウト", "")} {
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
			var events []Record
			before := 0
			engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
				return Record{"title": "件名", "body": "本文", "ready": true, "questions": []any{}}, nil
			}, Submit: func(context.Context, Record) (Record, error) { before = len(events); return nil, failure }, Publish: func(d Record) { events = append(events, d) }})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			engine.attempt(ctx, todoID)
			d, err := s.Draft(ctx, todoID, p)
			if err != nil || d["status"] != "auto_ready" || len(events) != before {
				t.Fatalf("一時停止後: %+v err=%v events=%d before=%d", d, err, len(events), before)
			}
		})
	}
}

func TestAutoSubmitFailureDoesNotOverwrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(Record)
	}{
		{"submit_error", func(d Record) { d["status"] = "submit_error" }}, {"submit_unknown", func(d Record) { d["status"] = "submit_unknown" }}, {"submitting", func(d Record) { d["status"] = "submitting" }}, {"submitted", func(d Record) { d["status"] = "submitted" }}, {"attempted", func(d Record) { d["attempted"] = true }}, {"issue", func(d Record) { d["issue"] = Record{"number": 1} }}, {"issues", func(d Record) { d["issues"] = []any{Record{"number": 1}} }}, {"preparedJob", func(d Record) { d["preparedJob"] = Record{"scope": "a"} }}, {"children", func(d Record) { d["splitChildren"] = []any{"child"} }}, {"concurrent", func(d Record) { d["body"] = "並行更新" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture(t)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
				t.Fatal(err)
			}
			expected, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) { d["status"] = "auto_ready"; return d, nil })
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) { tc.mutate(d); return d, nil })
			if err != nil {
				t.Fatal(err)
			}
			before, _, _ := s.options.Records.Get(ctx, "drafts", todoID)
			events := 0
			engine, _ := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) { return nil, nil }, Publish: func(Record) { events++ }})
			defer engine.Close()
			engine.SaveAutoSubmitFailure(ctx, todoID, expected, contract.NewProblem(400, "失敗", ""))
			after, _, _ := s.options.Records.Get(ctx, "drafts", todoID)
			a, _ := canonicalJSON(before)
			b, _ := canonicalJSON(after)
			if a != b || events != 0 {
				t.Fatalf("overwritten: %v", after)
			}
		})
	}
}

func TestAutoSubmitFailureSaveErrorReported(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	reports := 0
	events := 0
	engine, _ := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) { return nil, nil }, Report: func(error) { reports++ }, Publish: func(Record) { events++ }})
	defer engine.Close()
	db.DB().Close()
	engine.SaveAutoSubmitFailure(ctx, todoID, Record{"status": "auto_ready"}, contract.NewProblem(400, "失敗", ""))
	if reports != 1 || events != 0 {
		t.Fatalf("reports=%d events=%d", reports, events)
	}
}

func TestAutoSubmitSuccessRemainsSubmitted(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "依頼"}, p); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		return Record{"title": "件名", "body": "本文", "ready": true, "questions": []any{}}, nil
	}, Submit: func(ctx context.Context, _ Record) (Record, error) {
		return s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
			d["status"] = "submitted"
			d["issue"] = Record{"number": 1}
			return d, nil
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.attempt(ctx, todoID)
	d, err := s.Draft(ctx, todoID, p)
	if err != nil || d["status"] != "submitted" || record(d["issue"])["number"] == nil {
		t.Fatalf("success lost: %v %v", d, err)
	}
}

func TestDraftErrorPair(t *testing.T) {
	d := Record{"error": "old", "errorCode": "old_code"}
	SetDraftError(d, "new", "new_code")
	if d["error"] != "new" || d["errorCode"] != "new_code" {
		t.Fatal(d)
	}
	SetDraftError(d, nil, nil)
	if d["error"] != nil || d["errorCode"] != nil {
		t.Fatal(d)
	}
	SetDraftError(d, "failure", nil)
	if d["error"] != "failure" || d["errorCode"] != nil {
		t.Fatal(d)
	}
}

func TestEngineClearsOldCodeOnResponseAndFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			s, _, _ := fixture(t)
			ctx := context.Background()
			_, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) {
				return Record{"id": todoID, "project": "owner/app", "status": "thinking", "queued": true, "revision": 0, "error": "old", "errorCode": "old_code"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			e, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
				if failure {
					return nil, errors.New("AI failed")
				}
				return Record{"reply": "ok", "questions": []any{"確認事項"}}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			e.attempt(ctx, todoID)
			d, _, err := s.options.Records.Get(ctx, "drafts", todoID)
			if err != nil || d["errorCode"] != nil {
				t.Fatal(d, err)
			}
			if failure && (d["status"] != "error" || text(d["error"]) == "") {
				t.Fatal(d)
			}
			if !failure && (d["status"] != "draft" || d["error"] != nil) {
				t.Fatal(d)
			}
		})
	}
}

func TestRetryClearsSavedDraftErrorCode(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	s.options.Start = func(_ context.Context, _ string, d Record, _ auth.Principal) (Record, error) { return d, nil }
	_, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) {
		return Record{"id": todoID, "project": "owner/app", "status": "error", "revision": 0, "error": "old", "errorCode": "old_code", "messages": []any{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Chat(ctx, todoID, Record{"retry": true}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil || d["status"] != "thinking" || d["error"] != nil || d["errorCode"] != nil {
		t.Fatal(d, err)
	}
}

func TestChatRetryQueuesOnlyAnInterruptedPreparedSplit(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		parts        []any
		wantStart    bool
	}{
		{"interrupted", "error", []any{"子A", "子B"}, true},
		{"complete", "draft", []any{"子A", "子B"}, false},
		{"no saved partition", "error", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture(t)
			ids := []any{sendID, photoID}
			starts := 0
			s.options.Start = func(_ context.Context, _ string, d Record, _ auth.Principal) (Record, error) {
				starts++
				if d["status"] != "thinking" || fmt.Sprint(fleet.Items(d["consultationChildren"])) != fmt.Sprint(ids) ||
					fmt.Sprint(fleet.Items(d["consultationParts"])) != fmt.Sprint(tc.parts) {
					t.Fatalf("保存した子相談を起動処理へ引き継げません: %v", d)
				}
				return d, nil
			}
			if _, err := s.options.Records.Change(t.Context(), "drafts", todoID, func(*sql.Tx, Record) (Record, error) {
				return Record{"id": todoID, "project": "owner/app", "status": tc.status, "revision": 0,
					"consultationChildren": ids, "consultationParts": tc.parts}, nil
			}); err != nil {
				t.Fatal(err)
			}
			p := auth.Principal{Kind: auth.Screen}
			if _, err := s.Chat(t.Context(), todoID, Record{"text": "本文を変更する"}, p); err == nil || problem(err).Status != 409 {
				t.Fatalf("分割確定済みの親が変更を受理しました: %v", err)
			}
			_, err := s.Chat(t.Context(), todoID, Record{"retry": true}, p)
			if tc.wantStart {
				if err != nil || starts != 1 {
					t.Fatalf("中断した親を再試行できません: starts=%d err=%v", starts, err)
				}
			} else if err == nil || problem(err).Status != 409 || starts != 0 {
				t.Fatalf("再試行対象外の親を開始しました: starts=%d err=%v", starts, err)
			}
		})
	}
}

func TestProxyContinuationWithoutInitialDocumentsPreservesContext(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := t.Context()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "revision": 0, "text": "依頼"}, p); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		return Record{"_continue": true, "context": Record{"revision": "new"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.attempt(ctx, todoID)
	d, err := s.Draft(ctx, todoID, p)
	if err != nil {
		t.Fatal(err)
	}
	carried := record(d["carriedContext"])
	if carried["revision"] != "new" || carried["initialDocuments"] != nil || d["progress"] != nil || d["waitSource"] != "proxy" {
		t.Fatalf("continuation without sources: %+v", d)
	}
}

func TestAutoSubmitFailureSavesSplitParent(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	expected, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
		d["status"] = "auto_ready"
		d["splitChildren"] = []any{"child"}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.SaveAutoSubmitFailure(ctx, todoID, expected, contract.NewProblem(409, "計画が変わりました", ""))
	got, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	failure := record(got["lastFailure"])
	if err != nil || got["status"] != "submit_error" || got["error"] != "計画が変わりました" || failure["stage"] != "自動の起票" || failure["cause"] != "計画が変わりました" {
		t.Fatalf("%v %v", got, err)
	}
}

// Separate-connection receipt races share their fixture with replay tests.
func TestTwoEngineReceiptDiscovery(t *testing.T) { testTwoEngineReceiptDiscovery(t) }
