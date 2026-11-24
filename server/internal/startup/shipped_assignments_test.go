package startup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// shippedAssignments reads the shipped assignments for fixtures whose fleet
// only describes their own hosts. Test-only Mac hosts are validated against
// their fleet entry (darwin and a fixed test command), so they are left out
// together with their worker accounts.
func shippedAssignments(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "assignments.json"))
	if err != nil {
		t.Fatal(err)
	}
	assignments := map[string]any{}
	if err = json.Unmarshal(raw, &assignments); err != nil {
		t.Fatal(err)
	}
	hosts, _ := assignments["hosts"].(map[string]any)
	accounts, _ := assignments["workerAccounts"].(map[string]any)
	for host, entry := range hosts {
		if value, _ := entry.(map[string]any); value["role"] == "test-only" {
			delete(hosts, host)
			delete(accounts, host)
		}
	}
	return assignments
}

// shippedAssignmentsPath writes shippedAssignments to a temporary file.
func shippedAssignmentsPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "assignments.json")
	writeConfig(t, path, shippedAssignments(t))
	return path
}
