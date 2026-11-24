package consultations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFinishDuringThinkingPersistsWithoutIssue(t *testing.T) {
	env, service, _, mux, token, _ := familyListFixture(t)
	ctx := context.Background()
	for i, status := range []string{"thinking", "auto_ready"} {
		id := fmt.Sprintf("30000000-0000-4000-8000-%012d", i)
		raw := fmt.Sprintf(`{"id":%q,"project":"owner/app","status":%q,"createdAt":"2026-11-01T02:00:00Z","revision":1,"ready":true,"run":"old","queued":true,"retryAt":"later","messages":[{"role":"user","text":"依頼"}]}`, id, status)
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, id, raw); err != nil {
			t.Fatal(err)
		}
		service.options.Active = func(active string) bool { return active == id }
		req := httptest.NewRequest("POST", "http://localhost:8787/api/drafts/"+id+"/finish", strings.NewReader(`{"instructionAt":"2026-11-01T02:59:00Z"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", status, w.Code, w.Body.String())
		}
		d, found, err := service.options.Records.Get(ctx, "drafts", id)
		if err != nil || !found || d["status"] != "submitted" || d["finishedWithoutIssue"] != true || d["finishInstructionAt"] != "2026-11-01T02:59:00Z" || d["finishedAt"] == nil || d["issue"] != nil || d["run"] != nil || d["queued"] != nil || d["retryAt"] != nil || d["ready"] != false || len(fleet.Items(d["messages"])) != 1 {
			t.Fatalf("%s: %v %v", status, d, err)
		}
	}
}

func TestFinishRejectsFiledAndUnknownOutcome(t *testing.T) {
	env, _, _, mux, token, _ := familyListFixture(t)
	ctx := context.Background()
	for i, tc := range []struct{ status, extra, want string }{{"submitted", `,"issue":{"number":42}`, "42"}, {"submit_unknown", "", "照合"}} {
		id := fmt.Sprintf("40000000-0000-4000-8000-%012d", i)
		raw := fmt.Sprintf(`{"id":%q,"project":"owner/app","status":%q,"createdAt":"2026-11-01T02:00:00Z","revision":1%s}`, id, tc.status, tc.extra)
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, id, raw); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "http://localhost:8787/api/drafts/"+id+"/finish", strings.NewReader(`{"instructionAt":"2026-11-01T02:59:00Z"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 409 || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var after string
		if err := env.DB.QueryRowContext(ctx, `SELECT json FROM drafts WHERE id=$1`, id).Scan(&after); err != nil || after != raw {
			t.Fatalf("changed: %s %v", after, err)
		}
	}
}

func TestFinishSaveFailureAndAuthorization(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	token := stopToken(t, db, false)
	raw := `{"id":"11111111-1111-4111-8111-111111111111","project":"owner/app","status":"thinking","createdAt":"2026-11-01T02:00:00Z","revision":1}`
	if _, err := db.DB().ExecContext(ctx, bindSQL(`INSERT INTO drafts(id,json) VALUES(?,?)`), todoID, raw); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	request := func(tok, at string) int {
		req := httptest.NewRequest("POST", "http://localhost:8787/api/drafts/"+todoID+"/finish", strings.NewReader(`{"instructionAt":"`+at+`"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}
	if code := request(token, "2026-11-01T02:59:00Z"); code != 403 {
		t.Fatalf("nondelegated: %d", code)
	}
	if _, err := db.SetDelegated(ctx, "monitor", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := request(token, "2020-01-01T00:00:00Z"); code != 409 {
		t.Fatalf("instruction: %d", code)
	}
	if _, err := db.DB().ExecContext(ctx, `CREATE FUNCTION reject_finish_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'save failed'; END $$; CREATE TRIGGER reject_finish BEFORE UPDATE ON drafts FOR EACH ROW EXECUTE FUNCTION reject_finish_update()`); err != nil {
		t.Fatal(err)
	}
	if code := request(token, "2026-11-01T02:59:00Z"); code < 400 {
		t.Fatalf("save succeeded: %d", code)
	}
	var after string
	if err := db.DB().QueryRowContext(ctx, bindSQL(`SELECT json FROM drafts WHERE id=?`), todoID).Scan(&after); err != nil || after != raw {
		t.Fatalf("changed: %s %v", after, err)
	}
}

func TestFinishedThinkingIgnoresLateTurn(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	started, release := make(chan struct{}), make(chan struct{})
	var submissions atomic.Int32
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
		close(started)
		<-release
		return Record{"reply": "遅延回答", "ready": true}, nil
	}, Submit: func(context.Context, Record) (Record, error) { submissions.Add(1); return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	s.BindEngine(engine)
	if _, err = s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Chat(ctx, todoID, Record{"messageId": sendID, "text": "依頼"}, p); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err = s.Finish(ctx, todoID, Record{}, p); err != nil {
		close(release)
		engine.Close()
		t.Fatal(err)
	}
	close(release)
	engine.wg.Wait()
	engine.Close()
	d, found, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil || !found || d["status"] != "submitted" || d["finishedWithoutIssue"] != true || submissions.Load() != 0 {
		t.Fatalf("draft=%v submissions=%d err=%v", d, submissions.Load(), err)
	}
}
