package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"sync"
	"testing"
	"time"
)

func TestChatExpectedRevision(t *testing.T) {
	s, _, calls := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.PC, TokenID: "one"}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	first := Record{"messageId": sendID, "text": "回答", "expectedRevision": 0}
	if _, err := s.Chat(ctx, todoID, first, p); err != nil {
		t.Fatal(err)
	}
	before, err := s.Draft(ctx, todoID, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Chat(ctx, todoID, first, p); err != nil {
		t.Fatal(err)
	}
	_, err = s.Chat(ctx, todoID, Record{"text": "古い回答", "expectedRevision": json.Number("0.0")}, p)
	if err == nil || problem(err).Status != 409 {
		t.Fatalf("decimal revision: %v", err)
	}
	stale := Record{"text": "古い回答", "expectedRevision": 0, "tier": "heavy"}
	_, err = s.Chat(ctx, todoID, stale, p)
	if problem(err).Status != 409 {
		t.Fatalf("stale: %v", err)
	}
	after, err := s.Draft(ctx, todoID, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet.Items(after["messages"])) != 1 || after["revision"] != before["revision"] || text(record(after["tier"])["level"]) != text(record(before["tier"])["level"]) {
		t.Fatalf("stale changed draft: %+v", after)
	}
	if calls.Load() != 2 {
		t.Fatalf("stale started AI: %d", calls.Load())
	}
	for _, v := range []any{nil, -1, 1.5, "0", int64(9007199254740992)} {
		_, err = s.Chat(ctx, todoID, Record{"text": "x", "expectedRevision": v}, p)
		if err == nil || problem(err).Status != 400 {
			t.Fatalf("invalid %v: %v", v, err)
		}
	}
}

func TestStaleChatDoesNotBindImageOrRecordReply(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chat(ctx, todoID, Record{"messageId": sendID, "text": "先の回答", "expectedRevision": 0}, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
		d["status"] = "draft"
		return d, nil
	}); err != nil {
		t.Fatal(err)
	}
	const imageMessage = "44444444-4444-4444-8444-444444444444"
	meta := `{"id":"` + photoID + `","messageId":"` + imageMessage + `","mime":"image/png","width":1,"height":1}`
	if _, err := db.DB().ExecContext(ctx, bindSQL("INSERT INTO images(id,scope,message,json,bytes) VALUES(?,?,?,?,?)"), photoID, "draft:"+todoID, imageMessage, meta, []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	check := func(bound, replies, messages int) {
		t.Helper()
		var gotBound, gotReplies int
		if err := db.DB().QueryRowContext(ctx, bindSQL("SELECT bound FROM images WHERE id=? AND scope=? AND message=?"), photoID, "draft:"+todoID, imageMessage).Scan(&gotBound); err != nil {
			t.Fatal(err)
		}
		if err := db.DB().QueryRowContext(ctx, bindSQL("SELECT count(*) FROM reply_decisions WHERE draft=?"), todoID).Scan(&gotReplies); err != nil {
			t.Fatal(err)
		}
		draft, err := s.Draft(ctx, todoID, p)
		if err != nil {
			t.Fatal(err)
		}
		if gotBound != bound || gotReplies != replies || len(fleet.Items(draft["messages"])) != messages {
			t.Fatalf("bound=%d replies=%d messages=%d", gotBound, gotReplies, len(fleet.Items(draft["messages"])))
		}
	}
	check(0, 1, 1)
	answer := Record{"messageId": imageMessage, "text": "画像付き回答", "images": []any{photoID}, "expectedRevision": 0}
	if _, err := s.Chat(ctx, todoID, answer, p); err == nil || problem(err).Status != 409 {
		t.Fatalf("stale image answer: %v", err)
	}
	check(0, 1, 1)
	answer["expectedRevision"] = 1
	if _, err := s.Chat(ctx, todoID, answer, p); err != nil {
		t.Fatal(err)
	}
	check(1, 2, 2)
}

