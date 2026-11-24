package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/store"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSetDraftPriorityRejectsAdoptedProposal(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	for _, status := range []string{"auto_ready", "draft", "submit_error"} {
		t.Run(status, func(t *testing.T) {
			id := todoID
			draft := Record{"id": id, "project": "owner/app", "owner": "", "status": status, "proposal": Record{"key": "owner/app#42:proposal-1", "fingerprint": "fingerprint"}, "priority": "high", "prioritySource": Record{"via": "proposal"}, "revision": 0}
			if _, err := s.options.Records.Change(ctx, "drafts", id, func(*sql.Tx, Record) (Record, error) { return draft, nil }); err != nil {
				t.Fatal(err)
			}
			before, found, err := s.options.Records.Get(ctx, "drafts", id)
			if err != nil || !found {
				t.Fatalf("read before: found=%v err=%v", found, err)
			}
			for _, priority := range []string{"normal", "auto", "high"} {
				_, err := s.SetDraftPriority(ctx, id, Record{"priority": priority}, auth.Principal{Kind: auth.Screen})
				var problem *contract.Problem
				if !errors.As(err, &problem) || problem.Status != http.StatusConflict {
					t.Fatalf("priority %s: expected 409, got %v", priority, err)
				}
				after, found, err := s.options.Records.Get(ctx, "drafts", id)
				if err != nil || !found || !reflect.DeepEqual(before, after) {
					t.Fatalf("priority %s: draft changed: before=%v after=%v found=%v err=%v", priority, before, after, found, err)
				}
			}
		})
	}
}

func TestDraftPriorityNotification(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	id := todoID
	initial := Record{"id": id, "project": "owner/app", "owner": "token:owner", "status": "draft", "revision": 3, "body": "private"}
	if _, err := s.options.Records.Change(ctx, "drafts", id, func(*sql.Tx, Record) (Record, error) { return initial, nil }); err != nil {
		t.Fatal(err)
	}
	var notices []Record
	s.options.PublishDraft = func(event Record) {
		notices = append(notices, event)
		stored, found, err := s.options.Records.Get(ctx, "drafts", id)
		if err != nil || !found || stored["priority"] != "high" {
			t.Fatalf("通知時の保存値: %v found=%v err=%v", stored, found, err)
		}
	}
	screen := auth.Principal{Kind: auth.Screen}
	view, status, err := s.postDraftAction(ctx, Record{"priority": "high"}, screen, "", id, "priority")
	if err != nil || status != http.StatusOK {
		t.Fatalf("変更応答: view=%v status=%d err=%v", view, status, err)
	}
	revision, valid := integer(view.(Record)["revision"])
	if view.(Record)["priority"] != "high" || !valid || revision != 3 {
		t.Fatalf("変更応答の内容: %v", view)
	}
	if !reflect.DeepEqual(notices, []Record{{"type": "draft", "id": id}}) {
		t.Fatalf("通知: %v", notices)
	}
	notices = nil
	view, status, err = s.postDraftAction(ctx, Record{"priority": "high"}, screen, "", id, "priority")
	if err != nil || status != http.StatusOK || view.(Record)["priority"] != "high" || len(notices) != 0 {
		t.Fatalf("未変更: view=%v status=%d err=%v notices=%v", view, status, err, notices)
	}
	_, _, err = s.postDraftAction(ctx, Record{"priority": "normal"}, auth.Principal{Kind: auth.PC, TokenID: "other"}, "", id, "priority")
	var problem *contract.Problem
	if !errors.As(err, &problem) || problem.Status != http.StatusNotFound || len(notices) != 0 {
		t.Fatalf("認可拒否: err=%v notices=%v", err, notices)
	}
	if _, err := db.DB().ExecContext(ctx, `ALTER TABLE drafts ADD CONSTRAINT reject_normal_priority CHECK (json::jsonb->>'priority' IS DISTINCT FROM 'normal')`); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.postDraftAction(ctx, Record{"priority": "normal"}, screen, "", id, "priority")
	if err == nil || len(notices) != 0 {
		t.Fatalf("保存失敗: err=%v notices=%v", err, notices)
	}
	stored, found, err := s.options.Records.Get(ctx, "drafts", id)
	if err != nil || !found || stored["priority"] != "high" {
		t.Fatalf("保存失敗後: %v found=%v err=%v", stored, found, err)
	}
}

