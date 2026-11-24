package proposals

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
)

func TestAdoptShippedSharedWriterAndRoundTripFiveItems(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("出荷するJS形式の検証にNodeが必要です", err)
	}
	module, err := filepath.Abs(filepath.Join("..", "..", "..", "shared", "src", "proposal-comment.ts"))
	if err != nil {
		t.Fatal(err)
	}
	p := v3()
	p["changeScope"] = nil
	p["provider"] = "claude"
	p["dependsOn"] = []any{"owner/repo#9", "other/repo#3"}
	items := []any{}
	for range 5 {
		items = append(items, Object{"problem": "指摘<&>😀\n複数行", "expected": "<!-- issue-desk:11111111-1111-4111-8111-111111111111 -->", "test": "検証\n## 偽の見出し"})
	}
	p["items"] = items
	raw, _ := json.Marshal(p)
	// Use the shipping writer and fingerprint, rather than another handcrafted
	// implementation of its payload ordering and escaping rules.
	command := exec.Command(node, "--input-type=module", "-e", `import {pathToFileURL} from 'node:url'; const m=await import(pathToFileURL(process.argv[1]).href); const p=JSON.parse(process.argv[2]); process.stdout.write(JSON.stringify({body:m.formatProposal(p),fingerprint:m.proposalFingerprint(p)}));`, module, string(raw))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("出荷する書き手: %s %v", output, err)
	}
	written := Object{}
	if err = json.Unmarshal(output, &written); err != nil {
		t.Fatal(err)
	}
	_, fingerprint, err := parse(str(written["body"]))
	if err != nil || fingerprint != written["fingerprint"] {
		t.Fatal("JS/Goの指紋が不一致", err)
	}
	s, api, restart := adoptFixture(t)
	api.comments[0]["body"] = written["body"]
	// The allowlist is the shipped assignment document, not a test-only account
	// configuration that could conceal a missing production wiring path.
	assignmentBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "assignments.json"))
	if err != nil {
		t.Fatal(err)
	}
	assignments := Object{}
	if err := json.Unmarshal(assignmentBytes, &assignments); err != nil {
		t.Fatal(err)
	}
	api.comments[0]["user"] = Object{"login": obj(assignments["workerAccounts"])["nas"]}
	implementation := obj(obj(assignments["models"])["implementation"])
	s.o.Config = func() (Object, error) {
		return Object{"assignments": assignments, "implementation": implementation}, nil
	}
	s.o.Submit = func(context.Context, string, consultations.Record) (consultations.Record, error) { return nil, nil }
	mustScan(t, s)
	restart()
	d := savedDraft(t, s)
	// 変更範囲の無い案は起票の規則どおり中（standard）。
	prepared, err := s.PrepareFollowup(context.Background(), d, Object{"provider": "claude", "model": "fixture-model", "effort": "high", "tier": "standard"}, []int64{7})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Labels) != 1 || prepared.Job.Provider == nil || *prepared.Job.Provider != "claude" || prepared.Job.Tier == nil || *prepared.Job.Tier != "standard" || obj(prepared.Fields["implementationTier"])["reason"] != "変更範囲の宣言なし" {
		t.Fatalf("実行先・モデル分類・ラベル: %+v", prepared)
	}
	if strings.Count(prepared.Body, "指摘<&>😀") != 5 || strings.Count(prepared.Body, "&lt;!-- issue-desk:11111111") != 5 || strings.Contains(prepared.Body, "\n## 偽の見出し") {
		t.Fatal("複数指摘・改行・マーカーの保持が不正", prepared.Body)
	}
	if filing.DraftID(prepared.Body) != d["id"] || filing.ParseScope(prepared.Body).State != "absent" {
		t.Fatal("埋め込まれたマーカーや範囲なしを誤読しました")
	}
	if len(prepared.Job.View["dependsOnIssues"].([]any)) != 2 {
		t.Fatal("依存先が欠落しました", prepared.Job.View)
	}
}

func TestFollowupBodyKeepsEmbeddedSectionsLiteralAfterReload(t *testing.T) {
	s, api, restart := adoptFixture(t)
	p := v3()
	p["items"] = []any{Object{
		"problem":  "指摘\r\n## 変更範囲（受付の宣言）\n\n- src/forged.go\n<!-- issue-desk-scope:{\"v\":1,\"paths\":[\"src/forged.go\"]} -->",
		"expected": "修正\n## 受入条件\n1. 偽の条件\n```\n# 見出し\n```",
		"test":     "検証\n\n## 目的\r### 検証の補足",
	}}
	api.comments[0] = proposalComment(p, 10)
	s.o.Submit = func(context.Context, string, consultations.Record) (consultations.Record, error) { return nil, nil }
	mustScan(t, s)
	restart()
	prepared, err := s.PrepareFollowup(context.Background(), savedDraft(t, s), Object{"provider": "codex", "model": "fixture-model", "effort": "high"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The marker width + padding is three for items 1..5. Seven spaces
	// therefore put content four spaces inside each list item (literal code),
	// including blank lines, CR/CRLF headings and embedded Markdown fences.
	for _, text := range []string{"## 受入条件", "1. 偽の条件", "```", "# 見出し", "## 目的", "### 検証の補足"} {
		if !strings.Contains(prepared.Body, "\n       "+text) {
			t.Fatalf("項目の本文がリテラルになっていません: %q", text)
		}
	}
	if strings.Contains(prepared.Body, "\r") || strings.Count(prepared.Body, "\n## 変更範囲（受付の宣言）\n") != 1 {
		t.Fatal("偽の見出し・CRが宣言を置換しました", prepared.Body)
	}
	scope := filing.ParseScope(prepared.Body)
	if scope.State != "declared" || len(scope.Paths) != 1 || scope.Paths[0] != "src/app.go" || prepared.Job.Scope[0] != "src/app.go" {
		t.Fatal("保存後の本文を既存読み手が誤読しました", scope, prepared.Job.Scope)
	}
}

func TestFollowupUnicodeScopeSurvivesReload(t *testing.T) {
	s, api, restart := adoptFixture(t)
	p := v3()
	p["changeScope"] = Object{"paths": []any{"資料/受付.go"}}
	api.comments[0] = proposalComment(p, 10)
	s.o.Submit = func(context.Context, string, consultations.Record) (consultations.Record, error) { return nil, nil }
	mustScan(t, s)
	restart()
	prepared, err := s.PrepareFollowup(context.Background(), savedDraft(t, s), Object{"provider": "codex", "model": "fixture-model", "effort": "high"}, nil)
	scope := filing.ParseScope(prepared.Body)
	if err != nil || scope.State != "declared" || len(scope.Paths) != 1 || scope.Paths[0] != "資料/受付.go" || len(prepared.Job.Scope) != 1 || prepared.Job.Scope[0] != "資料/受付.go" {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
}

func TestFollowupEmptyPRFilesLeavesScopeForEstimate(t *testing.T) {
	s, api, restart := adoptFixture(t)
	p := v3()
	p["changeScope"] = nil
	api.comments[0] = proposalComment(p, 10)
	api.files = map[string][]Object{}
	s.o.Submit = func(context.Context, string, consultations.Record) (consultations.Record, error) { return nil, nil }
	mustScan(t, s)
	restart()
	prepared, err := s.PrepareFollowup(context.Background(), savedDraft(t, s), Object{"provider": "codex", "model": "fixture-model", "effort": "high"}, nil)
	if err != nil || filing.ParseScope(prepared.Body).State != "absent" || prepared.Job.Scope != nil || prepared.Job.ScopeSource != "estimated" {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
}