func TestChatWhileAnotherReplyIsUncommitted(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.PC, TokenID: "one"}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	tx, err := s.options.Records.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, bindSQL("INSERT INTO reply_decisions(id,project,draft,at,json) VALUES(?,?,?,?,?)"), "other", "owner/app", "other", "now", "{}"); err != nil {
		t.Fatal(err)
	}
	chatCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s.Chat(chatCtx, todoID, Record{"text": "回答", "expectedRevision": 0}, p); err != nil {
		t.Fatalf("chat blocked by an unrelated reply: %v", err)
	}
	draft, err := s.Draft(ctx, todoID, p)
	if err != nil || len(fleet.Items(draft["messages"])) != 1 {
		t.Fatalf("saved conversation: %+v %v", draft, err)
	}
}

func TestConcurrentChatExpectedRevision(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.PC, TokenID: "one"}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, body := range []string{"回答A", "回答B"} {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			_, err := s.Chat(ctx, todoID, Record{"text": body, "expectedRevision": 0}, p)
			results <- err
		}(body)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if problem(err).Status != 409 {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("accepted %d", success)
	}
	d, err := s.Draft(ctx, todoID, p)
	if err != nil || len(fleet.Items(d["messages"])) != 1 {
		t.Fatalf("draft: %+v %v", d, err)
	}
}

func TestAIQuestionUpdateRejectsOldRevision(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Chat(ctx, todoID, Record{"text": "依頼"}, p); err != nil {
		t.Fatal(err)
	}
	before, err := s.Draft(ctx, todoID, p)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		return Record{"questions": []any{"新しい質問"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engine.attempt(ctx, todoID)
	after, err := s.Draft(ctx, todoID, p)
	if err != nil {
		t.Fatal(err)
	}
	if after["revision"] == before["revision"] || len(fleet.Items(after["questions"])) != 1 || len(fleet.Items(after["messages"])) != len(fleet.Items(before["messages"])) {
		t.Fatalf("AI update: %+v", after)
	}
	_, err = s.Chat(ctx, todoID, Record{"text": "古い回答", "expectedRevision": before["revision"]}, p)
	if err == nil || problem(err).Status != 409 {
		t.Fatalf("stale: %v", err)
	}
}

func TestChatAcceptsCurrentAndOmittedRevision(t *testing.T) {
	for _, withRevision := range []bool{true, false} {
		t.Run(fmt.Sprint(withRevision), func(t *testing.T) {
			s, _, _ := fixture(t)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
				t.Fatal(err)
			}
			input := Record{"text": "回答"}
			if withRevision {
				input["expectedRevision"] = 0
			}
			d, err := s.Chat(ctx, todoID, input, p)
			if err != nil || len(fleet.Items(d["messages"])) != 1 {
				t.Fatalf("chat: %+v %v", d, err)
			}
		})
	}
}

// An unrelated writer must not prevent a chat or proxy save from using the
// already initialized history tables. PostgreSQL holds this lock until commit.
func TestConcurrentHistoryWritersDoNotBlockConsultation(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `LOCK TABLE reply_decisions, proxy_answers IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err = s.Chat(bounded, todoID, Record{"text": "回答", "expectedRevision": 0}, p); err != nil {
		t.Fatalf("chat blocked by unrelated writer: %v", err)
	}
	_, err = s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) { next := clone(d); next["run"] = "run"; return next, nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.options.Records.SaveProxy(bounded, todoID, "run", Record{"title": "保存済み"}, []Record{{"id": sendID, "project": "owner/app", "at": "2026-11-01T03:00:00.000Z"}})
	if err != nil {
		t.Fatalf("proxy save blocked by unrelated writer: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	d, err := s.Draft(ctx, todoID, p)
	if err != nil || text(d["title"]) != "保存済み" || len(fleet.Items(d["messages"])) != 1 {
		t.Fatalf("draft=%v err=%v", d, err)
	}
	decisions, err := s.options.Records.ProxyDecisions(ctx, "owner/app")
	if err != nil || len(decisions) != 1 {
		t.Fatalf("decisions=%v err=%v", decisions, err)
	}
}
