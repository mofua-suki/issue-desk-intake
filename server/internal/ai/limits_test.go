package ai

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeLimitEventsAndFailedCall(t *testing.T) {
	sample := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.1,"resetsAt":1800000000},"seven_day":{"utilization":0.2,"resetsAt":1800000000}}}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unified_windows":{"five_hour":{"utilization":0.35,"resets_at":1800000000},"seven_day":{"utilization":"bad"}}}}`
	request := testRequest(t, "claude")
	var got []LimitObservation
	runner := Runner{ObserveLimits: func(rows []LimitObservation) { got = rows }, Execute: func(_ context.Context, p ProcessRequest) (ProcessResult, error) {
		if p.Arguments[0] == "auth" {
			return ProcessResult{Stdout: []byte(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"oauth_token"}`)}, nil
		}
		return ProcessResult{Stdout: []byte(sample)}, errors.New("failed")
	}}
	if _, err := runner.Call(context.Background(), request); err == nil {
		t.Fatal("expected failure")
	}
	if len(got) != 2 || got[0]["limit"] != "five_hour" || got[0]["percent"] != float64(35) || got[1]["percent"] != nil || got[1]["resetsAt"] != nil {
		t.Fatalf("%#v", got)
	}
}
func TestCodexRolloutLimitEvents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	id := "12345678-1234-1234-1234-123456789abc"
	dir := filepath.Join(home, "sessions", "2026", "11", "03")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	sample := `{"timestamp":"2026-11-03T10:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":12,"window_minutes":300,"resets_at":1800000000},"secondary":{"used_percent":40,"window_minutes":10080,"resets_at":1800000000}}}}
{"timestamp":"2026-11-03T10:01:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":13,"window_minutes":300,"resets_at":1800000000},"secondary":{"used_percent":41,"window_minutes":10080,"resets_at":1800000000}}}}`
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-03T10-00-00-"+id+".jsonl"), []byte(sample), 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 3, 9, 59, 0, 0, time.UTC)
	rows := rolloutLimits(id, start)
	if len(rows) != 2 || rows[0]["limit"] != "five_hour" || rows[0]["percent"] != float64(13) || rows[1]["limit"] != "seven_day" || rows[1]["percent"] != float64(41) {
		t.Fatalf("%#v", rows)
	}
	if len(rolloutLimits(id, start.Add(3*time.Minute))) != 0 {
		t.Fatal("old observation reused")
	}
}
