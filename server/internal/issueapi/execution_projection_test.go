package issueapi

import (
	"context"
	"issue-desk-intake/server/internal/jobs"
	"testing"
	"time"
)

func TestExecutionProjectionRequiresCurrentOwner(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	s := &Service{o: Options{Now: func() time.Time { return now }}}
	host := "worker"
	until := now.Add(time.Minute).Format(time.RFC3339Nano)
	job := &jobs.Job{Status: "running", LeaseHost: &host, LeaseExpiresAt: &until, View: Object{}, Result: Object{}}
	check := func(status string, active, scheduled bool) {
		t.Helper()
		got, err := s.jobProjectionWithWait(context.Background(), "owner/repo", status, job, nil)
		if err != nil || got["executionActive"] != active || got["executionScheduled"] != scheduled {
			t.Fatalf("%s: %v %v", status, got, err)
		}
	}
	check("running", true, false)
	check("scope_waiting", false, true)
	until = now.Format(time.RFC3339Nano)
	check("running", false, false)
	until = now.Add(time.Minute).Format(time.RFC3339Nano)
	job.LeaseHost = nil
	check("running", false, false)
	job.Status = "queued"
	check("queued", false, true)
	job.Status = "stopped"
	check("stopped", false, false)
}
