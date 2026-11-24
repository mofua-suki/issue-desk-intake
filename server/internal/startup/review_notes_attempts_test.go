package startup

import (
	"errors"
	"testing"
	"time"
)

func TestReviewNotesAttemptsSkipAndRetry(t *testing.T) {
	var attempts reviewNotesAttempts
	clock := time.Date(2026, 11, 4, 14, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	calls := 0
	run := func() (bool, error) { calls++; return true, errors.New("read failed after claim") }
	if attempts.run("a", now, run) == nil {
		t.Fatal("failure lost")
	}
	if err := attempts.run("a", now, run); err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
	_ = attempts.run("b", now, run)
	clock = clock.Add(2 * time.Hour)
	_ = attempts.run("a", now, run)
	if calls != 3 {
		t.Fatal(calls)
	}
	failed := func() (bool, error) { calls++; return false, errors.New("claim failed") }
	_ = attempts.run("c", now, failed)
	_ = attempts.run("c", now, failed)
	if calls != 5 {
		t.Fatal(calls)
	}
	clock = clock.Add(-24 * time.Hour)
	_ = attempts.run("a", now, run)
	if calls != 5 {
		t.Fatal(calls)
	}
}

func TestReviewNotesAttemptsMidnightWait(t *testing.T) {
	var attempts reviewNotesAttempts
	clock := time.Date(2026, 11, 4, 14, 59, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	calls := 0
	run := func() (bool, error) { calls++; clock = clock.Add(2 * time.Minute); return true, nil }
	_ = attempts.run("a", now, run)
	_ = attempts.run("a", now, run)
	if calls != 2 {
		t.Fatal(calls)
	}
}
