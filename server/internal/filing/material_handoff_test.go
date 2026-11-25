package filing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/fleet"
)

func TestSplitHandoffReachesOnlyAssignedChild(t *testing.T) {
	d := splitReferenceFixture()
	row(fleet.Elements(d["splitIssues"])[1])["project"] = "owner/other"
	sum := sha256.Sum256([]byte("1: first\n2: second"))
	d["context"] = Record{"revision": strings.Repeat("a", 40), "projects": Record{"owner/other": Record{"revision": strings.Repeat("d", 40)}}, "handoff": []any{Record{"project": "owner/repo", "path": "docs/a.md", "revision": strings.Repeat("b", 40), "sha256": strings.Repeat("c", 64), "start": 1, "end": 2, "text": "1: first\n2: second", "textSha256": hex.EncodeToString(sum[:]), "children": []int{2}}}}
	children, err := PlanSplit(d)
	if err != nil {
		t.Fatal(err)
	}
	for i, child := range children {
		child["id"] = draftUUID
		p, err := Prepare(context.Background(), configFixture(), child)
		if err != nil {
			t.Fatal(err)
		}
		found := strings.Contains(p.Body, "1: first") && strings.Contains(p.Body, strings.Repeat("b", 40))
		if found != (i == 1) {
			t.Fatalf("child %d: %s", i+1, p.Body)
		}
	}
	doc := row(fleet.Elements(row(d["context"])["handoff"])[0])
	doc["children"] = "invalid"
	if _, err := PlanSplit(d); err == nil {
		t.Fatal("invalid handoff targets accepted")
	}
}

func TestPrepareRejectsMissingRequiredHandoff(t *testing.T) {
	d := draftFixture()
	d["body"] = "資料 owner/other:docs/a.md:1-2"
	if _, err := Prepare(context.Background(), configFixture(), d); err == nil || !strings.Contains(err.Error(), "owner/other:docs/a.md:1-2") {
		t.Fatal(err)
	}
}

func TestPrepareRejectsCorruptOrRevokedHandoff(t *testing.T) {
	d := draftFixture()
	d["body"] = "資料 owner/other:docs/a.md:1"
	content := "1: original"
	sum := sha256.Sum256([]byte(content))
	doc := Record{"project": "owner/other", "path": "docs/a.md", "revision": strings.Repeat("a", 40), "sha256": strings.Repeat("b", 64), "start": 1, "end": 1, "text": content, "textSha256": hex.EncodeToString(sum[:])}
	d["context"] = Record{"handoff": []any{doc}}
	doc["text"] = "1: damaged"
	if _, err := Prepare(context.Background(), configFixture(), d); err == nil {
		t.Fatal("corrupt body accepted")
	}
	doc["text"] = content
	cfg := configFixture()
	cfg.Project = func(repo string) error {
		if repo == "owner/other" {
			return errors.New("denied")
		}
		return nil
	}
	if _, err := Prepare(context.Background(), cfg, d); err == nil || !strings.Contains(err.Error(), "owner/other") {
		t.Fatalf("revoked project accepted: %v", err)
	}
}
