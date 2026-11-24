package consultationai

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/gitea"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type observedContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestBlobWaiterReleasedOnAbnormalExit(t *testing.T) {
	var cache blobCache
	key := blobKey{"owner/app", "sha"}
	entered, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ownerDone)
		_, _ = cache.get(context.Background(), key, 4, func() (string, error) {
			close(entered)
			<-release
			runtime.Goexit()
			return "", nil
		})
	}()
	<-entered
	waiterCtx := &observedContext{Context: context.Background(), waiting: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := cache.get(waiterCtx, key, 4, func() (string, error) { return "data", nil })
		result <- err
	}()
	<-waiterCtx.waiting
	close(release)
	<-ownerDone
	if err := <-result; err == nil {
		t.Fatal("abnormal exit must fail the waiting request")
	}
	got, err := cache.get(context.Background(), key, 4, func() (string, error) { return "data", nil })
	if err != nil || got != "data" {
		t.Fatalf("subsequent request: %q %v", got, err)
	}
}

func TestBlobWaiterRetriesCanceledRequest(t *testing.T) {
	body := "shared content"
	var calls atomic.Int32
	entered := make(chan struct{})
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New("https://gitea.invalid", token, transportFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		w := httptest.NewRecorder()
		_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": encoded(body)})
		return w.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	m := &materials{service: &Service{options: Options{Gitea: client}}}
	p := &projectState{Name: "owner/app"}
	entry := treeEntry{SHA: fixtureSHA(body), Size: len(body)}
	ownerCtx, cancel := context.WithCancel(context.Background())
	ownerDone := make(chan error, 1)
	go func() { _, err := m.blob(ownerCtx, p, entry); ownerDone <- err }()
	<-entered
	waiterCtx := &observedContext{Context: context.Background(), waiting: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		got, err := m.blob(waiterCtx, p, entry)
		if err != nil || got != body {
			result <- fmt.Errorf("got %q: %v", got, err)
			return
		}
		result <- nil
	}()
	<-waiterCtx.waiting
	cancel()
	if err := <-ownerDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests: %d", calls.Load())
	}
}

