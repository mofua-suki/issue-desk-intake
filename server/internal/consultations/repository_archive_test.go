package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/store"
)

func TestRepositoryArchiveLookup(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name              string
		lookup            func(context.Context, string) (bool, error)
		checked, rejected bool
	}{
		{"archived", func(_ context.Context, repo string) (bool, error) {
			if repo != "owner/app" {
				t.Errorf("repo: %s", repo)
			}
			return true, nil
		}, true, true},
		{"active", func(context.Context, string) (bool, error) { return false, nil }, true, false},
		{"error", func(context.Context, string) (bool, error) { return true, errors.New("unavailable") }, false, false},
		{"nil", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{options: Options{RepositoryArchived: tc.lookup}}
			checked, err := s.CheckRepositoryArchive(ctx, "owner/app")
			if checked != tc.checked {
				t.Fatalf("checked = %v", checked)
			}
			if !tc.rejected {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var p *contract.Problem
			if !errors.As(err, &p) || p.Status != 409 || p.Code != "repository_archived" || p.Message != "起票先 owner/app は保管済みで新しいIssueを受け付けません。同じ内容での再起票では解消しません。登録済みの別プロジェクトを選択するか、この相談を終了してください。" {
				t.Fatalf("problem: %#v", err)
			}
		})
	}
}

func TestDraftArchiveHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		lookup        func(context.Context, string) (bool, error)
		status, calls int
		code          string
	}{
		{"archived", `{"tier":"light","id":"` + todoID + `","project":"owner/app"}`, func(context.Context, string) (bool, error) { return true, nil }, 409, 1, "repository_archived"},
		{"active", `{"tier":"light","id":"` + todoID + `","project":"owner/app"}`, func(context.Context, string) (bool, error) { return false, nil }, 201, 1, ""},
		{"unavailable", `{"tier":"light","id":"` + todoID + `","project":"owner/app"}`, func(context.Context, string) (bool, error) { return false, errors.New("unavailable") }, 201, 1, ""},
		{"nil", `{"tier":"light","id":"` + todoID + `","project":"owner/app"}`, nil, 201, 0, ""},
		{"unassigned", `{"tier":"light","id":"` + todoID + `","unassigned":true}`, func(context.Context, string) (bool, error) { return true, nil }, 201, 0, ""},
		{"invalid unassigned", `{"tier":"light","unassigned":true,"project":"owner/app"}`, func(context.Context, string) (bool, error) { return true, nil }, 400, 0, ""},
		{"invalid flag", `{"tier":"light","unassigned":"yes","project":"owner/app"}`, func(context.Context, string) (bool, error) { return true, nil }, 400, 0, ""},
		{"unauthorized project", `{"tier":"light","project":"other/app"}`, func(context.Context, string) (bool, error) { return true, nil }, 400, 0, ""},
		{"missing project", `{"tier":"light"}`, func(context.Context, string) (bool, error) { return true, nil }, 400, 0, ""},
		{"invalid id", `{"tier":"light","id":"bad","project":"owner/app"}`, func(context.Context, string) (bool, error) { return true, nil }, 400, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db, ai := fixture(t)
			calls := 0
			if tc.lookup != nil {
				s.options.RepositoryArchived = func(ctx context.Context, repo string) (bool, error) {
					calls++
					if repo != "owner/app" {
						t.Fatalf("unexpected repository %q", repo)
					}
					return tc.lookup(ctx, repo)
				}
			}
			mux := http.NewServeMux()
			s.Register(mux)
			request := func(n int) *httptest.ResponseRecorder {
				key := fmt.Sprintf("archive-test-%d", n)
				if _, err := db.DB().Exec("INSERT INTO cli_keys(hash,expires) VALUES(?,?)", auth.Hash(key), time.Now().Add(time.Hour).UnixMilli()); err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest("POST", "http://localhost:8787/api/drafts", strings.NewReader(tc.body))
				req.RemoteAddr = "127.0.0.1:1234"
				req.Header.Set("Origin", "http://localhost:8787")
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Desk-Key", key)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				return w
			}
			w := request(1)
			if w.Code != tc.status || calls != tc.calls {
				t.Fatalf("status=%d body=%s calls=%d", w.Code, w.Body.String(), calls)
			}
			if tc.code != "" {
				var p contract.Problem
				if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Code != tc.code {
					t.Fatalf("problem=%s err=%v", w.Body.String(), err)
				}
			}
			_, found, err := s.options.Records.Get(context.Background(), "drafts", todoID)
			if err != nil || found != (tc.status == 201) {
				t.Fatalf("saved=%v err=%v", found, err)
			}
			if ai.Load() != 0 {
				t.Fatalf("AI called %d times", ai.Load())
			}
			if tc.name == "active" {
				s.options.RepositoryArchived = func(context.Context, string) (bool, error) { return true, nil }
				replay := request(2)
				if replay.Code != 409 {
					t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
				}
				var p contract.Problem
				if err := json.Unmarshal(replay.Body.Bytes(), &p); err != nil || p.Code != "repository_archived" {
					t.Fatalf("replay problem=%s err=%v", replay.Body.String(), err)
				}
			}
		})
	}
}

