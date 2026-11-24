package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
)

func TestLegacyStallCauseReadOnly(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	token := stopToken(t, db, true)
	mux := http.NewServeMux()
	s.Register(mux)
	cases := []struct {
		name   string
		fields Record
		want   string
	}{
		{"explicit", Record{"error": "相談を停止しました: 理由"}, "explicit"},
		{"timeout", Record{"error": "24時間以内にAIの応答を返せなかったため、自動処理を停止しました。", "consultationStops": []any{Record{"reason": "過去"}}}, "timeout"},
		{"unknown", Record{"consultationStops": []any{Record{"reason": "過去"}}}, ""},
		{"invalid", Record{"stallCause": "future", "error": "相談を停止しました: 理由"}, ""},
		{"valid", Record{"stallCause": "timeout", "error": "相談を停止しました: 理由"}, "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Record{"id": todoID, "project": "owner/app", "status": "stalled", "revision": 0}
			for k, v := range tc.fields {
				d[k] = v
			}
			if _, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
				t.Fatal(err)
			}
			before, _, err := s.options.Records.Get(ctx, "drafts", todoID)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts/"+todoID, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			var out Record
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
				t.Fatal(w.Code, err, w.Body.String())
			}
			if text(out["stallCause"]) != tc.want {
				t.Fatal(out)
			}
			after, _, err := s.options.Records.Get(ctx, "drafts", todoID)
			a, _ := canonicalJSON(before)
			b, _ := canonicalJSON(after)
			if err != nil || a != b {
				t.Fatal("GET modified record", err)
			}
		})
	}
}

func TestStallCauseTimeoutPaths(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "during"}[during], func(t *testing.T) {
			s, _, _ := fixture(t)
			ctx := context.Background()
			now := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
			s.options.Now = func() time.Time { return now }
			start := now.Add(-25 * time.Hour)
			if during {
				start = now.Add(-24*time.Hour + 10*time.Millisecond)
			}
			d := Record{"id": todoID, "project": "owner/app", "status": "thinking", "revision": 0, "waitStartedAt": timestamp(start)}
			if _, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
				t.Fatal(err)
			}
			called := false
			e, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(c context.Context, _ Record) (Record, error) { called = true; <-c.Done(); return nil, c.Err() }})
			if err != nil {
				t.Fatal(err)
			}
			timedOut := false
			defer func() {
				if timedOut {
					go e.Close()
					return
				}
				e.Close()
			}()
			if _, err = e.Start(ctx, todoID, d, auth.Principal{Kind: auth.Screen}); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(30 * time.Second)
			for e.Busy(todoID) {
				if time.Now().After(deadline) {
					timedOut = true
					t.Fatal("consultation engine did not finish within 30 seconds")
				}
				time.Sleep(time.Millisecond)
			}
			got, err := s.Draft(ctx, todoID, auth.Principal{Kind: auth.Screen})
			if err != nil || got["status"] != "stalled" || got["stallCause"] != "timeout" || called != during {
				t.Fatal(got, called, err)
			}
		})
	}
}
