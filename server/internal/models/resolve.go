package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"time"
)

// FilingCheckConfigKey is the model configuration key for the filing usage step.
const FilingCheckConfigKey = usage.FilingCheckStep

// ReviewNotesStep is the model step of the review-note summary. It uses the
// fixed small model (Claude haiku, or its Codex equivalent) at low effort, the
// same model it used before the intake's tier classifier was removed.
const ReviewNotesStep = "reviewNotes"

type Settings struct {
	Tier            string         `json:"tier,omitempty"`
	Provider        string         `json:"provider"`
	Model           string         `json:"model"`
	Effort          string         `json:"effort"`
	ProviderDisable map[string]any `json:"providerDisable,omitempty"`
}
type Service struct {
	DB  *sql.DB
	Now func() time.Time
}

func text(v any, fallback string) string {
	if s := fleet.Text(v); s != "" {
		return s
	}
	return fallback
}
func equivalent(c map[string]any, model, fallback string) string {
	return text(fleet.Object(c["codexEquivalents"])[model], fallback)
}
func provider(d map[string]any) (string, error) {
	value, exists := d["consultationProvider"]
	if !exists || value == nil {
		return "codex", nil
	}
	p, ok := value.(string)
	if !ok || p != "codex" && p != "claude" {
		return "", contract.NewProblem(400, "consultationProvider は codex または claude です。", "")
	}
	return p, nil
}

