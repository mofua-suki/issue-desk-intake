package screenapi

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestAttentionTimingListAndMeasurements(t *testing.T) {
	f := newFixture(t)
	failed := false
	f.s.o.Aux["attentionAlerts"] = func(ctx context.Context) (any, error) {
		for i, name := range attentionTimingNames {
			ReportAttentionTiming(ctx, name, time.Duration(i+1)*time.Millisecond)
		}
		ReportAttentionTiming(ctx, "secret-row-123", time.Second)
		if failed {
			return nil, errors.New("unavailable")
		}
		return []any{Object{"title": "secret-row-123"}}, nil
	}
	request := func(path string) (*httptest.ResponseRecorder, Object) {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8787"+path, nil)
		r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var body Object
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return w, body
	}
	w, body := request("/api/list?tabs=active")
	header := w.Header().Get("Server-Timing")
	pattern := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*;dur=[0-9]+\.[0-9]{3}$`)
	for _, item := range strings.Split(header, ", ") {
		if !pattern.MatchString(item) {
			t.Fatalf("invalid timing: %q", item)
		}
	}
	if strings.Contains(header, "secret-row-123") {
		t.Fatal(header)
	}
	for i, name := range attentionTimingNames {
		expected := "attentionAlerts_" + name + ";dur="
		if !strings.Contains(header, expected) {
			t.Fatalf("missing %s: %s", expected, header)
		}
		_ = i
	}
	if len(fleet.Items(fleet.ObjectOrEmpty(body["aux"])["attentionAlerts"])) != 1 {
		t.Fatal(body)
	}
	_, delta := request("/api/list?tabs=active&since=" + fleet.Text(body["cursor"]))
	if delta["full"] != false || len(fleet.ObjectOrEmpty(delta["aux"])) != 0 {
		t.Fatalf("unchanged auxiliary produced delta: %v", delta)
	}
	_, meta := request("/api/meta")
	stages := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(fleet.Items(fleet.ObjectOrEmpty(meta["listMeasurements"])["full"])[0])["stages"])
	for i, name := range attentionTimingNames {
		if stages["attentionAlerts_"+name] != float64(i+1) {
			t.Fatalf("stage %s: %v", name, stages)
		}
	}
	t.Logf("example stages in ms: %v", stages)
	failed = true
	_, unavailable := request("/api/list?tabs=active&since=" + fleet.Text(delta["cursor"]))
	if len(fleet.Items(unavailable["unavailable"])) == 0 {
		t.Fatalf("missing unavailable: %v", unavailable)
	}
}
