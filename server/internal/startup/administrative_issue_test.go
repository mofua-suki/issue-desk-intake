package startup

import "testing"

func TestNoticeRepositories(t *testing.T) {
	cases := []struct {
		name, repo string
		labels     []any
		body       string
		want       bool
	}{
		{"new", "mofua-suki/issue-desk-intake", []any{map[string]any{"name": "issue-desk/notice"}}, "<!-- issue-desk-notice -->", true},
		{"legacy", "mofua-suki/issue-desk", []any{"issue-desk/notice"}, "<!-- issue-desk-notice -->", true},
		{"label only", "mofua-suki/issue-desk-intake", []any{"issue-desk/notice"}, "", false},
		{"marker only", "mofua-suki/issue-desk", nil, "<!-- issue-desk-notice -->", false},
		{"other repository", "mofua-suki/other", []any{"issue-desk/notice"}, "<!-- issue-desk-notice -->", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := map[string]any{"labels": tc.labels, "body": tc.body}
			if got := administrativeIssue(tc.repo, issue); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
