package filingcheck

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/splitplan"
	"strings"
	"testing"
)

func proposal(work string, paths ...string) Record {
	return Record{"workType": work, "changeScope": Record{"known": true, "paths": paths}, "purpose": "目的", "acceptance": []any{"条件"}}
}

func TestSplitDependencyCheck(t *testing.T) {
	cases := []struct {
		name   string
		target Record
		deps   []any
		want   string
	}{
		{"contract and implementation", proposal("feature", "client/app.ts"), []any{1, 2}, "分割案2"},
		{"contract only", proposal("feature", "client/app.ts"), []any{1}, ""},
		{"skill markdown", proposal("docs", "docs/SKILL.md"), []any{1}, ""},
		{"agents markdown", proposal("docs", "AGENTS.md"), []any{1}, ""},
		{"unknown markdown", proposal("docs", "docs/other.md"), []any{1}, ""},
		{"markdown", proposal("feature", "README.md"), []any{2}, "分割案2"},
		{"docs work type", proposal("docs", "api/types.ts"), []any{2}, "分割案2"},
		{"implementation only", proposal("feature", "client/app.ts"), []any{2}, ""},
		{"exception", func() Record {
			p := proposal("docs", "README.md")
			p["purpose"] = "依頼者の指定による依存: 分割案1"
			return p
		}(), []any{1}, ""},
		{"unknown scope", Record{"changeScope": Record{"known": false}}, []any{1, 2}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) { calls++; return allowed(), nil })
			d["parentSplit"] = "parent"
			tc.target["dependsOn"] = tc.deps
			parent := Record{"id": "parent", "splitChildren": []any{"one", "two", d["id"]}, "splitIssues": []any{proposal("feature", "api/openapi/20-usage-ops.yaml", "api/types.ts"), proposal("feature", "server/main.go"), tc.target}}
			_, err := s.options.Records.Change(context.Background(), "drafts", "parent", func(*sql.Tx, Record) (Record, error) { return parent, nil })
			if err != nil {
				t.Fatal(err)
			}
			err = s.Check(context.Background(), d)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "修正後の依存") || calls != 0 || strings.Contains(err.Error(), "finish") || strings.Contains(err.Error(), "作り直し") || !strings.Contains(err.Error(), "自動補正が適用されていない") {
					t.Fatalf("error=%v calls=%d", err, calls)
				}
				saved, _, e := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
				if e != nil || saved["filingCheckCalls"] != nil || saved["filingCheckHold"] != nil {
					t.Fatalf("saved=%v error=%v", saved, e)
				}
			} else if err != nil || calls != 1 {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
		})
	}
}

func TestSplitDependencyExceptionAndClassification(t *testing.T) {
	p := proposal("docs", "api/types.ts")
	p["acceptance"] = []any{"依頼者の指定による依存: 分割案12"}
	split := toSplitProposal(p)
	if exceptions := splitplan.Exceptions(split); splitplan.ProposalKind(split) != "docs" || !exceptions["12"] || exceptions["1"] {
		t.Fatal(p)
	}
	for _, path := range []string{"SKILL.md", "docs/AGENTS.md", "docs/unknown.md"} {
		for _, work := range []string{"docs", "feature"} {
			if splitplan.ProposalKind(toSplitProposal(proposal(work, path))) != "implementation" {
				t.Fatal(path, work)
			}
		}
	}
	for _, path := range []string{"./api/openapi/a.yaml", "x/openapi/spec.yaml", "x/schemas/item.json", "x/item.schema.json", "x/item.proto"} {
		if splitplan.ProposalKind(toSplitProposal(proposal("feature", path))) != "contract" {
			t.Fatal(path)
		}
	}
	if splitplan.ProposalKind(toSplitProposal(proposal("feature", "docs/a.txt", "README.md"))) != "docs" {
		t.Fatal("docs paths")
	}
}

