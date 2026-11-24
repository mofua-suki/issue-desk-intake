package operations

import (
	"context"
	"issue-desk-intake/server/internal/ai"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAIFailuresPersistNotificationReservationAndRequireNewSuccess(t *testing.T) {
	dir := t.TempDir()
	var sent atomic.Int32
	notify := func(context.Context, map[string]any) (map[string]any, error) {
		sent.Add(1)
		return map[string]any{"sent": 1, "failed": 0, "subscriptions": 1}, nil
	}
	observer := NewAIFailures(context.Background(), dir, notify)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	observation := ai.Observation{Step: "相談", Provider: "codex", Model: "gpt-6.1-terra", Outcome: "failed", Cause: "authentication", StartedAt: start}
	for i := 1; i <= 3; i++ {
		observation.FinishedAt = start.Add(time.Duration(i) * time.Second)
		observer.Observe(observation)
	}
	rows, e := observer.Alerts(context.Background())
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	observer.Close()
	if sent.Load() != 1 {
		t.Fatal("notification missing", sent.Load())
	}
	restarted := NewAIFailures(context.Background(), dir, notify)
	observation.FinishedAt = start.Add(4 * time.Second)
	restarted.Observe(observation)
	observation.Outcome = "success"
	observation.StartedAt = start
	observation.FinishedAt = start.Add(5 * time.Second)
	restarted.Observe(observation)
	rows, e = restarted.Alerts(context.Background())
	if e != nil || len(rows) != 1 {
		t.Fatal("older in-flight success cleared incident", rows, e)
	}
	observation.StartedAt = start.Add(6 * time.Second)
	observation.FinishedAt = start.Add(7 * time.Second)
	restarted.Observe(observation)
	rows, e = restarted.Alerts(context.Background())
	if e != nil || len(rows) != 0 {
		t.Fatal(rows, e)
	}
	restarted.Close()
	if sent.Load() != 1 {
		t.Fatal("restart re-sent notification", sent.Load())
	}
}
func TestAIFailuresPreserveUnreadableRecordAndExcludeExpectedStops(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "state", "intake-ai-failures.json")
	os.MkdirAll(filepath.Dir(file), 0700)
	raw := []byte(`{"version":1,"ranges":{"bad":{"scope":{},"samples":[]}}}`)
	os.WriteFile(file, raw, 0600)
	var sent atomic.Int32
	observer := NewAIFailures(context.Background(), dir, func(context.Context, map[string]any) (map[string]any, error) { sent.Add(1); return nil, nil })
	at := time.Now()
	o := ai.Observation{Step: "相談", Provider: "codex", Model: "gpt-6.1-terra", StartedAt: at, FinishedAt: at.Add(time.Second), Outcome: "failed", Cause: "schema"}
	observer.Observe(o)
	observer.Close()
	saved, e := os.ReadFile(file)
	if e != nil || string(saved) != string(raw) || sent.Load() != 0 {
		t.Fatal("corrupted record overwritten or reservation repeated")
	}
	if _, e = observer.Alerts(context.Background()); e == nil {
		t.Fatal("unreadable record became success")
	}
	clean := NewAIFailures(context.Background(), t.TempDir(), nil)
	for _, cause := range []string{"usage_limit", "rate_limit", "intentional_stop", "service_shutdown"} {
		o.Cause = cause
		for range 4 {
			clean.Observe(o)
		}
	}
	rows, e := clean.Alerts(context.Background())
	if e != nil || len(rows) != 0 {
		t.Fatal(rows, e)
	}
	o.Cause = "schema"
	clean.Observe(o)
	rows, e = clean.Alerts(context.Background())
	if e != nil || len(rows) != 1 {
		t.Fatal("invalid schema did not alert immediately", rows, e)
	}
	clean.Close()
}

func TestIdleTimeoutWindowAndPersistence(t *testing.T) {
	dir := t.TempDir()
	s := NewAIFailures(context.Background(), dir, nil)
	now := time.Now().Truncate(time.Millisecond)
	o := ai.Observation{Step: "test", Provider: "codex", Model: "model-a", Effort: "high", Outcome: "failed", Cause: "timeout", DiagnosticKind: "timeout_idle", DiagnosticID: "id-a", StartedAt: now, FinishedAt: now}
	for i := 1; i <= 4; i++ {
		s.Observe(o)
		rows, err := s.Alerts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, raw := range rows {
			if raw.(map[string]any)["kind"] == "intake_ai_idle_timeout" {
				found = true
			}
		}
		if found != (i > 3) {
			t.Fatalf("count=%d rows=%v", i, rows)
		}
		if i == 2 {
			success := o
			success.Outcome = "success"
			success.DiagnosticKind = ""
			s.Observe(success)
		}
	}
	s.Close()
	s = NewAIFailures(context.Background(), dir, nil)
	defer s.Close()
	rows, err := s.Alerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["kind"] == "intake_ai_idle_timeout" {
			found = true
			detail := row["detail"].(string)
			for _, v := range []string{"4件", "1h0m0s", "model-a", "high", "id-a"} {
				if !strings.Contains(detail, v) {
					t.Fatal(detail)
				}
			}
		}
	}
	if !found {
		t.Fatal(rows)
	}
	if s.idleTimeoutAlert(now.Add(idleTimeoutWindow)) == nil || s.idleTimeoutAlert(now.Add(idleTimeoutWindow+time.Millisecond)) != nil {
		t.Fatal("window boundary")
	}
}

type idleTestTimeout struct{}

