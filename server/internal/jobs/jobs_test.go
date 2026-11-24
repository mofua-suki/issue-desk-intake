package jobs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/contract"
)

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func ptr[T any](value T) *T { return &value }
func code(t *testing.T, err error, status int, want string) {
	t.Helper()
	var problem *contract.Problem
	if !errors.As(err, &problem) || problem.Status != status || want != "" && problem.Code != want {
		t.Fatalf("wrong failure: %v; want HTTP %d code %s", err, status, want)
	}
}

func TestLeaseFence(t *testing.T) {
	job := &Job{Status: "running", LeaseHost: ptr("nas"), LeaseEpoch: 7, LeaseExpiresAt: ptr(iso(now.Add(time.Minute)))}
	if p := CheckFence(job, "nas", 7, now, false); p != nil {
		t.Fatal(p)
	}
	code(t, CheckFence(job, "other", 7, now, false), 409, "lease_lost")
	code(t, CheckFence(job, "nas", 6, now, false), 409, "lease_lost")
	code(t, CheckFence(job, "nas", 0, now, false), 400, "")
	code(t, CheckFence(job, "nas", 7, now.Add(time.Minute), false), 409, "lease_lost")
	job.Status = "merged"
	job.LeaseExpiresAt = nil
	code(t, CheckFence(job, "nas", 7, now, false), 409, "lease_lost")
	if p := CheckFence(job, "nas", 7, now, true); p != nil {
		t.Fatal(p)
	}
	code(t, CheckFence(job, "other", 7, now, true), 409, "lease_lost")
}

func TestReportValidationAndOptionalNull(t *testing.T) {
	var report Report
	if err := json.Unmarshal([]byte(`{"epoch":7,"status":"running","branch":null,"pr":42}`), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Branch.Present || report.Branch.Value != nil || !report.PR.Present || *report.PR.Value != 42 || report.HeadSHA.Present {
		t.Fatal("missing/null distinction lost")
	}
	if err := validateReport(report); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"status":"closed"}`,
		`{"status":"held"}`,
		`{"status":"running","holdReason":"question"}`,
		`{"status":"running","pr":0}`,
		`{"status":"running","headSha":"bad"}`,
	} {
		var invalid Report
		if err := json.Unmarshal([]byte(body), &invalid); err != nil {
			t.Fatal(err)
		}
		code(t, validateReport(invalid), 400, "")
	}
}

func TestViewPreservesAuthorityAndCumulativeUnknown(t *testing.T) {
	job := &Job{Generation: 3, View: map[string]any{"reviewHolds": float64(4), "reviewHoldsLowerBound": float64(4), "reviewHoldsUnknown": false, "instructions": "server"}}
	view, err := reportView(job, Report{Status: "running", View: map[string]any{"reviewHolds": float64(1), "instructions": "client", "phase": "review"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := view["instructions"]; ok {
		t.Fatal("client overwrote server-owned view")
	}
	if view["reviewHolds"] != int64(4) || view["reviewHoldsUnknown"] != false {
		t.Fatalf("cumulative count decreased: %v", view)
	}
	view = mergeHolds(job.View, map[string]any{})
	if view["reviewHolds"] != nil || view["reviewHoldsUnknown"] != true {
		t.Fatal("missing count was treated as zero")
	}
	view = mergeHolds(view, map[string]any{"reviewHolds": float64(8)})
	if view["reviewHolds"] != nil || view["reviewHoldsLowerBound"] != int64(8) {
		t.Fatal("later valid report erased an unknown interval")
	}
	job.View = map[string]any{}
	if !sameReport(&Job{Status: "merged", View: map[string]any{}}, Report{Status: "merged"}, mergeHolds(map[string]any{}, nil)) {
		t.Fatal("derived count fields invalidated a compatible terminal receipt")
	}
}

func TestPrerequisiteStateAndDrainExpiry(t *testing.T) {
	var report Report
	body := `{"status":"held","holdReason":"prerequisite_missing","result":{"status":"failed","questions":[],"prerequisiteMissing":{"reason":"成果物が必要","artifacts":[{"artifact":"API仕様","repo":"owner/repo","issue":1}]}}}`
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatal(err)
	}
	job := &Job{Generation: 2, View: map[string]any{}}
	view, err := reportView(job, report)
	if err != nil {
		t.Fatal(err)
	}
	if !equivalent(view["prerequisiteMissingGenerations"], []any{float64(2)}) {
		t.Fatalf("generation exclusion missing: %v", view)
	}
	job.Status = "held"
	job.HoldReason = ptr("question")
	job.Result = report.Result
	job.View = view
	restorePrerequisiteReason(job)
	if *job.HoldReason != "prerequisite_missing" {
		t.Fatal("stored question reason was not restored")
	}
	report.Result["questions"] = []any{"unanswered"}
	if _, err = reportView(job, report); err == nil {
		t.Fatal("invalid prerequisite result accepted")
	}
	if !activeDrain(map[string]any{"at": iso(now)}, now.Add(149*time.Minute), 150*time.Minute) {
		t.Fatal("implicit drain duration not applied")
	}
	if activeDrain(map[string]any{"at": iso(now)}, now.Add(150*time.Minute), 150*time.Minute) {
		t.Fatal("expired drain remained active")
	}
	if !activeDrain(map[string]any{"until": iso(now.Add(-time.Minute)), "hostd": map[string]any{"until": iso(now.Add(time.Minute))}}, now, 150*time.Minute) {
		t.Fatal("nested hostd drain ignored")
	}
}

func TestJSONUnitsMatchStringifyLimits(t *testing.T) {
	for _, test := range []struct {
		value any
		units int
	}{
		{map[string]any{"x": "あ😀"}, 11},
		{map[string]any{"x": "<\u2028"}, 10},
		{json.Number("1.0"), 1},
		{json.Number("1e20"), 21},
		{json.Number("1e21"), 5},
		{json.Number("1e-7"), 4},
	} {
		if got, ok := jsonUnits(test.value); !ok || got != test.units {
			t.Errorf("JSON units for %#v = %d,%v; want %d", test.value, got, ok, test.units)
		}
	}
	if checkObject(map[string]any{"x": strings.Repeat("あ", 32760)}, "view") != nil {
		t.Fatal("UTF-8 bytes incorrectly used as UTF-16 units")
	}
	code(t, checkObject(map[string]any{"x": strings.Repeat("あ", 32761)}, "view"), 400, "")
}

func TestWorkerCannotForgeServerImplementationResumeCount(t *testing.T) {
	current := &Job{View: map[string]any{"implementationResumes": int64(2), "reviewHolds": float64(0)}}
	view, e := reportView(current, Report{View: map[string]any{"implementationResumes": float64(999), "reviewHolds": float64(0)}})
	if e != nil {
		t.Fatal(e)
	}
	if _, present := view["implementationResumes"]; present {
		t.Fatal("worker report can change resume exclusions")
	}
}
