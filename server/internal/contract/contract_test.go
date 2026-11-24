package contract

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestErrorEnvelopePreservesFencingAndRetrySafety(t *testing.T) {
	problem := NewProblem(409, "lease is no longer owned", "lease_lost")
	response := httptest.NewRecorder()
	WriteProblem(response, problem)
	if response.Code != 409 || response.Header().Get(Header) != Version {
		t.Fatalf("wrong HTTP result: %d %v", response.Code, response.Header())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "lease_lost" || body["retryable"] != "same-request" || body["safeToChange"] != false {
		t.Fatalf("lost fencing or retry information: %v", body)
	}
	temporary := NewProblem(503, "temporarily unavailable", "")
	if temporary.Code != "TEMPORARY_UNAVAILABLE" || temporary.Recovery.Action != "retry" {
		t.Fatalf("temporary failure mapped incorrectly: %+v", temporary)
	}
}

func TestPinnedContractArtifacts(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	api := filepath.Join(filepath.Dir(source), "../../../api")
	read := func(name string, target any) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(api, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, target); err != nil {
			t.Fatal(err)
		}
	}
	var wire struct {
		Version  string              `json:"version"`
		Header   string              `json:"header"`
		Recovery map[string]Recovery `json:"recovery"`
	}
	read("contract.json", &wire)
	if wire.Version != Version || wire.Header != Header {
		t.Fatal("wire version and header disagree with the API contract")
	}
	if !reflect.DeepEqual(wire.Recovery, recoveries) {
		t.Fatal("recovery data diverged from the API contract")
	}
	types, err := os.ReadFile(filepath.Join(api, "types.ts"))
	if err != nil || !strings.Contains(string(types), "'"+Version+"'") {
		t.Fatal("browser wire version diverged from the API contract")
	}
	var spec struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	read("openapi.json", &spec)
	for _, required := range []string{"/api/v1/jobs/next", "/api/v1/jobs/{id}/lease", "/api/session", "/api/issues"} {
		if _, ok := spec.Paths[required]; !ok {
			t.Errorf("missing route contract: %s", required)
		}
	}
}
