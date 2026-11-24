package hosts

import (
	"testing"
)

func TestPublicHealthRejectsUntrustedStoredDiagnostics(t *testing.T) {
	cycle := map[string]any{"result": "failed", "stage": "swap-start", "exceptionType": nil, "at": "2026-11-01T00:00:00Z", "receivedAt": 1000.0, "failures": 1.0, "firstFailureAt": 1000.0, "lastSuccessAt": nil, "detail": "docker_exit"}
	health := map[string]any{"cycle": cycle}
	if publicHealth(health) == nil {
		t.Fatal("valid failure lost")
	}
	cycle["detail"] = "https://secret.example"
	if publicHealth(health) != nil {
		t.Fatal("stored unsafe diagnostic exposed")
	}
	cycle["detail"] = "docker_exit"
	cycle["swapDiagnostic"] = map[string]any{"logTail": []any{}, "ready": nil, "private": "secret"}
	if publicHealth(health) != nil {
		t.Fatal("unknown nested property exposed")
	}
}
