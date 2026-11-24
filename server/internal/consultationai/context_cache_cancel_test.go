package consultationai

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/gitea"
)

type cancelledResponseBody struct {
	ctx     context.Context
	reading chan struct{}
	once    sync.Once
}

func (b *cancelledResponseBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.reading) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (*cancelledResponseBody) Close() error { return nil }

type joinedCacheContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *joinedCacheContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

// Gitea's body-read failure is opaque even when caused by cancellation. Each
// cache must isolate the cancelled owner and let an already joined reader retry.
func TestCacheOwnerBodyCancellationDoesNotFailOtherConsultation(t *testing.T) {
	for _, kind := range []string{"tree", "blob"} {
		t.Run(kind, func(t *testing.T) {
			ctx, finish := context.WithTimeout(t.Context(), 5*time.Second)
			defer finish()
			owner, cancel := context.WithCancel(ctx)
			defer cancel()
			reading := make(chan struct{})
			token := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			client, err := gitea.New("https://gitea.invalid", token, transportFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &cancelledResponseBody{ctx: r.Context(), reading: reading}}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			var tree treeCache
			var blob blobCache
			key := blobKey{repo: "owner/app", sha: "fixed-revision"}
			load := func(ctx context.Context, fetch func() error) error {
				if kind == "tree" {
					_, err := tree.get(ctx, key, func() (map[string]treeEntry, error) {
						if err := fetch(); err != nil {
							return nil, err
						}
						return map[string]treeEntry{"ok.go": {Path: "ok.go"}}, nil
					})
					return err
				}
				_, err := blob.get(ctx, key, 2, func() (string, error) {
					if err := fetch(); err != nil {
						return "", err
					}
					return "ok", nil
				})
				return err
			}
			ownerDone := make(chan error, 1)
			go func() {
				ownerDone <- load(owner, func() error { return client.Do(owner, "GET", "/repos/owner/app/git/trees/fixed", nil, nil) })
			}()
			select {
			case <-reading:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			waiter := &joinedCacheContext{Context: ctx, joined: make(chan struct{})}
			retried := false
			waiterDone := make(chan error, 1)
			go func() { waiterDone <- load(waiter, func() error { retried = true; return nil }) }()
			select {
			case <-waiter.joined:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			cancel()
			if err := <-ownerDone; !errors.Is(err, context.Canceled) {
				t.Fatalf("owner=%v", err)
			}
			if err := <-waiterDone; err != nil {
				t.Fatalf("waiter=%v", err)
			}
			if !retried {
				t.Fatal("取消後に別相談が再取得しなかった")
			}
		})
	}
}
