package consultationai

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/usage"
)

func TestCallTimeoutByTierAttemptsStep(t *testing.T) {
	tests := []struct {
		name, tier, effort, step string
		attempts                 int
		want                     time.Duration
	}{
		{"heavy_draft_attempts0", "heavy", "low", usage.DraftStep, 0, 10 * time.Minute},
		{"high_effort_draft_attempts0", "standard", "high", usage.DraftStep, 0, 10 * time.Minute},
		{"light_draft_attempts0", "light", "low", usage.DraftStep, 0, 5 * time.Minute},
		{"standard_draft_attempts0", "standard", "medium", usage.DraftStep, 0, 5 * time.Minute},
		{"unspecified_draft_attempts0", "", "", usage.DraftStep, 0, 5 * time.Minute},
		{"draft_attempts1", "standard", "medium", usage.DraftStep, 1, 10 * time.Minute},
		{"other_step_heavy_high_attempts0", "heavy", "high", usage.ProxyConfirmationStep, 0, 5 * time.Minute},
		{"other_step_attempts1", "light", "low", usage.ProxyConfirmationStep, 1, 10 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			s := &Service{options: Options{RuntimeDir: t.TempDir(), Now: time.Now}}
			s.options.Runner = &ai.Runner{Execute: func(_ context.Context, request ai.ProcessRequest) (ai.ProcessResult, error) {
				if request.Arguments[0] == "login" {
					return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				calls++
				t.Logf("ProcessRequest.Timeout = %s", request.Timeout)
				if request.Timeout != tc.want || request.IdleTimeout != 240*time.Second {
					t.Errorf("timeout=%s idle=%s; want %s and 4m0s", request.Timeout, request.IdleTimeout, tc.want)
				}
				return ai.ProcessResult{}, errors.New("execution stopped by test")
			}}
			d := Record{"_ephemeral": true, "attempts": tc.attempts}
			settings := models.Settings{Provider: "codex", Model: "fixture", Tier: tc.tier, Effort: tc.effort}
			schema := filepath.Join("..", "..", "schemas", "priority.schema.json")
			_, err := s.call(t.Context(), d, nil, settings, tc.step, schema, "input", "input", nil, nil, nil, nil)
			if err == nil || calls != 1 {
				t.Fatalf("expected one stopped execution; calls=%d err=%v", calls, err)
			}
		})
	}
}
