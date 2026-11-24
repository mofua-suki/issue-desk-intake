package issueapi

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
			issue := Object{"labels": tc.labels, "body": tc.body}
			if got := coordination(tc.repo, issue); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCoordinationNeedsLabelAndTrailingPayload(t *testing.T) {
	const marker = "<!-- issue-desk-coordination {\"kind\":\"x\"} -->"
	coordination1 := []any{map[string]any{"name": "issue-desk/coordination"}}
	cases := []struct {
		name, repo string
		labels     []any
		body       string
		want       bool
	}{
		{"label and payload", "owner/repo", coordination1, "本文\n" + marker + "\n", true},
		{"plain label", "owner/repo", []any{"issue-desk/coordination"}, marker, true},
		{"multiline payload", "owner/repo", coordination1, "<!-- issue-desk-coordination {\n\"a\":1\n} -->", true},
		{"payload without label", "owner/repo", []any{"bug"}, marker, false},
		{"no labels", "owner/repo", nil, marker, false},
		{"label without payload", "owner/repo", coordination1, "本文", false},
		{"text after payload", "owner/repo", coordination1, marker + "\n続き", false},
		{"last marker decides", "owner/repo", coordination1, marker + "\n<!-- issue-desk-coordination 壊れ", false},
		{"earlier broken marker", "owner/repo", coordination1, "<!-- issue-desk-coordination 壊れ\n" + marker, true},
		{"notice label does not coordinate", "owner/repo", []any{"issue-desk/notice"}, marker, false},
		{"notice label with coordination payload", "mofua-suki/issue-desk-intake", []any{"issue-desk/notice"}, marker, false},
		{"both labels use notice", "mofua-suki/issue-desk-intake", []any{"issue-desk/notice", "issue-desk/coordination"}, "<!-- issue-desk-notice -->", true},
		{"both labels use coordination", "owner/repo", []any{"issue-desk/notice", "issue-desk/coordination"}, "<!-- issue-desk-notice -->\n" + marker, true},
		{"non-text body", "owner/repo", coordination1, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := coordination(tc.repo, Object{"labels": tc.labels, "body": tc.body}); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
