package issueapi

import (
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"sort"
	"testing"
)

// Worker固定契約: mofua-suki/issue-desk-worker origin/trunk 8911667,
// internal/runtime/job_failure.go:19-26 と execution_failure.go:18-55。
func TestExecutionErrorWorkerContract(t *testing.T) {
	worker := map[string]any{"kind": "api_rejected", "stage": "publishing", "reason": "rejected", "giteaHttpStatus": "503", "intakeHttpStatus": "409", "intakeCode": "turn_lost"}
	got := fleet.ObjectOrEmpty(leaseDiagnosticsProjection(Object{"lastExecutionError": worker})["lastExecutionError"])
	keys := func(m map[string]any) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if !reflect.DeepEqual(keys(worker), keys(got)) {
		t.Fatalf("Worker fields %v, projection %v", keys(worker), keys(got))
	}
	for k, v := range worker {
		if got[k] != v {
			t.Errorf("%s: %v", k, got[k])
		}
	}
	for _, code := range []string{"request_failed", "turn_lost", "contract_mismatch"} {
		got := fleet.ObjectOrEmpty(leaseDiagnosticsProjection(Object{"lastExecutionError": map[string]any{"kind": "api_rejected", "intakeCode": code}})["lastExecutionError"])
		if got["intakeCode"] != code {
			t.Errorf("code %s: %v", code, got)
		}
	}
}

// Worker固定契約: mofua-suki/issue-desk-worker origin/trunk 8911667,
// internal/runtime/job_failure.go:84-104 と execution_failure.go:18-55。
func TestExecutionErrorKindsAndIntakeDiagnostics(t *testing.T) {
	kinds := []string{"lease_expired", "lease_lost", "canceled", "timeout", "api_rejected", "transport_failure", "gitea_api_error", "filesystem_error", "provider_report_error", "provider_usage_limit", "execution_error", "future_kind"}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			raw := map[string]any{"kind": kind, "intakeHttpStatus": "409", "intakeCode": "turn_lost", "giteaHttpStatus": "503"}
			got := fleet.ObjectOrEmpty(leaseDiagnosticsProjection(Object{"lastExecutionError": raw})["lastExecutionError"])
			if got["kind"] != kind || got["giteaHttpStatus"] != "503" {
				t.Fatalf("kind or Gitea status lost: %v", got)
			}
			for _, key := range []string{"intakeHttpStatus", "intakeCode"} {
				value, present := got[key]
				if present != (kind == "api_rejected") || (present && value != raw[key]) {
					t.Errorf("%s: %v", key, got)
				}
			}
		})
	}
	for _, kind := range []any{nil, 42, []any{"api_rejected"}} {
		got := fleet.ObjectOrEmpty(leaseDiagnosticsProjection(Object{"lastExecutionError": map[string]any{"kind": kind, "intakeHttpStatus": "409", "intakeCode": "turn_lost"}})["lastExecutionError"])
		if _, ok := got["intakeHttpStatus"]; ok {
			t.Errorf("invalid kind: %v", got)
		}
		if _, ok := got["intakeCode"]; ok {
			t.Errorf("invalid kind: %v", got)
		}
	}
}

func TestExecutionErrorInvalidDiagnostics(t *testing.T) {
	for _, key := range []string{"intakeHttpStatus", "giteaHttpStatus"} {
		for _, value := range []any{"100", "599", "200"} {
			got := fleet.ObjectOrEmpty(leaseDiagnosticsProjection(Object{"lastExecutionError": map[string]any{"kind": "api_rejected", key: value}})["lastExecutionError"])
			if got[key] != value {
				t.Errorf("%s %v rejected", key, value)
			}
		}
		for _, value := range []any{"099", "600", "99", "1000", "1a0", "１００", " 200", "", 200, nil, true} {
			got := fleet.ObjectOrEmpty(leaseDiagnosticsProjection(Object{"lastExecutionError": map[string]any{"kind": "api_rejected", key: value}})["lastExecutionError"])
			if _, ok := got[key]; ok {
				t.Errorf("%s %v accepted", key, value)
			}
		}
	}
	for _, value := range []any{"unknown", "REQUEST_FAILED", "", 1, nil, true} {
		got := fleet.ObjectOrEmpty(leaseDiagnosticsProjection(Object{"lastExecutionError": map[string]any{"kind": "api_rejected", "intakeCode": value}})["lastExecutionError"])
		if _, ok := got["intakeCode"]; ok {
			t.Errorf("code %v accepted", value)
		}
	}
}
