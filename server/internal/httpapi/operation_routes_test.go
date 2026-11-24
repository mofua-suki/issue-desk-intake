package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOperationDelegationAndIncidentRespondBoundary(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, true, nil)
	status, value := workerRequest(t, handler, token, "POST", "/api/v1/ops-watch/lease", `{"ttlMinutes":1,"primary":false}`)
	if status != 200 || value["held"] != true {
		t.Fatal(status, value)
	}
	status, _ = workerRequest(t, handler, token, "POST", "/api/intake/force-handoff", `{"statement":"停止中のNASを今入れ替えてください"}`)
	if status != 400 {
		t.Fatal("force handoff without instructionAt", status)
	}
	at := time.Now().Add(-time.Minute).Format(time.RFC3339)
	status, value = workerRequest(t, handler, token, "POST", "/api/intake/force-handoff", `{"statement":"停止中のNASを今入れ替えてください","instructionAt":"`+at+`"}`)
	if status != 200 || value["id"] == nil {
		t.Fatal(status, value)
	}
	status, _ = workerRequest(t, handler, token, "POST", "/api/incidents/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/respond", `{}`)
	if status != 403 {
		t.Fatal("PC may respond", status)
	}
	status, _ = workerRequest(t, handler, token, "POST", "/api/ops-watch/archive/invalid/restore", `{}`)
	if status != 400 {
		t.Fatal("invalid archive id", status)
	}
	plain, _, plainToken := workerFixture(t)
	for _, path := range []string{"/api/v1/ops-watch/lease", "/api/intake/force-handoff", "/api/ops-watch/archive/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/restore", "/api/incidents/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/acknowledge"} {
		status, _ = workerRequest(t, plain, plainToken, "POST", path, `{}`)
		if status != 403 {
			t.Fatal(path, status)
		}
	}
}

func TestAlertRoutesReturnArraysAndKeepDraftAlerts(t *testing.T) {
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		o.AttentionDraftAlerts = func(context.Context) ([]any, error) {
			return []any{map[string]any{"key": "consultation-budget:test", "kind": "consultation_budget"}}, nil
		}
	})
	for _, path := range []string{"/api/stalls/alerts", "/api/attention/alerts", "/api/full-test/alerts", "/api/usage-review/alerts", "/api/ops-watch/archive"} {
		r := httptest.NewRequest("GET", "http://localhost:8787"+path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var rows []any
		if e := json.Unmarshal(w.Body.Bytes(), &rows); e != nil || w.Code != 200 || rows == nil {
			t.Fatalf("%s HTTP%d %s", path, w.Code, w.Body.String())
		}
		if path == "/api/attention/alerts" && (len(rows) != 1 || rows[0].(map[string]any)["kind"] != "consultation_budget") {
			t.Fatal(rows)
		}
	}
}
