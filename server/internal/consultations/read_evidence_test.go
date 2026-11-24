package consultations

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
)

func evidenceInput() map[string]any {
	return map[string]any{"version": 1, "documents": []any{map[string]any{"id": "a", "project": "owner/app", "path": "README.md", "revision": strings.Repeat("a", 40), "start": 1, "end": 1, "text": "body"}}, "facts": []any{map[string]any{"statement": "fact", "documents": []any{"a"}}}}
}
func evidenceRequest(t *testing.T, s *Service, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	req := httptest.NewRequest("POST", "http://localhost:8787/api/issues", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Origin", "http://localhost:8787")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}
func TestReadEvidenceValidation(t *testing.T) {
	s, db, _ := fixture(t)
	token := stopToken(t, db, false)
	invalid := []any{nil, map[string]any{"version": 2, "documents": []any{}, "facts": []any{}}, map[string]any{"version": 1, "documents": []any{}, "facts": []any{map[string]any{"statement": "x", "documents": []any{"absent"}}}}}
	badRevision := evidenceInput()
	badRevision["documents"].([]any)[0].(map[string]any)["revision"] = "main"
	badRange := evidenceInput()
	badRange["documents"].([]any)[0].(map[string]any)["end"] = 0
	badSize := evidenceInput()
	badSize["documents"].([]any)[0].(map[string]any)["text"] = strings.Repeat("x", 65536)
	duplicate := evidenceInput()
	duplicate["documents"] = append(duplicate["documents"].([]any), duplicate["documents"].([]any)[0])
	invalid = append(invalid, badRevision, badRange, badSize, duplicate)
	for _, e := range invalid {
		body, _ := json.Marshal(Record{"id": todoID, "project": "owner/app", "text": "本文", "readEvidence": e})
		w := evidenceRequest(t, s, token, string(body))
		if w.Code != 400 {
			t.Fatalf("status=%d %s", w.Code, w.Body.String())
		}
	}
	for _, table := range []string{"intake_receipts", "drafts"} {
		var n int
		if err := db.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s count=%d err=%v", table, n, err)
		}
	}
}
func TestReadEvidenceReceiptRoundTrip(t *testing.T) {
	s, db, _ := fixture(t)
	token := stopToken(t, db, false)
	ctx := context.Background()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	engine, err := NewEngine(ctx, EngineOptions{Service: s, Principal: func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{Kind: auth.PC, Name: "fixture"}, nil
	}, Turn: func(context.Context, Record) (Record, error) {
		entered <- struct{}{}
		<-release
		return Record{"reply": "確認"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(release); engine.Close() }()
	s.BindEngine(engine)
	input := Record{"id": todoID, "project": "owner/app", "tier": "light", "text": "本文", "readEvidence": evidenceInput()}
	raw, _ := json.Marshal(input)
	w := evidenceRequest(t, s, token, string(raw))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("processing did not start")
	}
	w = evidenceRequest(t, s, token, string(raw))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	input["readEvidence"].(map[string]any)["facts"].([]any)[0].(map[string]any)["statement"] = "changed"
	changed, _ := json.Marshal(input)
	w = evidenceRequest(t, s, token, string(changed))
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	receipt := readReceipt(t, s, todoID)
	d, found, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil || !found {
		t.Fatal(err)
	}
	for _, v := range []any{record(receipt["input"])["readEvidence"], d["readEvidence"]} {
		b, _ := json.Marshal(v)
		if !strings.Contains(string(b), `"statement":"fact"`) {
			t.Fatal(string(b))
		}
	}
	var count int
	if err := db.DB().QueryRow("SELECT count(*) FROM intake_receipts").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	users := 0
	for _, m := range fleet.Items(d["messages"]) {
		if record(m)["role"] == "user" {
			users++
		}
	}
	if users != 1 {
		t.Fatal(users)
	}
}
func TestReadEvidenceReceiptRecovery(t *testing.T) {
	for _, scenario := range []string{"legacy_receipt", "legacy_draft", "evidence"} {
		withEvidence := scenario == "evidence"
		t.Run(scenario, func(t *testing.T) {
			s, _, _ := fixture(t)
			ctx := context.Background()
			input := Record{"id": todoID, "project": "owner/app", "tier": "light", "text": "本文"}
			if withEvidence {
				input["readEvidence"] = evidenceInput()
			}
			if _, err := s.AcceptIssue(ctx, input, auth.Principal{Kind: auth.Screen}); err != nil {
				t.Fatal(err)
			}
			if scenario == "legacy_draft" {
				if _, err := s.CreateDraft(ctx, input, auth.Principal{Kind: auth.Screen}, true); err != nil {
					t.Fatal(err)
				}
			}
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			engine, err := NewEngine(ctx, EngineOptions{Service: s, Turn: func(context.Context, Record) (Record, error) {
				entered <- struct{}{}
				<-release
				return Record{"reply": "確認"}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { close(release); engine.Close() }()
			s.BindEngine(engine)
			if err := engine.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("recovery did not start")
			}
			d, found, err := s.options.Records.Get(ctx, "drafts", todoID)
			if err != nil || !found {
				t.Fatal(err)
			}
			_, present := d["readEvidence"]
			if present != withEvidence {
				t.Fatalf("evidence present=%v", present)
			}
			if withEvidence {
				raw, _ := json.Marshal(d["readEvidence"])
				if !strings.Contains(string(raw), `"statement":"fact"`) {
					t.Fatal(string(raw))
				}
			}
			if len(fleet.Items(d["messages"])) != 1 {
				t.Fatal(d["messages"])
			}
		})
	}
}
