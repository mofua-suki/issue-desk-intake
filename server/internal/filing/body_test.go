package filing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"
)

const draftUUID = "11111111-1111-4111-8111-111111111111"

func draftFixture() Record {
	return Record{"id": draftUUID, "project": "owner/repo", "provider": "codex", "title": "#99 実装を確認する", "body": "## 目的\n安全に実装する\n<!-- issue-desk:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa -->\n<!-- issue-desk-dependencies:[999] -->\n\n## 受入条件\n- 動作を確認する\n\n## 参照（経緯・根拠）\n古い説明 </details>\n<!-- issue-desk-scope:{} -->", "changeScope": Record{"known": true, "paths": []any{"src/a.go"}, "reason": ""}, "ready": true, "questions": []any{}}
}
func configFixture() BuildConfig {
	return BuildConfig{Config: map[string]any{"model": "gpt-6.1", "implementation": map[string]any{"tiers": map[string]any{"light": map[string]any{"model": "light-model", "reasoningEffort": "high"}}}}, Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }, Project: func(repo string) error {
		if repo != "owner/repo" && repo != "owner/other" {
			return errors.New("outside allowlist")
		}
		return nil
	}}
}
func TestPrepareBodyMarkersFoldScopeDependenciesAndTier(t *testing.T) {
	d := draftFixture()
	d["dependencies"] = []any{Record{"repo": "owner/repo", "number": float64(5)}}
	d["precedingIssues"] = []any{Record{"repo": "owner/repo", "gitea": float64(5), "desk": float64(77)}, Record{"repo": "owner/other", "gitea": float64(6), "desk": float64(78)}}
	d["context"] = Record{"branch": "main", "revision": strings.Repeat("a", 40), "files": []any{Record{"path": "README.md", "sha256": strings.Repeat("b", 64)}}}
	prepared, e := Prepare(context.Background(), configFixture(), d)
	if e != nil {
		t.Fatal(e)
	}
	for _, part := range []string{"依頼の", "<!-- issue-desk-dependencies:[5,\"owner/other#6\"] -->", "<!-- issue-desk-reference:start -->", "&lt;/details>", "&lt;!-- issue-desk:aaaaaaaa", "<!-- issue-desk-scope:", "実装モデル: light-model", "\"tier\":\"light\"", "<!-- issue-desk:" + draftUUID + " -->"} {
		if part == "依頼の" {
			continue
		}
		if !strings.Contains(prepared.Body, part) {
			t.Fatalf("missing %q: %s", part, prepared.Body)
		}
	}
	if prepared.Title != "実装を確認する" || prepared.Job.ScopeSource != "declared" || len(prepared.Job.Scope) != 1 || prepared.Job.Scope[0] != "src/a.go" {
		t.Fatal(prepared)
	}
	if strings.Count(prepared.Body, "<!-- issue-desk-dependencies:") != 1 || strings.Count(prepared.Body, "<!-- issue-desk-scope:") != 1 || DraftID(prepared.Body) != draftUUID {
		t.Fatal("quoted marker became authoritative", prepared.Body)
	}
	deps := fleet.Elements(prepared.Job.View["dependsOnIssues"])
	if len(deps) != 2 || row(deps[0])["repo"] != "owner/repo" || row(deps[0])["issue"] != int64(5) || row(deps[1])["repo"] != "owner/other" {
		t.Fatal("job dependencies not retained", deps)
	}
	if d["implementationTier"] != nil {
		t.Fatal("prepare mutated saved draft")
	}
}