func TestSplitDependencyNoContractInPlan(t *testing.T) {
	calls := 0
	s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) { calls++; return allowed(), nil })
	d["parentSplit"] = "parent"
	target := proposal("feature", "client/app.ts")
	target["dependsOn"] = []any{1}
	parent := Record{"id": "parent", "splitChildren": []any{"one", d["id"]}, "splitIssues": []any{proposal("feature", "server/main.go"), target}}
	_, err := s.options.Records.Change(context.Background(), "drafts", "parent", func(*sql.Tx, Record) (Record, error) { return parent, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Check(context.Background(), d); err != nil || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestSplitDependencyReadFailureStopsBeforeMaterial(t *testing.T) {
	calls := 0
	s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) {
		calls++
		return allowed(), nil
	})
	d["parentSplit"] = "parent"
	target := proposal("feature", "client/app.ts")
	target["dependsOn"] = []any{1, 2}
	parent := Record{"id": "parent", "splitChildren": []any{"one", "two", d["id"]}, "splitIssues": []any{proposal("feature", "api/types.proto"), proposal("feature", "server/main.go"), target}}
	_, err := s.options.Records.Change(context.Background(), "drafts", "parent", func(*sql.Tx, Record) (Record, error) {
		return parent, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Check(ctx, d); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "分割元の相談") || calls != 0 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
	if err := s.Check(context.Background(), d); err == nil || !strings.Contains(err.Error(), "不要な依存") || calls != 0 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestSplitInstructionBudget(t *testing.T) {
	const maxInstructionsBytes = 17600
	if len(instructions) > maxInstructionsBytes {
		t.Fatalf("instructions: %d > %d", len(instructions), maxInstructionsBytes)
	}
}

func TestSplitInputBudgetBoundary(t *testing.T) {
	if inputLimit != 160000 {
		t.Fatalf("input limit = %d", inputLimit)
	}
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprintf("extra=%d", extra), func(t *testing.T) {
			calls := 0
			var sent string
			s, d, material := fixture(t, func(_ context.Context, input map[string]any) (map[string]any, error) {
				calls++
				sent = fleet.Text(input["input"])
				return allowed(), nil
			})
			(*material)["requests"] = []any{}
			target := obj((*material)["target"])
			target["body"] = ""
			available := inputLimit - len(instructions) - len("\n確認資料:\n") - len(s.schema) - len(raw(*material))
			if available <= 0 {
				t.Fatal("no room for material")
			}
			target["body"] = strings.Repeat("a", available+extra)
			if size := len(instructions) + len("\n確認資料:\n") + len(s.schema) + len(raw(*material)); size != inputLimit+extra {
				t.Fatal(size)
			}
			err := s.Check(context.Background(), d)
			if extra == 0 && (err != nil || calls != 1 || len(sent)+len(s.schema) != inputLimit) {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
			if extra == 1 && (err == nil || !strings.Contains(err.Error(), "入力上限") || calls != 0) {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
		})
	}
}

func TestSplitDependencyCorrectedPlanPasses(t *testing.T) {
	calls := 0
	s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) { calls++; return allowed(), nil })
	d["parentSplit"] = "parent"
	target := proposal("feature", "client/app.ts")
	target["dependsOn"] = []any{1}
	parent := Record{"id": "parent", "splitChildren": []any{"one", "two", d["id"]}, "splitIssues": []any{proposal("feature", "api/types.proto"), proposal("feature", "server/main.go"), target}}
	_, err := s.options.Records.Change(context.Background(), "drafts", "parent", func(*sql.Tx, Record) (Record, error) { return parent, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Check(context.Background(), d); err != nil || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestSplitDependencyDisplaysRetainedValues(t *testing.T) {
	calls := 0
	s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) { calls++; return allowed(), nil })
	d["parentSplit"] = "parent"
	target := proposal("docs", "README.md")
	target["dependsOn"] = []any{int32(1), 2, "unknown", 99}
	parent := Record{"id": "parent", "splitChildren": []any{"one", "two", d["id"]}, "splitIssues": []any{proposal("feature", "api/types.proto"), proposal("feature", "server/main.go"), target}}
	_, err := s.options.Records.Change(context.Background(), "drafts", "parent", func(*sql.Tx, Record) (Record, error) { return parent, nil })
	if err != nil {
		t.Fatal(err)
	}
	err = s.Check(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "修正後の依存: 分割案1、不明な依存(unknown)、分割案99") || strings.Contains(err.Error(), "分割案0") || calls != 0 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}
