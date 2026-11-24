package consultationai

import (
	"issue-desk-intake/server/internal/fleet"
	"log/slog"
)

// Only the worker polling interval is known to be irrelevant to the resumed
// model conversation. Unknown settings, permissions, repositories and models
// remain in the key. Never mutate the configuration shared by other callers.
func sessionConfig(config map[string]any) map[string]any {
	result := fleet.Object(fleet.Clone(config))
	if assignments := fleet.Object(result["assignments"]); assignments != nil {
		delete(assignments, "pollSeconds")
	}
	return result
}

// One structured record per actual call attempt, including retries. Bytes are
// transport sizes, not token counts, billed money, or subscription consumption.
// No prompt, response, credential or session identifier is written.
func (s *Service) observeSessionInput(d Record, state *sessionState, resumed bool, input, full string) {
	if state == nil {
		return
	}
	mode, reason := "full", state.ResetReason
	if resumed {
		mode = "resumed"
	}
	reinputBytes := 0
	if reason != "" && mode == "full" {
		reinputBytes = len(input)
	}
	slog.Info("相談AIの会話入力", "event", "consultation_session_input", "id", fleet.Text(d["id"]),
		"mode", mode, "reset_reason", reason, "input_bytes", len(input), "full_input_bytes", len(full), "reinput_bytes", reinputBytes)
	state.ResetReason = ""
}
