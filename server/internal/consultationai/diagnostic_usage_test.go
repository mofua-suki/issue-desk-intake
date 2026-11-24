package consultationai

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/usage"
)

func TestDiagnosticUsageCallStoreRead(t *testing.T) {
	db := budgetAlertPostgres(t)
	kinds := []string{"timeout_total", "completed", "timeout_idle", "execution", "", "unrecognized", "completed", "canceled_by_server", "spawn_failed", "output_overflow", "exit_nonzero", "authentication", "rate_limit", "usage_limit", "format", "session", "input_invalid"}
	calls := 0
	s, d := testService(t, func(_ context.Context, _ ai.Request) (ai.Answer, error) {
		kind := kinds[calls]
		calls++
		a := measuredAnswer(map[string]any{"ok": true})
		a.Diagnostic.Kind = kind
		a.Usage.DiagnosticKind = "execution" // stale data must never leak from the runner
		if calls == 1 {
			return a, measuredTimeout{}
		}
		if kind == "timeout_idle" || kind == "execution" {
			return a, fmt.Errorf("fixture failure")
		}
		return a, nil
	})
	s.options.RecordUsage = func(ctx context.Context, req ai.Request, u ai.Usage, _ time.Duration, success bool) error {
		b, err := json.Marshal(u)
		if err != nil {
			return err
		}
		var row map[string]any
		if err = json.Unmarshal(b, &row); err != nil {
			return err
		}
		row["at"], row["host"], row["source"], row["kind"] = fmt.Sprintf("2026-11-03T00:00:%02d.000Z", calls), "nas", "intake", "intake"
		row["step"], row["provider"], row["model"] = req.Step, req.Provider, req.Model
		row["outcome"] = "failed"
		if success {
			row["outcome"] = "completed"
		}
		counts, err := (&usage.Store{DB: db}).AddRows(ctx, "intake", []map[string]any{row})
		if err != nil {
			return err
		}
		if counts.Added != 1 {
			return fmt.Errorf("save: %+v", counts)
		}
		return nil
	}
	config, _ := s.options.Config(t.Context())
	for calls < len(kinds) {
		_, _ = s.call(t.Context(), d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, filepath.Join(s.options.SchemaDir, "intake.schema.json"), "input", "full", nil, nil, nil, nil)
	}
	from := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	rows, skipped, err := (&usage.Store{DB: db}).Read(t.Context(), "", from, from.Add(time.Hour))
	if err != nil || skipped != 0 || len(rows) != len(kinds) {
		t.Fatalf("Read rows=%d skipped=%d err=%v", len(rows), skipped, err)
	}
	for i, row := range rows {
		got, present := row["diagnosticKind"]
		wantPresent := kinds[i] != "" && kinds[i] != "unrecognized"
		if present != wantPresent || (wantPresent && got != kinds[i]) {
			t.Errorf("row %d: %#v", i, row)
		}
		if row["source"] != "intake" {
			t.Errorf("row %d source: %#v", i, row)
		}
	}
}
