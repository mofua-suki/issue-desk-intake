package fleet

import (
	"path/filepath"
	"testing"
)

func TestRepositoryGroupsConfiguration(t *testing.T) {
	var original map[string]any
	if err := ReadJSON(filepath.Join("..", "..", "..", "assignments.json"), &original); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name           string
		value          any
		omitted, valid bool
	}{
		{"omitted", nil, true, true}, {"valid", []any{[]any{"Owner/one", "owner/two"}}, false, true},
		{"empty group", []any{[]any{}}, false, false}, {"single", []any{[]any{"a/b"}}, false, false},
		{"duplicate", []any{[]any{"a/b", "A/B"}}, false, false},
		{"multiple groups", []any{[]any{"a/b", "a/c"}, []any{"A/B", "a/d"}}, false, false},
		{"invalid name", []any{[]any{"a/b", "bad name"}}, false, false},
		{"null", nil, false, false}, {"wrong shape", []any{"a/b"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := Object(Clone(original))
			if tc.omitted {
				delete(raw, "repositoryGroups")
			} else {
				raw["repositoryGroups"] = tc.value
			}
			a, err := NormalizeAssignments(raw)
			if (err == nil) != tc.valid {
				t.Fatalf("error = %v", err)
			}
			if err == nil && tc.omitted && (a.RepositoryGroups == nil || len(a.RepositoryGroups) != 0) {
				t.Fatalf("omitted = %#v", a.RepositoryGroups)
			}
			if err == nil && !tc.omitted && (len(a.RepositoryGroups) != 1 || a.RepositoryGroups[0][0] != "Owner/one") {
				t.Fatalf("groups = %#v", a.RepositoryGroups)
			}
		})
	}
}
