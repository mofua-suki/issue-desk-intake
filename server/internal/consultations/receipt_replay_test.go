package consultations

import (
	"context"
	"database/sql"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

func TestReceiptReplayHTTP(t *testing.T) {
	for _, mode := range []string{"cli", "pc"} {
		t.Run(mode, func(t *testing.T) {
			s, db, _ := fixture(t)
			ctx := context.Background()
			var count atomic.Int32
			entered, release := make(chan struct{}, 1), make(chan struct{})
			engine, err := NewEngine(ctx, EngineOptions{Service: s, Principal: func(_ context.Context, id string) (auth.Principal, error) {
				return auth.Principal{Kind: auth.PC, TokenID: strings.TrimPrefix(id, "token:"), Name: "fixture"}, nil
			}, Turn: func(context.Context, Record) (Record, error) {
				count.Add(1)
				entered <- struct{}{}
				<-release
				return Record{"reply": "確認", "ready": false, "questions": []any{}}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			s.BindEngine(engine)
			token := stopToken(t, db, false)
			other := "idk_" + strings.Repeat("b", 43)
			if _, err := db.AddPCToken(ctx, store.PCTokenRecord{ID: "other", Name: "other", Hash: auth.Hash(other), IssuedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			s.Register(mux)
			n := 0
			request := func(method, route, body, principal string) *httptest.ResponseRecorder {
				n++
				req := httptest.NewRequest(method, "http://localhost:8787"+route, strings.NewReader(body))
				req.RemoteAddr = "127.0.0.1:1234"
				req.Header.Set("Origin", "http://localhost:8787")
				req.Header.Set("Content-Type", "application/json")
				if principal == "cli" {
					key := fmt.Sprintf("fixture-%d", n)
					if _, err := db.DB().Exec("INSERT INTO cli_keys(hash,expires) VALUES(?,?)", auth.Hash(key), time.Now().Add(time.Hour).UnixMilli()); err != nil {
						t.Fatal(err)
					}
					req.Header.Set("X-Desk-Key", key)
				} else {
					req.Header.Set("Authorization", "Bearer "+principal)
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				return w
			}
			principal := token
			if mode == "cli" {
				principal = "cli"
			}
			body := fmt.Sprintf(`{"id":%q,"project":"owner/app","text":"本文","hold":true,"tier":"light"}`, todoID)
			first := request("POST", "/api/issues", body, principal)
			if first.Code != 202 {
				t.Fatal(first.Code, first.Body.String())
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("engine did not start")
			}
			view := request("GET", "/api/drafts/"+todoID, "", principal)
			if view.Code != 200 || !strings.Contains(view.Body.String(), `"receiptId":"`+todoID+`"`) || !strings.Contains(view.Body.String(), `"requestId":"`+todoID+`"`) || !strings.Contains(view.Body.String(), `"accepted":true`) {
				t.Fatal(view.Code, view.Body.String())
			}
			replay := request("POST", "/api/issues", body, principal)
			if replay.Code != 202 || !strings.Contains(replay.Body.String(), todoID) {
				t.Fatal(replay.Code, replay.Body.String())
			}
			conflict := request("POST", "/api/issues", strings.Replace(body, "本文", "別の本文", 1), principal)
			if conflict.Code != 409 || !strings.Contains(conflict.Body.String(), `"code":"send_id_conflict"`) {
				t.Fatal(conflict.Code, conflict.Body.String())
			}
			conflict = request("POST", "/api/issues", strings.Replace(body, `"hold":true`, `"hold":false`, 1), principal)
			if conflict.Code != 409 || !strings.Contains(conflict.Body.String(), `"code":"send_id_conflict"`) {
				t.Fatal(conflict.Code, conflict.Body.String())
			}
			if mode == "pc" {
				changed := request("POST", "/api/issues", body, other)
				if changed.Code != 409 || !strings.Contains(changed.Body.String(), `"code":"send_id_conflict"`) {
					t.Fatal(changed.Code, changed.Body.String())
				}
				w := request("GET", "/api/drafts/"+todoID, "", other)
				if w.Code != 404 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			releaseOnce.Do(func() { close(release) })
			deadline := time.Now().Add(5 * time.Second)
			for engine.Busy(todoID) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			again := request("POST", "/api/issues", body, principal)
			if again.Code != 202 {
				t.Fatal(again.Code, again.Body.String())
			}
			view = request("GET", "/api/drafts/"+todoID, "", principal)
			if view.Code != 200 || !strings.Contains(view.Body.String(), `"receiptId":"`+todoID+`"`) || !strings.Contains(view.Body.String(), `"requestId":"`+todoID+`"`) || !strings.Contains(view.Body.String(), `"accepted":true`) {
				t.Fatal(view.Code, view.Body.String())
			}
			d, found, err := s.options.Records.Get(ctx, "drafts", todoID)
			if err != nil || !found {
				t.Fatal(err)
			}
			var receipts, drafts int
			if err := db.DB().QueryRow("SELECT count(*) FROM intake_receipts").Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if err := db.DB().QueryRow("SELECT count(*) FROM drafts").Scan(&drafts); err != nil {
				t.Fatal(err)
			}
			users := 0
			for _, m := range fleet.Items(d["messages"]) {
				if record(m)["role"] == "user" {
					users++
				}
			}
			if receipts != 1 || drafts != 1 || users != 1 || count.Load() != 1 {
				t.Fatalf("receipts=%d drafts=%d users=%d calls=%d", receipts, drafts, users, count.Load())
			}
		})
	}
}

func TestConcurrentReceiptAcrossConnections(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	other, err := store.OpenApplicationPostgres(ctx, env.DSN, env.Schema)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	makeService := func(db *sql.DB) *Service {
		r, err := OpenRecords(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		return &Service{options: Options{Records: r, Now: time.Now, Project: func(string) error { return nil }, Providers: func() []string { return []string{"codex"} }}}
	}
	services := []*Service{makeService(env.DB), makeService(other.DB())}
	p := auth.Principal{Kind: auth.PC, TokenID: "owner", Name: "owner"}
	for _, different := range []bool{false, true} {
		if _, err := env.DB.Exec("DELETE FROM intake_receipts"); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		views := make([]Record, 2)
		errs := make([]error, 2)
		for i := range services {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				body := "本文"
				if different && i == 1 {
					body = "別の本文"
				}
				views[i], errs[i] = services[i].AcceptIssue(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app", "text": body}, p)
			}(i)
		}
		close(start)
		wg.Wait()
		successes := 0
		for i, err := range errs {
			if err == nil {
				successes++
				if views[i]["receiptId"] != todoID || views[i]["accepted"] != true {
					t.Fatalf("view: %v", views[i])
				}
			} else if !different || problem(err).Status != 409 {
				t.Fatalf("error: %v", err)
			}
		}
		if (!different && successes != 2) || (different && successes != 1) {
			t.Fatalf("successes=%d errors=%v", successes, errs)
		}
		var count int
		var raw string
		if err := env.DB.QueryRow("SELECT count(*), min(json) FROM intake_receipts").Scan(&count, &raw); err != nil || count != 1 {
			t.Fatalf("count=%d err=%v", count, err)
		}
		saved, err := decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		if different {
			loser := "本文"
			if text(record(saved["input"])["text"]) == loser {
				loser = "別の本文"
			}
			_, err = services[1].AcceptIssue(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app", "text": loser}, p)
			if err == nil || problem(err).Status != 409 {
				t.Fatalf("replay conflict: %v", err)
			}
			var after string
			if err := env.DB.QueryRow("SELECT json FROM intake_receipts").Scan(&after); err != nil || after != raw {
				t.Fatal("saved receipt changed", err)
			}
		}
		_, err = services[1].AcceptIssue(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app", "text": text(record(saved["input"])["text"])}, auth.Principal{Kind: auth.PC, TokenID: "other"})
		if err == nil || problem(err).Status != 409 {
			t.Fatalf("owner conflict: %v", err)
		}
	}
}

func TestReceiptOwnerPeriodicRecovery(t *testing.T) { testReceiptOwnerPeriodicRecovery(t) }
