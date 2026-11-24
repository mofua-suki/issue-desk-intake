package consultationai

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var heading = regexp.MustCompile(`^(?:#{1,6}\s|\s*(?:(?:export\s+)?(?:async\s+)?function\s|(?:export\s+)?(?:class|interface|type|const)\s|(?:async\s+)?def\s|class\s|func\s|type\s|package\s))`)

func extract(source string, r ReadRequest) (string, int, error) {
	if r.Mode == "full" {
		return source, 0, nil
	}
	lines := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	start, end := r.Start, r.End
	if start == 0 {
		start = 1
	}
	if end == 0 || end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) {
		return "", 0, errors.New("開始行がファイルに存在しません")
	}
	selected := map[int]bool{}
	next, matches := 0, 0
	for n := start; n <= end; n++ {
		include := r.Mode == "lines" || r.Mode == "outline" && heading.MatchString(lines[n-1]) || r.Mode == "keyword" && strings.Contains(lines[n-1], r.Keyword)
		if !include {
			continue
		}
		limit := 50
		if r.Mode == "outline" {
			limit = 300
		}
		if r.Mode != "lines" && matches >= limit {
			next = n
			break
		}
		matches++
		left, right := n, n
		if r.Mode == "keyword" {
			left, right = n-r.Context, n+r.Context
			if left < start {
				left = start
			}
			if right > end {
				right = end
			}
		}
		for line := left; line <= right; line++ {
			selected[line] = true
		}
	}
	output := []string{}
	for n := start; n <= end; n++ {
		if selected[n] {
			output = append(output, fmt.Sprintf("%d: %s", n, lines[n-1]))
		}
	}
	return strings.Join(output, "\n"), next, nil
}

func projectBytes(reads []Document) int {
	full := map[string]int{}
	parts := map[string]map[string]bool{}
	for _, doc := range reads {
		if doc.Reason != "" {
			continue
		}
		if isContinuation(doc.Path, doc.Request) {
			full[doc.Path+"\x00"+requestKey(doc.Request)] = len(doc.Text)
			continue
		}
		if doc.Request.Mode == "full" {
			full[doc.Path] = len(doc.Text)
			continue
		}
		if parts[doc.Path] == nil {
			parts[doc.Path] = map[string]bool{}
		}
		for _, line := range strings.Split(doc.Text, "\n") {
			parts[doc.Path][line] = true
		}
	}
	total := 0
	for file, size := range full {
		total += size
		delete(parts, file)
	}
	for _, lines := range parts {
		for line := range lines {
			total += len(line) + 1
		}
	}
	return total
}
