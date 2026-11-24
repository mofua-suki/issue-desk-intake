package consultationai

import (
	"strings"
	"testing"
)

func deliveredFixture() (*materials, []Document) {
	docs := []Document{
		{Project: "p/r", Path: "server/internal/models/resolve.go", SHA256: "a", Request: ReadRequest{File: "server/internal/models/resolve.go", Mode: "full"}, Text: "package models"},
		{Project: "p/r", Path: "server/internal/models/models_test.go", SHA256: "b", Request: ReadRequest{File: "server/internal/models/models_test.go", Mode: "full"}, Text: "package models"},
		{Project: "p/r", Path: "assignments.json", SHA256: "c", Request: ReadRequest{File: "assignments.json", Mode: "outline"}, Text: "目次"},
		{Project: "p/r", Path: "docs/missing.md", SHA256: "e", Request: ReadRequest{File: "docs/missing.md", Mode: "full"}, Reason: "not_found"},
		{Project: "p/r", Path: "skills/user-interaction/SKILL.md", SHA256: "d", Request: ReadRequest{File: "skills/user-interaction/SKILL.md", Mode: "full"}, Text: "本文"},
	}
	m := &materials{state: contextState{Projects: map[string]*projectState{"p/r": {Name: "p/r", Reads: docs}}}}
	return m, docs
}

func TestCompactionKeepsEarlierDeliveredBodiesAsReviewed(t *testing.T) {
	m, docs := deliveredFixture()
	// 圧縮では、プロンプトに本文が残るのは最後の1件だけ。前の回に読んだ資料が「本文を確認していない」になっていた。
	_, _, bodies := m.data(true)
	if bodies["p/r:server/internal/models/resolve.go"] || !bodies["p/r:skills/user-interaction/SKILL.md"] {
		t.Fatalf("圧縮のときは最後の1件だけ: %v", bodies)
	}
	d := Record{"project": "p/r"}
	answer := Record{"ready": true, "reviewedFiles": []any{"server/internal/models/resolve.go", "server/internal/models/models_test.go", "skills/user-interaction/SKILL.md"}}
	if reviewedProblem(d, answer, bodies) == "" {
		t.Fatal("配信の記録が無いうちは未確認のまま")
	}
	// 前の回に配信した資料の鍵を保存したあとは、圧縮のあとも読んだ扱いになる。
	m.state.Delivered = map[string]bool{documentKey(docs[0]): true, documentKey(docs[1]): true}
	m.addDeliveredBodies(&sessionState{}, bodies)
	if problem := reviewedProblem(d, answer, bodies); problem != "" {
		t.Fatalf("配信済みの本文は読んだ扱い: %s", problem)
	}
}

func TestDeliveredBodiesExcludeUndeliveredOutlineAndFailedReads(t *testing.T) {
	m, docs := deliveredFixture()
	m.state.Delivered = map[string]bool{documentKey(docs[0]): true, documentKey(docs[2]): true, documentKey(docs[3]): true}
	bodies := map[string]bool{}
	m.addDeliveredBodies(nil, bodies)
	if !bodies["p/r:server/internal/models/resolve.go"] {
		t.Fatalf("配信済みの本文: %v", bodies)
	}
	for _, not := range []string{"p/r:server/internal/models/models_test.go", "p/r:assignments.json", "p/r:docs/missing.md", "p/r:skills/user-interaction/SKILL.md"} {
		if bodies[not] {
			t.Fatalf("%s は読んだ扱いにしない（未配信・目次・読めなかった資料）: %v", not, bodies)
		}
	}
	// 配信していない資料を「確認済み」に挙げたら、従来どおり止める。
	d := Record{"project": "p/r"}
	if reviewedProblem(d, Record{"reviewedFiles": []any{"server/internal/models/models_test.go"}}, bodies) == "" {
		t.Fatal("未配信の資料は確認済みにできない")
	}
	if reviewedProblem(d, Record{"reviewedFiles": []any{"assignments.json"}}, bodies) == "" {
		t.Fatal("目次だけの資料は確認済みにできない")
	}
}

func TestSessionSentStillCountsAsDeliveredWithoutSavedKeys(t *testing.T) {
	m, docs := deliveredFixture()
	bodies := map[string]bool{}
	m.addDeliveredBodies(&sessionState{Sent: map[string]bool{documentKey(docs[1]): true}}, bodies)
	if !bodies["p/r:server/internal/models/models_test.go"] || bodies["p/r:server/internal/models/resolve.go"] {
		t.Fatalf("会話で送った資料だけ: %v", bodies)
	}
}

func TestReviewedProblemNamesMissingFiles(t *testing.T) {
	d := Record{"project": "p/r"}
	answer := Record{"reviewedFiles": []any{"a.go", "b.go"}}
	problem := reviewedProblem(d, answer, map[string]bool{"p/r:a.go": true})
	if !strings.Contains(problem, "p/r:b.go") || strings.Contains(problem, "p/r:a.go") {
		t.Fatalf("本文を確認できていない資料だけを名指しします: %q", problem)
	}
}

func TestReviewedProblemIgnoresReferenceDocuments(t *testing.T) {
	d := Record{"project": "p/r"}
	bodies := map[string]bool{"p/r:a.go": true}
	if problem := reviewedProblem(d, Record{"ready": true, "reviewedFiles": []any{"a.go", "p/r:open-requests", "repo-index"}}, bodies); problem != "" {
		t.Fatalf("参照資料を挙げただけでは止めません: %q", problem)
	}
	if reviewedProblem(d, Record{"ready": true, "reviewedFiles": []any{"open-requests"}}, bodies) == "" {
		t.Fatal("参照資料だけでは、本文を読んだ扱いにしません")
	}
}

// 受付AIがパスの後ろへ版・読取範囲を書き足しても、本文を読んだ資料を「読んでいない」扱いにしない。
func TestReviewedProblemIgnoresRevisionAndRangeSuffix(t *testing.T) {
	d := Record{"project": "p/r"}
	bodies := map[string]bool{"p/r:dir/a.go": true}
	for _, item := range []string{
		"p/r:dir/a.go@6dda3b1563a0fa30ccef88893b604c114b820c28（全文）",
		"dir/a.go@6dda3b1",
		"dir/a.go（全文）",
		"dir/a.go (1-20行)",
		"dir/a.go:10-20",
		"dir/a.go:10",
		" p/r:dir/a.go ",
	} {
		if problem := reviewedProblem(d, Record{"ready": true, "reviewedFiles": []any{item}}, bodies); problem != "" {
			t.Errorf("%q は本文を読んだ資料です: %q", item, problem)
		}
	}
	reference := "p/r:open-requests@31245bb4a930bf35fd3b2805be9eaf81ad6b415c658c4ea21feb340fb6eb75f6（全文）"
	if problem := reviewedProblem(d, Record{"reviewedFiles": []any{reference}}, bodies); problem != "" {
		t.Errorf("版を書き足した参照資料でも止めません: %q", problem)
	}
	if reviewedProblem(d, Record{"ready": true, "reviewedFiles": []any{reference}}, bodies) == "" {
		t.Error("版を書き足した参照資料だけでは、本文を読んだ扱いにしません")
	}
	// 読んでいない資料は、書き足しがあっても止める。パスの一部を版と誤って削らない。
	for _, item := range []string{"dir/b.go@6dda3b1（全文）", "dir/a.go@main", "dir/a.go.bak"} {
		if problem := reviewedProblem(d, Record{"reviewedFiles": []any{item}}, bodies); problem == "" {
			t.Errorf("%q は読んでいない資料です", item)
		}
	}
}
