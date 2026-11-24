package fleet

import (
	"errors"
	"strings"
)

var sharedPaths = []string{"model", "claudeModel", "reasoningEffort", "codexEquivalents", "implementation.tiers", "implementation.disableLightTier", "intake.claudeModel", "intake.reasoningEffort", "intake.tiers.light", "intake.tiers.standard", "intake.tiers.heavy", "intake.tiers.filingCheck", "review.claudeModel", "review.tiers.light", "review.tiers.standard", "review.tiers.heavy", "priority.claudeModel", "antigravity.model"}

func applyModels(config map[string]any, value any) error {
	priority := Object(config["priority"])
	if priority == nil {
		if config["priority"] != nil {
			return errors.New("priorityはオブジェクトです")
		}
		priority = map[string]any{}
		config["priority"] = priority
	}
	priority["provider"], priority["claudeModel"] = "consultation", "haiku"
	review := Object(config["review"])
	if review == nil {
		review = map[string]any{}
		config["review"] = review
	}
	review["provider"] = "opposite"
	if value == nil {
		return nil
	}
	models, err := normalizeModels(value)
	if err != nil {
		return err
	}
	if !known(models, "model", "claudeModel", "reasoningEffort", "codexEquivalents", "implementation", "intake", "review", "priority", "antigravity") {
		return errors.New("モデル設定に未対応の項目があります")
	}
	for _, rawPath := range sharedPaths {
		if strings.HasPrefix(rawPath, "review.tiers.") && Object(Object(models["review"])["tiers"]) == nil {
			continue
		}
		parts := strings.Split(rawPath, ".")
		source := models
		var found any
		for i, key := range parts {
			if i == len(parts)-1 {
				found = source[key]
				break
			}
			source = Object(source[key])
			if source == nil {
				break
			}
		}
		target := config
		skip := false
		for _, key := range parts[:len(parts)-1] {
			child := Object(target[key])
			if child == nil {
				if key == "antigravity" || target[key] != nil {
					skip = true
					break
				}
				if found == nil {
					skip = true
					break
				}
				child = map[string]any{}
				target[key] = child
			}
			target = child
		}
		if skip {
			continue
		}
		key := parts[len(parts)-1]
		if found == nil {
			delete(target, key)
		} else {
			target[key] = Clone(found)
		}
	}
	return nil
}

// ApplyModels applies the shared model settings and fixed provider policy.
func ApplyModels(config map[string]any, value any) error { return applyModels(config, value) }
