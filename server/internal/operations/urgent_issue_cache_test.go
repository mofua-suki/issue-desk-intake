package operations

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type concurrentIssueReader struct {
	calls  atomic.Int64
	closed atomic.Bool
	active atomic.Int64
	peak   atomic.Int64
	ready  chan struct{}
	once   sync.Once
}

func (r *concurrentIssueReader) Do(ctx context.Context, _, _ string, _ any, out any) error {
	r.calls.Add(1)
	active := r.active.Add(1)
	defer r.active.Add(-1)
	for peak := r.peak.Load(); active > peak; peak = r.peak.Load() {
		if r.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	if active == urgentIssueWorkers {
		r.once.Do(func() { close(r.ready) })
	}
	select {
	case <-r.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	state := "open"
	if r.closed.Load() {
		state = "closed"
	}
	*out.(*map[string]any) = map[string]any{"state": state}
	return nil
}
func TestUrgentHoldsClosedIssuesConcurrentAndCachedPostgres(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	reader := &concurrentIssueReader{ready: make(chan struct{})}
	reader.closed.Store(true)
	s.Gitea = reader
	for i := 1; i <= 100; i++ {
		if _, err := s.PostgreSQL.Exec("INSERT INTO jobs(repo,issue,title,status,priority,updated_at) VALUES('owner/repo',$1,'closed','failed',1,$2)", i, s.now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.urgentHolds(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if reader.calls.Load() != 100 || reader.peak.Load() != urgentIssueWorkers {
		t.Fatalf("calls=%d peak=%d", reader.calls.Load(), reader.peak.Load())
	}
	reader.closed.Store(false)
	rows, err = s.urgentHolds(t.Context())
	if err != nil || len(rows) != 0 || reader.calls.Load() != 100 {
		t.Fatalf("cached rows=%d calls=%d err=%v", len(rows), reader.calls.Load(), err)
	}
	s.urgentCache.mu.Lock()
	for key, entry := range s.urgentCache.entries {
		entry.expires = time.Now().Add(-time.Second)
		s.urgentCache.entries[key] = entry
	}
	s.urgentCache.mu.Unlock()
	rows, err = s.urgentHolds(t.Context())
	if err != nil || len(rows) != 50 || reader.calls.Load() != 200 {
		t.Fatalf("expired rows=%d calls=%d err=%v", len(rows), reader.calls.Load(), err)
	}
}

func TestUrgentHoldsMergeCommentsGetFreshDeadlinePostgres(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	s.Gitea = &slowUrgentReader{}
	now := s.now()
	var id int64
	err := s.PostgreSQL.QueryRow(`INSERT INTO jobs(repo,issue,title,status,hold_reason,priority,pr,generation,lease_host,lease_epoch,updated_at) VALUES('owner/repo',1,'merge','held','review_held',1,42,1,'worker',1,$1) RETURNING id`, now.Add(-time.Hour)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct{ kind, data string }{{"leased", `{"generation":1,"epoch":1}`}, {"state", `{"to":"held","holdReason":"review_held","epoch":1}`}} {
		if _, err = s.PostgreSQL.Exec(`INSERT INTO job_events(job_id,kind,host,at,data) VALUES($1,$2,'worker',$3,$4::jsonb)`, id, event.kind, now.Add(-time.Hour), event.data); err != nil {
			t.Fatal(err)
		}
	}
	s.TrustedWriter = func(map[string]any) bool { return true }
	called := false
	s.ReadComments = func(ctx context.Context, repo string, issue int64) ([]map[string]any, error) {
		called = true
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []map[string]any{{"id": float64(1), "body": fmt.Sprintf("<!-- gcr:notice:%s#%d:complete:42 -->", repo, issue), "created_at": now.Format(time.RFC3339Nano), "user": map[string]any{}}}, nil
	}
	rows, err := s.urgentHolds(t.Context())
	if err != nil || len(rows) != 0 || !called {
		t.Fatalf("rows=%v called=%v err=%v", rows, called, err)
	}
}
