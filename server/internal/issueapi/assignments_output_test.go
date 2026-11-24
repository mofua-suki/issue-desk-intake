package issueapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/testutil"
)

func fixtureAssignments(t *testing.T) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}},"other":{"maxImplementations":1}},"repositories":{}}`), &raw); err != nil {
		t.Fatal(err)
	}
	a, err := fleet.NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	return a.Output
}

type handoverAPI struct{ *fakeAPI }

func (a handoverAPI) Pages(ctx context.Context, route string) ([]Object, error) {
	if strings.HasSuffix(route, "/labels") {
		return []Object{{"id": 42, "name": "host/gt12"}, {"id": 43, "name": "host/surface"}}, nil
	}
	return a.fakeAPI.Pages(ctx, route)
}
func (a handoverAPI) Do(ctx context.Context, method, route string, data, out any) error {
	if method == "PUT" && strings.HasSuffix(route, "/labels") {
		return nil
	}
	return a.fakeAPI.Do(ctx, method, route, data, out)
}

func TestShippedAssignmentsHandoverPostgres(t *testing.T) {
	output := testutil.ShippedAssignments(t)
	if _, ok := output["hosts"].([]string); !ok {
		t.Fatalf("hosts must be an array: %T", output["hosts"])
	}
	f := newFixture(t)
	f.options.Config["assignments"] = output
	f.options.Gitea = handoverAPI{f.api}
	f.api.issue["body"] = "<!-- issue-desk:11111111-1111-4111-8111-111111111111 -->"
	f.restart()
	for i, tc := range []struct {
		host   string
		status int
	}{{"gt12", 200}, {"unknown-host", 400}, {"surface", 200}, {"a9-max", 400}} {
		input := Object{"id": fmt.Sprintf("%08d-1111-4111-8111-111111111111", i+1), "project": "owner/repo", "number": 2, "host": tc.host}
		code, value := f.call("POST", "/api/handover", "screen", input)
		if code != tc.status || (code == 200 && fleet.ObjectOrEmpty(value)["host"] != tc.host) {
			t.Fatalf("%s: %d %v", tc.host, code, value)
		}
	}
	if f.api.commentCalls != 2 {
		t.Fatalf("comments: %d", f.api.commentCalls)
	}
}

func TestShippedAssignmentsHome(t *testing.T) {
	output := testutil.ShippedAssignments(t)
	s := &Service{}
	for repo, value := range fleet.ObjectOrEmpty(output["repositories"]) {
		if got := s.home(strings.ToUpper(repo), Object{"assignments": output}); got != value {
			t.Fatalf("%s: %q != %v", repo, got, value)
		}
	}
	if got := s.home("unknown/repo", Object{"assignments": output}); got != "" {
		t.Fatal(got)
	}
}

func TestCloseOwnerUsesAssignmentRepresentativePostgres(t *testing.T) {
	f := newFixture(t)
	input := Object{"id": "55555555-5555-4555-8555-555555555555", "project": "owner/repo", "number": 1, "reason": "resolved", "description": "対応済み", "comment": "終了"}
	for _, tc := range []struct {
		name         string
		repositories map[string]any
	}{{"other host", map[string]any{"owner/repo": "other"}}, {"no mapping", map[string]any{}}} {
		t.Run(tc.name, func(t *testing.T) {
			assignments := fixtureAssignments(t)
			assignments["repositories"] = tc.repositories
			f.options.Config["assignments"] = assignments
			f.restart()
			code, value := f.call("POST", "/api/issues/close", "nas", input)
			if code != 403 {
				t.Fatalf("close without ownership: %d %v", code, value)
			}
		})
	}
}

func TestAssignmentHasHost(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		host  string
		want  bool
	}{
		{"normalized", []string{"nas", "other"}, "other", true},
		{"decoded", []any{"nas", "other"}, "nas", true},
		{"unknown", []any{"nas"}, "other", false},
		{"case sensitive", []string{"nas"}, "NAS", false},
		{"missing", nil, "nas", false},
		{"empty", []any{}, "nas", false},
		{"mixed", []any{map[string]any{"host": "nas"}, 1, "other"}, "nas", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := assignmentHasHost(tc.value, tc.host); got != tc.want {
				t.Fatalf("assignmentHasHost(%v, %q) = %v, want %v", tc.value, tc.host, got, tc.want)
			}
		})
	}
}
