package operations

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"math"
)

func validFailureJSON(raw []byte) bool {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil || root["version"] != float64(1) {
		return false
	}
	if value, exists := root["idleTimeouts"]; exists {
		events, ok := value.([]any)
		if !ok {
			return false
		}
		for _, raw := range events {
			e, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			for _, key := range []string{"model", "effort", "diagnosticId"} {
				if _, ok := e[key].(string); !ok {
					return false
				}
			}
			if _, ok := e["at"].(float64); !ok {
				return false
			}
		}
	}
	ranges, ok := root["ranges"].(map[string]any)
	if !ok {
		return false
	}
	number := func(v any) bool {
		n, ok := v.(float64)
		return ok && !math.IsNaN(n) && !math.IsInf(n, 0) && math.Abs(n) <= 8.64e15
	}
	text := func(v any) bool { _, ok := v.(string); return ok }
	count := func(v any) bool { n, ok := v.(float64); return ok && n >= 0 && n == math.Trunc(n) }
	for _, raw := range ranges {
		r := fleet.Object(raw)
		scope := fleet.Object(r["scope"])
		for _, key := range []string{"step", "provider", "model"} {
			if !text(scope[key]) {
				return false
			}
		}
		samples, ok := r["samples"].([]any)
		if !ok {
			return false
		}
		for _, raw := range samples {
			sample := fleet.Object(raw)
			outcome := sample["outcome"]
			if !number(sample["at"]) || !text(sample["cause"]) || (outcome != "success" && outcome != "failure" && outcome != "input_invalid") {
				return false
			}
		}
		for _, key := range []string{"incident", "lastIncident"} {
			raw, exists := r[key]
			if !exists {
				continue
			}
			incident := fleet.Object(raw)
			if !text(incident["id"]) || !number(incident["detectedAt"]) || !count(incident["count"]) || !text(incident["summary"]) || !text(incident["explanation"]) {
				return false
			}
			if key == "lastIncident" && !number(incident["resolvedAt"]) {
				return false
			}
			if value, ok := incident["notifiedAt"]; ok && !number(value) {
				return false
			}
			if value, ok := incident["push"]; ok {
				push := fleet.Object(value)
				if push == nil || (push["state"] != "attempted" && push["state"] != "returned" && push["state"] != "failed") {
					return false
				}
				for _, key := range []string{"sent", "failed", "subscriptions"} {
					if value, ok := push[key]; ok && !count(value) {
						return false
					}
				}
			}
		}
	}
	return true
}
