package issueapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/releasepolicy"
	"regexp"
	"sort"
	"strings"
)

func (s *Service) legacyPermissionHold(ctx context.Context, route string, job *jobs.Job) Object {
	reviews, e := s.o.Gitea.Pages(ctx, route+"/reviews")
	if e != nil {
		return nil
	}
	config, e := s.configuration()
	if e != nil {
		return nil
	}
	r := latestTrustedReview(reviews, *job.HeadSHA, trustedReviewers(config))
	if r == nil {
		return nil
	}
	base, files := legacyHoldFiles(r, job)
	if files == nil {
		return nil
	}
	return Object{"v": 1, "head": *job.HeadSHA, "base": base, "pr": *job.PR, "files": files}
}

// trustedReviewers は保留のレビューを信じてよいアカウント（受付と実装担当）の集合を作る。
func trustedReviewers(config Object) map[string]bool {
	trusted := map[string]bool{}
	if user := fleet.Text(config["gitUser"]); user != "" {
		trusted[user] = true
	}
	for _, v := range fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(config["assignments"])["workerAccounts"]) {
		if name := fleet.Text(v); name != "" {
			trusted[name] = true
		}
	}
	return trusted
}

// latestTrustedReview は対象の版への信頼するアカウントのレビューのうち最新のものを返す。番号の読めないレビューがあれば nil を返す。
func latestTrustedReview(reviews []Object, head string, trusted map[string]bool) Object {
	filtered := []Object{}
	for _, r := range reviews {
		if r["commit_id"] == head && trusted[fleet.Text(fleet.ObjectOrEmpty(r["user"])["login"])] {
			if _, ok := number(r["id"]); !ok {
				return nil
			}
			filtered = append(filtered, r)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	sort.Slice(filtered, func(i, j int) bool {
		a, _ := number(filtered[i]["id"])
		b, _ := number(filtered[j]["id"])
		return a > b
	})
	return filtered[0]
}

// legacyHoldFiles は旧形式の保留のレビュー本文が決まった書式かを確かめ、基点と保留した権限ファイルを返す。書式が違えば files は nil。
func legacyHoldFiles(r Object, job *jobs.Job) (string, []any) {
	body := fleet.Text(r["body"])
	if !oneOf(fleet.Text(r["state"]), "COMMENT", "COMMENTED") || !strings.HasPrefix(body, "自動レビュー: 保留\n\n破壊的変更の可能性を検出しました\n\n破壊的変更: あり\n") || !strings.Contains(body, "\n独立レビューの実行: AIは実行していません（コントローラーの機械判定で保留）\n") || !strings.Contains(body, "\n対象: "+*job.HeadSHA+"\n") {
		return "", nil
	}
	base := regexp.MustCompile(`(?m)^基点: ([a-f0-9]{40})$`).FindStringSubmatch(body)
	if len(base) != 2 {
		return "", nil
	}
	tail := regexp.MustCompile(fmt.Sprintf(`\n<!-- issue-desk-review:%s:%s(?::[1-9][0-9]*)? -->$`, *job.HeadSHA, base[1]))
	if !tail.MatchString(body) {
		return "", nil
	}
	lines := strings.Split(body, "\n")
	files := []any{}
	for _, file := range releasepolicy.Files(job.Repo) {
		finding := "権限の範囲を定めるファイルの変更があります（利用者の承認が必要）: " + file
		for _, line := range lines {
			if line == finding {
				files = append(files, file)
				break
			}
		}
	}
	if len(files) == 0 {
		return "", nil
	}
	return base[1], files
}
