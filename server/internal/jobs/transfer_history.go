package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"strings"
)

func transferFingerprints(saved, binding, expected map[string]any) []string {
	provider := fleet.Text(saved["provider"])
	if provider != "codex" && provider != "claude" {
		return nil
	}
	paths := proposalFiles(expected["changePaths"])
	version := 1
	if paths != nil {
		version = 2
	}
	out := []string{}
	for i, id := range []any{binding["id"], binding["legacyId"]} {
		title := splitTitle(fleet.Text(expected["title"]))
		if i == 1 {
			title = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(fleet.Text(expected["title"]))
		}
		p := map[string]any{"version": version, "kind": "split", "id": id, "source": saved["source"], "title": title, "purpose": expected["purpose"], "acceptance": expected["acceptance"], "dependsOn": []any{}, "provider": provider}
		if paths != nil {
			p["changeScope"] = map[string]any{"paths": paths}
		}
		raw := map[string]json.RawMessage{}
		for key, value := range p {
			raw[key], _ = json.Marshal(value)
		}
		canonical, _ := usage.OrderedRow(raw, []string{"version", "kind", "id", "source", "title", "purpose", "acceptance", "dependsOn", "provider", "changeScope"})
		sum := sha256.Sum256([]byte(canonical))
		out = append(out, hex.EncodeToString(sum[:]))
	}
	return out
}
