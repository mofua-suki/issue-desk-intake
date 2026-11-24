package screenapi

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"runtime"
	"sync"
	"testing"

	"issue-desk-intake/server/internal/listscope"
)

type joinReader struct {
	mu      sync.Mutex
	calls   int
	value   string
	entered chan int
	release chan struct{}
}

func (r *joinReader) read(ctx context.Context, origin string) ([]Object, error) {
	r.mu.Lock()
	r.calls++
	n := r.calls
	value := r.value
	r.mu.Unlock()
	r.entered <- n
	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []Object{{"project": "p", "number": 1, "title": value}}, nil
}
func joinService(r *joinReader) *Service {
	empty := func(context.Context, string) ([]Object, error) { return nil, nil }
	return &Service{o: Options{Lists: map[string]ListReader{"issue": r.read, "draft": empty, "todo": empty, "closeRequest": empty}}}
}
func joinCall(s *Service, ctx context.Context, origin, scope string) <-chan struct {
	snap Snapshot
	err  error
} {
	done := make(chan struct {
		snap Snapshot
		err  error
	}, 1)
	go func() {
		v, e := s.joinSnapshot(listscope.WithScope(ctx, scope), origin, scope, Object{"repositories": []any{Object{"name": "p"}}}, newListTiming())
		done <- struct {
			snap Snapshot
			err  error
		}{v, e}
	}()
	return done
}
func waitJoined(s *Service, n int) {
	for {
		s.joins.mu.Lock()
		ready := false
		for _, b := range s.joins.running {
			if b.next != nil && b.next.waiters >= n {
				ready = true
			}
		}
		s.joins.mu.Unlock()
		if ready {
			return
		}
		runtime.Gosched()
	}
}
func TestListJoinFreshnessAndReadCount(t *testing.T) {
	r := &joinReader{value: "old", entered: make(chan int, 5), release: make(chan struct{}, 5)}
	s := joinService(r)
	first := joinCall(s, context.Background(), "a", "full")
	<-r.entered
	second := joinCall(s, context.Background(), "a", "full")
	third := joinCall(s, context.Background(), "a", "full")
	waitJoined(s, 2)
	r.mu.Lock()
	r.value = "new"
	r.mu.Unlock()
	r.release <- struct{}{}
	<-r.entered
	r.release <- struct{}{}
	for _, ch := range []<-chan struct {
		snap Snapshot
		err  error
	}{second, third} {
		got := <-ch
		if got.err != nil || len(got.snap.Rows) != 1 || got.snap.Rows[0].Item["title"] != "new" {
			t.Fatalf("後着結果: %+v", got)
		}
	}
	if got := <-first; got.err != nil || got.snap.Rows[0].Item["title"] != "old" {
		t.Fatalf("先行結果: %+v", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 2 {
		t.Fatalf("read count = %d", r.calls)
	}
}
func TestListJoinCancellation(t *testing.T) {
	for _, cancelFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "first", false: "later"}[cancelFirst], func(t *testing.T) {
			r := &joinReader{entered: make(chan int, 5), release: make(chan struct{}, 5)}
			s := joinService(r)
			c1, cancel1 := context.WithCancel(context.Background())
			defer cancel1()
			c2, cancel2 := context.WithCancel(context.Background())
			defer cancel2()
			first := joinCall(s, c1, "a", "full")
			<-r.entered
			later := joinCall(s, c2, "a", "full")
			waitJoined(s, 1)
			if cancelFirst {
				cancel1()
				if got := <-first; !errors.Is(got.err, context.Canceled) {
					t.Fatal(got.err)
				}
			} else {
				cancel2()
				if got := <-later; !errors.Is(got.err, context.Canceled) {
					t.Fatal(got.err)
				}
			}
			r.release <- struct{}{}
			<-r.entered
			r.release <- struct{}{}
			if cancelFirst {
				if got := <-later; got.err != nil {
					t.Fatal(got.err)
				}
			} else {
				if got := <-first; got.err != nil {
					t.Fatal(got.err)
				}
			}
		})
	}
}
func TestListJoinScopeIsolation(t *testing.T) {
	for _, tc := range []struct{ origin, scope string }{{"b", "full"}, {"a", "active"}} {
		t.Run(tc.origin+tc.scope, func(t *testing.T) {
			r := &joinReader{entered: make(chan int, 5), release: make(chan struct{}, 5)}
			s := joinService(r)
			first := joinCall(s, context.Background(), "a", "full")
			<-r.entered
			other := joinCall(s, context.Background(), tc.origin, tc.scope)
			<-r.entered
			r.release <- struct{}{}
			r.release <- struct{}{}
			if got := <-first; got.err != nil {
				t.Fatal(got.err)
			}
			if got := <-other; got.err != nil {
				t.Fatal(got.err)
			}
		})
	}
}

