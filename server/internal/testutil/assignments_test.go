package testutil

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
)

func TestShippedAssignmentsMatchesDeployedHosts(t *testing.T) {
	data, err := os.ReadFile(shippedAssignmentsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Hosts map[string]json.RawMessage `json:"hosts"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(raw.Hosts))
	for name := range raw.Hosts {
		want = append(want, name)
	}
	slices.Sort(want)
	got, ok := ShippedAssignments(t)["hosts"].([]string)
	if !ok || len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("shipped hosts = %v, want %v", got, want)
	}
}
