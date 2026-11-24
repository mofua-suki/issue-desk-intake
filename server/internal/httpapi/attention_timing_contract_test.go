package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttentionTimingErrorResponsesContract(t *testing.T) {
	root := filepath.Join("..", "..", "..", "api")
	raw, err := os.ReadFile(filepath.Join(root, "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]struct {
			Get struct {
				Responses map[string]struct {
					Headers map[string]struct {
						Ref string `json:"$ref"`
					} `json:"headers"`
					Content map[string]json.RawMessage `json:"content"`
				} `json:"responses"`
			} `json:"get"`
		} `json:"paths"`
		Components struct {
			Headers map[string]struct {
				Description string `json:"description"`
				Schema      struct {
					Type string `json:"type"`
				} `json:"schema"`
			} `json:"headers"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"502", "503", "default"} {
		response := spec.Paths["/api/attention/alerts"].Get.Responses[status]
		ref := response.Headers["Server-Timing"].Ref
		const prefix = "#/components/headers/"
		name, ok := strings.CutPrefix(ref, prefix)
		header, found := spec.Components.Headers[name]
		if !ok || ref != "#/components/headers/AttentionErrorServerTiming" || !found || header.Schema.Type != "string" || !strings.Contains(header.Description, "failed stage") || !strings.Contains(header.Description, "before timing begins") || len(response.Content["application/json"]) == 0 {
			t.Errorf("%s: 不完全なエラー応答定義: %+v", status, response)
		}
	}

}

func TestAttentionTimingAbsentBeforeRepositoriesFailure(t *testing.T) {
	var fleetPath string
	handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) { fleetPath = o.FleetPath })
	if err := os.Remove(fleetPath); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://localhost:8787/api/attention/alerts", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP%d: %s", w.Code, w.Body.String())
	}
	if values, ok := w.Header()[http.CanonicalHeaderKey("Server-Timing")]; ok {
		t.Fatalf("unexpected Server-Timing: %q", values)
	}
}
