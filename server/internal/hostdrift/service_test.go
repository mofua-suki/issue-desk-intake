package hostdrift

import (
	"strings"
	"testing"
	"time"
)

func TestAcquireRescueCountsOnlyFailuresSeenAfterRelease(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := float64(now.UnixMilli())
	desired := strings.Repeat("a", 40)
	s := Service{acquire: map[string]*acquireState{}}
	cycle := map[string]any{"result": "failed", "stage": "acquire", "exceptionType": nil, "at": iso(now), "receivedAt": at, "failures": float64(8), "firstFailureAt": at - 3600000, "lastSuccessAt": nil}
	if released, _ := s.acquireRelease("worker", desired, cycle, true, now); released {
		t.Fatal("old failures granted rescue")
	}
	for failures := 9; failures <= 11; failures++ {
		cycle["failures"] = float64(failures)
		cycle["receivedAt"] = at + float64(failures-8)*1000
		released, _ := s.acquireRelease("worker", desired, cycle, true, now.Add(time.Duration(failures-8)*time.Second))
		if released != (failures == 11) {
			t.Fatalf("failures=%d released=%v", failures, released)
		}
	}
	if released, _ := s.acquireRelease("worker", desired, cycle, false, now.Add(4*time.Second)); released {
		t.Fatal("rescue without drift candidate")
	}
	newDesired := strings.Repeat("b", 40)
	if released, _ := s.acquireRelease("worker", newDesired, cycle, true, now.Add(5*time.Second)); released {
		t.Fatal("new release reused old count")
	}
}
func TestCompatibleRequiresKnownChainAndNoMandatoryRelease(t *testing.T) {
	behind := 3
	hold := Hold{Behind: &behind}
	if !compatible(hold, 3) {
		t.Fatal("three releases should be compatible")
	}
	hold.IncludesAlign = true
	if compatible(hold, 3) {
		t.Fatal("mandatory release counted compatible")
	}
	hold.IncludesAlign = false
	hold.Behind = nil
	if compatible(hold, 3) {
		t.Fatal("unknown chain treated compatible")
	}
}

func TestSettingsDriftGraceResetsAndDisplayUsesActualHold(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s := Service{}
	rows := []map[string]any{{"host": "pc", "checks": map[string]any{"settingsVersion": "old"}}}
	if len(s.settingsAlerts(rows, "new", now)) != 0 || len(s.settingsAlerts(rows, "new", now.Add(30*time.Minute))) != 0 || len(s.settingsAlerts(rows, "new", now.Add(31*time.Minute))) != 1 {
		t.Fatal("猶予判定")
	}
	if len(s.settingsAlerts(rows, "newer", now.Add(time.Hour))) != 0 {
		t.Fatal("別版で時刻を再利用")
	}
	rows[0]["checks"] = map[string]any{"settingsVersion": "newer"}
	s.settingsAlerts(rows, "newer", now.Add(time.Hour))
	if len(s.settingsSeen) != 0 {
		t.Fatal("一致後も保留")
	}
	out := Snapshot{Holds: map[string]Hold{}, Allowed: map[string][]string{"pc": {"o/r"}}, Alerts: []any{map[string]any{"id": "host-drift-revision-pc", "host": "pc", "title": "ずれ", "detail": "版"}}}
	decorateAlerts(&out)
	alert := out.Alerts[0].(map[string]any)
	if !strings.Contains(alert["title"].(string), "配布は続けています") || !strings.Contains(alert["detail"].(string), "o/r") {
		t.Fatal(alert)
	}
}
