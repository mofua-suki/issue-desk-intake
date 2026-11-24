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

func TestFirstOutputSavedAcrossRetryAndMissing(t *testing.T) {
	db := budgetAlertPostgres(t)
	calls := 0
	values := []int64{123, 0}
	s, d := testService(t, func(_ context.Context, _ ai.Request) (ai.Answer, error) {
		answer := measuredAnswer(map[string]any{"ok": true})
		if calls < len(values) {
			answer.Diagnostic.FirstOutputMs = &values[calls]
		}
		calls++
		if calls == 1 {
			return answer, measuredTimeout{}
		}
		return answer, nil
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
			return fmt.Errorf("保存失敗: %+v", counts)
		}
		return nil
	}
	config, _ := s.options.Config(t.Context())
	invoke := func() error {
		_, err := s.call(t.Context(), d, config, models.Settings{Provider: "codex", Model: "fixture"}, usage.DraftStep, filepath.Join(s.options.SchemaDir, "intake.schema.json"), "input", "full", nil, nil, nil, nil)
		return err
	}
	if err := invoke(); err != nil {
		t.Fatal(err)
	}
	if err := invoke(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(t.Context(), `SELECT json FROM usage_rows ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []struct {
		value   any
		exists  bool
		outcome string
	}{{float64(123), true, "failed"}, {float64(0), true, "completed"}, {nil, false, "completed"}}
	i := 0
	for rows.Next() {
		if i >= len(want) {
			t.Fatal("余分な行")
		}
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			t.Fatal(err)
		}
		got, exists := row["firstOutputMs"]
		if got != want[i].value || exists != want[i].exists || row["outcome"] != want[i].outcome {
			t.Fatalf("row %d: %s", i, raw)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if i != len(want) || calls != len(want) {
		t.Fatalf("rows=%d calls=%d", i, calls)
	}
}
