package adminapi

import (
	"encoding/json"
	"fmt"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"issue-desk-intake/server/internal/processactivity"
	"os"
	"testing"
	"time"
)

func TestDiagnosticsActivity(t *testing.T) {
	f := newFixture(t)
	raw, err := os.ReadFile("../../../api/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	schemaDef := obj(obj(obj(obj(obj(obj(spec["paths"])["/api/diagnostics/activity"])["get"])["responses"])["200"])["content"])["application/json"]
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("urn:activity", obj(schemaDef)["schema"]); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("urn:activity")
	if err != nil {
		t.Fatal(err)
	}
	processactivity.Process.Periodic("activity-test", 1500*time.Microsecond, true)
	processactivity.Process.HTTP("GET", "/api/issues/123?secret=x", 503, "cli", 2*time.Millisecond)
	for _, tc := range []struct {
		method, credential string
		want               int
	}{
		{"GET", "", 401}, {"GET", "worker", 403}, {"GET", "screen", 403}, {"POST", "delegate", 405}, {"GET", "delegate", 200}, {"GET", "cli", 200},
	} {
		code, value := f.call(tc.method, "/api/diagnostics/activity", tc.credential, nil)
		status(t, code, tc.want, value)
		if code != 200 {
			continue
		}
		if err := schema.Validate(value); err != nil {
			t.Fatalf("応答がOpenAPIに一致しません: %v", err)
		}
		v := obj(value)
		if v["color"] == nil || v["instanceId"] != diagnosticInstance || v["startedAt"] == nil {
			t.Fatalf("identity: %v", v)
		}
		foundPeriodic, foundHTTP := false, false
		for _, h := range v["hours"].([]any) {
			for _, raw := range obj(h)["rows"].([]any) {
				row := obj(raw)
				if row["name"] == "activity-test" {
					foundPeriodic = fmt.Sprint(row["failures"]) == "1" && fmt.Sprint(row["count"]) == "1" && fmt.Sprint(row["totalMilliseconds"]) == "1.5" && fmt.Sprint(row["maxMilliseconds"]) == "1.5"
				}
				if row["name"] == "/api/issues/{n}" && row["credential"] == "cli" {
					foundHTTP = true
				}
			}
		}
		if !foundPeriodic || !foundHTTP {
			t.Fatalf("missing activity: %v", v)
		}
	}
}