func TestRepositoryArchiveWiringAndEntrypoints(t *testing.T) {
	s, _, _ := fixture(t)
	calls := 0
	s.options.RepositoryArchived = func(context.Context, string) (bool, error) { calls++; return false, nil }
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app", "hold": true}, p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Draft(ctx, todoID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptIssue(ctx, receiptInput(sendID), p); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("existing draft and issue paths performed %d archive lookups", calls)
	}
	const id = "44444444-4444-4444-8444-444444444444"
	if _, err := s.CreateTodo(ctx, Record{"id": id, "project": "owner/app"}, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveTodo(ctx, id, Record{"revision": 0, "text": "相談"}, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartTodo(ctx, id, Record{"tier": "light", "revision": 1, "id": "55555555-5555-4555-8555-555555555555"}, p); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("archive lookups: %d", calls)
	}
}

func TestTodoArchiveHTTP(t *testing.T) {
	for _, tc := range []struct {
		name          string
		lookup        bool
		archived      bool
		failure       bool
		body          string
		request       string
		status, calls int
	}{
		{"archived", true, true, false, "相談", `{"tier":"light","revision":1}`, 409, 1},
		{"active", true, false, false, "相談", `{"tier":"light","revision":1}`, 202, 1},
		{"unavailable", true, false, true, "相談", `{"tier":"light","revision":1}`, 202, 1},
		{"unset", false, false, false, "相談", `{"tier":"light","revision":1}`, 202, 0},
		{"empty", true, true, false, "", `{"tier":"light"}`, 400, 0},
		{"revision", true, true, false, "相談", `{"tier":"light","revision":0}`, 409, 0},
		{"send id", true, true, false, "相談", `{"tier":"light","id":"bad"}`, 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db, ai := fixture(t)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			if _, err := s.CreateTodo(ctx, Record{"id": todoID, "project": "owner/app"}, p); err != nil {
				t.Fatal(err)
			}
			if tc.body != "" {
				meta := `{"id":"` + photoID + `","messageId":"before","mime":"image/png","width":1,"height":1}`
				if _, err := db.DB().ExecContext(ctx, "INSERT INTO images(id,scope,message,json,bytes) VALUES(?,?,?,?,?)", photoID, "todo:"+todoID, "before", meta, []byte{1, 2}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.SaveTodo(ctx, todoID, Record{"revision": 0, "text": tc.body, "images": []string{photoID}}, p); err != nil {
					t.Fatal(err)
				}
			}
			before, _, err := s.options.Records.Get(ctx, "todos", todoID)
			if err != nil {
				t.Fatal(err)
			}
			beforeJSON, _ := json.Marshal(before)
			var imageJSON string
			var imageBound int
			if tc.body != "" {
				if err := db.DB().QueryRowContext(ctx, "SELECT json,bound FROM images WHERE id=?", photoID).Scan(&imageJSON, &imageBound); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			if tc.lookup {
				s.options.RepositoryArchived = func(context.Context, string) (bool, error) {
					calls++
					if tc.failure {
						return false, errors.New("unavailable")
					}
					return tc.archived, nil
				}
			}
			key := "todo-archive-key"
			if _, err := db.DB().Exec("INSERT INTO cli_keys(hash,expires) VALUES(?,?)", auth.Hash(key), time.Now().Add(time.Hour).UnixMilli()); err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			s.Register(mux)
			req := httptest.NewRequest("POST", "http://localhost:8787/api/todos/"+todoID+"/start", strings.NewReader(tc.request))
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("Origin", "http://localhost:8787")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Desk-Key", key)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != tc.status || calls != tc.calls {
				t.Fatalf("status=%d body=%s calls=%d", w.Code, w.Body.String(), calls)
			}
			draft, found, err := s.options.Records.Get(ctx, "drafts", todoID)
			if err != nil || found != (tc.status == 202) {
				t.Fatalf("draft=%v found=%v err=%v", draft, found, err)
			}
			if tc.body != "" {
				var scope, message, metadata string
				var bound int
				var contents []byte
				if err := db.DB().QueryRowContext(ctx, "SELECT scope,message,bytes,json,bound FROM images WHERE id=?", photoID).Scan(&scope, &message, &contents, &metadata, &bound); err != nil {
					t.Fatal(err)
				}
				expectedScope, expectedMessage := "todo:"+todoID, "before"
				if tc.status == 202 {
					expectedScope, expectedMessage = "draft:"+todoID, todoID
				}
				if tc.status != 202 && (metadata != imageJSON || bound != imageBound) {
					t.Fatalf("image metadata or bound changed: %s %d", metadata, bound)
				}
				if tc.status == 202 && !strings.Contains(w.Body.String(), photoID) {
					t.Fatalf("image missing from response: %s", w.Body.String())
				}
				if scope != expectedScope || message != expectedMessage || string(contents) != string([]byte{1, 2}) {
					t.Fatalf("image scope=%s message=%s bytes=%v", scope, message, contents)
				}
			}
			if tc.status == 202 {
				if ai.Load() != 1 || !strings.Contains(w.Body.String(), tc.body) {
					t.Fatalf("start=%d body=%s", ai.Load(), w.Body.String())
				}
				return
			}
			after, found, err := s.options.Records.Get(ctx, "todos", todoID)
			afterJSON, _ := json.Marshal(after)
			if err != nil || !found || string(beforeJSON) != string(afterJSON) || ai.Load() != 0 {
				t.Fatalf("todo changed: %s %s ai=%d err=%v", beforeJSON, afterJSON, ai.Load(), err)
			}
			if tc.name == "archived" {
				var got contract.Problem
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				_, expected := s.CheckRepositoryArchive(ctx, "owner/app")
				var want *contract.Problem
				if !errors.As(expected, &want) || got.Code != want.Code || got.Message != want.Message || w.Code != want.Status {
					t.Fatalf("problem=%+v expected=%+v", got, want)
				}
			}
		})
	}
}

