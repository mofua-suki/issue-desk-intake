package filing

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"strings"

	"issue-desk-intake/server/internal/jobs"
)

const scopeHeading = "## 変更範囲（受付の宣言）"
const scopeMarker = "<!-- issue-desk-scope:"
const trailerStart = "\n\n---\n受付: "

type ParsedScope struct {
	State      string
	Paths      []string
	Backfilled bool
}

func ParseScope(body string) ParsedScope {
	text := strings.ReplaceAll(body, "\r\n", "\n")
	at := strings.LastIndex(text, scopeMarker)
	if at < 0 {
		return ParsedScope{State: "absent"}
	}
	tail := text[at+len(scopeMarker):]
	end := strings.Index(tail, " -->")
	if end < 0 {
		return ParsedScope{State: "absent"}
	}
	after := tail[end+4:]
	if !strings.HasPrefix(after, trailerStart) {
		return ParsedScope{State: "absent"}
	}
	before := text[:at]
	heading := strings.LastIndex(before, "\n\n"+scopeHeading+"\n")
	if heading < 0 {
		return ParsedScope{State: "absent"}
	}
	lines := strings.Split(before[heading+len(scopeHeading)+3:], "\n")
	if len(lines) < 2 || lines[len(lines)-1] != "" {
		return ParsedScope{State: "absent"}
	}
	for _, line := range lines[:len(lines)-1] {
		if !strings.HasPrefix(line, "- ") {
			return ParsedScope{State: "absent"}
		}
	}
	var value map[string]any
	if json.Unmarshal([]byte(tail[:end]), &value) != nil || value["v"] != float64(1) {
		return ParsedScope{State: "unknown"}
	}
	if value["unknown"] == true {
		return ParsedScope{State: "unknown"}
	}
	if value["none"] == true {
		return ParsedScope{State: "declared", Paths: []string{}}
	}
	paths := []string{}
	for _, raw := range fleet.Items(value["paths"]) {
		path, ok := raw.(string)
		if !ok || path == "*" {
			return ParsedScope{State: "unknown"}
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 || len(paths) > 200 {
		return ParsedScope{State: "unknown"}
	}
	normalized, err := jobs.NormalizeScope(paths)
	if err != nil {
		return ParsedScope{State: "unknown"}
	}
	return ParsedScope{State: "declared", Paths: normalized, Backfilled: value["backfilled"] == true}
}
func BackfilledBody(body string, scope Record) string {
	text := strings.ReplaceAll(body, "\r\n", "\n")
	current := ParseScope(text)
	if current.State == "declared" || scope["known"] != true || len(fleet.Items(scope["paths"])) == 0 {
		return ""
	}
	at := strings.LastIndex(text, trailerStart)
	if at < 0 {
		return ""
	}
	before := text[:at]
	if current.State == "unknown" {
		heading := strings.LastIndex(before, "\n\n"+scopeHeading+"\n")
		if heading < 0 {
			return ""
		}
		before = before[:heading]
	}
	clean := scopeDeclaration(scope)
	if clean["known"] != true {
		return ""
	}
	section := scopeSection(clean)
	markerAt := strings.LastIndex(section, scopeMarker)
	if markerAt < 0 {
		return ""
	}
	section = section[:markerAt] + "- 後から付けた宣言（受付が起票後に書き足しました）\n" + scopeMarker + encoded(Record{"v": 1, "paths": clean["paths"], "backfilled": true}) + " -->"
	next := before + section + text[at:]
	parsed := ParseScope(next)
	if parsed.State != "declared" || !parsed.Backfilled || len(parsed.Paths) != len(fleet.Items(clean["paths"])) {
		return ""
	}
	return next
}
