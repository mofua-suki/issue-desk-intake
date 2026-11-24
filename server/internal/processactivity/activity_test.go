package processactivity

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAggregationAndRetention(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := &Collector{Now: func() time.Time { return now }}
	c.Periodic("task", 2*time.Second, false)
	c.Periodic("task", 3*time.Second, true)
	r := c.Report()[0].Rows[0]
	if r.Count != 2 || r.Failures != 1 || r.Total != 5*time.Second || r.Max != 3*time.Second {
		t.Fatalf("aggregate: %+v", r)
	}
	now = now.Add(24 * time.Hour)
	if len(c.Report()) != 0 {
		t.Fatal("expired hour retained")
	}
	for i := 0; i < RowsPerHour+1; i++ {
		c.Periodic(fmt.Sprintf("task-%d", i), time.Second, false)
	}
	if got := len(c.Report()[0].Rows); got != RowsPerHour {
		t.Fatalf("rows: %d", got)
	}
}
func TestHTTPNormalizationAndResponse(t *testing.T) {
	c := &Collector{}
	h := c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404); _, _ = w.Write([]byte("body")) }))
	r := httptest.NewRequest("GET", "http://example/api/issues/123?token=secret", nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 || w.Body.String() != "body" {
		t.Fatalf("response: %d %q", w.Code, w.Body.String())
	}
	row := c.Report()[0].Rows[0]
	if row.Name != "/api/issues/{n}" || row.Credential != "pc" || row.Status != "4xx" || row.Count != 1 || row.Failures != 1 {
		t.Fatalf("row: %+v", row)
	}
	if got := Normalize("/x/550e8400-e29b-41d4-a716-446655440000"); got != "/x/{id}" {
		t.Fatal(got)
	}
}

func TestPeriodicRecordedAfterHTTPRowLimit(t *testing.T) {
	c := &Collector{}
	h := c.Wrap(http.NotFoundHandler())
	for i := 0; i < RowsPerHour+10; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", fmt.Sprintf("/unknown/path-%d", i), nil))
	}
	c.Periodic("maintenance", time.Second, false)
	rows := c.Report()[0].Rows
	if len(rows) != RowsPerHour+1 {
		t.Fatalf("rows: %d", len(rows))
	}
	for _, row := range rows {
		if row.Kind == "periodic" && row.Name == "maintenance" && row.Count == 1 {
			return
		}
	}
	t.Fatal("periodic row missing")
}
