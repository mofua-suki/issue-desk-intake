package statistics

import "issue-desk-intake/server/internal/usage"

// Normalize copies for aggregation; persisted usage rows keep their original names.
func canonicalUsageRows(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		out[i] = row
		step, ok := row["step"].(string)
		if !ok || usage.CanonicalStep(step) == step {
			continue
		}
		copy := make(map[string]any, len(row))
		for k, v := range row {
			copy[k] = v
		}
		copy["step"] = usage.CanonicalStep(step)
		out[i] = copy
	}
	return out
}
