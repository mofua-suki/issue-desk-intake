package consultationai

import (
	"issue-desk-intake/server/internal/fleet"
	"log/slog"
)

// logSplitDependencies はAI回答ごとの依存番号の0出現数を記録する。
func logSplitDependencies(id string, answer Record) {
	plans := fleet.Elements(answer["splitIssues"])
	if len(plans) == 0 {
		return
	}
	references, zeros := 0, 0
	for _, raw := range plans {
		for _, value := range fleet.Elements(fleet.Object(raw)["dependsOn"]) {
			references++
			if n, ok := numericValue(value); ok && n == 0 {
				zeros++
			}
		}
	}
	slog.Info("相談AIの分割案の依存番号", "id", id, "plans", len(plans), "references", references, "zero", zeros)
}
