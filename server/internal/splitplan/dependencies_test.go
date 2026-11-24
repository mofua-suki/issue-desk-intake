package splitplan

import (
	"reflect"
	"testing"
)

func TestCorrectDependencies(t *testing.T) {
	p := func(path string, deps ...any) Proposal {
		return Proposal{ChangeScope: Scope{Known: true, Paths: []any{path}}, DependsOn: deps}
	}
	input := []Proposal{p("api/a.proto"), p("server/a.go"), p("client/a.go", 1, 2), p("README.md", 2, 1, 99, 7), p("docs/SKILL.md", 1, 2), p("README.md", 2), {ChangeScope: Scope{Known: false}}, p("README.md", 2)}
	input[5].Acceptance = []string{"依頼者の指定による依存: 分割案2"}
	original := append([]any(nil), input[2].DependsOn...)
	got, records := CorrectDependencies(input, map[int]bool{8: true})
	want := [][]any{nil, nil, {1}, {1, 99, 7}, {1}, {2}, nil, {2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dependencies: %#v", got)
	}
	if len(records) != 3 || records[0].ProposalNumber != 3 || records[0].RemovedDependency != 2 || records[0].Reason != ImplementationDependency || records[1].Reason != DocsDependency || records[2].ProposalNumber != 5 {
		t.Fatalf("records: %#v", records)
	}
	if !reflect.DeepEqual(records[0].Before, []any{1, 2}) || !reflect.DeepEqual(records[0].After, []any{1}) || !reflect.DeepEqual(input[2].DependsOn, original) {
		t.Fatalf("mutation or record: %#v", records[0])
	}
	for i := range input {
		input[i].DependsOn = got[i]
	}
	again, second := CorrectDependencies(input, map[int]bool{8: true})
	if !reflect.DeepEqual(got, again) || len(second) != 0 {
		t.Fatalf("not idempotent: %#v %#v", again, second)
	}
}

func TestCorrectDependenciesRetainsContractAndUnknownEdges(t *testing.T) {
	p := func(path string, deps ...any) Proposal {
		return Proposal{ChangeScope: Scope{Known: true, Paths: []any{path}}, DependsOn: deps}
	}
	input := []Proposal{p("api/a.proto"), p("server/a.go"), {ChangeScope: Scope{Known: false}}, p("README.md", 1), p("README.md", 1, int32(2), 3, 99, "unknown")}
	got, records := CorrectDependencies(input, nil)
	if !reflect.DeepEqual(got[3], []any{1}) || !reflect.DeepEqual(got[4], []any{1, 3, 99, "unknown"}) {
		t.Fatalf("dependencies: %#v", got)
	}
	if len(records) != 1 || records[0].ProposalNumber != 5 || records[0].RemovedDependency != 2 || records[0].Reason != DocsDependency || !reflect.DeepEqual(records[0].Before, input[4].DependsOn) || !reflect.DeepEqual(records[0].After, got[4]) {
		t.Fatalf("corrections: %#v", records)
	}
}
