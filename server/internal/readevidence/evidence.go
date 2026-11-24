// Package readevidence validates evidence reported by an issue submitter.
package readevidence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var revision = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Validate checks the JSON shape and returns a copy with normalized line endings.
func Validate(value any, project string) (any, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(value)
	raw := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if err != nil || encodedSize(raw) > 65536 {
		return nil, fmt.Errorf("readEvidence size")
	}
	var e struct {
		Version   int `json:"version"`
		Documents []struct {
			ID       string `json:"id"`
			Project  string `json:"project"`
			Path     string `json:"path"`
			Revision string `json:"revision"`
			Start    int    `json:"start"`
			End      int    `json:"end"`
			Text     string `json:"text"`
		} `json:"documents"`
		Facts []struct {
			Statement string   `json:"statement"`
			Documents []string `json:"documents"`
		} `json:"facts"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.Version != 1 || e.Documents == nil || e.Facts == nil || len(e.Documents) > 16 || len(e.Facts) > 64 {
		return nil, fmt.Errorf("readEvidence format")
	}
	ids := map[string]bool{}
	for i := range e.Documents {
		d := &e.Documents[i]
		if strings.TrimSpace(d.ID) == "" || ids[d.ID] || d.Project != project || strings.TrimSpace(d.Path) == "" || !revision.MatchString(d.Revision) || d.Start < 1 || d.End < d.Start || d.Text == "" {
			return nil, fmt.Errorf("readEvidence document")
		}
		ids[d.ID] = true
		d.Text = strings.ReplaceAll(strings.ReplaceAll(d.Text, "\r\n", "\n"), "\r", "\n")
	}
	for _, f := range e.Facts {
		if strings.TrimSpace(f.Statement) == "" || len(f.Documents) == 0 {
			return nil, fmt.Errorf("readEvidence fact")
		}
		for _, id := range f.Documents {
			if !ids[id] {
				return nil, fmt.Errorf("readEvidence reference")
			}
		}
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	docs := result["documents"].([]any)
	for i, d := range e.Documents {
		docs[i].(map[string]any)["text"] = d.Text
	}
	return result, nil
}

// encodedSize counts UTF-8 JSON bytes as emitted by JSON.stringify.
func encodedSize(raw []byte) int {
	n := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 < len(raw) {
			if raw[i+1] == '\\' {
				n += 2
				i++
				continue
			}
			if i+6 <= len(raw) && (string(raw[i:i+6]) == `\u2028` || string(raw[i:i+6]) == `\u2029`) {
				n += 3
				i += 5
				continue
			}
		}
		n++
	}
	return n
}
