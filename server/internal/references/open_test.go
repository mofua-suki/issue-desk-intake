package references

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestOpenPrioritizesMatchingTodoBeforeUnrelatedRequestsExhaustBudget(t *testing.T) {
	requests := []Record{}
	for i := 0; i < 100; i++ {
		requests = append(requests, Record{"number": i + 1, "title": strings.Repeat("無関係", 30), "body": strings.Repeat("文", 400), "created_at": "2026-10-31T00:00:00Z"})
	}
	todos := []Record{{"id": "matching", "body": "SQLite WALが停止する", "createdAt": "2026-10-01T00:00:00Z"}}
	draft := Record{"title": "SQLite WALの停止", "messages": []any{Record{"role": "user", "text": "SQLite"}}}
	text := Open("owner/app", requests, todos, draft)
	if len(text) > 24000 || !strings.Contains(text, "TODO matching") || !strings.Contains(text, "重なりの候補") || !strings.Contains(text, "省略:") {
		t.Fatal("matching TODO or bounded omission missing", len(text))
	}
}
func TestOpenRetainsInstructionRefusalAndAvoidsCommonWordMatches(t *testing.T) {
	draft := Record{"title": "機能の変更"}
	request := Record{"number": 12, "desk": 20, "title": "画面表示の修正", "body": "本文", "acceptance": Record{"acceptsInstructions": false, "instructionBlocks": []any{Record{"explanation": "実装中の上限"}}, "instructionSize": Record{"scopePaths": 4, "implementationRuns": 2, "reviewHolds": 1, "stopReason": "held"}}}
	text := Open("owner/app", []Record{request}, nil, draft)
	if strings.Contains(text, "【重なりの候補") || !strings.Contains(text, "受け付けない（実装中の上限）") || !strings.Contains(text, "共通番号20") {
		t.Fatal(text)
	}
	if len(Words("Ｇｉｔｅａ テスト ファイル 変更")) != 0 {
		t.Fatal("common words should not rank as overlap")
	}
}
func TestPrefixAndPathOverlap(t *testing.T) {
	if Prefix("【DB】 再開") != "【DB】" || Prefix("調査: SQLite") != "調査:" || !overlap("server/", "server/main.go") || overlap("server/a.go", "server/aa.go") {
		t.Fatal("prefix or scope boundary changed")
	}
}

func TestOpenOmissionsAndContinuation(t *testing.T) {
	draft := Record{"title": "SQLite"}
	requests, todos := []Record{}, []Record{}
	for i := 0; i < 120; i++ {
		requests = append(requests, Record{"number": i + 1, "title": "SQLite", "body": "本文", "created_at": fmt.Sprintf("%03d", 120-i)})
		todos = append(todos, Record{"id": fmt.Sprintf("t%d", i), "body": "SQLite 本文", "createdAt": fmt.Sprintf("%03d", 120-i)})
	}
	text := Open("owner/app", requests, todos, draft)
	if len(text) > 24000 || !strings.Contains(text, "省略: 40件（重なりの候補: 40件、同じ前置き: 0件、その他: 0件") || !strings.Contains(text, "省略: 90件（重なりの候補: 90件、同じ前置き: 0件、その他: 0件") {
		t.Fatal(text)
	}
	cursors := regexp.MustCompile(`重なり候補の続き: ([^\n]+)`).FindAllStringSubmatch(text, -1)
	if len(cursors) != 2 {
		t.Fatal(text)
	}
	for idx, match := range cursors {
		cursor := match[1]
		count := 0
		combined := ""
		for cursor != "" {
			page, err := OpenContinuation("owner/app", requests, todos, draft, cursor)
			if err != nil || len(page.Text) > 24000 {
				t.Fatalf("page: %v %d", err, len(page.Text))
			}
			combined += page.Text
			n := strings.Count(page.Text, "本文の先頭:")
			if n == 0 || n > 30 {
				t.Fatalf("page count %d", n)
			}
			count += n
			cursor = page.Next
		}
		start := []int{81, 30}[idx]
		for i := start; i < 120+(1-idx); i++ {
			id := fmt.Sprintf("owner/app#%d）", i)
			if idx == 1 {
				id = fmt.Sprintf("TODO t%d（", i)
			}
			if strings.Count(combined, id) != 1 {
				t.Fatalf("missing or duplicated %s", id)
			}
		}
		want := []int{40, 90}[idx]
		if count != want {
			t.Fatalf("list %d: %d != %d", idx, count, want)
		}
	}
	requests[0]["title"] = "変更"
	if _, err := OpenContinuation("owner/app", requests, todos, draft, cursors[0][1]); err == nil {
		t.Fatal("changed list accepted")
	}
}

