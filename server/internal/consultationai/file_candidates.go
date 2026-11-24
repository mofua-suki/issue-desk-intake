package consultationai

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const fileListLimit = 8 * 1024

// candidateFiles limits the encoded array itself, including JSON escaping and separators.
func candidateFiles(p *projectState, draft Record) ([]string, bool) {
	files := sortedFiles(p)
	if len(marshal(files)) <= fileListLimit {
		return files, false
	}
	var values any
	_ = json.Unmarshal(marshal(Record{"messages": draft["messages"], "title": draft["title"], "body": draft["body"], "questions": draft["questions"], "splitIssues": draft["splitIssues"], "changeScope": draft["changeScope"]}), &values)
	var texts []string
	var collect func(any)
	collect = func(v any) {
		switch v := v.(type) {
		case string:
			texts = append(texts, v)
		case []any:
			for _, item := range v {
				collect(item)
			}
		case map[string]any:
			for _, item := range v {
				collect(item)
			}
		}
	}
	collect(values)
	preferred := make(map[string]bool)
	for _, file := range files {
		for _, value := range texts {
			if containsExactPath(value, file) {
				preferred[file] = true
				break
			}
		}
	}
	selected := make([]string, 0)
	size := 2
	for _, priority := range []bool{true, false} {
		for _, file := range files {
			if preferred[file] != priority {
				continue
			}
			n := len(marshal(file))
			if len(selected) > 0 {
				n++
			}
			if size+n > fileListLimit {
				continue
			}
			selected = append(selected, file)
			size += n
		}
	}
	return selected, len(selected) != len(files)
}

func containsExactPath(value, file string) bool {
	pathRune := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./-", r)
	}
	for offset := 0; offset <= len(value)-len(file); {
		i := strings.Index(value[offset:], file)
		if i < 0 {
			return false
		}
		i += offset
		before, _ := utf8.DecodeLastRuneInString(value[:i])
		after, _ := utf8.DecodeRuneInString(value[i+len(file):])
		if (i == 0 || !pathRune(before)) && (i+len(file) == len(value) || !pathRune(after)) {
			return true
		}
		offset = i + 1
	}
	return false
}
