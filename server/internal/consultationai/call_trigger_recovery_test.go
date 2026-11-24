package consultationai

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"testing"

	"issue-desk-intake/server/internal/ai"
)

// Exercise the production Runner, persisted draft, and the next turn together.
func TestRecoveryTriggerFromRunnerDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name, cause, want string
		code              int
	}{
		{"failure", "exit_nonzero", "failure_retry", 1},
		{"interrupted", "canceled_by_server", "recovery_continuation", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return ai.Answer{}, nil })
			s.options.Runner = &ai.Runner{Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
				if r.Arguments[0] == "login" {
					return ai.ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				result := ai.ProcessResult{Code: tc.code, Cause: tc.cause, Stdout: []byte("{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"cached_input_tokens\":2,\"output_tokens\":3}}\n")}
				if tc.cause == "canceled_by_server" {
					return result, errors.New("interrupted")
				}
				return result, nil
			}}
			if _, err := s.Turn(t.Context(), d); err == nil {
				t.Fatal("expected failed call")
			}
			saved, ok, err := s.options.Records.Get(t.Context(), "drafts", draftID)
			if err != nil || !ok {
				t.Fatalf("saved: %v %v", ok, err)
			}
			history := fleet.Elements(saved["aiDiagnostics"])
			if len(history) != 1 || fleet.Text(fleet.Object(history[0])["kind"]) != tc.cause || fleet.Number(fleet.Object(history[0])["calls"]) != 1 || fleet.Number(fleet.Object(saved["consultationBudget"])["calls"]) != 1 {
				t.Fatalf("saved diagnostic=%v budget=%v", history, saved["consultationBudget"])
			}
			var got string
			s.options.Runner = callFunc(func(_ context.Context, r ai.Request) (ai.Answer, error) {
				got = r.CallTrigger
				return measuredAnswer(reply()), nil
			})
			if _, err = s.Turn(t.Context(), saved); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("trigger = %q, want %q", got, tc.want)
			}
		})
	}
}
