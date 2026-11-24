package consultationai

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/filing"
)

func TestSplitChildrenDefinitionsReachFiledIssue(t *testing.T) {
	ctx := context.Background()
	s, _ := testService(t, nil)
	parent := Record{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/app", "owner": "owner", "_ephemeral": true}
	result, err := s.SplitChildren(ctx, parent, []string{"定義: 子Aの基準", "定義: 子Bの基準"})
	if err != nil {
		t.Fatal(err)
	}
	children := fleet.Elements(result["consultationChildren"])
	if len(children) != 2 {
		t.Fatalf("expected two child consultations, got %d", len(children))
	}
	for i, id := range children {
		child, found, err := s.options.Records.Get(ctx, "drafts", fleet.Text(id))
		if err != nil || !found {
			t.Fatalf("child: %v %v", found, err)
		}
		child["title"] = "修正"
		child["body"] = "## 目的\n定義に従う"
		child["changeScope"] = Record{"known": true, "paths": []any{"a.go"}}
		p, err := filing.Prepare(ctx, filing.BuildConfig{Config: map[string]any{"model": "test-model"}}, filing.Record(child))
		if err != nil {
			t.Fatal(err)
		}
		want, other := "子Aの基準", "子Bの基準"
		if i == 1 {
			want, other = other, want
		}
		start, end := strings.Index(p.Body, "<!-- issue-desk-reference:start -->"), strings.Index(p.Body, "<!-- issue-desk-reference:end -->")
		if start < 0 || end <= start || !strings.Contains(p.Body[start:end], want) || strings.Contains(p.Body, other) {
			t.Fatal(p.Body)
		}
	}
}
