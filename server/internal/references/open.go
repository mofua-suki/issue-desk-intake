package references

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
)

type Record = map[string]any
type entry struct {
	record                              Record
	title, body, created, scope, prefix string
	paths, words                        []string
	order, tier                         int
	todo                                bool
}

var titleWord = regexp.MustCompile(`[a-z0-9][a-z0-9_-]*[a-z0-9]|[\p{Han}々〆]{2,}|[\p{Katakana}ー]{2,}`)
var justNumber = regexp.MustCompile(`^\d+$`)
var prefixes = []*regexp.Regexp{regexp.MustCompile(`^#\d+`), regexp.MustCompile(`^\[[^\]]{1,40}\]`), regexp.MustCompile(`^【[^】]{1,40}】`), regexp.MustCompile(`^([^:]{1,40}):`)}
var commonWords = func() map[string]bool {
	out := map[string]bool{}
	for _, v := range strings.Fields("依頼 追加 変更 対応 修正 機能 受付 画面 処理 確認 表示 テスト ファイル gitea issue") {
		out[v] = true
	}
	return out
}()

func Words(title string) map[string]bool {
	out := map[string]bool{}
	for _, w := range titleWord.FindAllString(strings.ToLower(norm.NFKC.String(title)), -1) {
		if !commonWords[w] && !justNumber.MatchString(w) {
			out[w] = true
		}
	}
	return out
}
func Prefix(title string) string {
	title = strings.TrimSpace(norm.NFKC.String(title))
	for _, p := range prefixes {
		if m := p.FindString(title); m != "" {
			return m
		}
	}
	return ""
}
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
func Lead(s string, n int) string {
	chars := []rune(oneLine(s))
	if len(chars) <= n {
		return string(chars)
	}
	return fmt.Sprintf("%s…（ほか%d文字を省略）", string(chars[:n]), len(chars)-n)
}
func overlap(a, b string) bool {
	return a == b || strings.HasSuffix(a, "/") && strings.HasPrefix(b, a) || strings.HasSuffix(b, "/") && strings.HasPrefix(a, b)
}

// Open preserves overlap-first ordering and a shared byte budget across both lists.
func openLists(project string, requests, todos []Record, draft Record) [][]*entry {
	targetPaths, targetWords := openTargets(project, draft)
	prefix := Prefix(fleet.Text(draft["title"]))
	knownPrefixes := map[string]bool{}
	if prefix != "" {
		knownPrefixes[prefix] = true
	}
	lists := [][]*entry{{}, {}}
	for index, records := range [][]Record{requests, todos} {
		for order, record := range records {
			e := newOpenEntry(record, order, index == 1)
			paths := []string{}
			for _, p := range e.paths {
				for _, target := range targetPaths {
					if overlap(p, target) {
						paths = append(paths, p)
						break
					}
				}
			}
			words := []string{}
			for word := range Words(e.title) {
				if targetWords[word] {
					words = append(words, word)
				}
			}
			sort.Strings(words)
			e.paths, e.words = paths, words
			e.prefix = Prefix(e.title)
			if len(paths) > 0 || len(words) > 0 {
				e.tier = 0
				if e.prefix != "" {
					knownPrefixes[e.prefix] = true
				}
			}
			lists[index] = append(lists[index], e)
		}
	}
	rankOpenLists(lists, knownPrefixes)
	return lists
}

// openTargets collects the paths the draft changes and the words of its title and first request.
func openTargets(project string, draft Record) ([]string, map[string]bool) {
	targetPaths := fleet.Strings(fleet.Object(draft["changeScope"])["paths"])
	for _, v := range fleet.List(draft["splitIssues"]) {
		p := fleet.Object(v)
		if p["project"] == nil || p["project"] == project {
			targetPaths = append(targetPaths, fleet.Strings(fleet.Object(p["changeScope"])["paths"])...)
		}
	}
	targetWords := Words(fleet.Text(draft["title"]))
	for _, v := range fleet.List(draft["messages"]) {
		m := fleet.Object(v)
		if m["role"] == "user" {
			for w := range Words(fleet.Text(m["text"])) {
				targetWords[w] = true
			}
			break
		}
	}
	return targetPaths, targetWords
}

// newOpenEntry reads an open request or TODO with its declared change scope.
func newOpenEntry(record Record, order int, todo bool) *entry {
	e := &entry{record: record, title: fleet.Text(record["title"]), body: fleet.Text(record["body"]), created: fleet.Text(record["created_at"]), order: order, tier: 2, todo: todo}
	if e.todo {
		e.created = fleet.Text(record["createdAt"])
		e.title = strings.Split(strings.TrimSpace(e.body), "\n")[0]
		e.scope = "宣言なし（TODO）"
	} else {
		scope := filing.ParseScope(e.body)
		e.paths = scope.Paths
		e.scope = "宣言なし"
		if scope.State == "declared" {
			e.scope = strings.Join(scope.Paths, ", ")
			if len(scope.Paths) == 0 {
				e.scope = "変更なし"
			}
		} else if scope.State == "unknown" {
			e.scope = "不明"
		}
	}
	return e
}

