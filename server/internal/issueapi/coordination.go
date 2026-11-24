package issueapi

import (
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
)

const noticeRepo = "mofua-suki/issue-desk-intake"
const legacyNoticeRepo = "mofua-suki/issue-desk"

const coordinationPrefix = "<!-- issue-desk-coordination "

var coordinationPayload = regexp.MustCompile(`^\{[\s\S]*\} -->\s*$`)

// coordination はお知らせ・調整用のIssueかを判定する。どちらもラベルが必須なので、
// ラベルが無い大多数のIssueは本文を調べずに外す。
func coordination(repo string, issue Object) bool {
	notice, coordinated := false, false
	for _, v := range fleet.Items(issue["labels"]) {
		name := fleet.Text(v)
		if name == "" {
			name = fleet.Text(fleet.ObjectOrEmpty(v)["name"])
		}
		switch name {
		case "issue-desk/notice":
			notice = true
		case "issue-desk/coordination":
			coordinated = true
		}
	}
	if !notice && !coordinated {
		return false
	}
	body := fleet.Text(issue["body"])
	if notice && (repo == noticeRepo || repo == legacyNoticeRepo) && strings.Contains(body, "<!-- issue-desk-notice -->") {
		return true
	}
	if !coordinated {
		return false
	}
	at := strings.LastIndex(body, coordinationPrefix)
	return at >= 0 && coordinationPayload.MatchString(body[at+len(coordinationPrefix):])
}
