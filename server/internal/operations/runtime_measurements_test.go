package operations

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostdrift"
	"strings"
	"testing"
	"time"
)

func TestRuntimeMeasurementsMissingAndRealObservations(t *testing.T) {
	m := NewRuntimeMeasurements(nil, nil)
	v := m.View()
	if v["timerDelayMsMean"] != nil || v["reason"] == nil {
		t.Fatal(v)
	}
	at := time.Now()
	m.observeDelay(at, 4)
	m.observeDelay(at.Add(time.Second), 8)
	m.observeDB(at, at.Add(10*time.Millisecond), at.Add(7*time.Millisecond), 10, nil)
	m.observeDB(at, at, at, 0, errors.New("db"))
	v = m.View()
	if v["timerDelayMsMean"] != float64(6) || v["dbSamples"] != 1 || v["dbFailures"] != 1 || v["source"] != "go-runtime" {
		t.Fatal(v)
	}
	if *v["dbClockOffsetMsLower"].(*float64) != -3 || *v["dbClockOffsetMsUpper"].(*float64) != 7 {
		t.Fatal(v)
	}
}
func TestHostImageFailuresDoNotDisappearBehindRouteSuccess(t *testing.T) {
	s := fixture(t)
	row := map[string]any{"name": "pc", "alive": true, "revision": strings.Repeat("a", 40), "image": map[string]any{"desired": map[string]any{"tag": "b", "revision": strings.Repeat("b", 40)}, "failedTags": []any{"b"}, "results": []any{map[string]any{"tag": "b", "status": "failed", "at": iso(s.now()), "succeededOn": []any{"other"}}, map[string]any{"tag": "a", "status": "failed", "at": iso(s.now().Add(-time.Hour)), "succeededOn": []any{"other"}}}, "lastFailure": map[string]any{"stage": "swap-start", "reason": "docker_exit", "detail": "exit 1", "selfCheckFailed": []any{"node"}}}}
	out := s.hostImageAlerts(map[string]any{"hosts": []any{row}})
	if len(out) != 2 || !strings.Contains(fleet.Text(fleet.Object(out[0])["detail"]), "失敗した自己検査: node") {
		t.Fatal(out)
	}
	row["alive"] = false
	if len(s.hostImageAlerts(map[string]any{"hosts": []any{row}})) != 0 {
		t.Fatal("停止PCに警告")
	}
}

func TestPostgresRuntimeMeasuresActualClockRead(t *testing.T) {
	db := postgres(t)
	m := NewRuntimeMeasurements(db, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if e := m.Run(ctx, 100*time.Millisecond); e == nil {
		t.Fatal("停止理由なし")
	}
	v := m.View()
	if v["samples"].(int) < 1 || v["dbSamples"].(int) < 1 || v["dbRoundTripMsMean"] == nil || v["dbClockOffsetMsLower"] == nil {
		t.Fatal(v)
	}
}

func TestGoDelayWindowsRequireObservationAndClearAfterHealthyWindows(t *testing.T) {
	m := NewRuntimeMeasurements(nil, nil)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for second := 0; second <= 180; second++ {
		m.observeDelay(start.Add(time.Duration(second)*time.Second), 3000)
	}
	alerts, e := m.Alerts(context.Background())
	if e != nil || len(alerts) != 1 {
		t.Fatal(alerts, e)
	}
	for second := 181; second <= 840; second++ {
		m.observeDelay(start.Add(time.Duration(second)*time.Second), 1)
	}
	alerts, e = m.Alerts(context.Background())
	if e != nil || len(alerts) != 0 {
		t.Fatal(alerts, e)
	}
	m = NewRuntimeMeasurements(nil, nil)
	for minute := 0; minute < 5; minute++ {
		m.observeDelay(start.Add(time.Duration(minute)*time.Minute), 3000)
	}
	alerts, _ = m.Alerts(context.Background())
	if len(alerts) != 0 {
		t.Fatal("未観測窓を成功扱い", alerts)
	}
}

func TestImageVersionWarningNeedsPublishedDrift(t *testing.T) {
	view := map[string]any{"hosts": []any{map[string]any{"name": "a", "alive": true, "imageRevision": "old"}, map[string]any{"name": "b", "alive": true, "imageRevision": "new"}}}
	if len(imageVersionAlerts(view, nil)) != 0 {
		t.Fatal("発行の根拠なし")
	}
	snapshot := hostdrift.Snapshot{Drifts: map[string]hostdrift.Hold{"a": {Since: time.Now()}}}
	out := imageVersionAlerts(view, &snapshot)
	if len(out) != 1 || fleet.Object(out[0])["id"] != "host-drift-image" {
		t.Fatal(out)
	}
}
