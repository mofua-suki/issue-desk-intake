package fleet

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

func TestScopeLimitConfigBeforeSharedDefaults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  int
	}{
		{"missing", nil, 8}, {"explicit", float64(12), 12},
		{"eight", float64(8), 8}, {"zero", float64(0), 8},
		{"negative", float64(-1), 8}, {"fraction", 1.5, 8},
		{"string", "12", 8}, {"bool", true, 8},
		{"unsafe", float64(9007199254740992), 8},
		{"maximum", float64(9007199254740991), 9007199254740991},
		{"nan", math.NaN(), 8}, {"infinity", math.Inf(1), 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := assignmentsBody(t)
			if tc.value != nil {
				raw["intakeSplit"] = map[string]any{"files": tc.value}
			}
			a, err := NormalizeAssignments(raw)
			if err != nil || a.ScopeLimitFiles() != tc.want {
				t.Fatalf("config: %+v %v", a, err)
			}
			if tc.name == "missing" && Number(Object(a.Output["intakeSplit"])["files"]) != 10 {
				t.Fatal("shared default changed")
			}
		})
	}
	var unavailable *Assignments
	if unavailable.ScopeLimitFiles() != 8 || (&Assignments{}).ScopeLimitFiles() != 8 {
		t.Fatal("unavailable must use 8")
	}
	for _, invalidSplit := range []any{nil, "bad", []any{}} {
		raw := assignmentsBody(t)
		raw["intakeSplit"] = invalidSplit
		if _, err := NormalizeAssignments(raw); err == nil {
			t.Fatal("invalid assignment document accepted")
		}
	}
}

func TestScopeLimitShippingAssignments(t *testing.T) {
	// Exercise the document shipped to the server, not just synthetic configs.
	bytes, err := os.ReadFile("../../../assignments.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err = json.Unmarshal(bytes, &raw); err != nil {
		t.Fatal(err)
	}
	a, err := NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	value := Object(raw["intakeSplit"])["files"]
	want := 8
	if integer(value, 1, 9007199254740991) {
		want = int(Number(value))
	}
	if a.ScopeLimitFiles() != want {
		t.Fatalf("shipping files=%v widening=%d", value, a.ScopeLimitFiles())
	}
}