func TestListJoinHTTPDeltaAndValidation(t *testing.T) {
	f := newFixture(t)
	r := &joinReader{value: "old", entered: make(chan int, 5), release: make(chan struct{}, 5)}
	f.options.Lists["issue"] = func(ctx context.Context, origin string) ([]Object, error) {
		rows, err := r.read(ctx, origin)
		for _, row := range rows {
			row["project"] = "owner/repo"
		}
		return rows, err
	}
	f.restart()
	type response struct {
		code int
		body any
	}
	call := func(path string) <-chan response {
		ch := make(chan response, 1)
		go func() { code, body := f.call("GET", path, "screen", nil); ch <- response{code, body} }()
		return ch
	}
	first := call("/api/list")
	<-r.entered
	second := call("/api/list")
	third := call("/api/list")
	waitJoined(f.s, 2)
	code, _ := f.call("GET", "/api/list?since=invalid!", "screen", nil)
	if code != 400 {
		t.Fatalf("invalid cursor: %d", code)
	}
	r.mu.Lock()
	r.value = "new"
	r.mu.Unlock()
	r.release <- struct{}{}
	<-r.entered
	r.release <- struct{}{}
	a := <-first
	b := <-second
	c := <-third
	for _, v := range []response{b, c} {
		rows := fleet.Items(fleet.ObjectOrEmpty(v.body)["rows"])
		if v.code != 200 || len(rows) == 0 || fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(rows[0])["item"])["title"] != "new" {
			t.Fatalf("response: %+v", v)
		}
	}
	if a.code != 200 {
		t.Fatal(a)
	}
	cursor := fleet.Text(fleet.ObjectOrEmpty(b.body)["cursor"])
	if cursor == "" {
		t.Fatal("missing cursor")
	}
	delta := call("/api/list?since=" + cursor)
	<-r.entered
	r.release <- struct{}{}
	d := <-delta
	if d.code != 200 || fleet.ObjectOrEmpty(d.body)["full"] != false {
		t.Fatalf("delta: %+v", d)
	}
}

func TestListJoinFailureAndPolicyIsolation(t *testing.T) {
	t.Run("failure reaches all waiters", func(t *testing.T) {
		r := &joinReader{entered: make(chan int, 5), release: make(chan struct{}, 5)}
		s := joinService(r)
		failure := errors.New("read failed")
		s.o.Lists["issue"] = func(ctx context.Context, origin string) ([]Object, error) {
			_, err := r.read(ctx, origin)
			if err != nil {
				return nil, err
			}
			return nil, failure
		}
		first := joinCall(s, context.Background(), "a", "full")
		<-r.entered
		second := joinCall(s, context.Background(), "a", "full")
		third := joinCall(s, context.Background(), "a", "full")
		waitJoined(s, 2)
		r.release <- struct{}{}
		<-r.entered
		r.release <- struct{}{}
		for _, ch := range []<-chan struct {
			snap Snapshot
			err  error
		}{first, second, third} {
			if got := <-ch; !errors.Is(got.err, failure) {
				t.Fatal(got.err)
			}
		}
	})
	t.Run("different allowed repositories", func(t *testing.T) {
		r := &joinReader{entered: make(chan int, 5), release: make(chan struct{}, 5)}
		s := joinService(r)
		first := joinCall(s, context.Background(), "a", "full")
		<-r.entered
		done := make(chan error, 1)
		go func() {
			_, err := s.joinSnapshot(listscope.WithScope(context.Background(), "full"), "a", "full", Object{"repositories": []any{Object{"name": "other"}}}, newListTiming())
			done <- err
		}()
		<-r.entered
		r.release <- struct{}{}
		r.release <- struct{}{}
		if got := <-first; got.err != nil {
			t.Fatal(got.err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}
