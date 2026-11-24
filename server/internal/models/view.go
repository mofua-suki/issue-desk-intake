package models

import "issue-desk-intake/server/internal/fleet"

func OverridesView(c map[string]any) map[string]any {
	d := map[string]any{}
	fixed, _ := TierSettings(c, d, "fixed")
	intake := map[string]any{"provider": fixed.Provider, "model": fixed.Model, "effort": fixed.Effort, "tiers": nil}
	if tiers(c) != nil {
		levels := map[string]any{}
		for _, l := range Levels {
			levels[l], _ = TierSettings(c, d, l)
		}
		intake["tiers"] = levels
		entry := fleet.Object(tiers(c)[FilingCheckConfigKey])
		if text(entry["model"], equivalent(c, fleet.Text(entry["claudeModel"]), "")) != "" {
			intake[FilingCheckConfigKey], _ = TierSettings(c, d, FilingCheckConfigKey)
		}
	} else {
		intake[FilingCheckConfigKey] = nil
	}
	claude := text(fleet.Object(c["review"])["claudeModel"], "opus")
	effort := text(c["reasoningEffort"], "medium")
	return map[string]any{"locked": true, "policy": map[string]any{"intake": "consultation", "filing": "consultation", "close": "consultation", "implementation": "codex", "repair": "implementation", "review": "opposite", "priority": "consultation"}, "overrides": []any{}, "defaults": map[string]any{"intake": intake, "review": map[string]any{"mode": "opposite", "effort": effort, "opposite": map[string]any{"claude": Settings{Provider: "codex", Model: equivalent(c, claude, fleet.Text(c["model"])), Effort: effort}, "codex": Settings{Provider: "claude", Model: claude, Effort: effort}}}, "priority": Settings{Provider: "codex", Model: "gpt-6-luna", Effort: "medium"}}, "steps": []any{map[string]any{"id": "intake", "name": "受付の相談"}, map[string]any{"id": "review", "name": "独立レビュー"}, map[string]any{"id": "priority", "name": "優先度の判定"}}, "providers": []string{"codex", "claude"}, "efforts": []string{"minimal", "low", "medium", "high", "xhigh", "max"}, "maxDays": 7, "note": "受付が公開し、各PCの監視が定期確認で写して使う（受付を持たないPCの監視へ届くまで最大1周期）", "problem": nil}
}
