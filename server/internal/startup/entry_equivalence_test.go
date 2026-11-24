package startup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/screenapi"
	"issue-desk-intake/server/internal/store"
)

const entryParent = "00000000-0000-4000-8000-000000000001"

func entryJobs(context.Context, []consultations.FamilyIssue) ([]consultations.FamilyJob, error) {
	return []consultations.FamilyJob{{Project: "owner/app", Number: 1, Status: "merged"}, {Project: "owner/app", Number: 2, Status: "closed"}, {Project: "owner/app", Number: 3, Status: "failed", Reason: "failed reason"}, {Project: "owner/app", Number: 4, Status: "running", Waiting: []string{"dependency"}}}, nil
}
func entryIssues(context.Context, string) (map[string]string, error) {
	return map[string]string{"owner/app#5": "closed", "owner/app#6": "open"}, nil
}

func entryData(t *testing.T, env pgtest.Env) []string {
	t.Helper()
	ids := []string{entryParent}
	children := []string{}
	for i := 2; i <= 10; i++ {
		children = append(children, fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
	}
	rows := []map[string]any{{"id": entryParent, "project": "owner/app", "status": "submit_error", "ready": true, "splitChildren": children, "splitIssues": []any{map[string]any{"project": "owner/app"}, map[string]any{}, map[string]any{}}, "body": "<!-- issue-desk:followup -->", "messages": []any{map[string]any{"role": "proxy"}, map[string]any{"role": "user", "text": "hello", "images": []any{map[string]any{"id": "00000000-0000-4000-8000-000000000099"}}}}}}
	for i := 0; i < 8; i++ {
		row := map[string]any{"id": children[i], "project": "owner/app", "parentSplit": entryParent, "status": "submitted"}
		if i < 7 {
			row["issue"] = map[string]any{"number": i + 1}
		} else {
			row["finishedWithoutIssue"] = true
		}
		rows = append(rows, row)
		ids = append(ids, children[i])
	}
	normal := "00000000-0000-4000-8000-000000000011"
	rows = append(rows, map[string]any{"id": normal, "project": "owner/app", "status": "draft", "body": "normal", "messages": []any{map[string]any{"role": "proxy"}}})
	ids = append(ids, normal)
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = env.DB.ExecContext(context.Background(), "INSERT INTO drafts(id,json) VALUES (?,?)", row["id"], string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func entryHTTPHandler(t *testing.T, env pgtest.Env, entries map[string]screenapi.EntryReader) http.Handler {
	t.Helper()
	db, err := store.OpenApplicationPostgres(context.Background(), env.DSN, env.Schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now()
	if err = db.CreateSession(context.Background(), auth.Hash("entry-test"), auth.Session{CSRF: "csrf", ExpiresAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(auth.Config{PublicURL: "http://127.0.0.1:8787"}, &store.AuthStore{SQLite: db})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	_, err = screenapi.Register(mux, screenapi.Options{Auth: a, AppDB: db.DB(), Config: func() (screenapi.Object, error) {
		return screenapi.Object{"repositories": []any{screenapi.Object{"name": "owner/app"}}}, nil
	}, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	return mux
}
func entryRequest(h http.Handler, id string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/list/entry?type=draft&id="+id, nil)
	r.AddCookie(&http.Cookie{Name: "desk_session", Value: "entry-test"})
	r.Header.Set("Origin", "http://127.0.0.1:8787")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDraftEntryHTTPAllFamilyStates(t *testing.T) {
	env, base, _, _, _ := screenFixture(t)
	ids := entryData(t, env)
	app, entries := entryTestApp(t, base.Records.DB, entryJobs, entryIssues)
	old := map[string]screenapi.EntryReader{"draft": func(_ context.Context, id map[string]any, _ string) (map[string]any, error) {
		return legacyDraftEntry(t, app, id["id"].(string)), nil
	}}
	legacy, current := entryHTTPHandler(t, env, old), entryHTTPHandler(t, env, entries)
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			a, b := entryRequest(legacy, id), entryRequest(current, id)
			var x, y any
			if a.Code != 200 || b.Code != 200 {
				t.Fatalf("status old=%d %s new=%d %s", a.Code, a.Body, b.Code, b.Body)
			}
			if err := json.Unmarshal(a.Body.Bytes(), &x); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b.Body.Bytes(), &y); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(x, y) {
				t.Fatalf("old=%s new=%s", a.Body, b.Body)
			}
			if id == "00000000-0000-4000-8000-000000000011" {
				actions := y.(map[string]any)["item"].(map[string]any)["actions"].([]any)
				if !slices.Contains(actions, any("proxy_undo")) {
					t.Errorf("proxy action missing: %v", actions)
				}
			}
			if id == entryParent {
				item := y.(map[string]any)["item"].(map[string]any)
				p := item["familyProgress"].(map[string]any)
				for key, want := range map[string]float64{"total": 9, "completed": 1, "closed": 3, "remaining": 3, "unknown": 2} {
					if p[key] != want {
						t.Errorf("%s=%v want %v", key, p[key], want)
					}
				}
				stopped := p["stopped"].([]any)
				if len(stopped) != 2 || stopped[0].(map[string]any)["reason"] != "failed reason" || stopped[1].(map[string]any)["status"] != "dependency_waiting" || stopped[1].(map[string]any)["reason"] != "dependency" {
					t.Errorf("stopped=%v", stopped)
				}
				if item["actions"] == nil || item["body"] != "<!-- issue-desk:followup -->" || len(item["messages"].([]any)) != 2 {
					t.Errorf("missing actions or markers: %v", item)
				}
			}
		})
	}
}
