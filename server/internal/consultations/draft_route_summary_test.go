package consultations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/store"
)

func TestDraftSingleSummaryKeepsWaitFieldsOnly(t *testing.T) {
	ctx := context.Background()
	s, db, _ := fixture(t)
	s.options.Start = nil
	const stoppedID = "22222222-2222-4222-8222-222222222222"
	const runningID = "33333333-3333-4333-8333-333333333333"
	const foreignID = "44444444-4444-4444-8444-444444444444"
	long := strings.Repeat("long-message-", 770)[:10000]
	rows := []Record{
		{
			"id": stoppedID, "project": "owner/app", "status": "draft", "revision": 7, "title": "題名", "shownTitle": "表示の題名",
			"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-02T00:00:00Z", "ready": true, "hold": true,
			"questions": []any{"質問1", "質問2"}, "error": "止まりました", "errorCode": "BUDGET", "issue": Record{"project": "owner/app", "number": 7},
			"issues": []any{Record{"project": "owner/app", "number": 8}}, "parentSplit": "parent", "splitChildren": []any{"child"}, "finishedWithoutIssue": true,
			"consultationBudget": Record{"stopped": Record{"reason": "上限", "at": "2026-01-02T00:00:00Z"}, "stops": []any{Record{"reason": "上限"}}, "turns": 9},
			"owner":              "token:monitor", "body": long, "messages": []any{Record{"role": "assistant", "text": long}}, "_private": "内部",
		},
		{"id": runningID, "project": "owner/app", "status": "thinking", "revision": 1, "consultationBudget": Record{"turns": 1}, "messages": []any{Record{"role": "user", "text": long}}},
		{"id": foreignID, "project": "owner/app", "status": "draft", "owner": "token:monitor"},
	}
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.options.Records.DB.ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), row["id"], string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	// A receipt whose draft does not exist yet is answered from the receipt alone.
	if _, err := s.AcceptIssue(ctx, Record{"id": todoID, "project": "owner/app", "tier": "light", "text": "保存された本文", "hold": true}, auth.Principal{Kind: auth.Screen}); err != nil {
		t.Fatal(err)
	}
	token := stopToken(t, db, true)
	plain := "idk_" + strings.Repeat("b", 43)
	if _, err := db.AddPCToken(ctx, store.PCTokenRecord{ID: "plain", Name: "plain", Hash: auth.Hash(plain), IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	request := func(id, query, bearer string) (int, map[string]any, string) {
		t.Helper()
		req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts/"+id+query, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return w.Code, out, w.Body.String()
	}
	allowed := []string{
		"id", "project", "unassigned", "status", "revision", "title", "shownTitle", "createdAt", "updatedAt",
		"ready", "hold", "questions", "error", "errorCode", "issue", "issues", "parentSplit", "splitChildren", "finishedWithoutIssue",
		"accepted", "receiptId", "requestId", "draftId", "receiptState",
	}
	for _, id := range []string{stoppedID, runningID, todoID} {
		fullCode, full, fullBody := request(id, "", token)
		code, summary, body := request(id, "?view=summary", token)
		if fullCode != 200 || code != 200 {
			t.Fatalf("%s: full=%d %s summary=%d %s", id, fullCode, fullBody, code, body)
		}
		if summary["view"] != "summary" || strings.Contains(body, long) || len(body) >= len(fullBody) {
			t.Fatalf("%s: summary is not reduced: %s", id, body)
		}
		expected := map[string]any{"view": "summary"}
		for _, key := range allowed {
			if v, ok := full[key]; ok {
				expected[key] = v
			}
		}
		budget, _ := full["consultationBudget"].(map[string]any)
		if stopped, ok := budget["stopped"]; ok {
			expected["consultationBudget"] = map[string]any{"stopped": stopped}
		}
		if !reflect.DeepEqual(summary, expected) {
			t.Fatalf("%s: got %v, want %v", id, summary, expected)
		}
	}
	_, stopped, _ := request(stoppedID, "?view=summary", token)
	if stopped["status"] != "draft" || stopped["revision"] != float64(7) || stopped["consultationBudget"].(map[string]any)["stopped"] == nil {
		t.Fatalf("stopped draft lost the wait fields: %v", stopped)
	}
	_, running, _ := request(runningID, "?view=summary", token)
	if _, present := running["consultationBudget"]; present || running["status"] != "thinking" {
		t.Fatalf("running draft: %v", running)
	}
	_, receipt, _ := request(todoID, "?view=summary", token)
	if receipt["accepted"] != true || receipt["receiptId"] != todoID || receipt["receiptState"] != "accepted" || receipt["status"] != "thinking" {
		t.Fatalf("receipt: %v", receipt)
	}
	for _, tc := range []struct {
		id, query, token string
		status           int
	}{
		{stoppedID, "?view=unknown", token, 400},
		{foreignID, "", plain, 404},
		{foreignID, "?view=summary", plain, 404},
		{"55555555-5555-4555-8555-555555555555", "?view=summary", token, 404},
	} {
		if code, _, body := request(tc.id, tc.query, tc.token); code != tc.status {
			t.Fatalf("%s%s: %d %s", tc.id, tc.query, code, body)
		}
	}
}
