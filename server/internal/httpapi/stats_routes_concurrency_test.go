package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOverviewAndReworkSharedRequestsHTTP(t *testing.T) {
	for _, path := range []string{"/api/stats/overview?range=today", "/api/stats/rework"} {
		for _, cancelOne := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%t", path, cancelOne), func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				s, token := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
					o.StatisticsDrafts = func(ctx context.Context) ([]map[string]any, error) {
						calls.Add(1)
						close(started)
						select {
						case <-release:
							return []map[string]any{}, nil
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
				})
				if strings.Contains(path, "rework") {
					s.options.DB.SetMaxOpenConns(1)
					conn, err := s.options.DB.Conn(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { conn.Close() })
					// Release the only connection after both requests join the flight.
					go func() { <-release; conn.Close() }()
				}
				done := make(chan *httptest.ResponseRecorder, 2)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go func() { done <- statsHTTP(s, token, "GET", path, ctx) }()
				if strings.Contains(path, "overview") {
					<-started
				} else {
					statsAwaitWaiters(s, path+"?", 1)
				}
				go func() { done <- statsHTTP(s, token, "GET", path, nil) }()
				key := path
				if !strings.Contains(path, "?") {
					key += "?"
				}
				statsAwaitWaiters(s, key, 2)
				if cancelOne {
					cancel()
					<-done
				}
				close(release)
				got := <-done
				if !cancelOne {
					other := <-done
					if other.Code != 200 || !bytes.Equal(got.Body.Bytes(), other.Body.Bytes()) {
						t.Fatalf("shared responses: %d %d", got.Code, other.Code)
					}
				}
				if got.Code != 200 || !bytes.Contains(got.Body.Bytes(), []byte(`"unavailable":[]`)) {
					t.Fatalf("remaining request: %d %s", got.Code, got.Body.String())
				}
				if strings.Contains(path, "overview") && calls.Load() != 1 {
					t.Fatalf("draft reads=%d", calls.Load())
				}
				cached := statsHTTP(s, token, "GET", path, nil)
				if cached.Code != 200 || !bytes.Equal(got.Body.Bytes(), cached.Body.Bytes()) {
					t.Fatalf("cached response: %d %s", cached.Code, cached.Body.String())
				}
			})
		}
	}
}

func TestOverviewFailureRecomputesHTTP(t *testing.T) {
	var calls atomic.Int32
	s, token := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
		o.StatisticsDrafts = func(context.Context) ([]map[string]any, error) {
			if calls.Add(1) == 1 {
				return nil, errors.New("draft read failure")
			}
			return []map[string]any{}, nil
		}
	})
	path := "/api/stats/overview?range=today"
	failed := statsHTTP(s, token, "GET", path, nil)
	if failed.Code == 200 {
		t.Fatalf("failed read: %d %s", failed.Code, failed.Body.String())
	}
	recovered := statsHTTP(s, token, "GET", path, nil)
	if recovered.Code != 200 || calls.Load() != 2 || !bytes.Contains(recovered.Body.Bytes(), []byte(`"unavailable":[]`)) {
		t.Fatalf("recovery: %d calls=%d %s", recovered.Code, calls.Load(), recovered.Body.String())
	}
}

func TestOverviewAndReworkAllWaitersCancelThenRecomputeHTTP(t *testing.T) {
	for _, path := range []string{"/api/stats/overview?range=today", "/api/stats/rework"} {
		t.Run(path, func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			s, token := statsHTTPFixture(t, func(o *WorkerRouteOptions) {
				o.StatisticsDrafts = func(ctx context.Context) ([]map[string]any, error) {
					if calls.Add(1) == 1 {
						close(started)
						<-ctx.Done()
						close(stopped)
						return nil, ctx.Err()
					}
					return []map[string]any{}, nil
				}
			})
			var releaseDB func()
			if strings.Contains(path, "rework") {
				s.options.DB.SetMaxOpenConns(1)
				conn, err := s.options.DB.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				releaseDB = func() { conn.Close() }
				t.Cleanup(releaseDB)
			}
			ctx1, cancel1 := context.WithCancel(context.Background())
			defer cancel1()
			ctx2, cancel2 := context.WithCancel(context.Background())
			defer cancel2()
			done := make(chan *httptest.ResponseRecorder, 2)
			go func() { done <- statsHTTP(s, token, "GET", path, ctx1) }()
			key := path
			if !strings.Contains(path, "?") {
				key += "?"
			}
			if releaseDB == nil {
				<-started
			} else {
				statsAwaitWaiters(s, key, 1)
			}
			go func() { done <- statsHTTP(s, token, "GET", path, ctx2) }()
			statsAwaitWaiters(s, key, 2)
			cancel1()
			<-done
			cancel2()
			<-done
			if releaseDB == nil {
				<-stopped
			} else {
				releaseDB()
			}
			got := statsHTTP(s, token, "GET", path, nil)
			if got.Code != 200 || !bytes.Contains(got.Body.Bytes(), []byte(`"unavailable":[]`)) {
				t.Fatalf("recomputed response: %d %s", got.Code, got.Body.String())
			}
			if releaseDB == nil && calls.Load() != 2 {
				t.Fatalf("draft reads=%d", calls.Load())
			}
		})
	}
}
