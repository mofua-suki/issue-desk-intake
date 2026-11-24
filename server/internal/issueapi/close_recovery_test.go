package issueapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

type closeRecoveryAPI struct {
	API
	detail atomic.Int64
	reads  atomic.Int64
	failPR bool
}

func (a *closeRecoveryAPI) Issue(ctx context.Context, repo string, n int64) (Object, error) {
	a.detail.Add(1)
	a.reads.Add(1)
	return a.API.Issue(ctx, repo, n)
}
func (a *closeRecoveryAPI) Comments(ctx context.Context, repo string, n int64) ([]Object, error) {
	a.reads.Add(1)
	return a.API.Comments(ctx, repo, n)
}
func (a *closeRecoveryAPI) Pages(ctx context.Context, route string) ([]Object, error) {
	a.reads.Add(1)
	return a.API.Pages(ctx, route)
}
func (a *closeRecoveryAPI) Do(ctx context.Context, method, route string, data, out any) error {
	if route == "/repos/owner/repo/pulls/12" {
		if method == "GET" {
			a.reads.Add(1)
		}
		if a.failPR {
			return fmt.Errorf("PR unavailable")
		}
		if p, ok := out.(*Object); ok {
			*p = Object{"state": "closed"}
		}
		return nil
	}
	return a.API.Do(ctx, method, route, data, out)
}
func recoveryFixture(t *testing.T, pr bool) (*fixture, *closeRecoveryAPI, *time.Time, Object) {
	t.Helper()
	f := newFixture(t)
	f.api.issue["state"] = "closed"
	f.api.issue["closed_at"] = "2026-11-01T00:00:00Z"
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	f.s.o.Now = func() time.Time { return now }
	if pr {
		if _, err := f.pg.Exec(`UPDATE jobs SET pr=12 WHERE id=$1`, f.job); err != nil {
			t.Fatal(err)
		}
	}
	api := &closeRecoveryAPI{API: f.api}
	f.s.o.Gitea = api
	r := Object{"id": "11111111-1111-4111-8111-111111111111", "status": "closed", "project": "owner/repo", "number": 1, "notePending": true, "reason": "voluntary"}
	return f, api, &now, r
}
func recoveryCycle(t *testing.T, f *fixture) {
	t.Helper()
	if err := f.s.recoverCloseCycle(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.s.wg.Wait()
}
func savedRecovery(t *testing.T, f *fixture, r Object) Object {
	t.Helper()
	v, err := f.s.read(f.ctx, "close_requests", fleet.Text(r["id"]))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestCloseRecoveryBacksOffUnknownNote(t *testing.T) {
	f, a, now, r := recoveryFixture(t, false)
	r["noteAttempted"] = true
	if err := f.s.saveClose(f.ctx, r); err != nil {
		t.Fatal(err)
	}
	recoveryCycle(t, f)
	saved := savedRecovery(t, f, r)
	retry, err := time.Parse(time.RFC3339Nano, fleet.Text(saved["retryAt"]))
	if err != nil || a.detail.Load() != 1 {
		t.Fatalf("saved=%v detail=%d err=%v", saved, a.detail.Load(), err)
	}
	for i := 0; i < 3; i++ {
		*now = now.Add(10 * time.Second)
		recoveryCycle(t, f)
		if a.detail.Load() != 1 {
			t.Fatal("early Detail", a.detail.Load())
		}
	}
	*now = retry.Add(time.Second)
	recoveryCycle(t, f)
	if a.detail.Load() != 2 {
		t.Fatal(a.detail.Load())
	}
}
func TestCloseRecoveryBacksOffFailedPRClose(t *testing.T) {
	f, a, now, r := recoveryFixture(t, true)
	a.failPR = true
	r["notePending"] = false
	r["prClose"] = Object{"result": "failed"}
	if err := f.s.saveClose(f.ctx, r); err != nil {
		t.Fatal(err)
	}
	for i, want := range []time.Duration{1, 2, 4, 8, 16, 32, 60, 60} {
		start := *now
		recoveryCycle(t, f)
		saved := savedRecovery(t, f, r)
		at, err := time.Parse(time.RFC3339Nano, fleet.Text(saved["retryAt"]))
		if err != nil || at.Sub(start) != want*time.Minute || fleet.ObjectOrEmpty(saved["prClose"])["result"] != "failed" {
			t.Fatalf("attempt %d: %v %v", i, saved, err)
		}
		*now = at
	}
	if a.detail.Load() != 8 {
		t.Fatal(a.detail.Load())
	}
}
func TestCloseRecoveryFinishesAndStops(t *testing.T) {
	f, a, _, r := recoveryFixture(t, true)
	r["prClose"] = Object{"result": "failed"}
	r["recoveryAttempts"] = 2
	r["recoveryPending"] = true
	r["retryAt"] = nil
	if err := f.s.saveClose(f.ctx, r); err != nil {
		t.Fatal(err)
	}
	recoveryCycle(t, f)
	saved := savedRecovery(t, f, r)
	if saved["notePending"] != false || fleet.ObjectOrEmpty(saved["prClose"])["result"] != "closed" || saved["retryAt"] != nil || saved["recoveryAttempts"] != nil || saved["recoveryPending"] != nil || f.api.commentCalls != 1 {
		t.Fatalf("saved=%v comments=%d", saved, f.api.commentCalls)
	}
	recoveryCycle(t, f)
	if a.detail.Load() != 1 || f.api.commentCalls != 1 {
		t.Fatal(a.detail.Load(), f.api.commentCalls)
	}
}
func TestCloseRecoveryDetailCallsPerCycle(t *testing.T) {
	f, a, now, _ := recoveryFixture(t, false)
	for i := 0; i < 1000; i++ {
		if _, err := f.app.DB().Exec(bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), fmt.Sprintf("draft-%d", i), `{"project":"owner/repo"}`); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50; i++ {
		r := Object{"id": fmt.Sprintf("%08d-1111-4111-8111-111111111111", i), "status": "closed", "project": "owner/repo", "number": 1, "notePending": true, "noteAttempted": true}
		if err := f.s.saveClose(f.ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	dueTotal := 0
	for cycle := 0; cycle < 10; cycle++ {
		records, err := f.s.closeRecords(f.ctx, closeRecoveryFilter())
		if err != nil {
			t.Fatal(err)
		}
		due := 0
		for _, r := range records {
			at, err := time.Parse(time.RFC3339Nano, fleet.Text(r["retryAt"]))
			if closeNotePending(r) && (err != nil || !at.After(*now)) {
				due++
			}
		}
		beforeCalls := a.detail.Load()
		recoveryCycle(t, f)
		calls := a.detail.Load() - beforeCalls
		if calls > int64(due) {
			t.Fatalf("cycle %d: detail %d > due %d", cycle, calls, due)
		}
		dueTotal += due
		*now = now.Add(time.Minute)
	}
	runtime.ReadMemStats(&after)
	t.Logf("detail=%d due=%d gitea_reads=%d allocated_bytes=%d", a.detail.Load(), dueTotal, a.reads.Load(), after.TotalAlloc-before.TotalAlloc)
	if a.detail.Load() == 0 || a.detail.Load() > int64(dueTotal) {
		t.Fatal("detail count", a.detail.Load(), dueTotal)
	}
}