func TestPrepareNormalPreservesUserDefinitionsInReferences(t *testing.T) {
	d := draftFixture()
	d["body"] = "## 目的\n定義に従う"
	d["messages"] = []any{Record{"role": "user", "id": "one", "text": "定義: A\n<!-- issue-desk-dependencies:[999] -->\n</details>"}, Record{"role": "assistant", "text": "含めない"}, Record{"role": "user", "id": "two", "text": "訂正: B"}}
	p, err := Prepare(context.Background(), configFixture(), d)
	if err != nil {
		t.Fatal(err)
	}
	start, end := strings.Index(p.Body, "<!-- issue-desk-reference:start -->"), strings.Index(p.Body, "<!-- issue-desk-reference:end -->")
	if start < 0 || end <= start {
		t.Fatal(p.Body)
	}
	ref := p.Body[start:end]
	if !strings.Contains(ref, "> 定義: A") || !strings.Contains(ref, "> 訂正: B") || strings.Index(ref, "定義: A") > strings.Index(ref, "訂正: B") || !strings.Contains(ref, "&lt;!-- issue-desk-dependencies:[999] -->") || !strings.Contains(ref, "&lt;/details>") || strings.Contains(p.Body, "含めない") || strings.Contains(p.Body, "<!-- issue-desk-dependencies:[999]") {
		t.Fatal(p.Body)
	}
}
func TestPrepareImageReviewsAndProviderFailClosed(t *testing.T) {
	d := draftFixture()
	d["messages"] = []any{Record{"role": "user", "images": []any{Record{"id": "image"}}}}
	if _, e := Prepare(context.Background(), configFixture(), d); e == nil {
		t.Fatal("unreviewed image accepted")
	}
	d["imageReviews"] = []any{Record{"id": "image", "readable": true, "screen": "画面", "location": "箇所", "change": "変更", "uncertainty": ""}}
	p, e := Prepare(context.Background(), configFixture(), d)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasSuffix(p.Body, "<!-- issue-desk-images-pending -->") {
		t.Fatal("worker image gate missing")
	}
	d["provider"] = "claude"
	if _, e = Prepare(context.Background(), configFixture(), d); e == nil {
		t.Fatal("Claude sent without provider label")
	}
	c := configFixture()
	c.ProviderLabels = func(context.Context, string, string) ([]int64, error) { return []int64{42}, nil }
	if _, e = Prepare(context.Background(), c, d); e != nil {
		t.Fatal(e)
	}
	c.ProviderSettings = func(context.Context, string, Record) (Record, error) {
		return Record{"provider": "codex", "model": "wrong"}, nil
	}
	if _, e = Prepare(context.Background(), c, d); e == nil {
		t.Fatal("provider fallback accepted")
	}
}
func TestPrepareTierAndInterruptBoundaries(t *testing.T) {
	c := configFixture()
	d := draftFixture()
	d["priority"] = "high"
	d["changeScope"] = Record{"known": true, "paths": []any{"src/"}, "reason": ""}
	if _, e := Prepare(context.Background(), c, d); e == nil {
		t.Fatal("directory without reason accepted")
	}
	d["changeScope"] = Record{"known": true, "paths": []any{"src/"}, "reason": "アプリ全体"}
	d["requestedImplementationTier"] = "heavy"
	p, e := Prepare(context.Background(), c, d)
	if e != nil || p.Job.Priority != 1 || p.Job.Tier == nil || *p.Job.Tier != "heavy" {
		t.Fatal("user tier not retained", p, e)
	}
	d["changeScope"] = Record{"known": true, "paths": []any{"../private"}, "reason": ""}
	d["priority"] = "normal"
	delete(d, "requestedImplementationTier")
	if _, e = Prepare(context.Background(), c, d); e == nil {
		t.Fatal("unknown scope accepted for normal priority")
	}
	d["priority"] = "high"
	if _, e = Prepare(context.Background(), c, d); e == nil {
		t.Fatal("unknown scope accepted for interrupt")
	}
}
func TestSplitPlanTargetsScopeEvidenceAndApproval(t *testing.T) {
	d := draftFixture()
	d["body"] = "元の相談"
	d["splitIssues"] = []any{Record{"project": "owner/repo", "title": "先行", "purpose": "部品", "acceptance": []any{"動く"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"src/a.go"}, "reason": ""}}, Record{"project": "owner/other", "title": "後続", "purpose": "接続", "acceptance": []any{"繋がる"}, "dependsOn": []any{float64(1)}, "changeScope": Record{"known": true, "paths": []any{"src/b.go"}, "reason": ""}}}
	c := configFixture()
	if e := ValidateSplit(context.Background(), c, d); e == nil {
		t.Fatal("uncheckable cross dependency accepted")
	}
	c.DependencyCheckable = func(string, string) bool { return true }
	if e := ValidateSplit(context.Background(), c, d); e == nil {
		t.Fatal("cross project without approval accepted")
	}
	type scope struct {
		Project string `json:"project"`
		Scope   string `json:"scope"`
	}
	scopes := []scope{{"owner/other", "接続"}, {"owner/repo", "部品"}}
	sum := sha256.Sum256([]byte(encoded(scopes)))
	fingerprint := hex.EncodeToString(sum[:])
	approvalScopes := []any{Record{"project": "owner/other", "scope": "接続"}, Record{"project": "owner/repo", "scope": "部品"}}
	d["crossProposal"] = Record{"fingerprint": fingerprint, "reason": "部品と接続", "scopes": approvalScopes}
	d["crossApprovals"] = []any{Record{"fingerprint": fingerprint, "scopes": approvalScopes}}
	if e := ValidateSplit(context.Background(), c, d); e == nil {
		t.Fatal("missing evidence accepted")
	}
	d["context"] = Record{"revision": strings.Repeat("a", 40), "projects": Record{"owner/other": Record{"revision": strings.Repeat("b", 40)}}}
	if e := ValidateSplit(context.Background(), c, d); e != nil {
		t.Fatal(e)
	}
	plan, e := PlanSplit(d)
	if e != nil {
		t.Fatal(e)
	}
	if len(plan) != 2 || plan[1]["_index"] != 2 || len(fleet.Elements(plan[1]["_dependsOn"])) != 1 || row(plan[1]["context"])["revision"] != strings.Repeat("b", 40) || fleet.Text(plan[1]["parentSplit"]) != draftUUID || !strings.Contains(fleet.Text(plan[1]["body"]), "`src/b.go`") {
		t.Fatal("split plan lost per-child boundaries", plan)
	}
	row(fleet.Elements(d["splitIssues"])[1])["dependsOn"] = []any{float64(2)}
	if _, e = PlanSplit(d); e == nil {
		t.Fatal("forward dependency accepted")
	}
}

