package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"issue-desk-intake/server/internal/fleet"
)

func shippedAssignmentsPath(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate shipped assignments")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "assignments.json")
}

// ShippedAssignments loads and normalizes the assignments deployed with the server.
func ShippedAssignments(t testing.TB) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(shippedAssignmentsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	a, err := fleet.NormalizeAssignments(input)
	if err != nil {
		t.Fatal(err)
	}
	return a.Output
}