func TestOpenOmissionBreakdownAtByteLimit(t *testing.T) {
	draft := Record{"title": "【あ】 SQLite"}
	requests, todos := []Record{}, []Record{}
	for i := 0; i < 50; i++ {
		requests = append(requests, Record{"number": i, "title": "SQLite", "body": strings.Repeat("文", 300)})
		todos = append(todos, Record{"id": fmt.Sprint(i), "body": "SQLite " + strings.Repeat("文", 300)})
	}
	text := Open("p", requests, todos, draft)
	if len(text) > 24000 || strings.Count(text, "重なり候補の続き:") != 2 {
		t.Fatal(len(text), text)
	}
	for _, match := range regexp.MustCompile(`重なり候補の続き: ([^\n]+)`).FindAllStringSubmatch(text, -1) {
		cursor := match[1]
		for cursor != "" {
			page, err := OpenContinuation("p", requests, todos, draft, cursor)
			if err != nil || len(page.Text) > 24000 || !strings.Contains(page.Text, "変更範囲:") {
				t.Fatalf("continuation: %v", err)
			}
			cursor = page.Next
		}
	}
	for _, m := range regexp.MustCompile(`省略: (\d+)件（重なりの候補: (\d+)件、同じ前置き: (\d+)件、その他: (\d+)件`).FindAllStringSubmatch(text, -1) {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		c, _ := strconv.Atoi(m[3])
		d, _ := strconv.Atoi(m[4])
		if a != b+c+d {
			t.Fatal(m)
		}
	}
	noCandidates := []Record{}
	for i := 0; i < 100; i++ {
		noCandidates = append(noCandidates, Record{"number": i, "title": "【あ】 別件"})
	}
	noCandidates = append(noCandidates, Record{"number": 101, "title": "無関係"})
	text = Open("p", noCandidates, nil, draft)
	if !strings.Contains(text, "重なりの候補: 0件") || !strings.Contains(text, "同じ前置き: 20件、その他: 1件") {
		t.Fatal(text)
	}
}

func expandedFixture(n int, body string) ([]Record, []Record, Record) {
	requests, todos := []Record{}, []Record{}
	for i := 0; i < n; i++ {
		requests = append(requests, Record{"number": i + 1, "title": "SQLite", "body": body, "created_at": fmt.Sprintf("%04d", n-i)})
	}
	todos = append(todos, Record{"id": "t1", "body": "SQLite 本文", "createdAt": "0001"})
	return requests, todos, Record{"title": "SQLite"}
}

func TestOpenExpandedListsEveryOmittedCandidateWithoutCursor(t *testing.T) {
	requests, todos, draft := expandedFixture(150, strings.Repeat("文", 100))
	text := OpenExpanded("owner/app", requests, todos, draft)
	if len(text) > openExpandedLimit || strings.Contains(text, "重なり候補の続き:") || !strings.Contains(text, openExpandedHeading) {
		t.Fatal(len(text), text[:400])
	}
	for i := 1; i <= 150; i++ {
		if !strings.Contains(text, fmt.Sprintf("#%d）", i)) {
			t.Fatalf("候補%dが一覧に無い", i)
		}
	}
}

func TestOpenExpandedKeepsCursorWhenLimitIsReached(t *testing.T) {
	requests, todos, draft := expandedFixture(400, strings.Repeat("文", 200))
	text := OpenExpanded("owner/app", requests, todos, draft)
	if len(text) > openExpandedLimit+1024 {
		t.Fatal(len(text))
	}
	cursor := openContinuationLine.FindStringSubmatch(text)
	if cursor == nil || !strings.Contains(text, "省略: ") {
		t.Fatal("上限に達した候補の続きと省略が示されていない")
	}
	page, err := OpenContinuation("owner/app", requests, todos, draft, cursor[1])
	if err != nil || page.Text == "" {
		t.Fatal(err)
	}
}

func TestOpenExpandedLeavesShortListUnchanged(t *testing.T) {
	requests, todos, draft := expandedFixture(3, "本文")
	if got, want := OpenExpanded("owner/app", requests, todos, draft), Open("owner/app", requests, todos, draft); got != want {
		t.Fatal(got)
	}
}

func TestOmittedOpenCandidatesKeepInstructionBlocks(t *testing.T) {
	const project = "owner/app"
	requests, todos, draft := expandedFixture(90, "本文")
	blocked := []struct {
		number      int
		explanation string
		stopReason  string
	}{
		{81, "実装回数の上限", "実装上限"},
		{82, "受付状態を取得できない", "未確認"},
	}
	for _, item := range blocked {
		requests[item.number-1]["acceptance"] = Record{
			"acceptsInstructions": false,
			"instructionBlocks":   []any{Record{"explanation": item.explanation}},
			"instructionSize":     Record{"scopePaths": 3, "implementationRuns": 2, "reviewHolds": 1, "stopReason": item.stopReason},
		}
	}
	initial := Open(project, requests, todos, draft)
	cursor := openContinuationLine.FindStringSubmatch(initial)
	if len(cursor) != 2 {
		t.Fatal("省略された重なり候補のカーソルが必要")
	}
	page, err := OpenContinuation(project, requests, todos, draft, cursor[1])
	if err != nil {
		t.Fatal(err)
	}
	expanded := OpenExpanded(project, requests, todos, draft)
	for _, item := range blocked {
		id := fmt.Sprintf("%s#%d）", project, item.number)
		if strings.Contains(initial, id) {
			t.Fatalf("候補%dは先頭一覧に収まっています", item.number)
		}
		for _, text := range []string{page.Text, expanded} {
			_, entry, found := strings.Cut(text, "共通番号なし（"+id)
			entry, _, _ = strings.Cut(entry, "\n- ")
			if !found || !strings.Contains(entry, "追加の指示: 受け付けない（"+item.explanation+"）") || !strings.Contains(entry, "停止理由: "+item.stopReason) {
				t.Fatalf("候補%dの受付不可の理由がありません", item.number)
			}
		}
	}
}
