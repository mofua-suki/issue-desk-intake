package consultationai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func searchFixture(t *testing.T, handler func(*http.Request, string) (string, string), count int) (*materials, *projectState) {
	t.Helper()
	entries := map[string]treeEntry{}
	contents := map[string]string{}
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("%02d.go", i)
		body := "needle " + name
		sha := fixtureSHA(body)
		entries[name] = treeEntry{Path: name, SHA: sha, Size: len(body)}
		contents[sha] = body
	}
	s, _ := blobFixture(t, func(r *http.Request) (string, string) {
		return handler(r, contents[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]])
	})
	return &materials{service: s}, &projectState{Name: "owner/app", entries: entries}
}
func TestSearchBoundedParallelOrdered(t *testing.T) {
	entered := make(chan string, 8)
	release := make(chan struct{})
	releaseFourth := make(chan struct{})
	var active, maximum atomic.Int32
	m, p := searchFixture(t, func(r *http.Request, body string) (string, string) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		entered <- body
		gate := release
		if strings.HasSuffix(body, "03.go") {
			gate = releaseFourth
		}
		select {
		case <-gate:
		case <-r.Context().Done():
		}
		return "base64", encoded(body)
	}, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type answer struct {
		text string
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		text, err := m.repoReference(ctx, p, "repo-search", ReadRequest{Mode: "keyword", Keyword: "needle"})
		done <- answer{text, err}
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("検索取得が並列に開始されません")
		}
	}
	close(release)
	for body := ""; !strings.HasSuffix(body, "04.go"); {
		select {
		case body = <-entered:
		case <-ctx.Done():
			t.Fatal("4件目の応答を待って次の取得が止まりました")
		}
	}
	close(releaseFourth)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	expected := []string{}
	for i := 0; i < 8; i++ {
		expected = append(expected, fmt.Sprintf("%02d.go:1: needle %02d.go", i, i))
	}
	if result.text != strings.Join(expected, "\n") || maximum.Load() != 4 {
		t.Fatalf("result=%q maximum=%d", result.text, maximum.Load())
	}
}
func TestSearchFirstErrorCancelsLaterReads(t *testing.T) {
	entered := make(chan struct{}, 4)
	ready := make(chan struct{})
	var finished atomic.Int32
	m, p := searchFixture(t, func(r *http.Request, body string) (string, string) {
		defer finished.Add(1)
		entered <- struct{}{}
		if strings.HasSuffix(body, "00.go") {
			select {
			case <-ready:
			case <-r.Context().Done():
			}
			return "invalid", ""
		}
		<-r.Context().Done()
		return "invalid", ""
	}, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := m.repoReference(ctx, p, "repo-search", ReadRequest{Mode: "keyword", Keyword: "needle"})
		done <- err
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("取得が開始されません")
		}
	}
	close(ready)
	err := <-done
	if err == nil || err.Error() != "資料検索 00.go: 資料の内容形式が不正です" || finished.Load() != 4 {
		t.Fatalf("err=%v finished=%d", err, finished.Load())
	}
}
func TestSearchCancellationJoinsReads(t *testing.T) {
	entered := make(chan struct{}, 4)
	var finished atomic.Int32
	m, p := searchFixture(t, func(r *http.Request, body string) (string, string) {
		entered <- struct{}{}
		<-r.Context().Done()
		finished.Add(1)
		return "base64", encoded(body)
	}, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := m.repoReference(ctx, p, "repo-search", ReadRequest{Mode: "keyword", Keyword: "needle"})
		done <- err
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("取得が開始されません")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || finished.Load() != 4 {
		t.Fatalf("err=%v finished=%d", err, finished.Load())
	}
}
