package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Codex strict output does not support uniqueItems. Only the transport copy
// drops this keyword; Call still validates answers against the original schema.
func writeCodexSchema(work string, data []byte) (string, error) {
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return "", failed("schema", "AI回答のスキーマが不正です", false)
	}
	codexSchema(document)
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	file := filepath.Join(work, "codex-schema.json")
	if err := os.WriteFile(file, encoded, 0600); err != nil {
		return "", err
	}
	return file, nil
}

func codexSchema(schema map[string]any) {
	delete(schema, "uniqueItems")
	// These values are maps of schemas, not schemas themselves. In particular,
	// a property named uniqueItems must retain its definition and required entry.
	for _, keyword := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		if entries, ok := schema[keyword].(map[string]any); ok {
			for _, entry := range entries {
				if child, ok := entry.(map[string]any); ok {
					codexSchema(child)
				}
			}
		}
	}
	for _, keyword := range []string{"items", "additionalProperties", "contains", "not", "if", "then", "else", "propertyNames", "unevaluatedProperties", "unevaluatedItems"} {
		if child, ok := schema[keyword].(map[string]any); ok {
			codexSchema(child)
		}
	}
	for _, keyword := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if entries, ok := schema[keyword].([]any); ok {
			for _, entry := range entries {
				if child, ok := entry.(map[string]any); ok {
					codexSchema(child)
				}
			}
		}
	}
}
