package models

import (
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"time"
)

var Levels = []string{"light", "standard", "heavy"}
var names = map[string]string{"light": "低", "standard": "中", "heavy": "高", "fixed": "固定値"}

func validLevel(level string) bool {
	return level == "light" || level == "standard" || level == "heavy"
}
func tierRecord(c, d map[string]any, level, source, reason string, at time.Time) (map[string]any, error) {
	setting, e := TierSettings(c, d, level)
	if e != nil {
		return nil, e
	}
	return map[string]any{"level": level, "model": setting.Model, "effort": setting.Effort, "source": source, "reason": reason, "decidedAt": contract.ISO(at), "history": []any{}}, nil
}

// TierRequiredMessage is the refusal for a new consultation without a tier.
// The requester's AI judges the difficulty and always sends it; the intake no
// longer decides a tier by itself.
const TierRequiredMessage = "相談の難易度（tier）がありません。依頼元が依頼の難しさで light・standard・heavy のどれかを必ず付けてください（create-issue.ts は --tier）。古い create-issue.mjs なら issue-desk を更新してください。"

// InitialTier records the tier the requester chose. The requester must always
// choose one: an empty requestedTier is refused.
func InitialTier(c, d map[string]any, at time.Time) (map[string]any, error) {
	user := fleet.Text(d["requestedTier"])
	if user == "" {
		return nil, contract.NewProblem(400, TierRequiredMessage, "TIER_REQUIRED")
	}
	if !validLevel(user) {
		return nil, contract.NewProblem(400, "難易度の指定は light・standard・heavy のどれかです。", "")
	}
	if tiers(c) == nil {
		return tierRecord(c, d, "fixed", "fixed", "難易度の設定（intake.tiers）が無いため固定値で相談します（依頼者の指定: "+names[user]+"）", at)
	}
	return tierRecord(c, d, user, "user", "依頼者の指定（難易度「"+names[user]+"」で相談する）", at)
}

// ChangeTier raises one level and retains the initial decision and all prior history.
func ChangeTier(c, d map[string]any, reason string, at time.Time, extra map[string]any) (map[string]any, error) {
	if tiers(c) == nil {
		return nil, nil
	}
	base := fleet.Object(d["tier"])
	if base == nil {
		return nil, nil
	}
	from := fleet.Text(base["level"])
	to := ""
	if from == "light" {
		to = "standard"
	}
	if from == "standard" {
		to = "heavy"
	}
	if to == "" {
		return nil, nil
	}
	setting, e := TierSettings(c, d, to)
	if e != nil {
		return nil, e
	}
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	out["level"], out["model"], out["effort"] = to, setting.Model, setting.Effort
	history := append([]any{}, fleet.List(base["history"])...)
	event := map[string]any{}
	for k, v := range extra {
		event[k] = v
	}
	event["from"], event["to"], event["reason"], event["at"] = from, to, reason, contract.ISO(at)
	out["history"] = append(history, event)
	return out, nil
}
