package issueapi

import "testing"

func TestFakeCommentsStayScopedToRepositoryAndIssue(t *testing.T) {
	api := &fakeAPI{}
	for _, target := range []struct {
		repo string
		n    int64
	}{{"owner/repo", 1}, {"owner/repo", 12}, {"other/repo", 1}} {
		if _, err := api.Comment(t.Context(), target.repo, target.n, "同じ監査本文"); err != nil {
			t.Fatal(err)
		}
	}
	for i, target := range []struct {
		repo string
		n    int64
	}{{"owner/repo", 1}, {"OWNER/REPO", 12}, {"other/repo", 1}} {
		comments, err := api.Comments(t.Context(), target.repo, target.n)
		if err != nil || len(comments) != 1 || comments[0]["id"] != float64(i+1) {
			t.Fatal("別のIssueやPRの投稿が混ざりました", comments, err)
		}
	}
	api.comments = append(api.comments, Object{"id": float64(4), "body": "既存の正本"})
	comments, err := api.Comments(t.Context(), "owner/repo", 1)
	if err != nil || len(comments) != 2 {
		t.Fatal("既存のIssueコメントが読めません", comments, err)
	}
}
