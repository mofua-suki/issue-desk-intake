package fleet

import (
	"errors"
	"strings"
)

func modelSchema() map[string]any {
	tier := map[string]any{"model": "model", "claudeModel": "model", "reasoningEffort": "effort"}
	return map[string]any{"model": "model", "claudeModel": "model", "reasoningEffort": "effort", "codexEquivalents": "table", "implementation": map[string]any{"disableLightTier": "bool", "tiers": map[string]any{"light": tier, "standard": tier, "heavy": tier}}, "intake": map[string]any{"claudeModel": "model", "reasoningEffort": "effort", "tiers": map[string]any{"light": tier, "standard": tier, "heavy": tier, "filingCheck": tier}}, "review": map[string]any{"claudeModel": "model", "tiers": map[string]any{"light": tier, "standard": tier, "heavy": tier}}, "priority": map[string]any{"claudeModel": "model"}, "antigravity": map[string]any{"model": "antigravity"}}
}
func normalizeModelValue(value, spec any) (any, error) {
	bad := func() (any, error) {
		return nil, errors.New("共通モデル設定の項目・型・値が不正です")
	}
	if schema := Object(spec); schema != nil {
		input := Object(value)
		if input == nil {
			return bad()
		}
		out := map[string]any{}
		for key, v := range input {
			child, exists := schema[key]
			if !exists {
				return bad()
			}
			normalized, e := normalizeModelValue(v, child)
			if e != nil {
				return nil, e
			}
			out[key] = normalized
		}
		return out, nil
	}
	switch spec {
	case "bool":
		if _, ok := value.(bool); !ok {
			return bad()
		}
		return value, nil
	case "model", "antigravity":
		name, ok := value.(string)
		if !ok || strings.TrimSpace(name) == "" {
			return bad()
		}
		name = strings.TrimSpace(name)
		lower := strings.ToLower(name)
		if spec == "antigravity" && (strings.HasPrefix(lower, "claude") || strings.HasPrefix(lower, "gpt")) {
			return bad()
		}
		return name, nil
	case "effort":
		if !oneOfModelEfforts(value) {
			return bad()
		}
		return value, nil
	case "table":
		table := Object(value)
		if table == nil {
			return bad()
		}
		out := map[string]any{}
		for key, v := range table {
			if strings.TrimSpace(key) == "" {
				return bad()
			}
			normalized, e := normalizeModelValue(v, "model")
			if e != nil {
				return nil, e
			}
			out[key] = normalized
		}
		return out, nil
	}
	return bad()
}
func oneOfModelEfforts(value any) bool {
	for _, effort := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
		if value == effort {
			return true
		}
	}
	return false
}
func normalizeModels(value any) (map[string]any, error) {
	normalized, e := normalizeModelValue(value, modelSchema())
	if e != nil {
		return nil, e
	}
	return Object(normalized), nil
}
