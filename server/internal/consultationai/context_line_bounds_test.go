package consultationai

import (
	"net/http"
	"strings"
	"testing"
)

func TestReadRejectsIncompleteLineRange(t *testing.T) {
	const body = "first\nsecond\n"
	s, _ := blobFixture(t, func(*http.Request) (string, string) {
		return "base64", encoded(body)
	})
	const repo, file = "owner/app", "article.md"
	p := &projectState{Name: repo, Branch: "main", Revision: strings.Repeat("a", 40), entries: map[string]treeEntry{file: {Path: file, Size: len(body), SHA: fixtureSHA(body)}}}
	m := &materials{service: s, draft: Record{"project": repo, "_ephemeral": true}, allowed: map[string]bool{repo: true}, state: contextState{Projects: map[string]*projectState{repo: p}}}
	for _, tc := range []struct {
		end    int
		reason string
		text   string
	}{{2, "", "1: first\n2: second"}, {3, "range", ""}} {
		doc, _, err := m.read(t.Context(), ReadRequest{File: file, Mode: "lines", Start: 1, End: tc.end})
		if err != nil || doc.Reason != tc.reason || doc.Text != tc.text {
			t.Fatalf("end=%d: doc=%+v err=%v", tc.end, doc, err)
		}
	}
}
