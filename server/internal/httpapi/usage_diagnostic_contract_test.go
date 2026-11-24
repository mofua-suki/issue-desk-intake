package httpapi

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestUsageDiagnosticKindUploadContract(t *testing.T) {
	handler, db, token := workerFixture(t)
	kinds := []string{"completed", "timeout_idle", "timeout_total", "canceled_by_server", "spawn_failed", "output_overflow", "exit_nonzero", "authentication", "rate_limit", "usage_limit", "format", "session", "input_invalid", "execution"}
	rows := []map[string]any{}
	add := func(source string, present bool, value any) {
		row := map[string]any{"at": fmt.Sprintf("2026-11-01T00:00:%02d.000Z", len(rows)), "host": "nas", "source": source}
		if present {
			row["diagnosticKind"] = value
		}
		rows = append(rows, row)
	}
	add("intake", false, nil)
	add("worker", false, nil)
	for _, kind := range kinds {
		add("intake", true, kind)
	}
	for _, value := range []any{nil, "", "unknown", 1, true, []any{}, map[string]any{}} {
		add("intake", true, value)
		add("worker", true, value)
	}
	for _, kind := range kinds {
		add("worker", true, kind)
	}
	body, err := json.Marshal(map[string]any{"usage": rows})
	if err != nil {
		t.Fatal(err)
	}
	status, result := workerRequest(t, handler, token, "POST", "/api/v1/usage", string(body))
	if status != 200 || result["added"] != float64(16) || result["rejected"] != float64(len(rows)-16) || result["duplicate"] != float64(0) {
		t.Fatalf("upload %d %v", status, result)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM usage_rows").Scan(&count); err != nil || count != 16 {
		t.Fatalf("saved rows=%d err=%v", count, err)
	}
}
