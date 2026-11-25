package consultationai

import (
	"context"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
)

// FilingMaterials reads explicitly named cross-repository ranges under the same
// repository and path restrictions used for consultation handoff.
func FilingMaterials(ctx context.Context, client *gitea.Client, config map[string]any, draft Record) ([]any, string, error) {
	if client == nil {
		return nil, "資料取得の接続がありません", nil
	}
	d := Record{"project": draft["project"], "body": draft["body"], "messages": draft["messages"], "_ephemeral": true}
	s := &Service{options: Options{Gitea: client}}
	m, err := s.newMaterials(d, config)
	if err != nil {
		return nil, "", err
	}
	_, problem := m.prepareHandoff(ctx, d, Record{"ready": true})
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return m.handoffRecords(), problem, nil
}

// HasFilingReferences avoids changing unrelated checks when no range is named.
func HasFilingReferences(draft Record) bool {
	hasOther := func(value string) bool {
		for _, match := range handoffReference.FindAllStringSubmatch(value, -1) {
			if match[1] != fleet.Text(draft["project"]) {
				return true
			}
		}
		return false
	}
	if hasOther(fleet.Text(draft["body"])) {
		return true
	}
	for _, raw := range fleet.Elements(draft["messages"]) {
		message := fleet.Object(raw)
		if message["role"] == "user" && hasOther(fleet.Text(message["text"])) {
			return true
		}
	}
	return false
}