func tiers(c map[string]any) map[string]any { return fleet.Object(fleet.Object(c["intake"])["tiers"]) }
func tierSettings(c map[string]any, p, level string) Settings {
	intake := fleet.Object(c["intake"])
	claude := text(intake["claudeModel"], "fable")
	out := Settings{Provider: p, Model: claude, Effort: text(intake["reasoningEffort"], "high")}
	if p == "codex" {
		out.Model = equivalent(c, claude, fleet.Text(c["model"]))
	}
	entry := fleet.Object(tiers(c)[level])
	model := ""
	if p == "claude" {
		model = fleet.Text(entry["claudeModel"])
	} else {
		model = text(entry["model"], equivalent(c, fleet.Text(entry["claudeModel"]), ""))
	}
	if model != "" {
		out.Model = model
		out.Effort = text(entry["reasoningEffort"], out.Effort)
	}
	return out
}
func TierSettings(c, d map[string]any, level string) (Settings, error) {
	p, e := provider(d)
	if e != nil {
		return Settings{}, e
	}
	return tierSettings(c, p, level), nil
}
func (s *Service) active(ctx context.Context, p string) (map[string]any, error) {
	if s.DB == nil {
		return nil, contract.NewProblem(503, "実行先の設定を読み取れません。", "")
	}
	var raw []byte
	var expires sql.NullTime
	e := s.DB.QueryRowContext(ctx, "SELECT value,expires_at FROM settings WHERE key=$1", "providerDisable."+p).Scan(&raw, &expires)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var entry map[string]any
	if json.Unmarshal(raw, &entry) != nil {
		return nil, nil
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	until, e := parseUntil(fleet.Text(entry["until"]))
	if e != nil || !until.After(now) || expires.Valid && !expires.Time.After(now) {
		return nil, nil
	}
	set, e := parseUntil(fleet.Text(entry["setAt"]))
	if e != nil || !until.After(set) || until.Sub(set) > 7*24*time.Hour || !safe(entry["by"], 100) {
		return nil, nil
	}
	if reason, ok := entry["reason"]; ok && !safe(reason, 200) {
		return nil, nil
	}
	entry["source"] = "settings"
	return entry, nil
}
func (s *Service) Resolve(ctx context.Context, c, d map[string]any, step string) (Settings, error) {
	p, e := provider(d)
	if e != nil {
		return Settings{}, e
	}
	level := text(fleet.Object(d["tier"])["level"], "heavy")
	if tiers(c) == nil {
		level = "fixed"
	}
	if step == "project" && tiers(c) != nil {
		level = "heavy"
	}
	base, claude, e := stepSettings(c, d, p, level, step)
	if e != nil {
		return Settings{}, e
	}
	return s.avoidDisabledProvider(ctx, c, base, claude, level, step)
}

// stepSettings chooses the settings of the step from the configuration, and
// the Claude model to use when the provider is switched.
func stepSettings(c, d map[string]any, p, level, step string) (Settings, string, error) {
	var e error
	base := tierSettings(c, p, level)
	if level == "light" || level == "standard" || level == "heavy" {
		base.Tier = level
	}
	claude := text(fleet.Object(tiers(c)[level])["claudeModel"], text(fleet.Object(c["intake"])["claudeModel"], "fable"))
	switch step {
	case "intake", "close", "project", "proxy":
	case FilingCheckConfigKey:
		entry := fleet.Object(tiers(c)[step])
		candidate := tierSettings(c, p, step)
		if entry != nil && (p == "claude" && fleet.Text(entry["claudeModel"]) != "" || p == "codex" && text(entry["model"], equivalent(c, fleet.Text(entry["claudeModel"]), "")) != "") {
			base = candidate
			claude = text(entry["claudeModel"], claude)
		}
	case ReviewNotesStep:
		claude = "haiku"
		base = Settings{Provider: p, Model: claude, Effort: "low"}
		if p == "codex" {
			base.Model = equivalent(c, claude, "gpt-6-luna")
		}
	case "priority":
		base = Settings{Provider: p, Model: "gpt-6-luna", Effort: "medium"}
		claude = "haiku"
		if p == "claude" {
			base.Model = claude
		}
	case "implementation", "repair":
		base, e = ImplementationSettings(c, "codex", fleet.Text(fleet.Object(d["implementationTier"])["level"]))
		if e != nil {
			return Settings{}, "", e
		}
		claude = text(c["claudeModel"], "opus")
	case "review":
		base, claude, e = reviewSettings(c, d)
		if e != nil {
			return Settings{}, "", e
		}
	default:
		return Settings{}, "", contract.NewProblem(400, "工程が不正です。", "")
	}
	return base, claude, nil
}

// reviewSettings chooses the reviewer opposite to the saved implementation provider.
func reviewSettings(c, d map[string]any) (Settings, string, error) {
	saved := fleet.Object(d["executionSettings"])
	if d["executionSettings"] != nil && saved == nil {
		return Settings{}, "", contract.NewProblem(409, "保存された実装の実行設定が不正です。", "")
	}
	if saved["provider"] != nil && saved["provider"] != "codex" && saved["provider"] != "claude" && saved["provider"] != "antigravity" {
		return Settings{}, "", contract.NewProblem(409, "保存された実装の実行先が不正です。", "")
	}
	if d["provider"] != nil && d["provider"] != "codex" && d["provider"] != "claude" && d["provider"] != "antigravity" {
		return Settings{}, "", contract.NewProblem(409, "保存された実装の実行先が不正です。", "")
	}
	implementation := fleet.Text(saved["provider"])
	legacy := fleet.Text(d["provider"])
	if implementation != "" && legacy != "" && implementation != legacy {
		return Settings{}, "", contract.NewProblem(409, "保存された実装の実行先が食い違っています。", "")
	}
	if implementation == "" {
		implementation = text(d["provider"], "codex")
	}
	if implementation != "codex" && implementation != "claude" && implementation != "antigravity" {
		return Settings{}, "", contract.NewProblem(409, "保存された実装の実行先が不正です。", "")
	}
	p := "claude"
	if implementation == "claude" {
		p = "codex"
	}
	if implementation == "antigravity" {
		p = "codex"
		if fleet.Object(c["review"])["antigravityReviewer"] == "claude" {
			p = "claude"
		}
	}
	claude := text(fleet.Object(c["review"])["claudeModel"], "opus")
	base := Settings{Provider: p, Model: claude, Effort: text(c["reasoningEffort"], "medium")}
	if p == "codex" {
		base.Model = equivalent(c, claude, fleet.Text(c["model"]))
	}
	return base, claude, nil
}

// avoidDisabledProvider switches to the other provider while the chosen one is disabled.
func (s *Service) avoidDisabledProvider(ctx context.Context, c map[string]any, base Settings, claude, level, step string) (Settings, error) {
	disable, e := s.active(ctx, base.Provider)
	if e != nil {
		return Settings{}, e
	}
	if disable == nil {
		return base, nil
	}
	from := base.Provider
	other := "codex"
	if from == "codex" {
		other = "claude"
	}
	blocked, e := s.active(ctx, other)
	if e != nil {
		return Settings{}, e
	}
	if blocked != nil {
		return Settings{}, contract.NewProblem(503, "Codex・Claude がともに無効のため、AIを実行しません。", "")
	}
	base.Provider = other
	base.Model = claude
	if other == "codex" {
		base.Model = equivalent(c, claude, fleet.Text(c["model"]))
		if step == "intake" || step == "proxy" || step == "close" || step == FilingCheckConfigKey {
			base.Model = tierSettings(c, "codex", level).Model
			if step == FilingCheckConfigKey {
				base.Model = tierSettings(c, "codex", step).Model
			}
		}
		if step == ReviewNotesStep {
			base.Model = equivalent(c, claude, "gpt-6-luna")
		}
	}
	if step == "priority" {
		if other == "codex" {
			base.Model = "gpt-6-luna"
		} else {
			base.Model = "haiku"
		}
	}
	disable["from"] = from
	base.ProviderDisable = disable
	return base, nil
}
