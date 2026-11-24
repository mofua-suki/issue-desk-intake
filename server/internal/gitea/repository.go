package gitea

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (c *Client) Issue(ctx context.Context, repo string, number int64) (map[string]any, error) {
	base, err := RepoPath(repo)
	if err != nil || number <= 0 {
		return nil, errors.New("Issueの指定が不正です")
	}
	var issue map[string]any
	err = c.Do(ctx, "GET", base+"/issues/"+strconv.FormatInt(number, 10), nil, &issue)
	return issue, err
}

func (c *Client) Issues(ctx context.Context, repo, state string) ([]map[string]any, error) {
	base, err := RepoPath(repo)
	if err != nil {
		return nil, err
	}
	if state == "all" {
		return c.issueHistory(ctx, repo, base)
	}
	return c.Pages(ctx, base+"/issues?type=issues&state="+url.QueryEscape(state))
}

// IssuesForFiling shares the full refresh and reduces closed normal issues to
// the fields needed for the filing comparison.
func (c *Client) IssuesForFiling(ctx context.Context, repo string) ([]map[string]any, error) {
	base, err := RepoPath(repo)
	if err != nil {
		return nil, err
	}
	return c.issueHistorySelected(ctx, repo, base, nil, true)
}

// IssuesByMarker shares the complete history refresh while cloning only matches.
func (c *Client) IssuesByMarker(ctx context.Context, repo, marker string) ([]map[string]any, error) {
	base, err := RepoPath(repo)
	if err != nil {
		return nil, err
	}
	return c.issueHistorySelected(ctx, repo, base, &marker, false)
}

func (c *Client) Comments(ctx context.Context, repo string, number int64) ([]map[string]any, error) {
	base, err := RepoPath(repo)
	if err != nil || number <= 0 {
		return nil, errors.New("Issueの指定が不正です")
	}
	// The deployed Gitea comments endpoint returns the complete collection and
	// ignores page parameters; paging it would duplicate the same comments.
	var comments []map[string]any
	err = c.Do(ctx, http.MethodGet, base+"/issues/"+strconv.FormatInt(number, 10)+"/comments", nil, &comments)
	if err == nil && comments == nil {
		comments = []map[string]any{}
	}
	return comments, err
}

func (c *Client) CreateIssue(ctx context.Context, repo string, input any) (map[string]any, error) {
	base, err := RepoPath(repo)
	if err != nil {
		return nil, err
	}
	var issue map[string]any
	err = c.Do(ctx, http.MethodPost, base+"/issues", input, &issue)
	return issue, err
}
func (c *Client) Comment(ctx context.Context, repo string, number int64, body string) (map[string]any, error) {
	base, err := RepoPath(repo)
	if err != nil || number <= 0 {
		return nil, errors.New("Issueの指定が不正です")
	}
	var comment map[string]any
	err = c.Do(ctx, http.MethodPost, base+"/issues/"+strconv.FormatInt(number, 10)+"/comments", map[string]any{"body": body}, &comment)
	return comment, err
}
func (c *Client) SetIssueState(ctx context.Context, repo string, number int64, state string) error {
	if state != "open" && state != "closed" {
		return errors.New("Issueの状態が不正です")
	}
	base, err := RepoPath(repo)
	if err != nil || number <= 0 {
		return errors.New("Issueの指定が不正です")
	}
	return c.Do(ctx, http.MethodPatch, base+"/issues/"+strconv.FormatInt(number, 10), map[string]any{"state": state}, nil)
}

// RepositoryArchived reads the repository's archived flag. The boolean is valid
// only when err is nil; missing or null flags must not imply an active repository.
func (c *Client) RepositoryArchived(ctx context.Context, repo string) (bool, error) {
	base, err := RepoPath(repo)
	if err != nil {
		return false, err
	}
	var info struct {
		Archived *bool `json:"archived"`
	}
	if err := c.Do(ctx, http.MethodGet, base, nil, &info); err != nil {
		return false, err
	}
	if info.Archived == nil {
		return false, errors.New("リポジトリの保管状態を確認できません")
	}
	return *info.Archived, nil
}

func (c *Client) File(ctx context.Context, repo, ref, file string, maxBytes int) ([]byte, error) {
	base, err := RepoPath(repo)
	if err != nil {
		return nil, err
	}
	if file == "" || strings.HasPrefix(file, "/") || strings.ContainsAny(file, "\\\x00\r\n") {
		return nil, errors.New("資料のパスが不正です")
	}
	parts := strings.Split(file, "/")
	for i, part := range parts {
		if part == ".." || part == "." || part == "" {
			return nil, errors.New("資料のパスが不正です")
		}
		parts[i] = url.PathEscape(part)
	}
	var value struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int    `json:"size"`
		Type     string `json:"type"`
	}
	if err := c.Do(ctx, "GET", base+"/contents/"+strings.Join(parts, "/")+"?ref="+url.QueryEscape(ref), nil, &value); err != nil {
		return nil, err
	}
	if value.Type != "file" || value.Encoding != "base64" || value.Size > maxBytes {
		return nil, errors.New("資料がファイルでないか読み取り上限を超えています")
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(value.Content, "\n", ""))
	if err != nil || len(data) > maxBytes {
		return nil, errors.New("資料を読み取り上限内で復号できません")
	}
	return data, nil
}