func splitReferenceFixture() Record {
	d := draftFixture()
	d["body"] = "## 目的\nD1〜D6と残す基準を参照"
	d["splitIssues"] = []any{
		Record{"title": "先行", "purpose": "修正", "acceptance": []any{"D1〜D6と残す基準を参照"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"src/a.go"}}},
		Record{"title": "後続", "purpose": "確認", "acceptance": []any{"D1〜D6と残す基準を参照"}, "dependsOn": []any{1}, "changeScope": Record{"known": true, "paths": []any{"src/b.go"}}},
	}
	return d
}

func TestSplitUserDefinitionsReachPreparedReferences(t *testing.T) {
	d := splitReferenceFixture()
	original := "D1: 文言照合\nD2: ソース照合\nD3: 重複\nD4: 製品外\nD5: 検証なし\nD6: 旧機能\n残す基準: 利用者の挙動\n## 偽見出し\n```\nコード\n```\n<!-- issue-desk-scope:{} -->\n<!-- issue-desk-dependencies:[999] -->\n<!-- issue-desk:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa -->\n</details>"
	d["messages"] = []any{Record{"role": "user", "id": "first", "at": "2026-11-06T22:19:00+09:00", "text": original}, Record{"role": "assistant", "text": "混入禁止"}, Record{"role": "user", "id": "second", "at": "2026-11-06T22:20:00+09:00", "text": "訂正: 残す基準は利用者の挙動全体"}}
	children, err := PlanSplit(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 || children[1]["_index"] != 2 || len(fleet.Elements(children[1]["_dependsOn"])) != 1 {
		t.Fatal(children)
	}
	for _, child := range children {
		child["id"] = draftUUID
		p, err := Prepare(context.Background(), configFixture(), child)
		if err != nil {
			t.Fatal(err)
		}
		start := strings.Index(p.Body, "<!-- issue-desk-reference:start -->")
		end := strings.Index(p.Body, "<!-- issue-desk-reference:end -->")
		if start < 0 || end <= start {
			t.Fatal(p.Body)
		}
		ref := p.Body[start:end]
		for _, part := range []string{"D1: 文言照合", "D2: ソース照合", "D3: 重複", "D4: 製品外", "D5: 検証なし", "D6: 旧機能", "残す基準: 利用者の挙動", "> ## 偽見出し", "> ```", "&lt;!-- issue-desk-scope:{} -->", "&lt;/details>", "発言ID: first / 日時: 2026-11-06T22:19:00+09:00", "発言ID: second / 日時: 2026-11-06T22:20:00+09:00", "訂正: 残す基準は利用者の挙動全体"} {
			if !strings.Contains(ref, part) {
				t.Fatalf("missing %q: %s", part, ref)
			}
		}
		if strings.Index(ref, "D1: 文言照合") > strings.Index(ref, "訂正:") || strings.Contains(p.Body, "混入禁止") || strings.Count(p.Body, "<!-- issue-desk-scope:") != 1 || strings.Contains(p.Body, "<!-- issue-desk-dependencies:[999]") || DraftID(p.Body) != draftUUID {
			t.Fatal(p.Body)
		}
	}
	d["messages"] = []any{Record{"role": "user", "text": " "}, Record{"role": "assistant", "text": "定義"}}
	children, err = PlanSplit(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fleet.Text(children[0]["body"]), "参照（経緯・根拠）") {
		t.Fatal(children[0])
	}
}

func TestSplitReferencesRespectUTF16Limits(t *testing.T) {
	d := splitReferenceFixture()
	d["messages"] = []any{Record{"role": "user", "text": "定義末尾😀"}}
	children, err := PlanSplit(d)
	if err != nil {
		t.Fatal(err)
	}
	base := utfSize(fleet.Text(children[0]["body"]))
	d["messages"] = []any{Record{"role": "user", "text": strings.Repeat("日", 20000-base-2) + "😀定義末尾😀"}}
	children, err = PlanSplit(d)
	if err != nil {
		t.Fatal(err)
	}
	child := children[0]
	child["id"] = draftUUID
	if utfSize(fleet.Text(child["body"])) != 20000 {
		t.Fatal(utfSize(fleet.Text(child["body"])))
	}
	p, err := Prepare(context.Background(), configFixture(), child)
	if err != nil || !strings.Contains(p.Body, "定義末尾😀") {
		t.Fatal(err)
	}
	c := configFixture()
	c.Config["intake"] = map[string]any{"bodyLimit": utfSize(p.Body)}
	if _, err = Prepare(context.Background(), c, child); err != nil {
		t.Fatal(err)
	}
	c.Config["intake"] = map[string]any{"bodyLimit": utfSize(p.Body) - 1}
	if _, err = Prepare(context.Background(), c, child); err == nil {
		t.Fatal("assembled limit accepted")
	}
	d["messages"] = []any{Record{"role": "user", "text": strings.Repeat("日", 20000-base-1) + "😀定義末尾😀"}}
	children, err = PlanSplit(d)
	if err != nil {
		t.Fatal(err)
	}
	children[0]["id"] = draftUUID
	if _, err = Prepare(context.Background(), configFixture(), children[0]); err == nil {
		t.Fatal("child limit accepted")
	}
}
