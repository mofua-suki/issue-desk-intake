package jobs

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"testing"
	"time"
)

// A Worker sends a heartbeat every 3 minutes and polls for work at other times,
// so the previous heartbeat is usually over a minute old when it asks.
func TestPostgresDispatchAcceptsHeartbeatWithinWorkerInterval(t *testing.T) {
	for _, tc := range []struct {
		name   string
		age    time.Duration
		leased bool
	}{{"just sent", 0, true}, {"one interval old", 3 * time.Minute, true}, {"missed two intervals", 6*time.Minute + time.Second, false}} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now()
			if _, err := db.ExecContext(ctx, `INSERT INTO hosts(host,ready,heartbeat_at,capacity)VALUES('nas',true,$1,1)`, at.Add(-tc.age)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status)VALUES(1,'owner/repo',1,'queued')`); err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			_ = json.Unmarshal([]byte(`{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":1,"repositories":{"owner/repo":{"rank":1}}}},"repositories":{}}`), &raw)
			a, err := fleet.NormalizeAssignments(raw)
			if err != nil {
				t.Fatal(err)
			}
			s, _ := New(db, Options{Now: func() time.Time { return at }})
			job, _, err := s.Next(ctx, "nas", 1, a)
			if err != nil || (job != nil) != tc.leased {
				t.Fatalf("job=%v err=%v", job, err)
			}
		})
	}
}

// Work stays with its owner while the owner still reports within the window,
// instead of moving to another host and resetting the worktree there.
func TestContinuationStaysWithRecentlyReportingOwner(t *testing.T) {
	at := time.Now()
	mark := map[string]any{"pending": true, "generation": float64(0), "host": "owner", "at": at.Add(-time.Minute).Format(time.RFC3339Nano)}
	job := &Job{View: map[string]any{"continuation": mark}, Repo: "owner/repo"}
	a := &fleet.Assignments{Implementers: map[string][]string{"owner/repo": {"owner", "other"}}}
	for _, tc := range []struct {
		age     time.Duration
		permits bool
		reason  string
	}{{3 * time.Minute, false, ""}, {6 * time.Minute, true, "stale"}} {
		states := map[string]hostState{"owner": {Host: "owner", Ready: true, Heartbeat: at.Add(-tc.age)}}
		_, permits, reason := continuationDecision(job, "other", states, a, at, time.Hour, func(hostState) string { return "" })
		if permits != tc.permits || reason != tc.reason {
			t.Fatalf("age=%v permits=%v reason=%q", tc.age, permits, reason)
		}
	}
}