// rankOpenLists ranks entries sharing a matched prefix after the overlaps, then orders each list.
func rankOpenLists(lists [][]*entry, knownPrefixes map[string]bool) {
	for _, list := range lists {
		for _, e := range list {
			if e.tier != 0 && e.prefix != "" && knownPrefixes[e.prefix] {
				e.tier = 1
			}
		}
		sort.SliceStable(list, func(i, j int) bool {
			a, b := list[i], list[j]
			if a.tier != b.tier {
				return a.tier < b.tier
			}
			return a.created > b.created
		})
	}
}

// Open preserves overlap-first ordering and a shared byte budget across both lists.
func Open(project string, requests, todos []Record, draft Record) string {
	lists := openLists(project, requests, todos, draft)
	return openFromSnapshot(project, lists, openSnapshot(project, lists))
}

func openFromSnapshot(project string, lists [][]*entry, snapshot string) string {
	shown := [2]int{}
	limits := [2]int{80, 30}
	render := func() string { return renderOpen(project, lists, shown, snapshot) }
	full := false
	for tier := 0; tier < 3 && !full; tier++ {
		for i, list := range lists {
			for shown[i] < len(list) && shown[i] < limits[i] && list[shown[i]].tier == tier {
				shown[i]++
				if len(render()) > 24000 {
					shown[i]--
					full = true
					break
				}
			}
			if full {
				break
			}
		}
	}
	return render()
}

// renderOpen formats the first shown entries of each list, and how many were omitted.
func renderOpen(project string, lists [][]*entry, shown [2]int, snapshot string) string {
	out := []string{fmt.Sprintf("# 開いている依頼とTODO（%s）", project), "コントローラー提供の参照データ（読んだターンの時点）。重なりの候補→同じ前置き→新しい順。", "上限: 開いている依頼80件・TODO30件、本文先頭200文字、全体24000 bytes。"}
	for i, list := range lists {
		heading := "開いている依頼"
		if i == 1 {
			heading = "TODO"
		}
		out = append(out, "", fmt.Sprintf("## %s（%d件）", heading, len(list)))
		if len(list) == 0 {
			out = append(out, "- なし")
		}
		for _, e := range list[:shown[i]] {
			mark := ""
			if e.tier == 0 {
				mark = "【重なりの候補（パス: " + strings.Join(e.paths, ", ") + "／題名の語: " + strings.Join(e.words, "・") + "）】"
			} else if e.tier == 1 {
				mark = "【同じ前置き: " + e.prefix + "】"
			}
			title := oneLine(e.title)
			if e.todo {
				title = Lead(e.title, 100)
			}
			line := fmt.Sprintf("- %s%s: %s", mark, openEntryID(project, e), title)
			out = append(out, line, "  本文の先頭: "+Lead(e.body, 200), "  変更範囲: "+e.scope)
			if line := instructionBlockLine(e.record); line != "" {
				out = append(out, line)
			}
		}
		if len(list) > shown[i] {
			counts := [3]int{}
			for _, e := range list[shown[i]:] {
				counts[e.tier]++
			}
			out = append(out, fmt.Sprintf("- 省略: %d件（重なりの候補: %d件、同じ前置き: %d件、その他: %d件。件数または全体24000 bytesの上限）", len(list)-shown[i], counts[0], counts[1], counts[2]))
			if counts[0] > 0 {
				out = append(out, "  重なり候補の続き: "+openCursor(snapshot, i, shown[i]))
			}
		}
	}
	return strings.Join(out, "\n") + "\n"
}

func openEntryID(project string, e *entry) string {
	if e.todo {
		return fmt.Sprintf("TODO %s（共通番号なし）", fleet.Text(e.record["id"]))
	}
	desk := e.record["desk"]
	if desk == nil {
		desk = "なし"
	}
	return fmt.Sprintf("共通番号%v（%s#%v）", desk, project, e.record["number"])
}

// instructionBlockLine keeps the reason for an unavailable operation with its candidate.
func instructionBlockLine(record Record) string {
	acceptance := fleet.Object(record["acceptance"])
	if acceptance["acceptsInstructions"] != false {
		return ""
	}
	reasons := []string{}
	for _, v := range fleet.List(acceptance["instructionBlocks"]) {
		reasons = append(reasons, oneLine(fleet.Text(fleet.Object(v)["explanation"])))
	}
	size := fleet.Object(acceptance["instructionSize"])
	return fmt.Sprintf("  追加の指示: 受け付けない（%s）／大きさ: パス%v件・実装%v回・レビュー保留%v回・停止理由: %v", strings.Join(reasons, "／"), size["scopePaths"], size["implementationRuns"], size["reviewHolds"], size["stopReason"])
}
