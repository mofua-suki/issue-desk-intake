package statistics

import (
	"encoding/json"
	"sort"
)

// intakeTiming summarizes saved intake rows without inferring missing dimensions.
func intakeTiming(rows []map[string]any) []any {
	keys := []string{"step", "provider", "model", "effort", "tier", "diagnosticKind"}
	groups := map[string][]map[string]any{}
	for _, row := range rows {
		if row["source"] != "intake" {
			continue
		}
		dimensions := make([]any, len(keys))
		for i, key := range keys {
			if v, ok := row[key].(string); ok && v != "" {
				dimensions[i] = v
			}
		}
		raw, _ := json.Marshal(dimensions)
		groups[string(raw)] = append(groups[string(raw)], row)
	}
	order := make([]string, 0, len(groups))
	for key := range groups {
		order = append(order, key)
	}
	sort.Strings(order)
	out := make([]any, 0, len(order))
	for _, key := range order {
		group := groups[key]
		var dimensions []any
		_ = json.Unmarshal([]byte(key), &dimensions)
		item := map[string]any{"count": len(group)}
		for i, name := range keys {
			item[name] = dimensions[i]
		}
		for _, name := range []string{"elapsedMs", "firstOutputMs"} {
			values := []float64{}
			for _, row := range group {
				if n, ok := wholeValue(row[name]); ok {
					values = append(values, n)
				}
			}
			item[name] = map[string]any{"median": percentile(values, .5), "p90": percentile(values, .9), "missing": len(group) - len(values)}
		}
		out = append(out, item)
	}
	return out
}
