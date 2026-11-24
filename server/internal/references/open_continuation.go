package references

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var ErrOpenListChanged = errors.New("open requests list changed")
var openCursorPattern = regexp.MustCompile(`^[a-f0-9]{64}:[0-9]+:[0-9]+$`)

// openSnapshot binds a cursor to the complete ordered, classified input.
func openSnapshot(project string, lists [][]*entry) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode(project)
	for _, list := range lists {
		for _, e := range list {
			// Include the rendered and classification inputs, even for non-candidates.
			_ = enc.Encode([]any{e.record, e.title, e.body, e.scope, e.tier, e.paths, e.words})
		}
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func openCursor(snapshot string, list, offset int) string {
	return fmt.Sprintf("%s:%d:%d", snapshot, list, offset)
}

// OpenContinuation returns omitted overlap candidates for one unchanged list.
// The cursor is emitted by Open or by the preceding page. A changed input or
// invalid cursor returns an error; an empty Next means the candidates are exhausted.
type OpenContinuationPage struct {
	Text string
	Next string
}

func OpenContinuation(project string, requests, todos []Record, draft Record, cursor string) (OpenContinuationPage, error) {
	if !openCursorPattern.MatchString(cursor) {
		return OpenContinuationPage{}, errors.New("invalid continuation cursor")
	}
	lists := openLists(project, requests, todos, draft)
	return openContinuationFromSnapshot(project, lists, openSnapshot(project, lists), cursor)
}

// The cursor format was checked by the public entry point or produced by this snapshot.
func openContinuationFromSnapshot(project string, lists [][]*entry, snapshot, cursor string) (OpenContinuationPage, error) {
	parts := strings.Split(cursor, ":")
	listIndex, err1 := strconv.Atoi(parts[1])
	offset, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || listIndex < 0 || listIndex >= len(lists) || offset < 0 {
		return OpenContinuationPage{}, errors.New("invalid continuation cursor")
	}
	if parts[0] != snapshot {
		return OpenContinuationPage{}, ErrOpenListChanged
	}
	list := lists[listIndex]
	if offset >= len(list) || list[offset].tier != 0 {
		return OpenContinuationPage{}, errors.New("continuation position is not an overlap candidate")
	}
	lines := []string{}
	next := offset
	for next < len(list) && list[next].tier == 0 && next-offset < 30 {
		e := list[next]
		id := openEntryID(project, e)
		line := fmt.Sprintf("- %s: %s\n  本文の先頭: %s\n  変更範囲: %s\n", Lead(id, 300), Lead(e.title, 200), Lead(e.body, 200), Lead(e.scope, 500))
		if detail := instructionBlockLine(e.record); detail != "" {
			line += detail + "\n"
		}
		// Reserve space for the next cursor and its label in the output budget.
		if len(strings.Join(lines, ""))+len(line)+256 > 24000 {
			break
		}
		lines = append(lines, line)
		next++
	}
	if next == offset {
		return OpenContinuationPage{}, errors.New("candidate exceeds continuation limit")
	}
	page := OpenContinuationPage{Text: strings.Join(lines, "")}
	if next < len(list) && list[next].tier == 0 {
		page.Next = openCursor(parts[0], listIndex, next)
		page.Text += "続き: " + page.Next + "\n"
	}
	return page, nil
}

// openExpandedLimit keeps the expanded list well inside the per-document read limit.
const openExpandedLimit = 120000

const openExpandedHeading = "## 省略された重なりの候補（一覧と同じ時点のデータ。読み足しは不要）"

var openContinuationLine = regexp.MustCompile(`(?m)^  重なり候補の続き: ([a-f0-9]{64}:[01]:[0-9]+)$`)

// OpenExpanded appends the omitted overlap candidates to Open from the same
// fetched lists, so the check does not depend on read rounds or on the list
// staying unchanged between rounds. A candidate that does not fit keeps its
// continuation cursor and is reported as omitted; it is never dropped silently.
func OpenExpanded(project string, requests, todos []Record, draft Record) string {
	lists := openLists(project, requests, todos, draft)
	snapshot := openSnapshot(project, lists)
	text := openFromSnapshot(project, lists, snapshot)
	sections := []string{}
	used := len(text) + len(openExpandedHeading) + 2
	replaceFrom := 0
	var out strings.Builder
	for _, loc := range openContinuationLine.FindAllStringSubmatchIndex(text, -1) {
		cursor := text[loc[2]:loc[3]]
		for cursor != "" {
			page, err := openContinuationFromSnapshot(project, lists, snapshot, cursor)
			if err != nil {
				break
			}
			body := strings.TrimSuffix(page.Text, "続き: "+page.Next+"\n")
			if used+len(body) > openExpandedLimit {
				break
			}
			used += len(body)
			sections = append(sections, body)
			cursor = page.Next
		}
		out.WriteString(text[replaceFrom:loc[0]])
		if cursor == "" {
			out.WriteString("  省略された重なりの候補は、この資料の末尾に全件あります。")
		} else {
			out.WriteString("  重なり候補の続き: " + cursor)
		}
		replaceFrom = loc[1]
	}
	if len(sections) == 0 {
		return text
	}
	out.WriteString(text[replaceFrom:])
	return out.String() + "\n" + openExpandedHeading + "\n" + strings.Join(sections, "")
}
