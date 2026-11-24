package startup

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/consultations"
)

// followupComment represents the worker's published v3 proposal for both OpenApp tests.
func followupComment(id int, sha, expected string, worker any, gitURL string) map[string]any {
	sum := sha256.Sum256([]byte(sha))
	p := map[string]any{"version": 3, "kind": "followup", "id": fmt.Sprintf("worker-job-1-%x-1", sum[:16]), "source": "owner/repo#1", "title": "軽微な指摘の修正", "pr": gitURL + "/owner/repo/pulls/2", "items": []any{map[string]any{"problem": "余白がずれる", "expected": expected, "test": "画面の余白を検証する"}}, "dependsOn": []any{}, "provider": "codex", "changeScope": map[string]any{"paths": []any{"src/app.go"}}}
	raw, _ := json.Marshal(p)
	return map[string]any{"id": id, "body": "<!-- issue-desk-proposal " + string(raw) + " -->", "user": map[string]any{"login": worker}, "issue_url": gitURL + "/api/v1/repos/owner/repo/issues/1", "created_at": "2026-11-01T00:00:00Z"}
}

// fixtureCommits returns a complete PR history where b is an ordinary change after a.
func fixtureCommits(r *http.Request, head string) []any {
	if r.URL.Query().Get("page") != "1" {
		return []any{}
	}
	a := strings.Repeat("a", 40)
	commits := []any{map[string]any{"sha": a, "parents": []any{map[string]any{"sha": strings.Repeat("f", 40)}}}}
	if head != a {
		commits = append(commits, map[string]any{"sha": head, "parents": []any{map[string]any{"sha": a}}})
	}
	return commits
}

// fixtureHead snapshots shared state before calling the potentially slow lookup.
func fixtureHead(mu *sync.Mutex, snapshot func() (string, bool, *consultations.Records), lookup func(*consultations.Records) (bool, error)) (string, bool, error) {
	mu.Lock()
	head, merged, records := snapshot()
	mu.Unlock()
	if merged && records != nil {
		hasDraft, err := lookup(records)
		if err != nil {
			return "", merged, err
		}
		if hasDraft {
			head = strings.Repeat("a", 40)
		}
	}
	return head, merged, nil
}

func TestFixtureHeadReleasesMutexDuringLookup(t *testing.T) {
	var mu sync.Mutex
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan string, 1)
	go func() {
		head, _, _ := fixtureHead(&mu, func() (string, bool, *consultations.Records) {
			return strings.Repeat("b", 40), true, &consultations.Records{}
		}, func(*consultations.Records) (bool, error) {
			close(entered)
			<-release
			return true, nil
		})
		done <- head
	}()
	<-entered
	locked := make(chan struct{})
	go func() {
		mu.Lock()
		defer mu.Unlock()
		close(locked) // Signal successful acquisition while holding the mutex.
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("DB参照中に共有ロックを取得できません")
	}
	close(release)
	if head := <-done; head != strings.Repeat("a", 40) {
		t.Fatalf("下書きありのhead: %s", head)
	}
}