func TestDraftListFiltersAccessBeforePagination(t *testing.T) {
	s, db, _ := fixture(t)
	token := stopToken(t, db, true)
	for i, d := range []Record{
		{"project": "owner/app", "status": "draft"},
		{"project": "owner/app", "status": "error"},
		{"project": "owner/app", "status": "finished"},
		{"project": "other/app", "status": "draft"},
	} {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		d["id"] = id
		if _, err := s.options.Records.Change(context.Background(), "drafts", id, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	s.Register(mux)
	request := func(query string) (int, map[string]any) {
		req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code == 200 {
			timing := w.Header().Get("Server-Timing")
			if len(strings.Split(timing, ", ")) != 5 {
				t.Fatalf("unexpected timing metrics: %q", timing)
			}
			for _, name := range []string{"draft_read", "draft_filter", "draft_page", "draft_full", "draft_view"} {
				if !regexp.MustCompile(`(?:^|, )` + name + `;dur=[0-9]+\.[0-9]{3}(?:, |$)`).MatchString(timing) {
					t.Fatalf("missing timing %s: %q", name, timing)
				}
			}
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err, w.Body.String())
		}
		return w.Code, out
	}
	code, first := request("?project=owner%2Fapp&status=draft,error&limit=1&page=1")
	if code != 200 || first["total"] != float64(2) || first["hasMore"] != true || len(first["items"].([]any)) != 1 {
		t.Fatal(code, first)
	}
	code, second := request("?project=owner%2Fapp&status=draft,error&limit=1&page=2")
	if code != 200 || second["total"] != float64(2) || second["hasMore"] != false || len(second["items"].([]any)) != 1 {
		t.Fatal(code, second)
	}
	code, all := request("")
	if code != 200 || all["total"] != float64(3) {
		t.Fatal(code, all)
	}
	code, excluded := request("?project=owner%2Fapp&excludeStatus=finished,error")
	if code != 200 || excluded["total"] != float64(1) || excluded["items"].([]any)[0].(map[string]any)["status"] != "draft" {
		t.Fatal(code, excluded)
	}
	code, both := request("?status=draft,error&excludeStatus=draft")
	if code != 200 || both["total"] != float64(1) {
		t.Fatal(code, both)
	}
	// The page is built with DraftView after slicing; each row keeps the projected fields.
	if row := first["items"].([]any)[0].(map[string]any); row["actions"] == nil || row["url"] == nil {
		t.Fatal(row)
	}
	code, _ = request("?limit=201")
	if code != 400 {
		t.Fatal(code)
	}
}

func TestDraftTimingAgainstPreviousPath(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	delegated := stopToken(t, db, true)
	plain := "idk_" + strings.Repeat("b", 43)
	if _, err := db.AddPCToken(ctx, store.PCTokenRecord{ID: "plain", Name: "plain", Hash: auth.Hash(plain), IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(ctx, auth.Hash("test-session"), auth.Session{ExpiresAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 150; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		project := "owner/app"
		if i%5 == 0 {
			project = "other/app"
		}
		statuses := []string{"draft", "error", "finished"}
		d := Record{"id": id, "project": project, "status": statuses[i%len(statuses)], "body": "本文", "owner": "pc:monitor"}
		if _, err := s.options.Records.Change(ctx, "drafts", id, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
			t.Fatal(err)
		}
	}
	parentID := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	childID := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	for _, d := range []Record{
		{"id": parentID, "project": "owner/app", "status": "draft", "body": "parent", "splitChildren": []any{childID}, "splitIssues": []any{Record{"project": "owner/app"}}, "messages": []any{Record{"role": "user", "images": []any{Record{"id": "33333333-3333-4333-8333-333333333333"}}}}},
		{"id": childID, "project": "owner/app", "status": "submitted", "parentSplit": parentID, "issue": Record{"number": 7}},
	} {
		id := d["id"].(string)
		if _, err := s.options.Records.Change(ctx, "drafts", id, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
			t.Fatal(err)
		}
	}
	queries := []struct{ name, query string }{
		{"defaults", ""},
		{"first", "?limit=1&page=1"},
		{"filtered", "?project=owner%2Fapp&status=draft,error&excludeStatus=error&limit=3&page=2"},
		{"excluded", "?excludeStatus=finished&limit=7&page=3"},
		{"no_matches", "?project=other%2Fapp&status=draft&limit=2&page=1"},
		{"past_end", "?status=finished&limit=2&page=999"},
		{"conflicting", "?status=draft&excludeStatus=draft&limit=200&page=1"},
		{"invalid", "?limit=0"},
		{"parent", "?limit=1&page=1"},
		{"child", "?limit=1&page=2"},
		{"empty_project", "?project=absent%2Fapp"},
		{"invalid_page", "?page=abc"},
	}
	credentials := []struct {
		name, kind, token string
		principal         auth.Principal
		denied            bool
	}{
		{"cliKey", "cli", "", auth.Principal{Kind: auth.CLI}, false},
		{"pcDelegated", "bearer", delegated, auth.Principal{Kind: auth.PC, Name: "fixture", TokenID: "monitor", Delegated: true}, false},
		{"pcNotDelegated", "bearer", plain, auth.Principal{Kind: auth.PC, Name: "plain", TokenID: "plain"}, true},
		{"screen", "screen", "", auth.Principal{Kind: auth.Screen}, false},
	}
	mux := http.NewServeMux()
	s.Register(mux)
	for _, failJobs := range []bool{false, true} {
		s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
			if failJobs {
				return nil, errors.New("fixture failure")
			}
			return []FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
		}
		for _, credential := range credentials {
			for _, query := range queries {
				t.Run(fmt.Sprintf("jobsFailure=%t/%s/%s", failJobs, credential.name, query.name), func(t *testing.T) {
					req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts"+query.query, nil)
					switch credential.kind {
					case "cli":
						key := "test-cli-key"
						if err := db.AddCLIKey(ctx, auth.Hash(key), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
							t.Fatal(err)
						}
						req.RemoteAddr = "127.0.0.1:1234"
						req.Header.Set("X-Desk-Key", key)
					case "bearer":
						req.Header.Set("Authorization", "Bearer "+credential.token)
					case "screen":
						req.AddCookie(&http.Cookie{Name: "desk_session", Value: "test-session"})
					}
					var expected *httptest.ResponseRecorder
					if credential.denied {
						expected = httptest.NewRecorder()
						contract.WriteProblem(expected, auth.Authorize(credential.principal, auth.Policy{Screen: true, CLI: true, PC: true, RequireDelegation: true}))
					} else {
						pagination, failure := contract.ParsePagination(req.URL.Query())
						if failure != nil {
							expected = httptest.NewRecorder()
							contract.WriteProblem(expected, failure)
						} else {
							ac, failure := s.options.Auth.Context(req)
							if failure != nil {
								t.Fatal(failure)
							}
							rows, err := s.options.Records.List(ctx, "drafts")
							if err != nil {
								t.Fatal(err)
							}
							rows = s.FamilyProgress(ctx, rows)
							views := []Record{}
							q := req.URL.Query()
							for _, d := range rows {
								if s.draftAccess(d, credential.principal) != nil || (q.Get("project") != "" && d["project"] != q.Get("project")) || (q.Get("status") != "" && !containsStatus(strings.Split(q.Get("status"), ","), d["status"])) || (q.Get("excludeStatus") != "" && containsStatus(strings.Split(q.Get("excludeStatus"), ","), d["status"])) {
									continue
								}
								views = append(views, s.DraftView(d, credential.principal, ac.Origin))
							}
							expected = httptest.NewRecorder()
							respond(expected, http.StatusOK, contract.Paginate(views, pagination))
						}
					}
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, req)
					var got, want any
					if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
						t.Fatal(err, w.Body.String())
					}
					if err := json.Unmarshal(expected.Body.Bytes(), &want); err != nil {
						t.Fatal(err)
					}
					if w.Code != expected.Code || !reflect.DeepEqual(got, want) {
						t.Fatalf("response differs: got %d %v, want %d %v", w.Code, got, expected.Code, want)
					}
				})
			}
		}
	}
}
