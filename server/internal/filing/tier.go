package filing

import (
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/tierpolicy"
)

func validTier(v string) bool { return tierpolicy.Valid(v) }
func acceptanceCount(body string) (int, bool) {
	inside := false
	count := 0
	for _, row := range markdownLines(strings.ReplaceAll(body, "\r\n", "\n")) {
		line := row.text
		if row.fenced {
			continue
		}
		if !inside {
			if regexp.MustCompile(`^##\s*受入条件`).MatchString(line) {
				inside = true
			}
			continue
		}
		if heading.MatchString(line) {
			break
		}
		if regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+\S`).MatchString(line) {
			count++
		}
	}
	return count, inside
}

// 起票時は Go が判定する。
func decideTier(c BuildConfig, d Record) Record {
	count, has := acceptanceCount(fleet.Text(d["body"]))
	return decideTierWith(c, d, count, has)
}

// DecideFollowupTier は PR の軽微な指摘への対応（後続案）の実装の難易度を、起票と同じ規則で決める。
// 後続案は依頼者の指定・止まりの修正・複数プロジェクト・分けられない理由を持たないので、変更範囲（paths の配列。
// 無ければ nil）と受入条件（指摘ごとのテスト方法）の件数だけで決まる。難易度の設定が無ければ nil（起票と同じく目印を付けない）。
func DecideFollowupTier(c BuildConfig, paths []string, acceptance int) Record {
	d := Record{}
	if len(paths) > 0 {
		declared := make([]any, len(paths))
		for i, p := range paths {
			declared[i] = p
		}
		d["changeScope"] = Record{"known": true, "paths": declared}
	}
	return decideTierWith(c, d, acceptance, acceptance > 0)
}

// TierDescription は起票本文の実行先の行に付ける難易度の文（判定の元と理由）。
func TierDescription(t Record) string { return tierDescription(t) }

func decideTierWith(c BuildConfig, d Record, count int, has bool) Record {
	if row(row(c.Config["implementation"])["tiers"]) == nil {
		return nil
	}
	var known *bool
	if scope := scopeDeclaration(d["changeScope"]); scope != nil {
		value := scope["known"] == true
		known = &value
	}
	reason := strings.TrimSpace(fleet.Text(d["heavyReason"]))
	reason = contract.ClipUTF16(strings.ReplaceAll(whitespace.ReplaceAllString(reason, " "), "<!--", "&lt;!--"), 200)
	decision := tierpolicy.Decide(tierpolicy.Input{
		Requested: fleet.Text(d["requestedImplementationTier"]), StallFix: truth(d["stallFix"]),
		ScopeKnown: known, CrossProject: d["crossProject"] == true, HeavyReason: reason,
		HasAcceptance: has, AcceptanceCount: count,
		DisableLight: row(c.Config["implementation"])["disableLightTier"] == true,
	})
	return Record{"level": decision.Level, "source": decision.Source, "reason": decision.Reason, "decidedAt": iso(c.now())}
}
func tierDescription(t Record) string {
	if !validTier(fleet.Text(t["level"])) {
		return ""
	}
	name := map[string]string{"light": "低", "standard": "中", "heavy": "高"}[fleet.Text(t["level"])]
	if t["source"] == "user" {
		return " / 難易度: " + name + "（依頼者の指定）"
	}
	return " / 難易度: " + name + "（受付の判定: " + inert(fleet.Text(t["reason"])) + "）"
}
func sizeExemption(d Record) string {
	reason := strings.TrimSpace(fleet.Text(d["heavyReason"]))
	if truth(d["stallFix"]) || strings.HasPrefix(reason, "止血") || strings.HasPrefix(reason, "有事") {
		return "hotfix"
	}
	if reason != "" {
		return "heavy"
	}
	for _, raw := range fleet.Elements(d["messages"]) {
		m := row(raw)
		if m["role"] == "user" && regexp.MustCompile(`分けないで|分割しない|分けずに|1件で`).MatchString(fleet.Text(m["text"])) {
			return "single"
		}
	}
	return ""
}
