package filing

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

// handoffForChild keeps globally required documents and documents assigned to this child.
func handoffForChild(value any, child int) ([]any, error) {
	out := []any{}
	for _, raw := range fleet.Elements(value) {
		item := row(raw)
		childrenValue := item["children"]
		children := fleet.Elements(childrenValue)
		if childrenValue != nil && children == nil {
			return nil, fail(409, "資料の引継ぎ対象が不正です")
		}
		if len(children) == 0 {
			out = append(out, raw)
			continue
		}
		selected := false
		for _, n := range children {
			v, ok := integer(n)
			if !ok || v < 1 || v > 25 {
				return nil, fail(409, "資料の引継ぎ対象が不正です")
			}
			selected = selected || int(v) == child
		}
		if selected {
			out = append(out, raw)
		}
	}
	return out, nil
}

func handoffTargets(value any) string {
	var targets []string
	for _, raw := range fleet.Elements(value) {
		item := row(raw)
		targets = append(targets, fmt.Sprintf("%s:%s:%v-%v", fleet.Text(item["project"]), fleet.Text(item["path"]), item["start"], item["end"]))
	}
	return strings.Join(targets, ", ")
}

func handoffReferenceSection(value any) string {
	var lines []string
	for _, raw := range fleet.Elements(value) {
		item := row(raw)
		if item == nil {
			continue
		}
		// JSON quotes embedded newlines and Markdown delimiters as data.
		encoded, _ := json.Marshal(Record{"project": item["project"], "path": item["path"], "revision": item["revision"], "sha256": item["sha256"], "start": item["start"], "end": item["end"], "text": item["text"]})
		lines = append(lines, string(encoded))
	}
	if len(lines) == 0 {
		return ""
	}
	return "## 固定版の資料本文（参照データ）\n\n" + strings.Join(lines, "\n")
}
