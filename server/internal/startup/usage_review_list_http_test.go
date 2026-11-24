package startup

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// Exercise the wiring installed by OpenApp, not a replacement auxiliary reader.
func checkUsageReviewListHTTP(t *testing.T, local, usage *sql.DB, call func(string, string, string, string) *httptest.ResponseRecorder) {
	t.Helper()
	now := time.Now().UTC()
	at := func(days int) string { return now.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339Nano) }
	addRound := func(i, merge int) {
		t.Helper()
		b, err := json.Marshal(map[string]any{"round": i, "draftId": fmt.Sprint("review-", i), "startedAt": at(-30), "treatmentIssues": []any{map[string]any{"mergedAt": at(merge)}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = local.Exec("INSERT INTO usage_review_rounds(round,json) VALUES(?,?)", i, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	addRound(1, -5)
	addRound(2, -5)
	addRound(3, -1)
	if _, err := local.Exec("INSERT INTO usage_review_acks(round,at) VALUES(1,?)", at(0)); err != nil {
		t.Fatal(err)
	}
	for i, row := range []map[string]any{
		{"at": at(-30), "provider": "claude", "costUsd": float64(1)},
		{"at": at(-3), "provider": "claude", "costUsd": float64(20), "source": "intake", "draft": "review-1"},
		{"at": at(-3), "provider": "claude", "costUsd": float64(20), "source": "intake", "draft": "review-2"},
	} {
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = usage.Exec("INSERT INTO usage_rows(key,at,host,source,repo,json,sender) VALUES($1,$2,'nas','intake','owner/repo',$3,'nas')", fmt.Sprint("review-test-", i), row["at"], string(b)); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		if _, err := local.Exec("DELETE FROM usage_review_acks WHERE round IN (1,2,3,4)"); err != nil {
			t.Error(err)
		}
		if _, err := local.Exec("DELETE FROM usage_review_rounds WHERE round IN (1,2,3,4)"); err != nil {
			t.Error(err)
		}
		if _, err := usage.Exec("DELETE FROM usage_rows WHERE key LIKE 'review-test-%'"); err != nil {
			t.Error(err)
		}
	}()
	alert := call("GET", "/api/usage-review/alerts", "screen", "")
	var alerts []any
	if err := json.Unmarshal(alert.Body.Bytes(), &alerts); err != nil || alert.Code != 200 || len(alerts) != 1 || alerts[0].(map[string]any)["round"] != float64(2) {
		t.Fatalf("alert %d %s %v", alert.Code, alert.Body.String(), err)
	}
	list := call("GET", "/api/list", "screen", "")
	var body map[string]any
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil || list.Code != 200 {
		t.Fatalf("list %d %s %v", list.Code, list.Body.String(), err)
	}
	aux, ok := body["aux"].(map[string]any)
	if !ok || !reflect.DeepEqual(aux["reviews"], alerts) {
		t.Fatalf("list reviews=%v, alert=%v", body["aux"], alerts)
	}
	if _, err := usage.Exec("ALTER TABLE usage_rows RENAME TO usage_rows_hidden_test"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := usage.Exec("ALTER TABLE usage_rows_hidden_test RENAME TO usage_rows"); err != nil {
			t.Error(err)
		}
	}()
	alert = call("GET", "/api/usage-review/alerts", "screen", "")
	if alert.Code != 503 {
		t.Fatalf("alert read failure: %d %s", alert.Code, alert.Body.String())
	}
	// The list keeps the judgments it made for five minutes, so it answers
	// the kept rounds without reading the usage rows.
	list = call("GET", "/api/list", "screen", "")
	body = nil
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil || list.Code != 200 {
		t.Fatalf("list with kept reviews: %d %s %v", list.Code, list.Body.String(), err)
	}
	if aux, _ = body["aux"].(map[string]any); !reflect.DeepEqual(aux["reviews"], alerts) {
		t.Fatalf("kept reviews=%v, alert=%v", body["aux"], alerts)
	}
	// A new round has no kept judgment: the list reads the usage rows and fails.
	addRound(4, -5)
	list = call("GET", "/api/list", "screen", "")
	body = nil
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil || list.Code != 200 {
		t.Fatalf("list read failure: %d %s %v", list.Code, list.Body.String(), err)
	}
	// A failed auxiliary source is unavailable, not an empty successful list.
	aux, _ = body["aux"].(map[string]any)
	if _, ok := aux["reviews"]; ok {
		t.Fatalf("failed reviews returned as available: %s", list.Body.String())
	}
}
