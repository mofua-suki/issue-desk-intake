package models

import (
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

func ProviderSettings(c map[string]any, provider string) (Settings, error) {
	if provider == "" {
		provider = "codex"
	}
	if provider != "codex" && provider != "claude" && provider != "antigravity" {
		return Settings{}, contract.NewProblem(400, "実行先が不正です。", "")
	}
	claude := text(c["claudeModel"], "opus")
	model := claude
	if provider == "codex" {
		model = equivalent(c, claude, fleet.Text(c["model"]))
	}
	if provider == "antigravity" {
		model = text(fleet.Object(c["antigravity"])["model"], "gemini-3.8-flash-high")
	}
	return Settings{Provider: provider, Model: model, Effort: text(c["reasoningEffort"], "medium")}, nil
}
func ImplementationSettings(c map[string]any, provider, level string) (Settings, error) {
	base, e := ProviderSettings(c, provider)
	if e != nil {
		return Settings{}, e
	}
	if provider == "antigravity" || !validLevel(level) {
		return base, nil
	}
	entry := fleet.Object(fleet.Object(fleet.Object(c["implementation"])["tiers"])[level])
	modelKey := "model"
	if base.Provider == "claude" {
		modelKey = "claudeModel"
	}
	model := fleet.Text(entry[modelKey])
	if strings.TrimSpace(model) == "" {
		return base, nil
	}
	base.Model = model
	base.Effort = text(entry["reasoningEffort"], base.Effort)
	return base, nil
}
