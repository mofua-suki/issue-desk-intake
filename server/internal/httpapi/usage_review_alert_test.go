package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUsageReviewAlertHTTPMixedAcknowledgementsAndReadFailure(t *testing.T) {
	var local *sql.DB
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	handler, db, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) { local = o.AppDB; o.Now = func() time.Time { return now } })
	for _, item := range []struct {
		round  int
		draft  string
		merged string
	}{{1, "one", "2026-10-27T00:00:00Z"}, {2, "two", "2026-10-27T00:00:00Z"}, {3, "three", "2026-10-31T00:00:00Z"}} {
		raw, err := json.Marshal(map[string]any{"round": item.round, "draftId": item.draft, "startedAt": "2026-10-01T15:00:00Z", "treatmentIssues": []any{map[string]any{"mergedAt": item.merged}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = local.Exec("INSERT INTO usage_review_rounds(round,json) VALUES(?,?)", item.round, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := local.Exec("INSERT INTO usage_review_acks(round,at) VALUES(1,?)", "2026-11-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	for i, row := range []map[string]any{
		{"at": "2026-10-15T15:00:00Z", "provider": "claude", "costUsd": float64(1)},
		{"at": "2026-10-29T00:00:00Z", "provider": "claude", "costUsd": float64(20), "source": "intake", "draft": "one"},
		{"at": "2026-10-29T00:00:00Z", "provider": "claude", "costUsd": float64(20), "source": "intake", "draft": "two"},
	} {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec("INSERT INTO usage_rows(key,at,host,source,repo,json,sender) VALUES($1,$2,'nas','intake','owner/repo',$3,'nas')", fmt.Sprint(i), row["at"], string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://localhost:8787/api/usage-review/alerts", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := request()
	var alerts []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &alerts); err != nil || w.Code != 200 {
		t.Fatalf("HTTP %d %s: %v", w.Code, w.Body.String(), err)
	}
	if len(alerts) != 1 || alerts[0]["round"] != float64(2) || alerts[0]["title"] != "利用量の見直し 第2回: 見直しの費用が減った量を上回っています" || alerts[0]["reason"] != "元が取れていない実行先があります" || alerts[0]["startDate"] != "2026-10-02" {
		t.Fatalf("alerts: %v", alerts)
	}
	if _, err := db.Exec("DROP TABLE usage_rows"); err != nil {
		t.Fatal(err)
	}
	w = request()
	if w.Code != 503 {
		t.Fatalf("usage read failure: HTTP %d %s", w.Code, w.Body.String())
	}
	var problem map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil || problem["code"] != "TEMPORARY_UNAVAILABLE" {
		t.Fatalf("failure response: %s (%v)", w.Body.String(), err)
	}
}
