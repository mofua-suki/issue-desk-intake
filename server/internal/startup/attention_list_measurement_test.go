package startup

import "testing"

// Exercise the real Attention reader and its database-backed stages through the list API.
func TestAttentionListMeasurementsFromReader(t *testing.T) {
	_, _, _, _, handler := setupScopeCase(t, scopeCase{})
	list, _, _ := scopeRequest(t, handler, "/api/list?tabs=active")
	if unavailable, ok := list["unavailable"].([]any); ok {
		for _, name := range unavailable {
			if name == "attentionAlerts" {
				t.Fatalf("Attention reader unavailable: %v", unavailable)
			}
		}
	}
	meta, _, _ := scopeRequest(t, handler, "/api/meta")
	measurements := meta["listMeasurements"].(map[string]any)
	full := measurements["full"].([]any)
	if len(full) != 1 {
		t.Fatalf("full measurements: %v", full)
	}
	stages := full[0].(map[string]any)["stages"].(map[string]any)
	positive := false
	for _, name := range []string{"drafts", "saved", "notices", "stage", "incidents", "jobs", "jobRecovery", "checkout", "rework", "rc", "visible"} {
		value, ok := stages["attentionAlerts_"+name].(float64)
		if !ok || value < 0 {
			t.Errorf("invalid Attention stage %s: %v", name, stages["attentionAlerts_"+name])
		}
		positive = positive || value > 0
	}
	if !positive {
		t.Fatal("no Attention stage recorded a duration")
	}
	t.Logf("isolated database Attention stages (ms): %v", stages)
}
