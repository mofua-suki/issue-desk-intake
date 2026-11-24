package proposals

import (
	"context"
	"fmt"
	"testing"
)

func TestAdoptCommitPagesInEitherOrder(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "forward"
		if reverse {
			name = "reverse"
		}
		t.Run(name, func(t *testing.T) {
			s, api, _ := adoptFixture(t)
			// 501 commits ensure that the former 500-commit limit is crossed.
			commits := make([]Object, 501)
			for i := range commits {
				sha := fmt.Sprintf("%040x", i+1)
				parents := []any{Object{"sha": fmt.Sprintf("%040x", i+10000)}}
				if i > 0 {
					parents = []any{Object{"sha": fmt.Sprintf("%040x", i)}, Object{"sha": fmt.Sprintf("%040x", i+10000)}}
				}
				commits[i] = Object{"sha": sha, "parents": parents}
			}
			api.pull["head"] = Object{"sha": fmt.Sprintf("%040x", 501)}
			api.pull["commits"] = 501
			api.commits = map[string][]Object{}
			for i := range commits {
				index := i
				if reverse {
					index = len(commits) - 1 - i
				}
				route := fmt.Sprintf("/repos/owner/repo/pulls/2/commits?limit=50&page=%d", i/50+1)
				api.commits[route] = append(api.commits[route], commits[index])
			}
			p := v3()
			p["id"] = workerFixtureID(fmt.Sprintf("%040x", 1), "1")
			c := proposalComment(p, 10)
			api.comments = []Object{c}
			retry, err := s.adopt(context.Background(), "owner/repo", 1, c)
			if retry || err != nil || api.posts != 1 {
				t.Fatalf("retry=%v err=%v posts=%d", retry, err, api.posts)
			}
		})
	}
}