func (idleTestTimeout) Error() string   { return "timeout" }
func (idleTestTimeout) Timeout() bool   { return true }
func (idleTestTimeout) Temporary() bool { return true }

func TestRunnerObservationsCountOnlyPhysicalIdleTimeouts(t *testing.T) {
	dir := t.TempDir()
	schema := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schema, []byte(`{"type":"object"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewAIFailures(context.Background(), t.TempDir(), nil)
	defer s.Close()
	var observed ai.Observation
	cause := "timeout_idle"
	known := true
	runner := ai.Runner{ObserveResult: func(o ai.Observation) { observed = o; s.Observe(o) }, Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
		if r.Arguments[0] == "login" {
			return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
		}
		output := ""
		if known {
			output = `{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":2,"output_tokens":3}}`
		}
		return ai.ProcessResult{Cause: cause, Stdout: []byte(output), Stderr: []byte("SECRET_SAMPLE")}, idleTestTimeout{}
	}}
	req := ai.Request{Provider: "codex", Model: "fixture", Effort: "high", Workspace: dir, SchemaFile: schema, Input: "SECRET_PROMPT"}
	for i := 0; i < 4; i++ {
		known = i%2 == 0
		answer, err := runner.Call(context.Background(), req)
		if err == nil || observed.DiagnosticKind != "timeout_idle" || observed.DiagnosticID == "" || observed.DiagnosticID != answer.Diagnostic.ID || (answer.Usage.InputTokens != nil) != known {
			t.Fatalf("answer=%+v observed=%+v err=%v", answer, observed, err)
		}
	}
	for _, other := range []string{"timeout_total", "canceled_by_server"} {
		cause = other
		a, _ := runner.Call(context.Background(), req)
		if observed.DiagnosticKind != other || a.Diagnostic.Kind != other {
			t.Fatal(other, observed, a.Diagnostic)
		}
	}
	saved, err := os.ReadFile(s.file)
	if err != nil || strings.Contains(string(saved), "SECRET") {
		t.Fatalf("sensitive content persisted: %v", err)
	}
	rows, err := s.Alerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range rows {
		r := raw.(map[string]any)
		if r["kind"] == "intake_ai_idle_timeout" {
			d := r["detail"].(string)
			if !strings.Contains(d, "4件") || strings.Contains(d, "SECRET") {
				t.Fatal(d)
			}
			return
		}
	}
	t.Fatal(rows)
}

func TestIdleTimeoutLegacyAndUnreadableRecords(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "state", "intake-ai-failures.json")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version":1,"ranges":{"legacy":{"scope":{"step":"step","provider":"codex","model":"model"},"samples":[{"at":1000.25,"outcome":"failure","cause":"timeout"}]}}}`
	if err := os.WriteFile(file, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewAIFailures(context.Background(), dir, nil)
	rows, err := s.Alerts(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	now := time.Now()
	o := ai.Observation{StartedAt: now, FinishedAt: now, Outcome: "failed", Cause: "timeout", DiagnosticKind: "timeout_idle"}
	for i := 0; i < 4; i++ {
		s.Observe(o)
	}
	rows, err = s.Alerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["kind"] == "intake_ai_idle_timeout" {
			found = true
			if !strings.Contains(row["detail"].(string), "不明") {
				t.Fatal(row)
			}
		}
	}
	if !found {
		t.Fatal(rows)
	}
	s.Close()
	raw, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(raw), "1000.25") {
		t.Fatal(string(raw), err)
	}
	broken := `{"version":1,"ranges":{},"idleTimeouts":[{"at":"bad"}]}`
	if err = os.WriteFile(file, []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}
	s = NewAIFailures(context.Background(), dir, nil)
	s.Observe(o)
	if _, err = s.Alerts(context.Background()); err == nil {
		t.Fatal("unreadable record accepted")
	}
	s.Close()
	raw, err = os.ReadFile(file)
	if err != nil || string(raw) != broken {
		t.Fatal(string(raw), err)
	}
}

func TestIdleTimeoutReadExpiresAndGroupsModels(t *testing.T) {
	s := NewAIFailures(context.Background(), t.TempDir(), nil)
	defer s.Close()
	now := time.Now()
	o := ai.Observation{StartedAt: now, FinishedAt: now, Outcome: "failed", Cause: "timeout", DiagnosticKind: "timeout_idle", Model: "model-a", Effort: "high", DiagnosticID: "old"}
	for i := 0; i < 4; i++ {
		if i >= 2 {
			o.Model = "model-b"
			o.Effort = "low"
			o.DiagnosticID = "latest"
		}
		s.Observe(o)
	}
	rows, err := s.Alerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["kind"] == "intake_ai_idle_timeout" {
			found = true
			d := row["detail"].(string)
			for _, v := range []string{"model-a / 推論の強さ: high: 2件", "model-b / 推論の強さ: low: 2件", "直近の診断ID: latest"} {
				if !strings.Contains(d, v) {
					t.Fatal(d)
				}
			}
		}
	}
	if !found {
		t.Fatal(rows)
	}
	// A saved event older than the window must disappear on read without another call.
	s.mu.Lock()
	for i := range s.record.IdleTimeouts {
		s.record.IdleTimeouts[i].At = float64(now.Add(-idleTimeoutWindow - time.Second).UnixMilli())
	}
	if err = s.save(); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	rows, err = s.Alerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range rows {
		if raw.(map[string]any)["kind"] == "intake_ai_idle_timeout" {
			t.Fatal(rows)
		}
	}
}
