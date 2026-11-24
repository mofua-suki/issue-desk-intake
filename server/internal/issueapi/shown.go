package issueapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/numbers"
	"regexp"
	"strconv"
	"strings"
)

type numberLookup func(int64) int64

type detailLookupKey struct{}
type detailLookup struct {
	repo   string
	lookup numberLookup
}

var (
	shownMarkers           = regexp.MustCompile(`<!-- issue-desk(?::followup -->|-followup:|-dependencies:)`)
	shownFollowup          = regexp.MustCompile(`<!-- issue-desk(?::followup -->|-followup:)`)
	shownTitleFollowup     = regexp.MustCompile(`^(\[フォローアップ\] |)#([1-9][0-9]*)`)
	shownTitleDependencies = regexp.MustCompile(`^(\[フォローアップ\] )#([1-9][0-9]*)`)
	shownPending           = regexp.MustCompile(`(先行IssueのPRマージ待ち: )((?:#[1-9][0-9]*(?:, )?)+)`)
	shownHash              = regexp.MustCompile(`#([1-9][0-9]*)`)
	shownOriginal          = regexp.MustCompile(`(元の依頼: )#([1-9][0-9]*)`)
	shownReview            = regexp.MustCompile(`(^|\n)#([1-9][0-9]*)( の独立レビューで、)`)
	shownDependency        = regexp.MustCompile(`(^|\n)#([1-9][0-9]*)(:)`)
	shownFollow            = regexp.MustCompile(`(後続Issueを起票しました: )#([1-9][0-9]*)`)
	shownKind              = regexp.MustCompile(`<!-- gcr:[^>]*#[0-9]+:([a-z-]+)`)
	shownCommentMarkers    = regexp.MustCompile(`<!-- (?:gcr|issue-desk)[\s\S]*?-->`)
)

func shownNumber(value string, lookup numberLookup) string {
	n, _ := strconv.ParseInt(value, 10, 64)
	if desk := lookup(n); desk > 0 {
		return fmt.Sprintf("#%d（Gitea番号 %s）", desk, value)
	}
	return "Gitea番号 " + value
}
func replaceNumber(text string, re *regexp.Regexp, lookup numberLookup) string {
	return re.ReplaceAllStringFunc(text, func(value string) string {
		m := re.FindStringSubmatch(value)
		return m[1] + shownNumber(m[2], lookup) + strings.Join(m[3:], "")
	})
}
func shownIssue(title, body string, lookup numberLookup) (string, string) {
	if !shownMarkers.MatchString(body) {
		return title, body
	}
	pattern := shownTitleDependencies
	if shownFollowup.MatchString(body) {
		pattern = shownTitleFollowup
	}
	title = replaceNumber(title, pattern, lookup)
	pending := shownPending
	body = pending.ReplaceAllStringFunc(body, func(value string) string {
		return shownHash.ReplaceAllStringFunc(value, func(n string) string { return shownNumber(n[1:], lookup) })
	})
	body = replaceNumber(body, shownOriginal, lookup)
	body = replaceNumber(body, shownReview, lookup)
	return title, body
}
func shownReason(text string, lookup numberLookup) string {
	return replaceNumber(text, shownDependency, lookup)
}
func shownComment(raw, text string, lookup numberLookup) string {
	if !strings.Contains(raw, "<!-- gcr:") {
		return text
	}
	text = replaceNumber(text, shownFollow, lookup)
	if strings.HasPrefix(text, "依存待機中\n") {
		text = shownReason(text, lookup)
	}
	return text
}
func commentKind(body string) string {
	if !strings.Contains(body, "<!-- gcr:") {
		return "you"
	}
	match := shownKind.FindStringSubmatch(body)
	suffix := ""
	if len(match) == 2 {
		suffix = match[1]
	}
	if oneOf(suffix, "question", "review-hold", "failed", "follow-held", "intake-required") {
		return "ask"
	}
	if oneOf(suffix, "priority-late", "priority-direct") {
		return "notice"
	}
	if oneOf(suffix, "accepted", "start", "stop", "status", "closed", "busy", "paused", "follow", "review-return", "release-followup", "release-superseded", "priority") {
		return "log"
	}
	return "result"
}
func (s *Service) numberLookup(ctx context.Context, repo string) numberLookup {
	if shared, ok := ctx.Value(detailLookupKey{}).(detailLookup); ok && shared.repo == repo {
		return shared.lookup
	}
	cache := map[int64]int64{}
	return func(n int64) int64 {
		if value, ok := cache[n]; ok {
			return value
		}
		if snapshot, ok := ctx.Value(listSnapshotKey{}).(*listSnapshot); ok {
			return snapshot.numbers[numbers.Issue{Project: repo, Number: n}]
		}
		desk, found, e := s.o.Numbers.Number(ctx, repo, n)
		if e != nil {
			return 0
		}
		if !found {
			desk = 0
		}
		cache[n] = desk
		return desk
	}
}