func TestBlobWaiterRetriesExpiredFetchAndCachesResult(t *testing.T) {
	body := "shared after timeout"
	entered := make(chan struct{})
	var calls atomic.Int32
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New("https://gitea.invalid", token, transportFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		w := httptest.NewRecorder()
		_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": encoded(body)})
		return w.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	m := &materials{service: &Service{options: Options{Gitea: client}}}
	p := &projectState{Name: "owner/app"}
	entry := treeEntry{SHA: fixtureSHA(body), Size: len(body)}
	ownerDone := make(chan error, 1)
	ownerCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { _, err := m.blob(ownerCtx, p, entry); ownerDone <- err }()
	<-entered
	waiterCtx := &observedContext{Context: context.Background(), waiting: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		got, err := m.blob(waiterCtx, p, entry)
		if err != nil || got != body {
			result <- fmt.Errorf("got %q: %v", got, err)
			return
		}
		result <- nil
	}()
	<-waiterCtx.waiting
	if err := <-ownerDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("owner: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	got, err := m.blob(context.Background(), p, entry)
	if err != nil || got != body || calls.Load() != 2 {
		t.Fatalf("cached retry: %q %v, requests: %d", got, err, calls.Load())
	}
}

func fixtureSHA(s string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(s), s))))
}
func encoded(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
func blobFixture(t *testing.T, handler func(*http.Request) (string, string)) (*Service, *atomic.Int32) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	client, err := gitea.New("https://gitea.invalid", token, transportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		enc, body := handler(r)
		w := httptest.NewRecorder()
		_ = json.NewEncoder(w).Encode(map[string]string{"encoding": enc, "content": body})
		return w.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{options: Options{Gitea: client}}, &calls
}
func TestBlobSearchReuseAcrossMaterials(t *testing.T) {
	bodies := []string{"alpha and beta\n", "alpha\nbeta\n"}
	entries := map[string]treeEntry{}
	contents := map[string]string{}
	for i, body := range bodies {
		sha := fixtureSHA(body)
		name := fmt.Sprintf("%d.go", i)
		entries[name] = treeEntry{Path: name, SHA: sha, Size: len(body)}
		contents[sha] = body
	}
	s, calls := blobFixture(t, func(r *http.Request) (string, string) {
		return "base64", encoded(contents[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]])
	})
	m := &materials{service: s}
	p := &projectState{Name: "owner/app", Revision: strings.Repeat("a", 40), entries: entries}
	first, err := m.repoReference(context.Background(), p, "repo-search", ReadRequest{Mode: "keyword", Keyword: "alpha"})
	if err != nil || first != "0.go:1: alpha and beta\n1.go:1: alpha" || calls.Load() != 2 {
		t.Fatalf("first %q %v calls %d", first, err, calls.Load())
	}
	second, err := m.repoReference(context.Background(), p, "repo-search", ReadRequest{Mode: "keyword", Keyword: "beta"})
	if err != nil || second != "0.go:1: alpha and beta\n1.go:2: beta" || calls.Load() != 2 {
		t.Fatalf("second %q %v calls %d", second, err, calls.Load())
	}
	other := &materials{service: s}
	for _, entry := range entries {
		got, err := other.blob(context.Background(), &projectState{Name: "owner/app"}, entry)
		if err != nil || got != contents[entry.SHA] {
			t.Fatalf("other %q %v", got, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
	for _, entry := range entries {
		_, err := other.blob(context.Background(), &projectState{Name: "other/app"}, entry)
		if err != nil {
			t.Fatal(err)
		}
		break
	}
	if calls.Load() != 3 {
		t.Fatal(calls.Load())
	}
}
func TestBlobConcurrentReads(t *testing.T) {
	body := "concurrent blob"
	entered := make(chan struct{})
	release := make(chan struct{})
	s, calls := blobFixture(t, func(*http.Request) (string, string) { close(entered); <-release; return "base64", encoded(body) })
	entry := treeEntry{SHA: fixtureSHA(body), Size: len(body)}
	const n = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := (&materials{service: s}).blob(context.Background(), &projectState{Name: "owner/app"}, entry)
			if err != nil || got != body {
				errs <- fmt.Errorf("%q: %v", got, err)
			}
		}()
	}
	close(start)
	<-entered
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}
func TestBlobCacheEvictionAndLimits(t *testing.T) {
	contents := map[string]string{}
	for _, body := range []string{"aaaa", "bbbbb", "ccccccccccc"} {
		contents[fixtureSHA(body)] = body
	}
	s, calls := blobFixture(t, func(r *http.Request) (string, string) {
		return "base64", encoded(contents[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]])
	})
	s.blobs.limit = 8
	m := &materials{service: s}
	p := &projectState{Name: "owner/app"}
	for _, body := range []string{"aaaa", "bbbbb", "ccccccccccc", "aaaa"} {
		got, err := m.blob(context.Background(), p, treeEntry{SHA: fixtureSHA(body), Size: len(body)})
		if err != nil || got != body {
			t.Fatalf("%q %v", got, err)
		}
		if s.blobs.size > 8 {
			t.Fatal(s.blobs.size)
		}
	}
	if calls.Load() != 4 {
		t.Fatal(calls.Load())
	}
}
func TestBlobValidationFailuresAreNotCached(t *testing.T) {
	body := "valid text"
	entry := treeEntry{SHA: fixtureSHA(body), Size: len(body)}
	cases := []struct {
		name, encoding, content string
		size                    int
	}{{"size", "base64", encoded(body), len(body) + 1}, {"encoding", "hex", encoded(body), len(body)}, {"base64", "base64", "%%%", len(body)}, {"utf8", "base64", encoded(string([]byte{0xff})), 1}, {"sha", "base64", encoded("wrong text"), len(body)}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var attempt atomic.Int32
			s, calls := blobFixture(t, func(*http.Request) (string, string) {
				if attempt.Add(1) == 1 {
					return tc.encoding, tc.content
				}
				return "base64", encoded(body)
			})
			m := &materials{service: s}
			p := &projectState{Name: "owner/app"}
			_, err := m.blob(context.Background(), p, treeEntry{SHA: entry.SHA, Size: tc.size})
			if err == nil {
				t.Fatal("expected failure")
			}
			got, err := m.blob(context.Background(), p, entry)
			if err != nil || got != body || calls.Load() != 2 {
				t.Fatalf("retry %q %v calls %d", got, err, calls.Load())
			}
			_, err = m.blob(context.Background(), p, treeEntry{SHA: entry.SHA, Size: len(body) + 1})
			if err == nil || calls.Load() != 2 {
				t.Fatalf("cached size %v calls %d", err, calls.Load())
			}
		})
	}
}
func TestRepoSearchTooManyResults(t *testing.T) {
	body := strings.Repeat(strings.Repeat("x", 500)+"\n", 4000)
	s, _ := blobFixture(t, func(*http.Request) (string, string) { return "base64", encoded(body) })
	m := &materials{service: s}
	p := &projectState{Name: "owner/app", entries: map[string]treeEntry{}}
	for i := 0; i < 3; i++ {
		p.entries[fmt.Sprintf("%d.go", i)] = treeEntry{SHA: fixtureSHA(body), Size: len(body)}
	}
	_, err := m.repoReference(context.Background(), p, "repo-search", ReadRequest{Mode: "keyword", Keyword: "x"})
	if err == nil || err.Error() != "検索結果が多いため、キーワードを絞ってください" {
		t.Fatalf("error %v", err)
	}
}

func TestPinnedSearchResultReuseAcrossMaterials(t *testing.T) {
	const body = "alpha beta\n"
	var fail atomic.Bool
	fail.Store(true)
	service, calls := blobFixture(t, func(*http.Request) (string, string) {
		if fail.Load() {
			return "base64", encoded("does not match the pinned blob")
		}
		return "base64", encoded(body)
	})
	service.blobs.limit = 1 // The second search cannot be served by the blob cache.
	search := func(ctx context.Context, repo, revision, word string) (string, error) {
		m := &materials{service: service}
		p := &projectState{Name: repo, Revision: strings.Repeat(revision, 40), entries: map[string]treeEntry{
			"main.go": {Path: "main.go", SHA: fixtureSHA(body), Size: len(body)},
		}}
		return m.repoReference(ctx, p, "repo-search", ReadRequest{Mode: "keyword", Keyword: word})
	}
	if _, err := search(t.Context(), "owner/app", "a", "alpha"); err == nil {
		t.Fatal("invalid blob was accepted")
	}
	fail.Store(false)
	before := calls.Load()
	for i := 0; i < 2; i++ {
		got, err := search(t.Context(), "owner/app", "a", "alpha")
		if err != nil || got != "main.go:1: alpha beta" || calls.Load() != before+1 {
			t.Fatalf("search %d: %q, calls=%d, err=%v", i, got, calls.Load(), err)
		}
	}
	for _, args := range [][3]string{{"owner/app", "a", "beta"}, {"owner/app", "b", "alpha"}, {"owner/other", "a", "alpha"}} {
		before := calls.Load()
		if _, err := search(t.Context(), args[0], args[1], args[2]); err != nil || calls.Load() != before+1 {
			t.Fatalf("search crossed cache key %v: calls=%d err=%v", args, calls.Load(), err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := search(ctx, "owner/app", "a", "alpha"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached result ignored cancellation: %v", err)
	}
}
