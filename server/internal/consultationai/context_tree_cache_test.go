package consultationai

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/gitea"
)

func treeFixture(t *testing.T, handler func(*http.Request) any) *Service {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := gitea.New("https://gitea.invalid", token, transportFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		if err := json.NewEncoder(w).Encode(handler(r)); err != nil {
			return nil, err
		}
		return w.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{options: Options{Gitea: client}}
}

func TestTreeCacheJarSearch(t *testing.T) {
	bodies := map[string]string{
		"gradle/wrapper.jar": "\x00lower jar",
		"lib/UPPER.JAR":      "\x00upper jar",
		"src/main.go":        "package main\nneedle source\n",
	}
	var entries []treeEntry
	blobs := map[string]string{}
	shas := map[string]string{}
	for name, body := range bodies {
		sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(body), body)))
		sha := fmt.Sprintf("%x", sum)
		shas[name], blobs[sha] = sha, body
		entries = append(entries, treeEntry{Path: name, Type: "blob", Mode: "100644", SHA: sha, Size: len(body)})
	}
	var mu sync.Mutex
	reads := map[string]int{}
	s := treeFixture(t, func(r *http.Request) any {
		if strings.Contains(r.URL.Path, "/git/trees/") {
			return map[string]any{"tree": entries, "truncated": false}
		}
		if strings.Contains(r.URL.Path, "/git/blobs/") {
			sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			mu.Lock()
			reads[sha]++
			mu.Unlock()
			return map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(blobs[sha]))}
		}
		t.Errorf("unexpected request: %s", r.URL.Path)
		return nil
	})
	p := &projectState{Name: "owner/app", Branch: "trunk", Revision: strings.Repeat("a", 40)}
	m := &materials{service: s, draft: Record{"project": p.Name}, allowed: map[string]bool{p.Name: true}, state: contextState{Projects: map[string]*projectState{p.Name: p}}, restoring: true}
	doc, _, err := m.read(context.Background(), ReadRequest{File: "repo-search", Mode: "keyword", Keyword: "needle"})
	if err != nil || doc.Reason != "" || !strings.Contains(doc.Text, "src/main.go:2: needle source") {
		t.Fatalf("search: %+v err=%v", doc, err)
	}
	for _, name := range []string{"gradle/wrapper.jar", "lib/UPPER.JAR"} {
		if reads[shas[name]] != 0 {
			t.Errorf("%s blob reads=%d", name, reads[shas[name]])
		}
	}
	if reads[shas["src/main.go"]] != 1 {
		t.Errorf("source blob reads=%d", reads[shas["src/main.go"]])
	}
}
func TestTreeCacheCompleteScopedAndIsolated(t *testing.T) {
	var calls atomic.Int32
	entry := treeEntry{Path: "src/app.go", Type: "blob", Mode: "100644", SHA: strings.Repeat("b", 40), Size: 12}
	denied := entry
	denied.Path = "private/key.go"
	symlink := entry
	symlink.Path = "link.go"
	symlink.Mode = "120000"
	s := treeFixture(t, func(r *http.Request) any {
		calls.Add(1)
		return map[string]any{"tree": []treeEntry{entry, denied, symlink}, "truncated": false}
	})
	load := func(repo, revision string) *projectState {
		t.Helper()
		p := &projectState{Name: repo, Revision: revision, Branch: "trunk"}
		m := &materials{service: s, allowed: map[string]bool{repo: true}, state: contextState{Projects: map[string]*projectState{repo: p}}}
		if _, err := m.project(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
		if len(p.entries) != 1 || p.entries[entry.Path] != entry {
			t.Fatalf("entries=%v", p.entries)
		}
		return p
	}
	revision := strings.Repeat("a", 40)
	p := load("owner/app", revision)
	delete(p.entries, entry.Path)
	load("owner/app", revision)
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	load("owner/other", revision)
	load("owner/app", strings.Repeat("c", 40))
	if calls.Load() != 3 {
		t.Fatal(calls.Load())
	}
	deniedMaterials := &materials{service: s, allowed: map[string]bool{}, state: contextState{Projects: map[string]*projectState{}}}
	if _, err := deniedMaterials.project(context.Background(), "owner/app"); err == nil {
		t.Fatal("許可のないプロジェクトを再利用しました")
	}
}
func TestTreeCacheDoesNotRememberIncompleteListing(t *testing.T) {
	var calls atomic.Int32
	entry := treeEntry{Path: "ok.go", Type: "blob", Mode: "100644", SHA: strings.Repeat("b", 40)}
	s := treeFixture(t, func(r *http.Request) any {
		switch calls.Add(1) {
		case 1:
			return map[string]any{"tree": []treeEntry{entry}, "truncated": true}
		case 2:
			return map[string]any{"tree": nil, "truncated": false}
		default:
			return map[string]any{"tree": []treeEntry{entry}, "truncated": false}
		}
	})
	m := &materials{service: s}
	p := &projectState{Name: "owner/app", Revision: strings.Repeat("a", 40)}
	if err := m.loadTree(context.Background(), p.Name, p); err == nil || p.entries != nil {
		t.Fatalf("err=%v entries=%v", err, p.entries)
	}
	if err := m.loadTree(context.Background(), p.Name, p); err != nil {
		t.Fatal(err)
	}
	if err := m.loadTree(context.Background(), p.Name, &projectState{Revision: p.Revision}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || len(p.entries) != 1 {
		t.Fatalf("calls=%d entries=%v", calls.Load(), p.entries)
	}
}

func TestTreeCacheConcurrentReuseAndEviction(t *testing.T) {
	var c treeCache
	var calls atomic.Int32
	key := blobKey{"owner/app", "revision"}
	entries := map[string]treeEntry{"a.go": {Path: "a.go"}}
	c.limit = treeSize(key, entries)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := c.get(ctx, key, func() (map[string]treeEntry, error) { calls.Add(1); return entries, nil })
			if err != nil || len(got) != 1 {
				t.Errorf("got=%v err=%v", got, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	for _, k := range []blobKey{{"owner/app", "revisio2"}, key} {
		if _, err := c.get(ctx, k, func() (map[string]treeEntry, error) { calls.Add(1); return entries, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 || c.size > c.limit || len(c.entries) != 1 {
		t.Fatalf("calls=%d size=%d entries=%d", calls.Load(), c.size, len(c.entries))
	}
}
func TestTreeCacheCancelledOwnerDoesNotPoisonWaiter(t *testing.T) {
	var c treeCache
	key := blobKey{"owner/app", "revision"}
	owner, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	ownerDone := make(chan error, 1)
	go func() {
		_, err := c.get(owner, key, func() (map[string]treeEntry, error) { close(entered); <-owner.Done(); return nil, owner.Err() })
		ownerDone <- err
	}()
	<-entered
	ctx, finish := context.WithTimeout(context.Background(), 5*time.Second)
	defer finish()
	waiter := make(chan error, 1)
	go func() {
		got, err := c.get(ctx, key, func() (map[string]treeEntry, error) { return map[string]treeEntry{"ok.go": {}}, nil })
		if err == nil && len(got) != 1 {
			err = errors.New("再取得の結果がありません")
		}
		waiter <- err
	}()
	cancel()
	if err := <-ownerDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-waiter; err != nil {
		t.Fatal(err)
	}
}

func TestTreeCacheCancelledWaiterLeavesOwnerUsable(t *testing.T) {
	var c treeCache
	key := blobKey{"owner/app", "revision"}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := c.get(context.Background(), key, func() (map[string]treeEntry, error) {
			close(entered)
			<-release
			return map[string]treeEntry{"ok.go": {}}, nil
		})
		done <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.get(ctx, key, func() (map[string]treeEntry, error) {
		t.Error("取消済みの取得を開始しました")
		return nil, nil
	})
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := c.get(context.Background(), key, func() (map[string]treeEntry, error) {
		t.Error("正常な結果を再取得しました")
		return nil, nil
	})
	if err != nil || len(got) != 1 {
		t.Fatalf("got=%v err=%v", got, err)
	}
}
