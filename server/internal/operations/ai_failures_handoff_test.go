package operations

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
)

// notificationsAreStopped reports whether the reservation boundary is established.
func (s *AIFailures) notificationsAreStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notificationsStopped
}

func handoffObservation(step string) ai.Observation {
	now := time.Now()
	return ai.Observation{Step: step, Provider: "test", Model: "test", Outcome: "failed", Cause: "schema", StartedAt: now, FinishedAt: now.Add(time.Second)}
}

func TestAIFailuresStopNotificationsWaitsAndPreservesRecords(t *testing.T) {
	dir := t.TempDir()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s := NewAIFailures(context.Background(), dir, func(ctx context.Context, _ map[string]any) (map[string]any, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			t.Error("send canceled by stop")
		}
		return map[string]any{"sent": 1}, nil
	})
	s.Observe(handoffObservation("before"))
	<-started
	done := make(chan struct{})
	go func() { s.StopNotifications(); close(done) }()
	// Wait until the stop boundary is established, without releasing the send.
	for !s.notificationsAreStopped() {
		time.Sleep(time.Millisecond)
	}
	secondDone := make(chan struct{})
	go func() { s.StopNotifications(); close(secondDone) }()
	s.Observe(handoffObservation("after"))
	select {
	case <-done:
		t.Fatal("stop returned before send completed")
	default:
	}
	select {
	case <-secondDone:
		t.Fatal("repeated stop returned before send completed")
	default:
	}
	close(release)
	<-done
	<-secondDone
	s.StopNotifications()
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
	rows, err := s.Alerts(context.Background())
	if err != nil || len(rows) != 2 {
		t.Fatalf("alerts = %v, %v", rows, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state", "intake-ai-failures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record failureRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	for _, r := range record.Ranges {
		if r.Scope.Step == "before" && (r.Incident.Notified == nil || r.Incident.Push["state"] != "returned" || r.Incident.Push["sent"] != float64(1)) {
			t.Fatalf("send result: %+v", r.Incident)
		}
		if r.Scope.Step == "after" && (r.Incident.Notified != nil || r.Incident.Push != nil) {
			t.Fatalf("suppressed reservation: %+v", r.Incident)
		}
	}
	s.Close()
}

func TestAIFailuresStopNotificationsConcurrentStart(t *testing.T) {
	for i := 0; i < 40; i++ {
		var calls, active atomic.Int32
		dir := t.TempDir()
		s := NewAIFailures(context.Background(), dir, func(context.Context, map[string]any) (map[string]any, error) {
			active.Add(1)
			defer active.Add(-1)
			calls.Add(1)
			return map[string]any{"sent": 1}, nil
		})
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(3)
		go func() { defer workers.Done(); <-start; s.Observe(handoffObservation("race")) }()
		for j := 0; j < 2; j++ {
			go func() {
				defer workers.Done()
				<-start
				s.StopNotifications()
				if n := active.Load(); n != 0 {
					t.Errorf("active sends after stop = %d", n)
				}
			}()
		}
		close(start)
		workers.Wait()
		before := calls.Load()
		s.Observe(handoffObservation("after"))
		s.StopNotifications()
		if calls.Load() != before || before > 1 {
			t.Fatalf("calls before=%d after=%d", before, calls.Load())
		}
		raw, err := os.ReadFile(filepath.Join(dir, "state", "intake-ai-failures.json"))
		if err != nil {
			t.Fatal(err)
		}
		var record failureRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range record.Ranges {
			if r.Scope.Step != "race" {
				continue
			}
			found = true
			if r.Incident == nil {
				t.Fatal("missing race incident")
			}
			if before == 0 && (r.Incident.Notified != nil || r.Incident.Push != nil) {
				t.Fatalf("unexpected reservation: %+v", r.Incident)
			}
			if before == 1 && (r.Incident.Notified == nil || r.Incident.Push["state"] != "returned" || r.Incident.Push["sent"] != float64(1)) {
				t.Fatalf("missing send result: %+v", r.Incident)
			}
		}
		if !found {
			t.Fatal("missing race range")
		}
		s.Close()
	}
}

// A stopped instance persists an unnotified incident for the next instance.
func TestAIFailuresHandoffNotifiesOnNextFailure(t *testing.T) {
	dir := t.TempDir()
	var oldCalls, newCalls atomic.Int32
	old := NewAIFailures(context.Background(), dir, func(context.Context, map[string]any) (map[string]any, error) { oldCalls.Add(1); return nil, nil })
	old.StopNotifications()
	old.Observe(handoffObservation("handoff"))
	old.Close()
	raw, err := os.ReadFile(filepath.Join(dir, "state", "intake-ai-failures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved failureRecord
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Ranges) != 1 {
		t.Fatalf("ranges = %d", len(saved.Ranges))
	}
	for _, r := range saved.Ranges {
		if r.Incident == nil || r.Incident.Notified != nil || r.Incident.Push != nil {
			t.Fatalf("stopped incident: %+v", r.Incident)
		}
	}
	next := NewAIFailures(context.Background(), dir, func(context.Context, map[string]any) (map[string]any, error) {
		newCalls.Add(1)
		return map[string]any{"sent": 1}, nil
	})
	if rows, err := next.Alerts(context.Background()); err != nil || len(rows) != 1 {
		t.Fatalf("reloaded alerts = %v, %v", rows, err)
	}
	if newCalls.Load() != 0 {
		t.Fatal("reload sent without a new failure")
	}
	next.Observe(handoffObservation("handoff"))
	next.StopNotifications()
	if oldCalls.Load() != 0 || newCalls.Load() != 1 {
		t.Fatalf("old calls = %d, new calls = %d", oldCalls.Load(), newCalls.Load())
	}
	raw, err = os.ReadFile(filepath.Join(dir, "state", "intake-ai-failures.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	for _, r := range saved.Ranges {
		if r.Incident == nil || r.Incident.Notified == nil || r.Incident.Push["state"] != "returned" || r.Incident.Push["sent"] != float64(1) {
			t.Fatalf("handoff result: %+v", r.Incident)
		}
	}
	next.Close()
}
