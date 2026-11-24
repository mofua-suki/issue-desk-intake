package operations

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRecoveryReservationDeadlinePostgres(t *testing.T) {
	r, c, _ := recoveryActFixture(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.Service.Now = func() time.Time { return now }
	r.ReservationTimeout = time.Hour
	if d, _, e := r.Reserve(context.Background(), c); e != nil || d != "proceed" {
		t.Fatal(d, e)
	}
	now = now.Add(time.Hour - time.Nanosecond)
	other := c
	other.Cause = "other"
	if d, _, e := r.Reserve(context.Background(), other); e != nil || d != "already_reserved" {
		t.Fatal(d, e)
	}
	now = now.Add(time.Nanosecond)
	if d, _, e := r.Reserve(context.Background(), other); e != nil || d != "proceed" {
		t.Fatal(d, e)
	}
	if v := recoveryStored(t, r, c); v.State != "send_unknown" {
		t.Fatal(v)
	}
}

func TestRecoveryDatabaseReadFailuresRetryPostgres(t *testing.T) {
	for _, failure := range []string{"records", "recheck"} {
		t.Run(failure, func(t *testing.T) {
			r, c, a := recoveryActFixture(t)
			sends := 0
			a.Send = func(context.Context, string, int64, string, string, string) error { sends++; return nil }
			if failure == "records" {
				calls := 0
				a.records = func(ctx context.Context) ([]RecoveryRecord, error) {
					calls++
					if calls == 1 {
						return nil, errors.New("temporary")
					}
					return r.records(ctx)
				}
			} else {
				calls := 0
				a.recheck = func(ctx context.Context, c RecoveryCandidate) (bool, error) {
					calls++
					if calls == 1 {
						return false, errors.New("temporary")
					}
					return r.Recheck(ctx, c)
				}
			}
			got, err := a.Act(context.Background(), c)
			if got != Skipped || err == nil || sends != 0 || recoveryStored(t, r, c).State != "pre_send_error" {
				t.Fatal(got, err, sends)
			}
			got, err = a.Act(context.Background(), c)
			if got != Acted || err != nil || sends != 1 {
				t.Fatal(got, err, sends)
			}
		})
	}
}

func TestRecoveryPreSendErrorLimitPostgres(t *testing.T) {
	r, c, a := recoveryActFixture(t)
	r.PreSendErrorLimit = 3
	calls := 0
	a.Judge = func(context.Context, []any) (map[string]any, error) { calls++; return nil, errors.New("temporary") }
	for i := 0; i < 4; i++ {
		_, _ = a.Act(context.Background(), c)
	}
	if calls != 3 || recoveryStored(t, r, c).State != "skipped" {
		t.Fatal(calls, recoveryStored(t, r, c))
	}
}

func TestRecoveryPreSendLimitPersistsAcrossRestartPostgres(t *testing.T) {
	r, c, a := recoveryActFixture(t)
	r.PreSendErrorLimit = 2
	calls := 0
	a.Judge = func(context.Context, []any) (map[string]any, error) { calls++; return nil, errors.New("temporary") }
	if _, err := a.Act(context.Background(), c); err == nil {
		t.Fatal("expected temporary error")
	}
	a.Recovery = &JobRecovery{Service: r.Service, PreSendErrorLimit: 2}
	if _, err := a.Act(context.Background(), c); err == nil {
		t.Fatal("expected temporary error after restart")
	}
	a.Recovery = &JobRecovery{Service: r.Service, PreSendErrorLimit: 2}
	if result, err := a.Act(context.Background(), c); err != nil || result != Skipped || calls != 2 {
		t.Fatal(result, err, calls)
	}
	if state := recoveryStored(t, a.Recovery, c); state.State != "skipped" || state.PreSendErrors != 2 {
		t.Fatal(state)
	}
}
