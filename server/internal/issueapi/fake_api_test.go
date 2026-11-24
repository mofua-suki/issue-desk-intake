package issueapi

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"sync"

	"issue-desk-intake/server/internal/gitea"
)

type fakeAPI struct {
	mu             sync.Mutex
	issue          Object
	issues         []Object
	comments       []Object
	timeline       []Object
	unknownComment bool
	commentCalls   int
	closed         int
}

func (f *fakeAPI) Do(_ context.Context, method, route string, data, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var value any
	switch {
	case method == "GET" && route == "/repos/owner/repo":
		value = Object{"default_branch": "main"}
	case method == "GET" && strings.Contains(route, "/branches/"):
		value = Object{"commit": Object{"id": strings.Repeat("a", 40)}}
	case method == "PATCH" && route == "/repos/owner/repo/issues/1":
		for k, v := range fleet.ObjectOrEmpty(data) {
			f.issue[k] = v
		}
		if fleet.ObjectOrEmpty(data)["state"] == "closed" {
			f.closed++
			f.issue["closed_at"] = "2026-11-01T00:00:00.000Z"
			f.timeline = append(f.timeline, Object{"id": float64(len(f.timeline) + 1), "type": "close"})
		}
		value = f.issue
	default:
		return fmt.Errorf("fixture route: %s %s", method, route)
	}
	if out != nil {
		raw, _ := json.Marshal(value)
		return json.Unmarshal(raw, out)
	}
	return nil
}
func (f *fakeAPI) Pages(_ context.Context, route string) ([]Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(route, "/timeline") {
		return append([]Object{}, f.timeline...), nil
	}
	if strings.Contains(route, "/blocks") {
		return []Object{}, nil
	}
	return nil, fmt.Errorf("fixture pages %s", route)
}
func (f *fakeAPI) Issue(context.Context, string, int64) (Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return clone(f.issue), nil
}
func (f *fakeAPI) Issues(_ context.Context, _ string, state string) ([]Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issues != nil {
		out := []Object{}
		for _, issue := range f.issues {
			if state != "open" || issue["state"] == "open" {
				out = append(out, clone(issue))
			}
		}
		return out, nil
	}
	if state == "open" && f.issue["state"] != "open" {
		return []Object{}, nil
	}
	return []Object{clone(f.issue)}, nil
}
func (f *fakeAPI) Comments(_ context.Context, repo string, issue int64) ([]Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []Object{}
	for _, v := range f.comments {
		location := fleet.Text(v["issue_url"])
		if location == "" {
			// Existing seeded comments belong to the fixture's main Issue.
			location = commentLocation("owner/repo", 1)
		}
		if !strings.EqualFold(location, commentLocation(repo, issue)) {
			continue
		}
		out = append(out, clone(v))
	}
	return out, nil
}
func (f *fakeAPI) Comment(_ context.Context, repo string, issue int64, body string) (Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commentCalls++
	r := Object{"id": float64(len(f.comments) + 1), "body": body, "issue_url": commentLocation(repo, issue), "user": Object{"login": "intake"}, "created_at": "2026-11-01T00:00:00Z", "updated_at": "2026-11-01T00:00:00Z"}
	f.comments = append(f.comments, r)
	if f.unknownComment {
		f.unknownComment = false
		return nil, &gitea.Error{UnknownOutcome: true}
	}
	return clone(r), nil
}

func commentLocation(repo string, issue int64) string {
	return fmt.Sprintf("https://gitea.example/api/v1/repos/%s/issues/%d", repo, issue)
}
func (f *fakeAPI) SetIssueState(ctx context.Context, repo string, n int64, state string) error {
	return f.Do(ctx, "PATCH", fmt.Sprintf("/repos/%s/issues/%d", repo, n), Object{"state": state}, nil)
}