func TestTodoArchiveConcurrentUpdate(t *testing.T) {
	s, _, ai := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateTodo(ctx, Record{"id": todoID, "project": "owner/app"}, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveTodo(ctx, todoID, Record{"revision": 0, "text": "old"}, p); err != nil {
		t.Fatal(err)
	}
	s.options.RepositoryArchived = func(ctx context.Context, repo string) (bool, error) {
		_, err := s.SaveTodo(ctx, todoID, Record{"revision": 1, "text": "new"}, p)
		return false, err
	}
	_, err := s.StartTodo(ctx, todoID, Record{"tier": "light"}, p)
	var problem *contract.Problem
	if !errors.As(err, &problem) || problem.Status != 409 || ai.Load() != 0 {
		t.Fatalf("err=%v ai=%d", err, ai.Load())
	}
	todo, found, err := s.options.Records.Get(ctx, "todos", todoID)
	if err != nil || !found || text(todo["body"]) != "new" {
		t.Fatalf("todo=%v found=%v err=%v", todo, found, err)
	}
}

// archiveStartRequest goes through the same authentication and route as a client.
func archiveStartRequest(t *testing.T, s *Service, db *store.SQLite, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://localhost:8787/api/todos/"+todoID+"/start", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Origin", "http://localhost:8787")
	req.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(token, "idk_") {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		if _, err := db.DB().Exec("INSERT INTO cli_keys(hash,expires) VALUES(?,?)", auth.Hash(token), time.Now().Add(time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Desk-Key", token)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestTodoArchiveExistingRejectionsHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, body, project      string
		status                   int
		missing, offline, denied bool
	}{
		{"missing", `{"tier":"light"}`, "owner/app", 404, true, false, false},
		{"permission", `{"tier":"light"}`, "owner/app", 403, false, false, true},
		{"offline", `{"tier":"light"}`, "owner/app", 503, false, true, false},
		{"empty project", `{"tier":"light"}`, "", 400, false, false, false},
		{"invalid project", `{"tier":"light"}`, "other/app", 400, false, false, false},
		{"provider", `{"tier":"light","provider":"invalid"}`, "owner/app", 400, false, false, false},
		{"consultation provider", `{"tier":"light","consultationProvider":"invalid"}`, "owner/app", 400, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db, ai := fixture(t)
			ctx := context.Background()
			p := auth.Principal{Kind: auth.Screen}
			if !tc.missing {
				if _, err := s.CreateTodo(ctx, Record{"id": todoID, "project": "owner/app"}, p); err != nil {
					t.Fatal(err)
				}
				if _, err := s.SaveTodo(ctx, todoID, Record{"revision": 0, "text": "相談"}, p); err != nil {
					t.Fatal(err)
				}
				if tc.project != "owner/app" {
					_, err := s.options.Records.Change(ctx, "todos", todoID, func(_ *sql.Tx, r Record) (Record, error) { r["project"] = tc.project; return r, nil })
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.offline {
				s.options.Start = nil
			}
			calls := 0
			s.options.RepositoryArchived = func(context.Context, string) (bool, error) { calls++; return true, nil }
			token := "reject-key"
			if tc.denied {
				token = "idk_" + strings.Repeat("b", 43)
				if _, err := db.AddPCToken(ctx, store.PCTokenRecord{ID: "plain", Name: "plain", Hash: auth.Hash(token), IssuedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			w := archiveStartRequest(t, s, db, tc.body, token)
			if w.Code != tc.status || calls != 0 || ai.Load() != 0 {
				t.Fatalf("status=%d body=%s calls=%d ai=%d", w.Code, w.Body.String(), calls, ai.Load())
			}
			_, found, err := s.options.Records.Get(ctx, "drafts", todoID)
			if err != nil || found {
				t.Fatalf("draft found=%v err=%v", found, err)
			}
		})
	}
}

func TestTodoArchiveReplayHTTP(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateTodo(ctx, Record{"id": todoID, "project": "owner/app"}, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveTodo(ctx, todoID, Record{"revision": 0, "text": "相談"}, p); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.options.RepositoryArchived = func(context.Context, string) (bool, error) { calls++; return false, nil }
	first := archiveStartRequest(t, s, db, `{"tier":"light","id":"`+sendID+`"}`, "replay-first")
	if first.Code != 202 || calls != 1 {
		t.Fatalf("first=%d %s calls=%d", first.Code, first.Body.String(), calls)
	}
	before, found, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil || !found {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(before)
	calls = 0
	s.options.RepositoryArchived = func(context.Context, string) (bool, error) { calls++; return true, nil }
	second := archiveStartRequest(t, s, db, `{"tier":"light","id":"`+sendID+`"}`, "replay-second")
	if second.Code != 202 || calls != 0 {
		t.Fatalf("replay=%d %s calls=%d", second.Code, second.Body.String(), calls)
	}
	after, found, err := s.options.Records.Get(ctx, "drafts", todoID)
	raw, _ := json.Marshal(after)
	if err != nil || !found || string(encoded) != string(raw) || len(after["messages"].([]any)) != 1 {
		t.Fatalf("before=%s after=%s err=%v", encoded, raw, err)
	}
	s.options.Project = func(string) error { return refuse(400, "project denied") }
	denied := archiveStartRequest(t, s, db, `{"tier":"light"}`, "replay-denied")
	if denied.Code != 400 || calls != 0 {
		t.Fatalf("project status=%d calls=%d", denied.Code, calls)
	}
	if _, err := s.StartTodo(ctx, todoID, Record{"tier": "light"}, auth.Principal{Kind: auth.PC, TokenID: "other"}); err == nil {
		t.Fatal("other owner accepted")
	}
}

func TestTodoArchiveOutsideTransactionAndProjectChange(t *testing.T) {
	s, db, ai := fixture(t)
	ctx := context.Background()
	p := auth.Principal{Kind: auth.Screen}
	s.options.Project = func(project string) error {
		if project == "owner/app" || project == "owner/other" {
			return nil
		}
		return refuse(400, "invalid project")
	}
	if _, err := s.CreateTodo(ctx, Record{"id": todoID, "project": "owner/app"}, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveTodo(ctx, todoID, Record{"revision": 0, "text": "old"}, p); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan string, 1), make(chan struct{})
	s.options.RepositoryArchived = func(ctx context.Context, repo string) (bool, error) {
		inUse := db.DB().Stats().InUse
		entered <- fmt.Sprintf("%s:%d", repo, inUse)
		<-release
		return false, nil
	}
	if _, err := db.DB().Exec("INSERT INTO cli_keys(hash,expires) VALUES(?,?)", auth.Hash("concurrent-key"), time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "http://localhost:8787/api/todos/"+todoID+"/start", strings.NewReader(`{"tier":"light"}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Origin", "http://localhost:8787")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Desk-Key", "concurrent-key")
	mux := http.NewServeMux()
	s.Register(mux)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { w := httptest.NewRecorder(); mux.ServeHTTP(w, req); done <- w }()
	var repo string
	select {
	case repo = <-entered:
	case w := <-done:
		t.Fatalf("lookup was not reached: status=%d body=%s", w.Code, w.Body.String())
	}
	if repo != "owner/app:0" {
		t.Errorf("repository and open connections=%s", repo)
	}
	_, err := s.options.Records.Change(ctx, "todos", todoID, func(_ *sql.Tx, r Record) (Record, error) {
		r["project"] = "owner/other"
		r["body"] = "new"
		return r, nil
	})
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	w := <-done
	if w.Code != 409 || ai.Load() != 0 {
		t.Fatalf("status=%d body=%s ai=%d", w.Code, w.Body.String(), ai.Load())
	}
	todo, found, err := s.options.Records.Get(ctx, "todos", todoID)
	if err != nil || !found || text(todo["project"]) != "owner/other" || text(todo["body"]) != "new" {
		t.Fatalf("todo=%v found=%v err=%v", todo, found, err)
	}
	_, found, err = s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil || found {
		t.Fatalf("draft=%v err=%v", found, err)
	}
}
